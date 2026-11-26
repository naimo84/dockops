package reconciler

import (
	"fmt"
	"sort"
)

// POST /pull: run the image update check now instead of waiting for
// global.update_check_interval. The endpoint only queues stacks; the check
// itself runs in the next reconcile pass, which the handler triggers right
// away — the same code path as the hourly check, so update_policy decides
// what happens: apply stacks (the self stack included, via its helper) pull,
// recreate and health-gate, notify stacks only report. Nothing about the
// check changes, it is merely due early.

// RequestUpdateCheck queues the selected stacks for an image update check in
// the next pass. names empty means every stack of the cached configuration.
// Unknown names are reported and nothing is queued (validated first, like
// Restart). Stacks the check never covers — disabled, or update_policy:
// pinned — land in skipped with the reason instead of failing the request.
// The error return is reserved for requests that cannot be served (no
// configuration loaded yet).
func (r *Reconciler) RequestUpdateCheck(names []string) (queued []string, skipped map[string]string, unknown []string, err error) {
	configured, ok := r.currentStacks()
	if !ok {
		return nil, nil, nil, fmt.Errorf("reconciler: pull: no configuration loaded yet")
	}
	targets, unknown := restartTargets(configured, names)
	if len(unknown) > 0 {
		return nil, nil, unknown, nil
	}
	skipped = make(map[string]string)
	queued = make([]string, 0, len(targets))
	for _, stack := range targets {
		switch {
		case !stack.Enabled:
			skipped[stack.Name] = "stack disabled"
		case !stack.ChecksImageUpdates():
			skipped[stack.Name] = "update_policy: pinned"
		default:
			queued = append(queued, stack.Name)
		}
	}
	if len(queued) == 0 {
		return queued, skipped, nil, nil
	}
	r.forcedMu.Lock()
	defer r.forcedMu.Unlock()
	if r.forcedChecks == nil {
		r.forcedChecks = make(map[string]struct{}, len(queued))
	}
	for _, name := range queued {
		r.forcedChecks[name] = struct{}{}
	}
	r.logger.Info("image update check requested", "stacks", queued)
	return queued, skipped, nil, nil
}

// takeForcedChecks drains the queued stack names. The pass that takes them
// owns them: a request arriving while the check runs lands in the fresh set
// and is served by the following pass.
func (r *Reconciler) takeForcedChecks() map[string]struct{} {
	r.forcedMu.Lock()
	defer r.forcedMu.Unlock()
	taken := r.forcedChecks
	r.forcedChecks = nil
	return taken
}

// sortedNames returns the keys of set in lexical order (for logs).
func sortedNames(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
