package reconciler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/state"
)

// Health gate and rollback constants of the phase-11 apply step.
const (
	// defaultHealthPoll is the interval the health gate polls the affected
	// services with. The reconciler field healthPoll carries it; tests
	// shrink it to a millisecond.
	defaultHealthPoll = 3 * time.Second
	// rollbackFilePattern is the name of the generated compose override in
	// the stack directory. It is written right before the rollback up and
	// removed again by a defer, so it never outlives the attempt; the name
	// carries the stack because several stacks may share a checkout
	// directory in principle.
	rollbackFilePattern = "dockops-rollback-%s.yaml"
)

// Container states and health values the health gate distinguishes. docker
// reports more (created, paused, dead, restarting, removing, …); the
// immediate failures are the ones that cannot become healthy by waiting.
const (
	containerRunning  = "running"
	containerExited   = "exited"
	containerDead     = "dead"
	containerRestart  = "restarting"
	healthHealthy     = "healthy"
	healthStarting    = "starting"
	healthUnhealthy   = "unhealthy"
	unnamedContainer  = "(unnamed container)"
	rollbackFailedTag = "rollback FAILED"
)

// applyCandidate is one image whose update is about to be applied: the
// record's map key (the image reference) plus a copy of the record.
type applyCandidate struct {
	ref string
	rec state.ImageRecord
}

// applyUpdates applies the detected image updates of one stack — pull,
// targeted up, health gate, rollback on failure — and records the outcome
// (phase 11).
//
// It is called at the end of checkImageUpdates, and only for stacks with
// update_policy: apply that were due for a check in this pass. Consequences
// that shape the implementation:
//   - the state entry must exist and be deployed: a stack whose deployment
//     failed in this very pass is skipped (its records describe an image set
//     that is not running), as is a stack that was never deployed;
//   - a record whose ApplyBlockedDigest already equals its RemoteDigest is
//     skipped: this digest failed before, and update_policy: apply degrades to
//     notify for it until the tag moves to another digest;
//   - a record without a local CurrentDigest describes a locally built image:
//     there is no registry identity to pull and no previous digest to roll
//     back to, so it is logged and skipped (never applied);
//   - every failure is logged and notified; only a state persistence failure
//     is returned, because that is the one thing the pass cannot shrug off.
func (r *Reconciler) applyUpdates(ctx context.Context, stack config.Stack, pn *passNotify) error {
	st, ok := r.state.Get(stack.Name)
	if !ok || st.Status != state.StatusDeployed {
		// A failed deployment in this pass (or a missing entry) means the
		// running containers are not the ones the records describe.
		r.logger.Debug("image update apply: stack is not deployed, skipping",
			"stack", stack.Name, "status", st.Status, "found", ok)
		return nil
	}
	recs := copyImageRecords(st.Images)
	cands := r.applyCandidates(stack.Name, recs)
	if len(cands) == 0 {
		return nil
	}
	services := candidateServices(cands)
	req := compose.RequestFor(stack, stack.Dir)
	r.logger.Info("image update apply: applying updates", "stack", stack.Name, "services", services)

	if err := r.compose.Pull(ctx, req, services); err != nil {
		r.logger.Error("image update apply: pull failed", "stack", stack.Name, "services", services, "error", err)
		text := fmt.Sprintf("%s: %s pull failed: %v", strings.Join(candidateServices(cands), ", "), candidateRefs(cands), err)
		for _, c := range cands {
			rec := recs[c.ref]
			rec.ApplyError = fmt.Sprintf("pull: %v", err)
			rec.ApplyBlockedDigest = rec.RemoteDigest
			recs[c.ref] = rec
		}
		pn.updateFailed(ctx, stack.Name, st.Hash, stagePull, text)
		return r.persistImageRecords(stack.Name, recs)
	}

	// The rollback target is remembered before anything is recreated: a
	// gate that fails must be able to name the digest the stack ran until
	// this pass.
	for _, c := range cands {
		rec := recs[c.ref]
		rec.PreviousDigest = rec.CurrentDigest
		recs[c.ref] = rec
	}

	// Targeted recreation: only the services whose image moved, without
	// their dependencies.
	stage := stageHealthGate
	var cause error
	if err := r.compose.UpOverride(ctx, req, nil, services); err != nil {
		stage = stageComposeUp
		cause = fmt.Errorf("compose up --no-deps: %w", err)
		r.logger.Error("image update apply: recreate failed", "stack", stack.Name, "services", services, "error", err)
	} else if err := r.waitHealthy(ctx, stack.Name, services, stack.EffectiveHealthTimeout()); err != nil {
		cause = err
		r.logger.Error("image update apply: health gate failed", "stack", stack.Name, "services", services, "error", err)
	}
	if cause != nil {
		return r.rollbackImages(ctx, stack, recs, cands, services, req, st.Hash, stage, cause, pn)
	}

	// Success: the tag now runs the remote digest.
	now := time.Now().UTC()
	texts := make([]string, 0, len(cands))
	for _, c := range cands {
		rec := recs[c.ref]
		previous := rec.CurrentDigest
		rec.CurrentDigest = rec.RemoteDigest
		rec.UpdateAvailable = false
		rec.AppliedAt = now
		rec.ApplyError = ""
		rec.ApplyBlockedDigest = ""
		rec.RolledBackAt = time.Time{}
		recs[c.ref] = rec
		texts = append(texts, updateText(rec.Service, c.ref, previous, rec.RemoteDigest))
	}
	r.logger.Info("image update apply: applied", "stack", stack.Name, "services", services)
	pn.updateApplied(ctx, stack.Name, st.Hash, strings.Join(texts, "; "))
	// A working apply clears the failure dedupe key, so the next failure of
	// this stack alarms immediately instead of waiting for the window.
	pn.recovery(config.NotificationUpdateFailed, stack.Name)
	return r.persistImageRecords(stack.Name, recs)
}

