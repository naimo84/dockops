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
// what is running. For stacks with update_policy: notify or pinned it deploys
// nothing; for update_policy: apply it hands the detected updates to
// applyUpdates at the end of the step (phase 11), which pulls, recreates,
// health-gates and — if need be — rolls back.
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
//   - The apply step runs only for stacks whose records this pass actually
//     refreshed, so an unreadable compose configuration cannot trigger an
//     apply based on yesterday's picture.
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
	// recorded collects the stacks whose records were refreshed in this
	// pass. Only they are candidates for the phase-11 apply step: applying
	// against records that this pass could not re-check would act on a
	// stale picture of the stack.
	recorded := make(map[string]struct{}, len(due))
	for _, stack := range due {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("reconciler: image update check aborted: %w", err))
			break
		}
		st, _ := r.state.Get(stack.Name)
		resolved, err := r.compose.Images(ctx, compose.RequestFor(stack, stack.Dir))
		if err != nil {
			// The compose configuration could not be resolved (broken
			// file, docker problem, an env_file only the daemon's
			// namespace can serve): never invent records — but do not
			// swallow the blindness either. Without the stored reason such
			// a stack is indistinguishable from "never checked" in
			// GET /status, which is exactly the stack an operator must
			// see. The warning stays for the log stream.
			r.logger.Warn("image update check: cannot resolve stack images", "stack", stack.Name, "error", err)
			if serr := r.state.SetImageCheckError(stack.Name, err.Error(), now); serr != nil {
				r.logger.Error("image update check: cannot record the unresolved configuration", "stack", stack.Name, "error", serr)
				continue
			}
			changed = true
			continue
		}
		// Non-portable absolute bind sources (issue #18, suggestion 3):
		// reported here because this is the only step that reads the
		// compose files without deploying, and the finding belongs next to
		// the image situation in GET /status. Purely informational — the
		// deploy already succeeded or failed on its own terms.
		r.recordNonPortableBinds(stack)
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
		recorded[stack.Name] = struct{}{}
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
	// Phase 11: apply the detected updates of the stacks configured for it.
	// This runs at the end of the check — the records of this pass are the
	// input, and they are already persisted — and only in a pass that was
	// due for a check, which bounds the whole apply step to one attempt per
	// update_check_interval. Every failure stays in the records and the
	// notifications; only a state persistence failure is returned.
	for _, stack := range due {
		if !stack.AppliesImageUpdates() {
			continue
		}
		if _, ok := recorded[stack.Name]; !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			r.logger.Warn("image update apply: aborted", "stack", stack.Name, "error", err)
			break
		}
		if err := r.applyUpdates(ctx, stack, pn); err != nil {
			errs = append(errs, err)
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
		// The phase-11 apply bookkeeping survives a re-check: this record is
		// rebuilt from scratch on every check, and dropping
		// ApplyBlockedDigest would retry a failed update on every pass.
		rec.PreviousDigest = prev.PreviousDigest
		rec.AppliedAt = prev.AppliedAt
		rec.RolledBackAt = prev.RolledBackAt
		rec.ApplyBlockedDigest = prev.ApplyBlockedDigest
		rec.ApplyError = prev.ApplyError
		// The phase-12 self-update marker survives a re-check as well: a
		// check that runs while the helper is still working must not drop
		// the proof that a recreate is in flight (it would allow a second
		// plan). It is cleared by the finalization, not by the check.
		rec.ApplyPendingDigest = prev.ApplyPendingDigest
		rec.PendingAt = prev.PendingAt
		if !rec.UpdateAvailable {
			// Nothing is pending, so a recorded apply failure is history:
			// the tag moved back, or the image was updated by other means.
			rec.ApplyBlockedDigest = ""
			rec.ApplyError = ""
		}
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
	// A failed resolve counts as an attempt: without it a stack whose
	// configuration cannot be read would run one compose call on every
	// poll (5m) instead of once per interval.
	if st.ImageCheckErrorAt.After(newest) {
		newest = st.ImageCheckErrorAt
	}
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

// recordNonPortableBinds refreshes the stored bind findings of one stack.
// Failures degrade to a warning: a bind source that cannot be classified must
// not stop the image check, and the last known findings stay until a later
// pass can read the files again.
func (r *Reconciler) recordNonPortableBinds(stack config.Stack) {
	roots := r.resolveBindRoots(stack)
	if len(roots) == 0 {
		// Host mode (no container namespace to translate) or an unreadable
		// mount list: there is nothing to compare against, and an empty
		// roots list deliberately means "on the host everything is
		// portable". Leave the state alone rather than flag everything.
		return
	}
	findings, err := compose.NonPortableBinds(stack.EffectiveComposeFiles(), stack.Dir, roots)
	if err != nil {
		r.logger.Warn("non-portable bind check: cannot read compose files", "stack", stack.Name, "error", err)
		return
	}
	if err := r.state.SetNonPortableBinds(stack.Name, findings); err != nil {
		r.logger.Warn("non-portable bind check: cannot record findings", "stack", stack.Name, "error", err)
		return
	}
	if len(findings) > 0 {
		r.logger.Warn("non-portable bind sources: outside every mount of the container, the daemon may mount an empty directory",
			"stack", stack.Name, "sources", findings)
	}
}

// resolveBindRoots returns the agent's mount mappings for the non-portable
// check, reading them at most once per pass. A read failure degrades to nil
// for this pass: a missing mapping must never turn into "every absolute bind
// is non-portable", which would be a false alarm on every stacked host.
func (r *Reconciler) resolveBindRoots(stack config.Stack) []compose.BindRoot {
	if r.bindRoots != nil {
		return r.bindRoots
	}
	type mountReader interface {
		OwnMounts(ctx context.Context, name string) ([]compose.BindRoot, error)
	}
	reader, ok := r.compose.(mountReader)
	if !ok || r.selfContainer == "" {
		// Injected test double or no container: nothing to read.
		r.bindRoots = []compose.BindRoot{}
		return nil
	}
	roots, err := reader.OwnMounts(context.Background(), r.selfContainer)
	if err != nil {
		r.logger.Warn("non-portable bind check: cannot read the agent's own mounts", "container", r.selfContainer, "error", err)
		r.bindRoots = []compose.BindRoot{}
		return nil
	}
	r.bindRoots = roots
	return roots
}
