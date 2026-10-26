package reconciler

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/selfupdate"
	"github.com/local/dockops/internal/state"
)

// Phase-12 self update: defaults of the hand-over plan.
const (
	// selfUpdateGraceSeconds is how long the helper waits before its first
	// compose call. It exists so the agent can finish its pass (state save,
	// log line) instead of being killed in the middle of it; ten seconds is
	// short enough not to delay a nightly update visibly.
	selfUpdateGraceSeconds = 10
	// helperOutputLimit bounds the command output embedded into a helper
	// error (same spirit as the compose executor's bound).
	helperOutputLimit = 512
)

// startSelfUpdate is the phase-12 alternative to the in-process apply: it
// records the intended move, writes the helper plan and starts the detached
// helper container, then leaves the rest to it.
//
// Called from applyUpdates after a successful pull and before any UpOverride,
// so the stack is untouched when this returns: the helper is the only actor
// that recreates anything. Consequences that shape the implementation:
//   - the state is persisted before the helper starts. The agent's own
//     container is what the helper replaces, so this is the last moment the
//     process can write an honest interim state; without it the next agent
//     start would have no idea an update was in flight;
//   - ApplyPendingDigest doubles as the duplicate guard. A second pass that
//     runs while the helper is still working (poll, webhook) finds it set and
//     starts nothing — one helper per update is the whole safety story;
//   - a failure to write the plan or start the helper rolls the pending
//     markers back and leaves ApplyError behind, so a broken hand-over is
//     retried on the next pass instead of blocking the digest forever. The
//     digest is deliberately not blocked here: nothing was recreated;
//   - no update_applied / update_failed event: only the process that knows
//     the outcome may report it, and that is the next agent start (see
//     finalizeSelfUpdate).
func (r *Reconciler) startSelfUpdate(ctx context.Context, stack config.Stack, recs map[string]state.ImageRecord, cands []applyCandidate, services []string, req compose.Request) error {
	for _, ref := range sortedKeys(recs) {
		if rec := recs[ref]; rec.ApplyPendingDigest != "" {
			r.logger.Debug("self update: update already handed to a helper, skipping",
				"stack", stack.Name, "image", ref, "digest", shortDigest(rec.ApplyPendingDigest))
			return nil
		}
	}

	now := time.Now().UTC()
	targets := make([]selfupdate.Target, 0, len(cands))
	handed := make([]string, 0, len(cands))
	for _, c := range cands {
		repository, err := imageRepository(c.ref)
		if err != nil {
			// No repository means no rollback override; the helper could
			// not recover from a bad image. Leave the record alone (the
			// next check reports it again) and skip this image.
			r.logger.Error("self update: cannot derive the repository, skipping image",
				"stack", stack.Name, "image", c.ref, "error", err)
			continue
		}
		rec := recs[c.ref]
		rec.PreviousDigest = rec.CurrentDigest
		rec.ApplyPendingDigest = rec.RemoteDigest
		rec.PendingAt = now
		// A new attempt starts clean: the digest is not blocked, it is
		// being applied by somebody else.
		rec.ApplyError = ""
		recs[c.ref] = rec
		handed = append(handed, c.ref)
		targets = append(targets, selfupdate.Target{
			Image:      c.ref,
			Repository: repository,
			Digest:     rec.RemoteDigest,
			Previous:   rec.PreviousDigest,
			Service:    rec.Service,
		})
	}
	if len(targets) == 0 {
		return nil
	}

	plan := selfupdate.Plan{
		Version:              selfupdate.Version,
		Project:              stack.Name,
		StackDir:             stack.Dir,
		ComposeFiles:         req.ComposeFiles,
		EnvFile:              filepath.Join(stack.Dir, ".env"),
		Services:             services,
		Containers:           r.selfUpdateContainers(ctx, stack.Name, services),
		GraceSeconds:         selfUpdateGraceSeconds,
		HealthTimeoutSeconds: secondsAtLeastOne(stack.EffectiveHealthTimeout()),
		HealthPollSeconds:    secondsAtLeastOne(r.healthPoll),
		Targets:              targets,
	}
	if err := plan.Validate(); err != nil {
		// A programming error, not an operational one: nothing was started,
		// so roll the pending markers back and report it in the records.
		return r.abandonSelfUpdate(stack.Name, recs, handed, fmt.Errorf("invalid plan: %w", err))
	}

	// Honest interim state first: from here on the process may die at any
	// moment (the helper's recreate is what kills it).
	if err := r.persistImageRecords(stack.Name, recs); err != nil {
		return err
	}

	stateDir := filepath.Dir(r.state.Path())
	planPath := selfupdate.PlanPath(stateDir)
	if err := selfupdate.WriteAtomic(planPath, plan); err != nil {
		r.logger.Error("self update: cannot write the plan", "stack", stack.Name, "path", planPath, "error", err)
		return r.abandonSelfUpdate(stack.Name, recs, handed, fmt.Errorf("write plan: %w", err))
	}

	helperArgs, err := selfupdate.HelperArgs(plan, targets[0].Image, stateDir, fmt.Sprintf("%d", now.Unix()), r.selfContainer)
	if err != nil {
		// Nothing was started: the pending markers still have to go.
		r.logger.Error("self update: cannot build the helper command", "stack", stack.Name, "error", err)
		return r.abandonSelfUpdate(stack.Name, recs, handed, err)
	}
	if err := r.runHelper(ctx, helperArgs); err != nil {
		r.logger.Error("self update: cannot start the helper", "stack", stack.Name, "error", err)
		return r.abandonSelfUpdate(stack.Name, recs, handed, fmt.Errorf("start helper: %w", err))
	}

	r.logger.Info("self update: helper started",
		"stack", stack.Name, "services", services, "image", targets[0].Image,
		"container", selfupdate.HelperName(plan, fmt.Sprintf("%d", now.Unix())))
	return nil
}