// rollbackImages reverts a failed apply attempt: it writes a compose
// override pinning every affected service to its PreviousDigest, recreates
// those services with it and runs the health gate again. The override file
// lives in the stack directory and is removed when this function returns.
func (r *Reconciler) rollbackImages(ctx context.Context, stack config.Stack, recs map[string]state.ImageRecord, cands []applyCandidate, services []string, req compose.Request, hash, stage string, cause error, pn *passNotify) error {
	now := time.Now().UTC()
	for _, c := range cands {
		rec := recs[c.ref]
		rec.ApplyError = cause.Error()
		// The digest that just failed is blocked whether or not the
		// rollback works: retrying it would repeat the same outage.
		rec.ApplyBlockedDigest = rec.RemoteDigest
		rec.RolledBackAt = now
		recs[c.ref] = rec
	}

	rbErr := r.rollbackUp(ctx, stack, recs, cands, services, req)
	if rbErr != nil {
		// The case that needs a human: the stack now runs neither the old
		// nor the new digest reliably.
		errText := fmt.Sprintf("%v; %s: %v", cause, rollbackFailedTag, rbErr)
		for _, c := range cands {
			rec := recs[c.ref]
			rec.ApplyError = errText
			recs[c.ref] = rec
		}
		r.logger.Error("image update apply: rollback failed", "stack", stack.Name, "services", services, "error", rbErr)
		text := fmt.Sprintf("%s: %s %s: %v", strings.Join(services, ", "), candidateRefs(cands), rollbackFailedTag, rbErr)
		pn.updateFailed(ctx, stack.Name, hash, stageRollback, text)
		return r.persistImageRecords(stack.Name, recs)
	}

	r.logger.Warn("image update apply: rolled back after a failed update",
		"stack", stack.Name, "services", services, "stage", stage, "error", cause)
	texts := make([]string, 0, len(cands))
	for _, c := range cands {
		texts = append(texts, fmt.Sprintf("%s: %s rollback to %s",
			c.rec.Service, c.ref, shortDigest(recs[c.ref].PreviousDigest)))
	}
	pn.updateFailed(ctx, stack.Name, hash, stage, strings.Join(texts, "; "))
	return r.persistImageRecords(stack.Name, recs)
}

