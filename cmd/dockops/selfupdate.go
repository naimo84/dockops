// Phase-12 helper mode: the detached container that recreates the stack
// dockops itself runs in.
//
// It is the same image, started with -selfupdate-helper -plan <path>, and it
// owns the part of a self update the agent cannot do: recreate its own stack,
// watch the health gate and roll back. Everything it needs is in the plan
// file; everything it decides is in the result file and the append-only log
// next to it, because after a failed recreate the helper is the only
// diagnosis source left.
//
// Two plan kinds share this mode (phase 13): an image move (kind empty,
// phase 12: targeted up with --no-deps, health gate, rollback) and a full
// deploy (kind=deploy: a normal up, no gate, no rollback). The arguments of
// both come from internal/compose, so the helper cannot drift away from what
// the agent would have run in-process.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/selfupdate"
)

// Container states and health values the helper's gate distinguishes — the
// same vocabulary the in-process gate uses (internal/reconciler apply.go).
const (
	helperContainerRunning   = "running"
	helperContainerExited    = "exited"
	helperContainerDead      = "dead"
	helperContainerRestart   = "restarting"
	helperHealthHealthy      = "healthy"
	helperHealthStarting     = "starting"
	helperHealthUnhealthy    = "unhealthy"
	helperStateFormat        = "{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}"
	helperDockerBinary       = "docker"
	helperComposeCommandName = "compose"
)

// helper executes one plan. The command runner is a field so tests exercise
// the whole sequence — grace, up, gate, rollback, result — without a docker
// daemon, exactly like the reconciler injects its runHelper.
type helper struct {
	plan     selfupdate.Plan
	stateDir string
	logger   *slog.Logger
	log      io.Writer
	// run executes a docker command line (without the binary) and returns
	// its stdout; nil selects exec.CommandContext with helperDockerBinary.
	run func(ctx context.Context, args []string) ([]byte, error)
	// sleep waits for d; nil selects time.After with ctx cancellation.
	// Tests replace it so the grace period costs no wall time.
	sleep func(ctx context.Context, d time.Duration) error
	// now is the clock of the result timestamp; nil selects time.Now.
	now func() time.Time
}

// runSelfUpdateHelper is the entire -selfupdate-helper mode: read the plan,
// execute it, write the result, exit. No reconciler, no HTTP server, no
// repository access.
func runSelfUpdateHelper(ctx context.Context, planPath string, logger *slog.Logger) error {
	if planPath == "" {
		return fmt.Errorf("-selfupdate-helper requires -selfupdate-plan")
	}
	plan, err := selfupdate.ReadPlan(planPath)
	if err != nil {
		return err
	}
	stateDir := filepath.Dir(planPath)
	logFile, err := openHelperLog(selfupdate.HelperLogPath(stateDir))
	if err != nil {
		return err
	}
	defer logFile.Close()

	h := &helper{
		plan:     plan,
		stateDir: stateDir,
		logger:   logger,
		log:      logFile,
		run:      runDockerCommand,
		sleep:    sleepCtx,
		now:      time.Now,
	}
	return h.runPlan(ctx)
}

// openHelperLog opens the append-only helper log. 0o600 like every other file
// of the self-update pair: paths and digests, nothing secret, restrictive by
// default.
func openHelperLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("self update helper: open log %s: %w", path, err)
	}
	return f, nil
}