// abandonSelfUpdate rolls a failed hand-over back: the pending markers are
// cleared (no helper will ever report for them) and the cause is recorded as
// ApplyError. The digest stays unblocked — nothing was recreated, so the next
// pass may try again.
func (r *Reconciler) abandonSelfUpdate(stack string, recs map[string]state.ImageRecord, refs []string, cause error) error {
	for _, ref := range refs {
		rec := recs[ref]
		rec.ApplyPendingDigest = ""
		rec.PendingAt = time.Time{}
		rec.ApplyError = "self update: " + cause.Error()
		recs[ref] = rec
	}
	if err := r.persistImageRecords(stack, recs); err != nil {
		return err
	}
	return nil
}

// selfUpdateContainers resolves the health-gate targets of the plan: the
// container names of the services being recreated. A failed or empty listing
// is not fatal — the plan just carries no names and the helper falls back to
// the service names (see cmd/dockops' helper). The lookup is read-only; the
// gate itself belongs to the helper.
func (r *Reconciler) selfUpdateContainers(ctx context.Context, project string, services []string) []string {
	containers, err := r.compose.PsProject(ctx, project)
	if err != nil {
		r.logger.Warn("self update: cannot list containers for the health gate; the helper will resolve them itself",
			"stack", project, "error", err)
		return nil
	}
	want := make(map[string]struct{}, len(services))
	for _, s := range services {
		want[s] = struct{}{}
	}
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		if c.Name == "" {
			continue
		}
		if _, ok := want[c.Service]; !ok {
			continue
		}
		names = append(names, c.Name)
	}
	return names
}

// secondsAtLeastOne converts a duration to whole seconds, rounding up and
// never returning 0: the plan's timeouts must satisfy Validate's "> 0", and
// a sub-second poll (the tests) would otherwise produce an unusable plan.
func secondsAtLeastOne(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	secs := int((d + time.Second - 1) / time.Second)
	return secs
}

