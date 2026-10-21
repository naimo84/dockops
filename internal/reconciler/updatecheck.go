package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/registry"
	"github.com/local/dockops/internal/state"
)

// locallyBuiltImage is the recorded reason of an image without a repository
// digest. A build-only service has no registry identity, so there is nothing
// to compare and nothing to alarm about.
const locallyBuiltImage = "locally built image"

// checkImageUpdates compares the deployed images of every due stack with the
// digest their registry currently reports and records the outcome in the
// state (phase 10). It runs after the deployments — the comparison is about
// what is running — and deploys nothing: pulling or recreating is a later
// phase's job.
//
// Safety properties that shape the implementation:
//   - update_check_interval 0 disables the whole step; stacks with
//     update_policy pinned are skipped; a stack is only checked once per
//     interval (newest record timestamp), so the 5-minute poll never turns
//     into a registry hammer.
//   - A registry problem (rate limit, auth, missing tag, network) is stored
//     per image and logged — it never fails the pass and never raises an
//     update alarm.
//   - A locally built image (no RepoDigest) records "locally built image"
//     and no alarm.
//   - A stack without any checkable image (build-only services) has no
//     records to carry a check time, so it is resolved again on every pass:
//     one `docker compose config` call, never registry traffic.
//
// Only state persistence failures are returned to the caller.
func (r *Reconciler) checkImageUpdates(ctx context.Context, cfg *config.Config, pn *passNotify) []error {
	interval := cfg.Global.EffectiveUpdateCheckInterval()
	if interval <= 0 {
		r.logger.Debug("image update check disabled (update_check_interval: 0)")
		return nil
	}

	now := time.Now().UTC()
	var due []config.Stack
	for _, stack := range cfg.Stacks {
		if !stack.Enabled || !stack.ChecksImageUpdates() {
			continue
		}
		st, ok := r.state.Get(stack.Name)
		if !ok {
			// No deployment recorded yet: nothing was compared, nothing to
			// report (the stack deploys on this or a later pass and gets
			// its records then).
			continue
		}
		if !updateDue(st, interval, now) {
			r.logger.Debug("image update check: not due yet", "stack", stack.Name, "interval", interval.String())
			continue
		}
		due = append(due, stack)
	}
	if len(due) == 0 {
		return nil
	}

	resolver := r.registryFor(cfg.Registries)
	var (
		errs    []error
		images  int
		pending int
		changed bool
	)
	for _, stack := range due {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("reconciler: image update check aborted: %w", err))
			break
		}
		st, _ := r.state.Get(stack.Name)
		resolved, err := r.compose.Images(ctx, compose.RequestFor(stack, stack.Dir))
		if err != nil {
			// The compose configuration could not be resolved (broken
			// file, docker problem): degrade to "not checked this round"
			// instead of inventing records.
			r.logger.Warn("image update check: cannot resolve stack images", "stack", stack.Name, "error", err)
			continue
		}
		recs := r.checkStackImages(ctx, stack, resolved, st, resolver, now, pn)
		if len(recs) == 0 {
			r.logger.Debug("image update check: stack has no checkable images", "stack", stack.Name)
			continue
		}
		if err := r.state.SetImages(stack.Name, recs); err != nil {
			r.logger.Error("image update check: cannot record results", "stack", stack.Name, "error", err)
			continue
		}
		changed = true
		images += len(recs)
		for _, rec := range recs {
			if rec.UpdateAvailable {
				pending++
			}
		}
	}
	if changed {
		// Persist right away: the records carry the due date of the next
		// check, and a pass that dies before the final save must not turn
		// into one that queries the registries again on every restart.
		if err := r.state.Save(); err != nil {
			errs = append(errs, fmt.Errorf("reconciler: persist image update records: %w", err))
		}
	}
	r.logger.Info("image update check finished", "stacks", len(due), "images", images, "updates", pending)
	return errs
}