// runPlan is the helper's whole lifecycle: grace, forward attempt, rollback
// if needed, result file. The returned error only reports a failure to write
// the result — a failed update is a *result*, not a process error, so the
// agent's finalization can read it.
func (h *helper) runPlan(ctx context.Context) error {
	h.logf("helper started: project=%s kind=%s stack_dir=%s services=%v targets=%d",
		h.plan.Project, h.plan.Kind, h.plan.StackDir, h.plan.Services, len(h.plan.Targets))

	// The grace period lets the agent finish its pass (state save, log) and
	// stop writing before its container disappears.
	if err := h.sleep(ctx, time.Duration(h.plan.GraceSeconds)*time.Second); err != nil {
		return err
	}

	// Phase 13: a deploy is the other kind of recreate — same hand-over, no
	// health gate, no rollback (there is no previous digest to return to).
	if h.plan.Kind == selfupdate.KindDeploy {
		return h.runDeploy(ctx)
	}

	res := selfupdate.Result{Version: selfupdate.Version, At: h.now().UTC()}
	if err := h.composeUp(ctx, nil); err != nil {
		h.logf("compose up failed: %v", err)
		h.rollback(ctx, &res, fmt.Errorf("compose up: %w", err))
	} else if err := h.healthGate(ctx); err != nil {
		h.logf("health gate failed: %v", err)
		h.rollback(ctx, &res, err)
	} else {
		res.Status = selfupdate.StatusOK
		// The target digest, not an inspect result: the plan says what this
		// attempt was about, and the agent compares against its record.
		res.Digest = h.targetDigest()
		h.logf("health gate passed: status=%s digest=%s", res.Status, res.Digest)
	}

	return h.writeResult(res)
}

// runDeploy executes a phase-13 deploy plan: one normal compose up of the
// stack, exactly the command line compose.Up would have run in-process
// (profiles, env file, positional services, dependencies resolved — no
// --no-deps), and a result that reports ok or failed.
//
// Deliberately no health gate and no rollback: the in-process deploy has no
// gate either (parity), and a deploy has no previous digest to roll back to —
// a broken compose file would fail the rollback with the same error that
// broke the up.
func (h *helper) runDeploy(ctx context.Context) error {
	res := selfupdate.Result{
		Version:    selfupdate.Version,
		Kind:       selfupdate.KindDeploy,
		DeployHash: h.plan.DeployHash,
		At:         h.now().UTC(),
	}
	args, err := deployUpArgs(h.plan)
	if err == nil {
		h.logf("running: %s %s", helperDockerBinary, strings.Join(args, " "))
		var out []byte
		out, err = h.exec(ctx, args)
		if err == nil {
			if text := strings.TrimSpace(string(out)); text != "" {
				h.logf("compose output: %s", text)
			}
		}
	}
	if err != nil {
		res.Status = selfupdate.StatusFailed
		res.Error = err.Error()
		h.logf("deploy failed: %v", err)
	} else {
		res.Status = selfupdate.StatusOK
		h.logf("deploy finished: status=%s hash=%s", res.Status, res.DeployHash)
	}
	return h.writeResult(res)
}

// deployUpArgs builds the deploy command line through internal/compose: the
// plan is a compose.Request plus profiles, env file and services, so the
// helper runs the same flag set as the agent's own Up instead of a second,
// slowly drifting copy of it.
func deployUpArgs(p selfupdate.Plan) ([]string, error) {
	return compose.UpArgs(compose.Request{
		Project:      p.Project,
		StackDir:     p.StackDir,
		ComposeFiles: p.ComposeFiles,
	}, p.Profiles, p.EnvFile, false, p.Services)
}

// writeResult persists the helper's verdict and reports a write failure as a
// process error (the agent's finalization can only read what exists).
func (h *helper) writeResult(res selfupdate.Result) error {
	resultPath := selfupdate.ResultPath(h.stateDir)
	if err := selfupdate.WriteAtomic(resultPath, res); err != nil {
		h.logf("cannot write result: %v", err)
		return err
	}
	h.logf("helper finished: status=%s error=%q", res.Status, res.Error)
	return nil
}

// targetDigest returns the digest the attempt targets; the plan usually has
// exactly one target (one self stack per host).
func (h *helper) targetDigest() string {
	if len(h.plan.Targets) == 0 {
		return ""
	}
	return h.plan.Targets[0].Digest
}