// rollbackUp writes the rollback override and recreates the affected
// services with it, then waits for the health gate again (same window as the
// forward direction). Both steps are one unit: an override file that is
// written but not applied, or applied but unhealthy, is a failed rollback.
func (r *Reconciler) rollbackUp(ctx context.Context, stack config.Stack, recs map[string]state.ImageRecord, cands []applyCandidate, services []string, req compose.Request) error {
	doc := rollbackDocument{Services: make(map[string]rollbackService, len(cands))}
	for _, c := range cands {
		// The working copy, not the candidate snapshot: the rollback target
		// was recorded there before the up.
		rec := recs[c.ref]
		if rec.Service == "" {
			return fmt.Errorf("rollback: no service recorded for image %s", c.ref)
		}
		if rec.PreviousDigest == "" {
			return fmt.Errorf("rollback: no previous digest recorded for image %s", c.ref)
		}
		repository, err := imageRepository(c.ref)
		if err != nil {
			return fmt.Errorf("rollback: %w", err)
		}
		doc.Services[rec.Service] = rollbackService{Image: repository + "@" + rec.PreviousDigest}
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("rollback: render override: %w", err)
	}
	path := filepath.Join(stack.Dir, fmt.Sprintf(rollbackFilePattern, stack.Name))
	// 0o600: the file names image digests, nothing secret, but the stack
	// directory is a checkout and restrictive defaults cost nothing.
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("rollback: write override %s: %w", path, err)
	}
	defer os.Remove(path)

	if err := r.compose.UpOverride(ctx, req, []string{path}, services); err != nil {
		return fmt.Errorf("rollback: compose up with override: %w", err)
	}
	if err := r.waitHealthy(ctx, stack.Name, services, stack.EffectiveHealthTimeout()); err != nil {
		return fmt.Errorf("rollback: health gate: %w", err)
	}
	return nil
}

// waitHealthy polls the affected services until all of them run healthy, and
// reports why it gave up. A hard failure (see gateStatus) returns
// immediately; everything else is polled until timeout expires. timeout <= 0
// falls back to config.DefaultHealthTimeout, a zero healthPoll to
// defaultHealthPoll.
func (r *Reconciler) waitHealthy(ctx context.Context, project string, services []string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = config.DefaultHealthTimeout
	}
	poll := r.healthPoll
	if poll <= 0 {
		poll = defaultHealthPoll
	}
	deadline := time.Now().Add(timeout)
	for {
		ready, hard, soft := r.gateStatus(ctx, project, services)
		if ready {
			return nil
		}
		if hard != nil {
			return hard
		}
		if !time.Now().Before(deadline) {
			if soft != nil {
				return fmt.Errorf("health gate timed out after %s: %w", timeout, soft)
			}
			return fmt.Errorf("health gate timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health gate: %w", ctx.Err())
		case <-time.After(poll):
		}
	}
}

// gateStatus is one health gate poll. It returns ready=true when every
// affected service satisfies the gate, a hard error when the situation
// cannot heal by waiting, and a soft error describing what is still pending
// (the caller keeps polling and reports it on timeout).
//
// Success per affected service: at least one container is assigned to it
// (compose ps reports the service name), every one of those containers is
// "running" and its health is either empty (no healthcheck) or "healthy".
//
// Conservative decisions:
//   - "unhealthy", "exited", "dead" and "restarting" are hard failures:
//     they are what a broken image looks like, and waiting 90 seconds for
//     them would only delay the rollback.
//   - a service without any container is a hard failure as well, and so is
//     a ps answer whose containers carry no service name at all: an
//     unattributable answer must never read as a green gate.
//   - any other state (created, paused, removing, …) is treated as "not yet
//     ready" and polled; the timeout turns it into a failure anyway.
func (r *Reconciler) gateStatus(ctx context.Context, project string, services []string) (ready bool, hard, soft error) {
	if len(services) == 0 {
		// Defensive: an empty service list would make the loop below
		// vacuous and the gate green without having looked at anything.
		return false, fmt.Errorf("health gate: no services to check in project %s", project), nil
	}
	containers, err := r.compose.PsProject(ctx, project)
	if err != nil {
		return false, nil, fmt.Errorf("list containers: %w", err)
	}
	byService := make(map[string][]compose.Container, len(services))
	for _, c := range containers {
		if c.Service == "" {
			// Older compose versions omit the service name: the container
			// cannot be attributed, so it counts as "no container found"
			// for every service (never as a silent success).
			continue
		}
		byService[c.Service] = append(byService[c.Service], c)
	}
	var pending []string
	for _, service := range services {
		list := byService[service]
		if len(list) == 0 {
			return false, fmt.Errorf("service %s: no container in project %s", service, project), nil
		}
		for _, c := range list {
			name := c.Name
			if name == "" {
				name = unnamedContainer
			}
			if c.State != containerRunning {
				switch c.State {
				case containerExited, containerDead, containerRestart:
					return false, fmt.Errorf("service %s: container %s is %s", service, name, c.State), nil
				}
				pending = append(pending, fmt.Sprintf("service %s: container %s is %s", service, name, c.State))
				continue
			}
			switch c.Health {
			case "", healthHealthy:
			case healthStarting:
				pending = append(pending, fmt.Sprintf("service %s: container %s is starting", service, name))
			case healthUnhealthy:
				return false, fmt.Errorf("service %s: container %s is unhealthy", service, name), nil
			default:
				pending = append(pending, fmt.Sprintf("service %s: container %s reports health %q", service, name, c.Health))
			}
		}
	}
	if len(pending) > 0 {
		return false, nil, fmt.Errorf("%s", strings.Join(pending, "; "))
	}
	return true, nil, nil
}

