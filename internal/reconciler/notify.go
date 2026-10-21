package reconciler

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/notify"
)

// Stages reported with a deploy_failed / repo_failed event; they name the
// step of the pass that failed (see notify.Event.Stage).
const (
	stageSecrets     = "secrets"
	stagePreDeploy   = "pre_deploy"
	stageComposeUp   = "compose_up"
	stagePostDeploy  = "post_deploy"
	stageConfig      = "config"
	stageRepo        = "repo"
	stageUpdateCheck = "update_check"
)

// passNotify carries the notification calls of one reconcile pass. The
// embedded notifier may be nil (no notifications block, nothing injected), in
// which case every method is a no-op. It also guarantees that a pass reports
// at most one repo_failed event, as the briefing requires: repo failures
// abort or degrade the pass, and one alarm is enough — the returned error
// lists every failing remote.
type passNotify struct {
	notifier notify.Notifier
	repoSent bool
}

// set replaces the notifier mid-pass: the pass starts with the cached one
// (failures before the root config is readable have no other source) and
// switches to the notifier built for this pass's configuration once it is
// loaded.
func (p *passNotify) set(n notify.Notifier) {
	p.notifier = n
}

// deployFailed reports a failed deployment of one stack.
func (p *passNotify) deployFailed(ctx context.Context, stack, stage, hash string, err error) {
	if p == nil || p.notifier == nil || err == nil {
		return
	}
	p.notifier.Notify(ctx, notify.Event{
		Kind:  config.NotificationDeployFailed,
		Stack: stack,
		Stage: stage,
		Err:   err.Error(),
		Hash:  hash,
	})
}

// pruneFailed reports a failed teardown of a removed stack.
func (p *passNotify) pruneFailed(ctx context.Context, stack string, err error) {
	if p == nil || p.notifier == nil || err == nil {
		return
	}
	p.notifier.Notify(ctx, notify.Event{
		Kind:  config.NotificationPruneFailed,
		Stack: stack,
		Err:   err.Error(),
	})
}

// repoFailed reports a repository or configuration failure of the pass, at
// most once per pass.
func (p *passNotify) repoFailed(ctx context.Context, remote, stage string, err error) {
	if p == nil || p.notifier == nil || err == nil || p.repoSent {
		return
	}
	p.repoSent = true
	p.notifier.Notify(ctx, notify.Event{
		Kind:   config.NotificationRepoFailed,
		Remote: remote,
		Stage:  stage,
		Err:    err.Error(),
	})
}

// updateAvailable reports that a stack's image tag now points at a new
// remote digest (phase 10). One event per new digest: the caller compares
// against the persisted record and only calls this on a change, and the
// notifier's own dedupe window is the second safety net.
func (p *passNotify) updateAvailable(ctx context.Context, stack, hash, text string) {
	if p == nil || p.notifier == nil {
		return
	}
	p.notifier.Notify(ctx, notify.Event{
		Kind:  config.NotificationUpdateAvailable,
		Stack: stack,
		Stage: stageUpdateCheck,
		Err:   text,
		Hash:  hash,
	})
}

// recovery tells the notifier that kind is healthy again for stack, clearing
// its dedupe key.
func (p *passNotify) recovery(kind, stack string) {
	if p == nil || p.notifier == nil {
		return
	}
	p.notifier.Recovery(kind, stack)
}

// notifierFor returns the notifier for the notification block of the current
// pass and rebuilds it only when the block changed. The comparison uses the
// serialised structure, so any edit (a new destination, a changed timeout)
// takes effect on the next pass without a restart. nil config means the block
// is gone: the cached notifier is dropped, so a removed block really silences
// the agent.
func (r *Reconciler) notifierFor(n *config.Notifications) notify.Notifier {
	// An injected notifier wins always — tests use it without a config
	// block; production never injects one.
	if r.injected {
		return r.notifier
	}
	if n == nil {
		r.notifier = nil
		r.notifierKey = ""
		return nil
	}
	key := notificationsKey(*n)
	if r.notifier != nil && key != "" && key == r.notifierKey {
		return r.notifier
	}
	r.notifier = r.newNotifier(*n, r.logger)
	r.notifierKey = key
	return r.notifier
}

// notificationsKey serialises a notifications block for change detection.
// json.Marshal is deterministic for this struct (map keys are sorted); an
// error is impossible for it and only means "always rebuild".
func notificationsKey(n config.Notifications) string {
	raw, err := json.Marshal(n)
	if err != nil {
		return ""
	}
	return string(raw)
}

// logNotifications emits the once-per-pass INFO line that operators use to
// verify the wiring on the hosts (docker logs). A block with enabled: false
// is loaded and validated but silent, and says so.
func (r *Reconciler) logNotifications(n *config.Notifications) {
	on := "[" + strings.Join(n.On, " ") + "]"
	if !n.Enabled {
		r.logger.Info("notifications inactive (enabled: false)", "destinations", len(n.Destinations), "on", on)
		return
	}
	r.logger.Info("notifications active", "destinations", len(n.Destinations), "on", on)
}