// finalizeSelfUpdate book-keeps a self update the previous agent process
// handed to a helper. It runs at the very start of every pass — before the
// repository update, because the state file is the only thing that survives
// the recreate and the pending marker must not outlive its outcome.
//
// The result file is the primary source. The two fallbacks exist because the
// helper can die harder than it writes: if it vanished without a result but
// the tag already points at the target digest, the recreate worked; if it
// still points at the previous digest, the recreate never took effect and the
// digest is blocked so the next pass does not repeat the same failed attempt
// forever. Anything else (no result, an unreadable digest, a digest that is
// neither) stays pending — the helper may simply still be working.
//
// The digest comparison uses the same source as the update check
// (compose.ImageDigest on the reference), never docker inspect on our own
// container: the tag is what the stack will run after the recreate.
//
// Every error is logged and swallowed: finalization is bookkeeping, and a
// bookkeeping problem must never cost the pass. Notifications go through the
// cached passNotify of the previous pass, because this runs before the root
// configuration (and with it this pass's notifier) is readable.
func (r *Reconciler) finalizeSelfUpdate(ctx context.Context) {
	if r.state == nil {
		return
	}
	project := selfupdate.Project(r.selfProject)
	st, ok := r.state.Get(project)
	if !ok {
		return
	}
	stateDir := filepath.Dir(r.state.Path())
	resultPath := selfupdate.ResultPath(stateDir)

	pending := make([]string, 0, len(st.Images))
	for _, ref := range sortedKeys(st.Images) {
		if st.Images[ref].ApplyPendingDigest != "" {
			pending = append(pending, ref)
		}
	}

	res, err := selfupdate.ReadResult(resultPath)
	haveResult := err == nil
	switch {
	case haveResult:
	case selfupdate.IsNotExist(err):
		// The normal second case: no helper result (yet).
		r.logger.Debug("self update: no result file", "stack", project, "path", resultPath)
	default:
		r.logger.Error("self update: unreadable result file; falling back to the digest comparison",
			"stack", project, "path", resultPath, "error", err)
	}

	if len(pending) == 0 {
		if haveResult {
			// A result without a pending record is a leftover the state
			// cannot use (the state was replaced, the update was finalized
			// already). Remove it so it is not reported forever.
			r.logger.Warn("self update: result file without a pending update, removing", "path", resultPath)
			r.removeResult(resultPath)
		}
		return
	}

	recs := copyImageRecords(st.Images)
	pn := &passNotify{notifier: r.notifier}
	now := time.Now().UTC()
	if haveResult {
		r.applySelfUpdateResult(ctx, project, st, recs, pending, res, pn, now)
	} else {
		r.fallbackSelfUpdate(ctx, project, st, recs, pending, pn, now)
	}
}

// applySelfUpdateResult books the verdict of a finished helper run and
// deletes the result file. It mirrors the in-process apply (phase 11): a
// success clears the failure bookkeeping, a rollback blocks the digest that
// failed and keeps the error text, a failed rollback adds the marker that
// says a human must look.
func (r *Reconciler) applySelfUpdateResult(ctx context.Context, project string, st state.State, recs map[string]state.ImageRecord, refs []string, res selfupdate.Result, pn *passNotify, now time.Time) {
	switch res.Status {
	case selfupdate.StatusOK:
		texts := make([]string, 0, len(refs))
		for _, ref := range refs {
			rec := recs[ref]
			digest := res.Digest
			if digest == "" {
				// A helper may omit the digest on success; the record knows
				// which digest it was applying.
				digest = rec.ApplyPendingDigest
			}
			rec.CurrentDigest = digest
			rec.UpdateAvailable = false
			rec.AppliedAt = now
			rec.ApplyError = ""
			rec.ApplyBlockedDigest = ""
			rec.RolledBackAt = time.Time{}
			rec.ApplyPendingDigest = ""
			rec.PendingAt = time.Time{}
			recs[ref] = rec
			texts = append(texts, updateText(rec.Service, ref, rec.PreviousDigest, digest))
		}
		r.logger.Info("self update: helper applied the update", "stack", project, "digest", shortDigest(res.Digest))
		pn.updateApplied(ctx, project, st.Hash, strings.Join(texts, "; "))
		// A working self update clears the failure dedupe key, exactly like
		// a working in-process apply.
		pn.recovery(config.NotificationUpdateFailed, project)

	case selfupdate.StatusRolledBack, selfupdate.StatusRollbackFailed:
		stage := stageHealthGate
		errText := strings.TrimSpace(res.Error)
		if errText == "" {
			errText = "self update: helper reported " + res.Status + " without an error text"
		}
		if res.Status == selfupdate.StatusRollbackFailed {
			stage = stageRollback
			// The same marker the in-process path uses, so an alert reads
			// identically no matter which path produced it.
			errText += "; " + selfupdate.RollbackFailedTag
		}
		texts := make([]string, 0, len(refs))
		for _, ref := range refs {
			rec := recs[ref]
			digest := res.Digest
			if digest == "" {
				digest = rec.ApplyPendingDigest
			}
			// The digest that failed is blocked whether or not the rollback
			// worked: retrying it would repeat the same outage.
			rec.ApplyBlockedDigest = digest
			rec.ApplyError = errText
			rec.ApplyPendingDigest = ""
			rec.PendingAt = time.Time{}
			if res.Status == selfupdate.StatusRolledBack {
				rec.RolledBackAt = now
			}
			recs[ref] = rec
			texts = append(texts, fmt.Sprintf("%s: %s: %s", rec.Service, ref, errText))
		}
		if res.Status == selfupdate.StatusRolledBack {
			r.logger.Warn("self update: helper rolled back after a failed update", "stack", project, "error", res.Error)
		} else {
			r.logger.Error("self update: helper rollback FAILED", "stack", project, "error", res.Error)
		}
		pn.updateFailed(ctx, project, st.Hash, stage, strings.Join(texts, "; "))
	}

	if err := r.persistImageRecords(project, recs); err != nil {
		// Keep the result file: the state was not written, so the verdict
		// must survive to the next pass instead of being lost.
		r.logger.Error("self update: cannot persist the finalized state", "stack", project, "error", err)
		return
	}
	r.removeResult(selfupdate.ResultPath(filepath.Dir(r.state.Path())))
}

