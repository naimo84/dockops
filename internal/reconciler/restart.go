package reconciler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/local/dockops/internal/config"
)

// POST /restart: restarting stacks without a deployment. The operator path
// behind webhook's /restart — the containers of a stack are restarted, the
// deployment is not repeated. Deliberately no secrets resolution, no shm env
// file, no pre/post_deploy hooks, no hash and no state write: a restart is
// not a deployment, and a pass right after it must not think anything
// changed.
//
// The stack details (directory, compose files, generated .env) come from the
// configuration of the last reconcile pass — the same values deployStack
// works with — so the endpoint duplicates none of the loader's path
// resolution.

// Restart restarts the containers of the selected stacks:
//
//   - names empty → every stack of the cached configuration, in
//     configuration order,
//   - names non-empty → those stacks in request order, duplicates collapsed.
//
// Unknown names are reported in unknown and nothing is restarted at all: the
// selection is validated before the first command runs, so a typo can never
// leave a half-restarted set behind. A failing stack lands in failed and does
// not stop the remaining ones — one wedged stack must not keep the others
// down. The error return is reserved for requests that cannot be served (no
// configuration loaded yet, missing restarter, cancelled context); it is a
// 500, never a per-stack result.
func (r *Reconciler) Restart(ctx context.Context, names []string) (restarted []string, failed map[string]string, unknown []string, err error) {
	configured, ok := r.currentStacks()
	if !ok {
		return nil, nil, nil, fmt.Errorf("reconciler: restart: no configuration loaded yet")
	}
	targets, unknown := restartTargets(configured, names)
	if len(unknown) > 0 {
		return nil, nil, unknown, nil
	}
	if r.restarter == nil {
		return nil, nil, nil, fmt.Errorf("reconciler: restart: no compose restarter configured")
	}
	restarted = make([]string, 0, len(targets))
	failed = make(map[string]string)
	for _, stack := range targets {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return restarted, failed, nil, fmt.Errorf("reconciler: restart aborted before stack %s: %w", stack.Name, ctxErr)
		}
		envFile := stackRestartEnvFile(stack)
		if envFile == "" {
			r.logger.Debug("restart without --env-file: no generated stack .env",
				"stack", stack.Name, "dir", stack.Dir)
		}
		if restartErr := r.restarter.Restart(ctx, stack, stack.Dir, envFile); restartErr != nil {
			r.logger.Error("stack restart failed", "stack", stack.Name, "error", restartErr)
			failed[stack.Name] = restartErr.Error()
			continue
		}
		r.logger.Info("restarted stack", "stack", stack.Name)
		restarted = append(restarted, stack.Name)
	}
	return restarted, failed, nil, nil
}

// restartTargets resolves the requested names against the configured stacks.
// An empty selection means "all" (configuration order); explicit names keep
// their request order with duplicates collapsed. Unknown names are collected
// separately so the caller can reject the whole request before running
// anything. Disabled stacks are valid targets either way: /restart is an
// operator action on what runs, not a deployment decision.
func restartTargets(configured []config.Stack, names []string) (targets []config.Stack, unknown []string) {
	if len(names) == 0 {
		return append([]config.Stack(nil), configured...), nil
	}
	byName := make(map[string]config.Stack, len(configured))
	for _, stack := range configured {
		byName[stack.Name] = stack
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		stack, ok := byName[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		targets = append(targets, stack)
	}
	return targets, unknown
}

// stackRestartEnvFile returns the --env-file of a restart: the generated
// <stackDir>/.env the last deployment wrote (env.WriteStackEnv), which is the
// file every compose call outside a deployment interpolates from. It is
// optional — a stack that never deployed through dockops has none, and a
// checkout-updating pass may have removed it — and a restart of running
// containers does not need the values, so a missing file yields "" and the
// command line omits --env-file (docker compose would otherwise fail on the
// missing path and the stack could not be restarted at all).
func stackRestartEnvFile(stack config.Stack) string {
	if stack.Dir == "" {
		return ""
	}
	path := filepath.Join(stack.Dir, ".env")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}

// storeStacks caches the stacks of a freshly loaded configuration for
// POST /restart. The slice is copied so a later reload cannot mutate the set
// a running request iterates.
func (r *Reconciler) storeStacks(stacks []config.Stack) {
	copied := append([]config.Stack(nil), stacks...)
	r.stacksMu.Lock()
	defer r.stacksMu.Unlock()
	r.stacks = copied
}

// currentStacks returns the cached stack set and whether a configuration was
// loaded at all (false before the first pass).
func (r *Reconciler) currentStacks() ([]config.Stack, bool) {
	r.stacksMu.RLock()
	defer r.stacksMu.RUnlock()
	if r.stacks == nil {
		return nil, false
	}
	return append([]config.Stack(nil), r.stacks...), true
}
