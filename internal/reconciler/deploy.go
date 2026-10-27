package reconciler

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/selfupdate"
	"github.com/local/dockops/internal/state"
)

// Phase-13 deploy hand-over: the counterpart of the phase-12 image hand-over
// for every recreate of the self stack that is not an image move — a changed
// stack hash (compose file, env, profile, new service) and, most importantly,
// the first adoption of the self stack.
//
// The in-process path cannot work here either: `compose up` replaces the
// container it runs in, and with `container_name:` compose removes the old
// container before it creates the new one. A process dying in that window
// leaves the stack down — `restart:` has nothing to restart and the systemd
// unit only fires on boot. So the agent writes the plan, persists the honest
// interim state and starts the detached helper; the recreate belongs to it.

// startSelfDeploy hands the deploy of the self stack over to the detached
// helper instead of running compose up in-process.
//
// Order of the steps matters and mirrors the phase-12 hand-over: everything
// that can fail without side effects (image lookup, plan validation) happens
// before the pending marker is written; the marker is persisted before the
// helper starts, because the helper's recreate is what kills this process and
// the next start must know that a deploy is in flight.
//
// Deliberately not done here:
//   - no rollbackStackEnv on a successful hand-over: the helper reads the
//     generated <stackDir>/.env, and only a failed hand-over (nothing was
//     started) restores the previous file — see the caller;
//   - no state.Set as deployed and no deploy_failed: only the process that
//     sees the outcome may book it, and that is the next agent start (see
//     finalizeDeployPending);
//   - no post_deploy hooks: they would run while the helper recreates the
//     stack, i.e. race the recreate and die with the agent. pre_deploy runs
//     in the agent as before (it starts nothing).
func (r *Reconciler) startSelfDeploy(ctx context.Context, stack config.Stack, newHash string, services []string, pn *passNotify) error {
	// Defensive: deployStack only enters the hand-over when the image is
	// known (helperImageKnown), so an empty image here is a wiring bug — and
	// still one that must not start a helper.
	image, err := r.resolveSelfImage()
	if err != nil {
		err = fmt.Errorf("stack %s: self deploy: %w", stack.Name, err)
		r.logger.Error("self deploy: refusing to start the helper", "stack", stack.Name, "error", err)
		pn.deployFailed(ctx, stack.Name, stageComposeUp, newHash, err)
		return err
	}

	now := time.Now().UTC()
	plan := selfupdate.Plan{
		Version:              selfupdate.Version,
		Kind:                 selfupdate.KindDeploy,
		Project:              stack.Name,
		StackDir:             stack.Dir,
		ComposeFiles:         stack.EffectiveComposeFiles(),
		EnvFile:              filepath.Join(stack.Dir, ".env"),
		Services:             services,
		GraceSeconds:         selfUpdateGraceSeconds,
		HealthTimeoutSeconds: secondsAtLeastOne(stack.EffectiveHealthTimeout()),
		HealthPollSeconds:    secondsAtLeastOne(r.healthPoll),
		Profiles:             stack.Profiles,
		DeployHash:           newHash,
	}
	// The plan is validated before anything is persisted: a plan the helper
	// would reject is a bug on this side, and it must not leave a pending
	// marker behind that blocks the stack until the finalization timeout.
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("stack %s: self deploy: invalid plan: %w", stack.Name, err)
	}

	if err := r.setDeployPending(stack, newHash, now); err != nil {
		return fmt.Errorf("stack %s: self deploy: %w", stack.Name, err)
	}

	stateDir := filepath.Dir(r.state.Path())
	planPath := selfupdate.PlanPath(stateDir)
	if err := selfupdate.WriteAtomic(planPath, plan); err != nil {
		r.logger.Error("self deploy: cannot write the plan", "stack", stack.Name, "path", planPath, "error", err)
		return r.abandonSelfDeploy(ctx, stack.Name, newHash, pn, fmt.Errorf("write plan: %w", err))
	}

	helperArgs, err := selfupdate.HelperArgs(plan, image, stateDir, fmt.Sprintf("%d", now.Unix()), r.selfContainer, r.selfMounts...)
	if err != nil {
		r.logger.Error("self deploy: cannot build the helper command", "stack", stack.Name, "error", err)
		return r.abandonSelfDeploy(ctx, stack.Name, newHash, pn, err)
	}
	if err := r.runHelper(ctx, helperArgs); err != nil {
		r.logger.Error("self deploy: cannot start the helper", "stack", stack.Name, "error", err)
		return r.abandonSelfDeploy(ctx, stack.Name, newHash, pn, fmt.Errorf("start helper: %w", err))
	}

	r.logger.Info("self deploy: helper started",
		"stack", stack.Name, "hash", newHash, "services", services, "image", image,
		"container", selfupdate.HelperName(plan, fmt.Sprintf("%d", now.Unix())))
	return nil
}