// rollback reverts the failed attempt: write the override that pins every
// service to its previous digest, up again (override after the stack files,
// so it wins), gate again. The outcome distinguishes "the old image is back"
// from "neither digest is reliable" — the latter is the case for a human.
func (h *helper) rollback(ctx context.Context, res *selfupdate.Result, cause error) {
	res.Digest = h.targetDigest()
	fail := func(stage string, err error) {
		res.Status = selfupdate.StatusRollbackFailed
		res.Error = fmt.Sprintf("%v; %s: %s: %v", cause, selfupdate.RollbackFailedTag, stage, err)
		h.logf("rollback FAILED at %s: %v", stage, err)
	}

	raw, err := selfupdate.RollbackOverride(h.plan)
	if err != nil {
		fail("override", err)
		return
	}
	path := filepath.Join(h.plan.StackDir, selfupdate.RollbackFileName(h.plan.Project))
	// 0o600 like the agent's rollback file: digests, nothing secret.
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		fail("write override", err)
		return
	}
	defer os.Remove(path)

	if err := h.composeUp(ctx, []string{path}); err != nil {
		fail("compose up", err)
		return
	}
	if err := h.healthGate(ctx); err != nil {
		fail("health gate", err)
		return
	}
	res.Status = selfupdate.StatusRolledBack
	res.Error = cause.Error()
	h.logf("rollback succeeded: status=%s", res.Status)
}

// composeUpArgs builds the compose up command line:
//
//	compose -p <project> -f <file>... [--env-file <env>] up -d --no-deps <service>...
//
// override files follow the stack's own files (compose merge rules: the later
// file wins), --no-deps keeps the recreation to the named services, and
// --remove-orphans stays out on purpose: a self update must not prune
// anything the agent did not ask about.
func composeUpArgs(p selfupdate.Plan, overrideFiles []string) []string {
	args := []string{helperComposeCommandName, "-p", p.Project}
	for _, f := range p.ComposeFiles {
		args = append(args, "-f", resolveStackPath(p.StackDir, f))
	}
	for _, f := range overrideFiles {
		args = append(args, "-f", resolveStackPath(p.StackDir, f))
	}
	if p.EnvFile != "" {
		args = append(args, "--env-file", p.EnvFile)
	}
	args = append(args, "up", "-d", "--no-deps")
	args = append(args, p.Services...)
	return args
}

// resolveStackPath joins a relative plan path against the stack directory,
// the same rule internal/compose applies: the loaded configuration normally
// carries absolute paths, but the helper must not depend on that — its
// working directory is not the stack directory.
func resolveStackPath(stackDir, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(stackDir, path)
}

// composeUp runs one compose up for the plan.
func (h *helper) composeUp(ctx context.Context, overrideFiles []string) error {
	args := composeUpArgs(h.plan, overrideFiles)
	h.logf("running: %s %s", helperDockerBinary, strings.Join(args, " "))
	out, err := h.exec(ctx, args)
	if err != nil {
		return err
	}
	if text := strings.TrimSpace(string(out)); text != "" {
		h.logf("compose output: %s", text)
	}
	return nil
}

// healthGate polls the plan's containers until all are healthy, a hard
// failure appears or the timeout expires. The plan's timeouts are seconds;
// a zero (impossible after Validate) falls back to one poll.
func (h *helper) healthGate(ctx context.Context) error {
	timeout := time.Duration(h.plan.HealthTimeoutSeconds) * time.Second
	poll := time.Duration(h.plan.HealthPollSeconds) * time.Second
	if poll <= 0 {
		poll = time.Second
	}
	targets := h.gateTargets()
	if len(targets) == 0 {
		return fmt.Errorf("health gate: plan has neither containers nor services")
	}
	deadline := time.Now().Add(timeout)
	for {
		ready, hard, soft := h.gateStatus(ctx, targets)
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
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("health gate: %w", err)
		}
		if err := h.sleep(ctx, poll); err != nil {
			return fmt.Errorf("health gate: %w", err)
		}
	}
}

// gateTargets returns the containers to check: the plan's container names,
// or the service names when the plan carries none (the agent could not list
// containers). The resolution then happens per target in gateStatus.
func (h *helper) gateTargets() []string {
	if len(h.plan.Containers) > 0 {
		return h.plan.Containers
	}
	return h.plan.Services
}