// applyCandidates returns the records of one stack whose update is applied
// now: update available, a remote digest that is not the blocked one, and a
// known local digest (a locally built image has nothing to roll back to).
func (r *Reconciler) applyCandidates(stack string, recs map[string]state.ImageRecord) []applyCandidate {
	var out []applyCandidate
	for _, ref := range sortedKeys(recs) {
		rec := recs[ref]
		if !rec.UpdateAvailable || rec.RemoteDigest == "" {
			continue
		}
		if rec.CurrentDigest == "" {
			r.logger.Info("image update apply: locally built image, nothing to apply",
				"stack", stack, "image", ref)
			continue
		}
		if rec.ApplyBlockedDigest != "" && rec.ApplyBlockedDigest == rec.RemoteDigest {
			// Digest comparison instead of a backoff: the same broken
			// image is not retried, and a new digest is tried immediately.
			r.logger.Info("image update apply: digest already failed, staying with notify",
				"stack", stack, "image", ref, "digest", shortDigest(rec.RemoteDigest))
			continue
		}
		out = append(out, applyCandidate{ref: ref, rec: rec})
	}
	return out
}

// candidateServices returns the sorted, deduplicated service names of the
// candidates (one image may serve several services).
func candidateServices(cands []applyCandidate) []string {
	seen := make(map[string]struct{}, len(cands))
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if c.rec.Service == "" {
			continue
		}
		if _, ok := seen[c.rec.Service]; ok {
			continue
		}
		seen[c.rec.Service] = struct{}{}
		out = append(out, c.rec.Service)
	}
	sort.Strings(out)
	return out
}

// candidateRefs renders the image references of the candidates for a log or
// notification text.
func candidateRefs(cands []applyCandidate) string {
	refs := make([]string, 0, len(cands))
	for _, c := range cands {
		refs = append(refs, c.ref)
	}
	return strings.Join(refs, ", ")
}

// copyImageRecords copies the record map before the apply step mutates it:
// Store.Get hands out the stored map, and writing into it would bypass the
// store's lock and leak half-applied state to GET /status.
func copyImageRecords(recs map[string]state.ImageRecord) map[string]state.ImageRecord {
	out := make(map[string]state.ImageRecord, len(recs))
	for ref, rec := range recs {
		out[ref] = rec
	}
	return out
}

// persistImageRecords writes the mutated records back and saves the state.
// These two are the only failures the apply step returns: the caller (the
// pass) treats them like any other persistence failure, while every docker,
// registry or gate problem stays in the records and the notifications.
func (r *Reconciler) persistImageRecords(stack string, recs map[string]state.ImageRecord) error {
	if err := r.state.SetImages(stack, recs); err != nil {
		return fmt.Errorf("reconciler: record image update result of stack %s: %w", stack, err)
	}
	if err := r.state.Save(); err != nil {
		return fmt.Errorf("reconciler: persist image update result of stack %s: %w", stack, err)
	}
	return nil
}

// imageRepository strips the tag or digest from an image reference, leaving
// the repository the rollback override pins: "ghcr.io/x/app:1.2" →
// "ghcr.io/x/app", "nginx" → "nginx", "gitea.example.com:3000/a/b@sha256:…" →
// "gitea.example.com:3000/a/b". The tag separator is the last colon behind
// the last slash — the same rule registry.ParseRef uses — so a registry port
// survives; the digest form is cut at its "@" first, which is where
// registry.ParseRef splits it, too.
func imageRepository(ref string) (string, error) {
	name, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	if name == "" {
		return "", fmt.Errorf("image reference %q has no repository", ref)
	}
	return name, nil
}

// rollbackDocument is the generated override of a rollback: one image pin
// per affected service, in compose's own schema (yaml.Marshal sorts the map
// keys, so the file is deterministic).
type rollbackDocument struct {
	Services map[string]rollbackService `yaml:"services"`
}

// rollbackService is one service entry of a rollback override.
type rollbackService struct {
	Image string `yaml:"image"`
}