// fallbackSelfUpdate handles the case without a result file: either the
// helper died after a successful recreate, or it never got that far.
func (r *Reconciler) fallbackSelfUpdate(ctx context.Context, project string, st state.State, recs map[string]state.ImageRecord, refs []string, pn *passNotify, now time.Time) {
	var (
		succeeded []string
		failed    []string
		changed   bool
	)
	for _, ref := range refs {
		rec := recs[ref]
		running, err := r.compose.ImageDigest(ctx, ref)
		if err != nil {
			r.logger.Warn("self update: cannot inspect the running digest; keeping the update pending",
				"stack", project, "image", ref, "error", err)
			continue
		}
		switch {
		case running != "" && running == rec.ApplyPendingDigest:
			// The recreate worked; only the helper's reporting did not.
			r.logger.Warn("self update: no result file, digest matches target",
				"stack", project, "image", ref, "digest", shortDigest(running))
			rec.CurrentDigest = running
			rec.UpdateAvailable = false
			rec.AppliedAt = now
			rec.ApplyError = ""
			rec.ApplyBlockedDigest = ""
			rec.RolledBackAt = time.Time{}
			rec.ApplyPendingDigest = ""
			rec.PendingAt = time.Time{}
			recs[ref] = rec
			succeeded = append(succeeded, ref)
			changed = true
		case running != "" && running == rec.PreviousDigest:
			// Nothing moved: the stack still runs the digest it ran
			// before. Block the target so the next pass does not repeat
			// the same attempt forever.
			rec.ApplyBlockedDigest = rec.ApplyPendingDigest
			rec.ApplyError = "self update: helper left no result and the stack still runs the previous digest"
			rec.ApplyPendingDigest = ""
			rec.PendingAt = time.Time{}
			recs[ref] = rec
			failed = append(failed, ref)
			changed = true
		default:
			// Neither digest (or none at all): the helper may still be
			// recreating. Leave the record pending and look again next
			// pass — that is the only safe reading.
			r.logger.Warn("self update: no result file and the running digest is neither target nor previous; staying pending",
				"stack", project, "image", ref, "running", shortDigest(running),
				"target", shortDigest(rec.ApplyPendingDigest), "previous", shortDigest(rec.PreviousDigest))
		}
	}
	if !changed {
		return
	}
	if len(succeeded) > 0 {
		texts := make([]string, 0, len(succeeded))
		for _, ref := range succeeded {
			rec := recs[ref]
			texts = append(texts, updateText(rec.Service, ref, rec.PreviousDigest, rec.CurrentDigest))
		}
		pn.updateApplied(ctx, project, st.Hash, strings.Join(texts, "; "))
		pn.recovery(config.NotificationUpdateFailed, project)
	}
	if len(failed) > 0 {
		texts := make([]string, 0, len(failed))
		for _, ref := range failed {
			rec := recs[ref]
			texts = append(texts, fmt.Sprintf("%s: %s: the stack still runs %s",
				rec.Service, ref, shortDigest(rec.PreviousDigest)))
		}
		// stageComposeUp: the up never took effect, not the gate.
		pn.updateFailed(ctx, project, st.Hash, stageComposeUp, strings.Join(texts, "; "))
	}
	if err := r.persistImageRecords(project, recs); err != nil {
		r.logger.Error("self update: cannot persist the fallback state", "stack", project, "error", err)
	}
}

// removeResult deletes a consumed result file; a failure is logged and left
// to the next pass (which then treats the file as orphaned and retries).
func (r *Reconciler) removeResult(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		r.logger.Warn("self update: cannot remove the result file", "path", path, "error", err)
	}
}

// runDockerHelper is the production implementation of Reconciler.runHelper:
// it runs the helper container through the plain docker CLI. The compose
// executor's binary stays its own business; docker run is not compose, and
// "docker" is the same default compose.New uses.
func runDockerHelper(ctx context.Context, args []string) error {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if len(text) > helperOutputLimit {
			text = text[:helperOutputLimit] + "..."
		}
		if text != "" {
			return fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, text)
		}
		return fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