// gateStatus is one poll over every target: ready when all are running and
// healthy (or running without a healthcheck), a hard error for the states
// that cannot heal by waiting, a soft error describing the rest.
func (h *helper) gateStatus(ctx context.Context, targets []string) (bool, error, error) {
	var pending []string
	for _, target := range targets {
		state, health, err := h.inspect(ctx, target)
		if err != nil {
			return false, nil, err
		}
		if state != helperContainerRunning {
			switch state {
			case helperContainerExited, helperContainerDead, helperContainerRestart:
				return false, fmt.Errorf("container %s is %s", target, state), nil
			}
			pending = append(pending, fmt.Sprintf("container %s is %s", target, state))
			continue
		}
		switch health {
		case "", helperHealthHealthy:
		case helperHealthStarting:
			pending = append(pending, fmt.Sprintf("container %s is starting", target))
		case helperHealthUnhealthy:
			return false, fmt.Errorf("container %s is unhealthy", target), nil
		default:
			pending = append(pending, fmt.Sprintf("container %s reports health %q", target, health))
		}
	}
	if len(pending) > 0 {
		return false, nil, fmt.Errorf("%s", strings.Join(pending, "; "))
	}
	return true, nil, nil
}

// inspect reads state and health of one target. The plan's containers are
// names, so inspect works directly; a service-name target (the fallback) is
// first resolved to its container IDs through `compose ps -q`, because
// inspecting a service name would only ever fail.
func (h *helper) inspect(ctx context.Context, target string) (state, health string, err error) {
	out, err := h.exec(ctx, []string{"inspect", "--format", helperStateFormat, target})
	if err == nil {
		return parseHelperState(string(out))
	}
	ids, idErr := h.serviceContainers(ctx, target)
	if idErr != nil {
		// Report the original inspect failure: it names the target the plan
		// asked about, while the service lookup failing twice would only
		// obscure it.
		return "", "", err
	}
	var lastErr error
	for _, id := range ids {
		out, idErr = h.exec(ctx, []string{"inspect", "--format", helperStateFormat, id})
		if idErr != nil {
			lastErr = idErr
			continue
		}
		state, health, parseErr := parseHelperState(string(out))
		if parseErr != nil {
			lastErr = parseErr
			continue
		}
		// One running container per service is the normal case; if a
		// service has several, the first usable one answers. A stricter
		// "all must be healthy" would need a service→container list the
		// plan does not carry.
		return state, health, nil
	}
	if lastErr != nil {
		return "", "", lastErr
	}
	return "", "", fmt.Errorf("container %s: no inspectable container found", target)
}

// serviceContainers resolves a service name to its container IDs.
func (h *helper) serviceContainers(ctx context.Context, service string) ([]string, error) {
	args := []string{helperComposeCommandName, "-p", h.plan.Project}
	for _, f := range h.plan.ComposeFiles {
		args = append(args, "-f", resolveStackPath(h.plan.StackDir, f))
	}
	args = append(args, "ps", "-q", service)
	out, err := h.exec(ctx, args)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ids = append(ids, line)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("service %s: no container", service)
	}
	return ids, nil
}

// parseHelperState splits one inspect answer ("running|healthy").
func parseHelperState(out string) (state, health string, err error) {
	line := strings.TrimSpace(out)
	if line == "" {
		return "", "", fmt.Errorf("docker inspect returned no state")
	}
	state, health, _ = strings.Cut(line, "|")
	if state = strings.TrimSpace(state); state == "" {
		return "", "", fmt.Errorf("docker inspect returned an empty state (%q)", line)
	}
	return state, strings.TrimSpace(health), nil
}

// exec runs one docker command line through the injected runner.
func (h *helper) exec(ctx context.Context, args []string) ([]byte, error) {
	if h.run != nil {
		return h.run(ctx, args)
	}
	return runDockerCommand(ctx, args)
}

// runDockerCommand is the production runner: the docker CLI, stdout only
// (callers parse it), stderr folded into the error, both bounded.
func runDockerCommand(ctx context.Context, args []string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, helperDockerBinary, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(stderr.String() + stdout.String())
		if len(text) > 512 {
			text = text[:512] + "..."
		}
		if text != "" {
			return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, text)
		}
		return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// logf writes one line to the helper log and the agent's slog. The file is
// the point: the container is started with --rm, so its docker logs are gone
// exactly when somebody needs them.
func (h *helper) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if h.log != nil {
		fmt.Fprintf(h.log, "%s %s\n", h.now().UTC().Format(time.RFC3339), line)
	}
	if h.logger != nil {
		h.logger.Info("self update helper: "+line, "project", h.plan.Project)
	}
}