// abandonSelfDeploy rolls a failed deploy hand-over back: the pending marker
// is cleared and persisted again, the failure is notified and returned. The
// recreate never happened, so the next pass may try again immediately (the
// hash is unchanged and the recorded status is not "deployed").
func (r *Reconciler) abandonSelfDeploy(ctx context.Context, stack, newHash string, pn *passNotify, cause error) error {
	if err := r.clearDeployPending(stack); err != nil {
		// The marker is what keeps the stack untouched; if it cannot be
		// cleared, the error must say both things.
		cause = fmt.Errorf("%w (clearing the deploy pending failed: %v)", cause, err)
	}
	err := fmt.Errorf("stack %s: self deploy: %w", stack, cause)
	pn.deployFailed(ctx, stack, stageComposeUp, newHash, err)
	return err
}

// setDeployPending persists the hand-over: the stack hash whose deploy is now
// the helper's business plus its timestamp. Everything else the entry carries
// is preserved — the deploy traces are refreshed from the current stack so a
// pass dying right here still prunes correctly.
func (r *Reconciler) setDeployPending(stack config.Stack, newHash string, at time.Time) error {
	st, ok := r.state.Get(stack.Name)
	if !ok {
		// First adoption: there is no previous entry. An in-flight attempt
		// is not a deployment, so the status is failed — the next pass must
		// retry instead of mistaking the entry for a deployed stack.
		st = state.State{Status: state.StatusFailed}
	}
	st.DeployPendingHash = newHash
	st.DeployPendingAt = at
	st.StackDir = stack.Dir
	st.ComposeFile = stack.ComposeFile
	st.ComposeFiles = stack.ComposeFiles
	st.UpdatePolicy = stack.EffectiveUpdatePolicy()
	if err := r.state.Set(stack.Name, st); err != nil {
		return fmt.Errorf("record the deploy hand-over: %w", err)
	}
	if err := r.state.Save(); err != nil {
		return fmt.Errorf("persist the deploy hand-over: %w", err)
	}
	return nil
}

// clearDeployPending removes the deploy pending marker and persists the
// store. A missing entry is not an error: nothing can be pending without one.
func (r *Reconciler) clearDeployPending(stack string) error {
	st, ok := r.state.Get(stack)
	if !ok {
		return nil
	}
	st.DeployPendingHash = ""
	st.DeployPendingAt = time.Time{}
	if err := r.state.Set(stack, st); err != nil {
		return fmt.Errorf("clear the deploy pending: %w", err)
	}
	if err := r.state.Save(); err != nil {
		return fmt.Errorf("persist the cleared deploy pending: %w", err)
	}
	return nil
}

// resolveSelfImage returns the image the deploy helper starts from. A deploy
// plan has no target image, so the helper runs the agent's own image — the
// build that is known to work on this host.
//
// The value comes from Deps.SelfImage, which cmd/dockops fills at startup:
// DOCKOPS_SELF_IMAGE when set, otherwise the answer of
// `docker inspect <self container> --format '{{.Config.Image}}'`. The lookup
// deliberately does NOT happen here: it needs the docker daemon, and a
// reconciler that shells out to docker for its own image would behave
// differently on every host that happens to run a container with the same
// name as the machine it runs on (tests included).
//
// An empty value is never used to start a helper: a helper with a wrong or
// missing image would deploy the stack with a version the agent does not have.
func (r *Reconciler) resolveSelfImage() (string, error) {
	if image := strings.TrimSpace(r.selfImage); image != "" {
		return image, nil
	}
	return "", fmt.Errorf("the helper image is unknown (set %s, or let the agent detect its own image)", envSelfImage)
}

// helperImageKnown reports whether a deploy helper can be started at all. It
// is the gate of the phase-13 hand-over: without an image the deploy stays
// in-process (see deployStack) — the phase-12 image path of the same pass
// reads the records that deploy writes, so refusing it outright would also
// stall the update path.
func (r *Reconciler) helperImageKnown() bool {
	return strings.TrimSpace(r.selfImage) != ""
}

// envSelfImage is the environment variable that overrides the detected agent
// image for deploy helpers. Kept here (not only in cmd/dockops) because the
// reconciler is where a missing value becomes an error, and the message must
// name the way out.
const envSelfImage = "DOCKOPS_SELF_IMAGE"