// checkStackImages compares the checkable images of one stack and returns the
// records. st supplies the
// previous records: the false→true transition (or a new remote digest next to
// an old one) is what turns into a notification, so an unchanged remote
// digest stays silent across passes.
func (r *Reconciler) checkStackImages(ctx context.Context, stack config.Stack, images map[string]string, st state.State, resolver registry.Resolver, now time.Time, pn *passNotify) map[string]state.ImageRecord {
	recs := make(map[string]state.ImageRecord, len(images))
	notified := make(map[string]struct{}, len(images))

	for _, service := range checkableServices(stack, images) {
		ref := images[service]
		rec := state.ImageRecord{Service: service, CheckedAt: now}

		local, err := r.compose.ImageDigest(ctx, ref)
		if err != nil {
			// No local digest means no comparison: record why and stay
			// silent, exactly like an unreadable registry answer.
			rec.Error = fmt.Sprintf("image inspect: %v", err)
			r.logger.Warn("image update check: cannot inspect local image", "stack", stack.Name, "service", service, "image", ref, "error", err)
			recs[ref] = rec
			continue
		}
		if local == "" {
			rec.Error = locallyBuiltImage
			r.logger.Debug("image update check: locally built image, nothing to compare", "stack", stack.Name, "service", service, "image", ref)
			recs[ref] = rec
			continue
		}
		rec.CurrentDigest = local

		remote, err := resolver.RemoteDigest(ctx, ref)
		if err != nil {
			rec.Error = err.Error()
			r.logger.Warn("image update check: registry lookup failed", "stack", stack.Name, "service", service, "image", ref, "error", err)
			recs[ref] = rec
			continue
		}
		rec.RemoteDigest = remote
		rec.UpdateAvailable = remote != "" && remote != local

		prev := st.Images[ref]
		switch {
		case rec.UpdateAvailable && prev.RemoteDigest != remote:
			// New digest (including the very first check that already finds
			// a newer image): one event. The recorded digest makes the next
			// pass silent even before the notifier's dedupe window applies.
			if _, ok := notified[ref]; !ok {
				notified[ref] = struct{}{}
				pn.updateAvailable(ctx, stack.Name, st.Hash, updateText(service, ref, local, remote))
			}
		case !rec.UpdateAvailable && prev.UpdateAvailable:
			// Back in sync (the image was pulled, the stack redeployed, or
			// the tag moved back): drop the notifier's dedupe key so the
			// next update alarms immediately.
			pn.recovery(config.NotificationUpdateAvailable, stack.Name)
		}
		recs[ref] = rec
	}
	return recs
}

// checkableServices returns the services of the resolved image map that the
// update check looks at, sorted: ignore_services always drops, and a stack
// with an explicit services list is limited to it — the same effective set
// docker compose up deploys.
func checkableServices(stack config.Stack, images map[string]string) []string {
	names := make([]string, 0, len(images))
	for name := range images {
		names = append(names, name)
	}
	sort.Strings(names)

	allowed := make(map[string]struct{}, len(stack.Services))
	for _, name := range stack.Services {
		allowed[name] = struct{}{}
	}
	ignored := make(map[string]struct{}, len(stack.IgnoreServices))
	for _, name := range stack.IgnoreServices {
		ignored[name] = struct{}{}
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := ignored[name]; ok {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[name]; !ok {
				continue
			}
		}
		out = append(out, name)
	}
	return out
}

// updateDue reports whether a stack's image records are old enough for the
// next check. The newest CheckedAt of the stack's records is the basis, so
// all images of a stack are checked together once per interval.
func updateDue(st state.State, interval time.Duration, now time.Time) bool {
	var newest time.Time
	for _, rec := range st.Images {
		if rec.CheckedAt.After(newest) {
			newest = rec.CheckedAt
		}
	}
	if newest.IsZero() {
		return true
	}
	return !now.Before(newest.Add(interval))
}

// updateText is the short text of an update_available event:
// "<service>: <image> <current> -> <remote>". The digests are shortened to
// their algorithm and the first characters — a 64-character digest pair would
// bury the message in every channel.
func updateText(service, image, current, remote string) string {
	return fmt.Sprintf("%s: %s %s -> %s", service, image, shortDigest(current), shortDigest(remote))
}

// shortDigest shortens a digest to "sha256:<12 hex chars>"; anything shorter
// stays as it is.
func shortDigest(digest string) string {
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok || len(hex) <= 12 {
		return digest
	}
	return algo + ":" + hex[:12]
}

// registryFor returns the resolver for the configured registry credentials.
// An injected resolver always wins (tests). Otherwise the client is built
// once and rebuilt only when the credentials change, so its per-host token
// cache survives across passes.
func (r *Reconciler) registryFor(creds []config.Registry) registry.Resolver {
	if r.injectedReg {
		return r.registry
	}
	key := registriesKey(creds)
	if r.registry != nil && key == r.registryKey {
		return r.registry
	}
	r.registry = r.newRegistry(creds, r.logger)
	r.registryKey = key
	return r.registry
}

// registriesKey serialises the registries block for change detection.
// json.Marshal is deterministic for this struct; an error only means
// "rebuild".
func registriesKey(creds []config.Registry) string {
	if len(creds) == 0 {
		return ""
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		return ""
	}
	return string(raw)
}

// registryCredentials converts the configuration entries into the registry
// package's credential type (the two packages stay independent).
func registryCredentials(creds []config.Registry) []registry.Credentials {
	out := make([]registry.Credentials, 0, len(creds))
	for _, c := range creds {
		out = append(out, registry.Credentials{Host: c.Host, Username: c.Username, Password: c.Password})
	}
	return out
}
