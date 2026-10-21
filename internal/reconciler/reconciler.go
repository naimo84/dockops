// Package reconciler wires all dockops building blocks — git checkouts,
// configuration loading, secret resolution, hooks and docker compose — into
// one reconciliation pass that makes reality match the declarative
// configuration.
//
// A pass (Reconcile) is idempotent: git checkouts are updated with
// clone-or-update, unchanged stacks are skipped via their deterministic
// hash, and docker compose up is convergent. Failures are collected per
// stack and returned joined, so one broken stack never prevents the others
// from being reconciled. Reconcile is not safe for concurrent use — the CLI
// loop serializes passes, matching state.Store and vault.Client.
//
// Open points in the briefing are decided conservatively and documented at
// the decision sites; the two most prominent:
//
//   - The host repository is bootstrapped below Deps.WorkDir (the config
//     default when unset), while the remotes live below global.work_dir,
//     resolved exactly the way config.Load resolves it. With the default
//     configuration both are the same directory, so host and remotes share
//     one checkout root as the briefing describes. Configurable checkout
//     paths (global.host_checkout, remotes.<name>.checkout) then move the
//     effective checkouts: the bootstrap host checkout below the work dir
//     only carries the root config, and dockops adopts the existing clones
//     at their configured locations in place (adopt-or-clone via
//     git.Manager.EnsureRepoAt).
//   - Post-deploy hook failures are logged AND collected in the pass error
//     ("errors.Join aller aufgetretenen Fehler"), but the stack still
//     counts as deployed, exactly as the briefing prescribes.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/env"
	"github.com/local/dockops/internal/git"
	"github.com/local/dockops/internal/hash"
	"github.com/local/dockops/internal/hooks"
	"github.com/local/dockops/internal/notify"
	"github.com/local/dockops/internal/registry"
	"github.com/local/dockops/internal/secrets"
	"github.com/local/dockops/internal/sops"
	"github.com/local/dockops/internal/state"
	"github.com/local/dockops/internal/vault"
)

const (
	// HostRepoName is the checkout name of the primary host repository; the
	// root configuration is read from <workDir>/host/stacks.yaml. Exported
	// so the CLI can point other collaborators (GET /status' host commit)
	// at the same checkout.
	HostRepoName = "host"

	// rootConfigFile is the fixed file name of the root configuration in
	// the host repository root (stacks.yaml, plural — distinct from the
	// per-stack stack.yaml the include globs discover).
	rootConfigFile = "stacks.yaml"

	// hookTimeout is passed to the hook engine per command; 0 makes the
	// engine apply its own default (5m). There is no per-hook timeout
	// configuration in the schema yet — conservative: engine default.
	hookTimeout = 0 * time.Second
)

// GitManager is the subset of git.Manager the reconciler needs: guaranteed
// checkouts — by name below the manager's work dir, or at an absolute
// configured path — and the commit they point at.
type GitManager interface {
	// EnsureRepo guarantees that <work dir>/<name> is a checkout of url at
	// the latest commit of branch and returns the local path.
	EnsureRepo(name, url, branch string) (string, error)

	// EnsureRepoAt guarantees that the checkout at the absolute path is a
	// checkout of url at the latest commit of branch (adopt-or-clone, also
	// outside the work directory) and returns the path.
	EnsureRepoAt(path, url, branch string) (string, error)

	// HeadCommit returns the commit currently checked out at repoPath.
	HeadCommit(repoPath string) (string, error)
}

// ComposeExecutor is the subset of compose.Executor the reconciler needs.
type ComposeExecutor interface {
	// Up brings a stack up detached (compose.Executor.Up).
	Up(ctx context.Context, stack config.Stack, stackDir, envFile string) error

	// Down tears a stack down (compose.Executor.Down).
	Down(ctx context.Context, stack config.Stack, stackDir, envFile string) error

	// DownProject tears a project down by name only, without a compose
	// file (compose.Executor.DownProject).
	DownProject(ctx context.Context, project string) error

	// Images returns the resolved service→image map of a stack, build-only
	// services omitted (compose.Executor.Images; phase-10 update check).
	Images(ctx context.Context, req compose.Request) (map[string]string, error)

	// ImageDigest returns the local repository digest of an image
	// reference, "" when it has none (built locally) — compose.Executor's
	// ImageDigest.
	ImageDigest(ctx context.Context, ref string) (string, error)
}

// HookRunner is the subset of hooks.Engine the reconciler needs.
type HookRunner interface {
	// Run executes commands sequentially via sh -c (hooks.Engine.Run).
	Run(ctx context.Context, commands []string, dir string, env []string, timeout time.Duration) error
}

// VaultClient is the subset of vault.Client the reconciler needs.
type VaultClient interface {
	// Login authenticates the client (vault.Client.Login).
	Login(ctx context.Context) error

	// ReadKV reads a KV v2 secret (vault.Client.ReadKV).
	ReadKV(ctx context.Context, path string) (map[string]string, error)
}

// SopsDecryptFunc decrypts one sops-encrypted dotenv file; it matches the
// signature of sops.DecryptFileKeyfile.
type SopsDecryptFunc func(ctx context.Context, path, ageKeyfile string) (map[string]string, error)

// VaultFactory builds a Vault client for the given configuration; it
// matches the signature of vault.New.
type VaultFactory func(cfg config.Vault, logger *slog.Logger) (VaultClient, error)

// Compile-time assertions that the real implementations satisfy the
// injection interfaces, and that the resolver below satisfies the interface
// env.MergeStackEnv consumes.
var (
	_ GitManager       = (*git.Manager)(nil)
	_ ComposeExecutor  = (*compose.Executor)(nil)
	_ HookRunner       = (*hooks.Engine)(nil)
	_ VaultClient      = (*vault.Client)(nil)
	_ secrets.Resolver = (*stackResolver)(nil)
)

// Deps are the collaborators of a Reconciler. Git, Compose, Hooks, Sops and
// NewVault are injection points so tests run without the git, docker, sops
// and vault binaries; nil selects the real implementations from the
// corresponding internal packages.
type Deps struct {
	// RepoURL and Branch identify the primary host repository; the CLI
	// supplies them via -repo and -branch.
	RepoURL string
	Branch  string

	// WorkDir bootstraps the host checkout below <WorkDir>/host. Empty
	// selects config.DefaultWorkDir — the same value the config loader
	// defaults global.work_dir to, so with an unset work_dir the host and
	// all remotes share one directory.
	WorkDir string

	// State persists deployment hashes and prune metadata (required).
	State *state.Store

	// Logger; nil falls back to slog.Default().
	Logger *slog.Logger

	// OnConfigLoaded, when set, is called with the fully loaded
	// configuration at the start of every successful pass — the CLI uses
	// it to pick up global.poll_interval changes.
	OnConfigLoaded func(cfg *config.Config)

	// Git, nil → git.Manager below WorkDir.
	Git GitManager

	// Compose, nil → compose.Executor with the default binary.
	Compose ComposeExecutor

	// Hooks, nil → hooks.Engine.
	Hooks HookRunner

	// Sops, nil → sops.DecryptFileKeyfile when SopsSidecarURL is also
	// empty, else the sidecar resolver's decrypt function. An explicitly
	// injected Sops wins over the sidecar URL (tests control the decrypt
	// path that way).
	Sops SopsDecryptFunc

	// SopsSidecarURL switches sops_files decryption from the local sops
	// binary to the decrypt sidecar at this URL (flag -sops-sidecar, env
	// DOCKOPS_SOPS_SIDECAR_URL). Empty keeps the local exec fallback. It
	// has no effect while Sops is injected.
	SopsSidecarURL string

	// NewVault, nil → vault.New. Called at most once per pass, and only
	// when a stack actually has vault paths or vault templates.
	NewVault VaultFactory

	// Notifier, when set, receives the reconcile-failure events, wins over
	// NewNotifier and is used even without a notifications block (tests
	// inject a recording notifier that way). Regular runs leave it nil.
	Notifier notify.Notifier

	// NewNotifier builds the notifier for a notifications block; nil →
	// notify.New. It is only called when the configuration actually has a
	// block and Notifier is unset, and again only when the block changed.
	NewNotifier func(config.Notifications, *slog.Logger) notify.Notifier

	// Registry resolves remote image digests for the phase-10 update check.
	// When set it wins over NewRegistry and is used as-is (tests count the
	// lookups that way).
	Registry registry.Resolver

	// NewRegistry builds the resolver for the configured registries block;
	// nil → registry.New. Only called when Registry is unset and a pass
	// actually has a due stack, and rebuilt only when the credentials
	// change.
	NewRegistry func([]config.Registry, *slog.Logger) registry.Resolver
}

// Reconciler executes reconciliation passes over the declarative
// configuration. Build it with New and run Reconcile per pass.
type Reconciler struct {
	repoURL  string
	branch   string
	workDir  string
	state    *state.Store
	logger   *slog.Logger
	onConfig func(*config.Config)
	git      GitManager
	compose  ComposeExecutor
	hooks    HookRunner
	sops     SopsDecryptFunc
	newVault VaultFactory

	// Notification collaborators: an injected notifier wins (injected
	// records that fact), otherwise the notifier is built lazily from the
	// first pass that carries a notifications block and rebuilt whenever
	// that block changes (notifierKey holds its serialised form).
	notifier    notify.Notifier
	injected    bool
	newNotifier func(config.Notifications, *slog.Logger) notify.Notifier
	notifierKey string

	// Registry collaborators of the image update check: an injected
	// resolver wins, otherwise the registry client is built from the
	// root configuration's registries block and rebuilt when that block
	// changes (registryKey holds its serialised form).
	registry    registry.Resolver
	injectedReg bool
	newRegistry func([]config.Registry, *slog.Logger) registry.Resolver
	registryKey string
}

// New returns a Reconciler for deps, applying the defaults documented on
// Deps. Requirement violations (empty repo URL or branch, nil state store)
// are deliberately not rejected here — New has no error return per the
// briefing — but Reconcile reports them at the start of every pass.
func New(deps Deps) *Reconciler {
	workDir := deps.WorkDir
	if workDir == "" {
		workDir = config.DefaultWorkDir
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	composeExec := deps.Compose
	if composeExec == nil {
		composeExec = compose.New(logger, "")
	}
	hookRunner := deps.Hooks
	if hookRunner == nil {
		hookRunner = hooks.New(logger)
	}
	sopsFn := deps.Sops
	if sopsFn == nil {
		if deps.SopsSidecarURL != "" {
			// Sidecar mode: decrypt via the HTTP service instead of the
			// local sops binary (fleet reality: the sops credentials live in
			// the sidecar container, not in the dockops image).
			sopsFn = sops.NewSidecar(deps.SopsSidecarURL, nil).DecryptFileKeyfile
		} else {
			sopsFn = sops.DecryptFileKeyfile
		}
	}
	vaultFactory := deps.NewVault
	if vaultFactory == nil {
		vaultFactory = func(cfg config.Vault, logger *slog.Logger) (VaultClient, error) {
			return vault.New(cfg, logger)
		}
	}
	newNotifier := deps.NewNotifier
	if newNotifier == nil {
		newNotifier = func(cfg config.Notifications, logger *slog.Logger) notify.Notifier {
			return notify.New(cfg, logger, nil)
		}
	}
	newRegistry := deps.NewRegistry
	if newRegistry == nil {
		newRegistry = func(creds []config.Registry, logger *slog.Logger) registry.Resolver {
			return registry.New(registryCredentials(creds), logger)
		}
	}
	return &Reconciler{
		repoURL:     deps.RepoURL,
		branch:      deps.Branch,
		workDir:     filepath.Clean(workDir),
		state:       deps.State,
		logger:      logger,
		onConfig:    deps.OnConfigLoaded,
		git:         deps.Git,
		compose:     composeExec,
		hooks:       hookRunner,
		sops:        sopsFn,
		newVault:    vaultFactory,
		notifier:    deps.Notifier,
		injected:    deps.Notifier != nil,
		newNotifier: newNotifier,
		registry:    deps.Registry,
		injectedReg: deps.Registry != nil,
		newRegistry: newRegistry,
	}
}

// Reconcile runs one complete pass:
//
//  1. clone-or-update the host repository and read the root stacks.yaml,
//  2. clone-or-update every configured remote,
//  3. load the full configuration (local discovery + import merge),
//  4. deploy every enabled stack whose hash changed (secrets, env file,
//     pre_deploy hooks, compose up, post_deploy hooks, state — persisted
//     per stack immediately, so an interrupted pass cannot leave deployed
//     stacks unrecorded),
//  5. prune stacks that disappeared from the configuration, and
//  6. persist the state file.
//
// Failures are collected per step or stack and returned joined (nil when
// all is well); the pass is idempotent and aborts early when ctx is
// cancelled. Bootstrapping failures (host repo, root config) abort the
// whole pass since nothing downstream can run without them.
//
// Failures are also reported through the configured notifier
// (deploy_failed, prune_failed, repo_failed); the notifier's own problems
// are logged by internal/notify and never change the pass result.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reconciler: %w", err)
	}
	var errs []error

	// Notifications: the pass starts with the notifier of the previous pass
	// (an injected one always), so a failure before the root configuration is
	// readable can still be reported. Once the root config is loaded the
	// notifier is (re)built for this pass and logged once (see
	// logNotifications).
	pn := &passNotify{notifier: r.notifier}

	// 1. Primary host repository. The bootstrap is deliberately path-fixed
	// (Henne-Ei): the root configuration that may declare global.
	// host_checkout lives in this very checkout, so it must be readable
	// before any configured path can take effect.
	gitMgr := r.git
	if gitMgr == nil {
		m, err := git.New(r.workDir, r.logger)
		if err != nil {
			return fmt.Errorf("reconciler: git manager: %w", err)
		}
		gitMgr = m
	}
	hostPath, err := gitMgr.EnsureRepo(HostRepoName, r.repoURL, r.branch)
	if err != nil {
		err = fmt.Errorf("reconciler: host repo: %w", err)
		pn.repoFailed(ctx, HostRepoName, stageRepo, err)
		return err
	}
	rootFile := filepath.Join(hostPath, rootConfigFile)
	root, err := config.LoadRoot(rootFile)
	if err != nil {
		err = fmt.Errorf("reconciler: %w", err)
		pn.repoFailed(ctx, HostRepoName, stageConfig, err)
		return err
	}

	// The root configuration is readable from here on, so the notifier
	// reflects this pass's channels (rebuilt only when the block changed) and
	// announces itself once — the verification signal on the hosts
	// (docker logs).
	pn.set(r.notifierFor(root.Notifications))
	if root.Notifications != nil {
		r.logNotifications(root.Notifications)
	}

	// global.host_checkout relocates the effective host checkout away from
	// the bootstrap path: dockops adopts (or clones) the checkout at the
	// configured path in place and reads the full configuration — local
	// stacks included — from there. The bootstrap checkout below the work
	// dir then only carries the root config; it stays where it is (it may
	// even be an empty or stale clone — that hurts nothing) and is updated
	// again on the next pass. When host_checkout equals the bootstrap path
	// the ensure above already did the identical work.
	if hc := root.Global.HostCheckout; hc != "" && filepath.Clean(hc) != filepath.Clean(hostPath) {
		hostPath, err = gitMgr.EnsureRepoAt(hc, r.repoURL, r.branch)
		if err != nil {
			err = fmt.Errorf("reconciler: host repo: %w", err)
			pn.repoFailed(ctx, HostRepoName, stageRepo, err)
			return err
		}
		rootFile = filepath.Join(hostPath, rootConfigFile)
	}

	// 2. Remotes: each at its configured checkout path
	// (remotes.<name>.checkout) or below global.work_dir, resolved the same
	// way config.Load resolves it (relative against the effective host
	// repository root), so the imports are found exactly where the loader
	// looks for them. The git manager deliberately stays on the bootstrap
	// work dir (it owns it, MkdirAll etc.); EnsureRepoAt accepts absolute
	// paths anywhere on the host, so neither a differing work_dir nor
	// configured checkouts need a second manager.
	remotesDir := resolveWorkDir(rootFile, root.Global.WorkDir)
	remotePaths := make(map[string]string, len(root.Remotes))
	for _, name := range sortedKeys(root.Remotes) {
		rem := root.Remotes[name]
		path := rem.Checkout
		if path == "" {
			path = filepath.Join(remotesDir, name)
		}
		p, err := gitMgr.EnsureRepoAt(path, rem.URL, rem.Branch)
		if err != nil {
			err = fmt.Errorf("remote %s: %w", name, err)
			// One repo_failed per pass; the pass error still lists every
			// failing remote.
			pn.repoFailed(ctx, name, stageRepo, err)
			errs = append(errs, err)
			continue
		}
		remotePaths[name] = p
	}

	// 3. Full configuration: local stack discovery plus import merge.
	cfg, err := config.Load(rootFile)
	if err != nil {
		err = fmt.Errorf("reconciler: %w", err)
		pn.repoFailed(ctx, HostRepoName, stageConfig, err)
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	if r.onConfig != nil {
		r.onConfig(cfg)
	}
	// Load re-reads the root file, so follow whatever block this pass
	// actually loaded (a removed block drops the notifier).
	pn.set(r.notifierFor(cfg.Notifications))

	// GIT_COMMIT per stack: the commit of the checkout the stack
	// definition lives in (host checkout for local stacks, owning remote
	// checkout for imports). Missing git metadata only degrades GIT_COMMIT
	// to "" — it does not put the deployment at risk.
	commits := make(map[string]string, 1+len(remotePaths))
	recordCommit(gitMgr, commits, hostPath, r.logger)
	for _, name := range sortedKeys(remotePaths) {
		recordCommit(gitMgr, commits, remotePaths[name], r.logger)
	}

	// 4. Deploy every enabled stack; a failing stack is skipped while the
	// pass continues with the next one.
	vaultSess := &vaultSession{factory: r.newVault, cfg: cfg.Global.Vault, logger: r.logger}
	for _, stack := range cfg.Stacks {
		if !stack.Enabled {
			r.logger.Debug("stack disabled, skipping", "stack", stack.Name)
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("reconciler: aborted before stack %s: %w", stack.Name, err))
			break
		}
		if err := r.deployStack(ctx, stack, vaultSess, commits, pn); err != nil {
			r.logger.Error("stack deployment failed", "stack", stack.Name, "error", err)
			errs = append(errs, err)
		}
	}

	// 5. Prune stacks that disappeared from the configuration.
	if cfg.Global.PruneRemovedStacks {
		errs = append(errs, r.prune(ctx, cfg, pn)...)
	}

	// 6. Image update check (phase 10): compare the deployed images with
	// their registries and record the outcome. Deliberately last — it needs
	// the state records the deployments just wrote — and it deploys
	// nothing: the apply actor is a later phase. Its per-image failures
	// stay in the records and never fail the pass.
	errs = append(errs, r.checkImageUpdates(ctx, cfg, pn)...)

	// 7. Persist the state. Deployments and failures are already saved per
	// stack the moment they are recorded (see deployStack and markFailed),
	// so this write persists the prunes and any stragglers; a failure to
	// persist is itself a pass error.
	if err := r.state.Save(); err != nil {
		errs = append(errs, fmt.Errorf("reconciler: save state: %w", err))
	}
	return errors.Join(errs...)
}

// deployStack runs the deployment sequence of one enabled stack: hash,
// change detection, secrets, environment, hooks and compose up. Per-stack
// failures are returned; the caller collects them and continues with the
// next stack. Every deployment failure is reported once as deploy_failed
// with the stage it happened in, and a fully successful deployment clears the
// dedupe key (Recovery). All deferred cleanup (the shm env file) runs when
// this function returns, i.e. at the end of the stack; the generated
// stack-dir .env deliberately outlives a successful deployment (see the
// WriteStackEnv call below).
func (r *Reconciler) deployStack(ctx context.Context, stack config.Stack, vaultSess *vaultSession, commits map[string]string, pn *passNotify) error {
	composeFiles := stack.EffectiveComposeFiles()
	contents := make([][]byte, 0, len(composeFiles))
	for _, file := range composeFiles {
		content, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("stack %s: read compose file: %w", stack.Name, err)
		}
		contents = append(contents, content)
	}
	newHash, err := stackhash.Compute(stack, contents...)
	if err != nil {
		return fmt.Errorf("stack %s: compute hash: %w", stack.Name, err)
	}

	// Unchanged hash and last attempt deployed → skip (logged at debug per
	// the briefing). Changed() also redeploys entries whose last attempt
	// failed, so a transient failure is retried on the next pass even with
	// an unchanged hash.
	if !r.state.Changed(stack.Name, newHash) {
		r.logger.Debug("stack unchanged, skipping", "stack", stack.Name, "hash", newHash)
		return nil
	}
	r.logger.Info("deploying stack", "stack", stack.Name, "hash", newHash)

	// Secrets: the Vault client is built and logged in lazily, at most once
	// per pass, and only when a stack actually carries vault paths or vault
	// templates (the templates resolve through the same KV v2 read).
	var vaultClient VaultClient
	if len(stack.VaultPaths) > 0 || len(stack.VaultEnvTemplates) > 0 {
		vaultClient, err = vaultSess.get(ctx)
		if err != nil {
			err = fmt.Errorf("stack %s: vault: %w", stack.Name, err)
			pn.deployFailed(ctx, stack.Name, stageSecrets, newHash, err)
			r.markFailed(stack, newHash)
			return err
		}
	}

	sysEnv := env.SystemVars{GitCommit: commitFor(stack.Dir, commits)}
	resolver := &stackResolver{stack: stack, decrypt: r.sops, vault: vaultClient}
	merged, err := env.MergeStackEnv(ctx, stack, resolver, stack.Dir, sysEnv)
	if err != nil {
		err = fmt.Errorf("stack %s: resolve environment: %w", stack.Name, err)
		pn.deployFailed(ctx, stack.Name, stageSecrets, newHash, err)
		r.markFailed(stack, newHash)
		return err
	}

	envFile, cleanup, err := env.WriteShm(merged)
	if err != nil {
		// The generated environment is part of the secret pipeline; a
		// failure here aborts the deployment just like a resolution error.
		err = fmt.Errorf("stack %s: write env file: %w", stack.Name, err)
		pn.deployFailed(ctx, stack.Name, stageSecrets, newHash, err)
		r.markFailed(stack, newHash)
		return err
	}
	defer cleanup()

	hookEnv := envList(merged)
	if err := r.hooks.Run(ctx, stack.Hooks.PreDeploy, stack.Dir, hookEnv, hookTimeout); err != nil {
		err = fmt.Errorf("stack %s: pre_deploy: %w", stack.Name, err)
		pn.deployFailed(ctx, stack.Name, stagePreDeploy, newHash, err)
		r.markFailed(stack, newHash)
		return err
	}

	// The stack-dir .env backs compose's stack-relative env_file: ./.env
	// references and ${...} interpolation, which the shm --env-file alone
	// cannot serve (docker compose resolves them against the compose
	// file's directory). It is written from the same merged environment,
	// after pre_deploy so a failing hook leaves the previous file intact.
	// It stays after a successful deployment; a checkout-updating pass may
	// remove or revert it, and the next deployment regenerates it. It is
	// deliberately not part of the stack hash: a changed secret value must
	// not trigger a redeployment (Komodo reality; see env.WriteStackEnv).
	stackEnvPath, rollbackStackEnv, err := env.WriteStackEnv(stack.Dir, merged)
	if err != nil {
		err = fmt.Errorf("stack %s: write stack env file: %w", stack.Name, err)
		pn.deployFailed(ctx, stack.Name, stageSecrets, newHash, err)
		r.markFailed(stack, newHash)
		return err
	}
	r.logger.Debug("wrote stack env file", "stack", stack.Name, "path", stackEnvPath)

	if err := r.compose.Up(ctx, stack, stack.Dir, envFile); err != nil {
		// Deployment failed: restore whatever .env was there before, so the
		// stack directory does not advertise an environment its services
		// are not running with.
		rollbackStackEnv()
		err = fmt.Errorf("stack %s: up: %w", stack.Name, err)
		pn.deployFailed(ctx, stack.Name, stageComposeUp, newHash, err)
		r.markFailed(stack, newHash)
		return err
	}

	// A failed post_deploy hook does not undeploy the stack — the briefing
	// says it still counts as deployed — but the error is logged here and
	// returned so it joins the pass result. The generated .env stays: the
	// services are up and later compose calls need the same environment.
	var postErr error
	if err := r.hooks.Run(ctx, stack.Hooks.PostDeploy, stack.Dir, hookEnv, hookTimeout); err != nil {
		r.logger.Error("post_deploy failed; stack counts as deployed anyway", "stack", stack.Name, "error", err)
		postErr = fmt.Errorf("stack %s: post_deploy: %w", stack.Name, err)
		pn.deployFailed(ctx, stack.Name, stagePostDeploy, newHash, postErr)
	}

	if err := r.state.Set(stack.Name, state.State{
		Hash:         newHash,
		Status:       state.StatusDeployed,
		StackDir:     stack.Dir,
		ComposeFile:  stack.ComposeFile,
		ComposeFiles: stack.ComposeFiles,
	}); err != nil {
		return fmt.Errorf("stack %s: record state: %w", stack.Name, err)
	}
	// Persist immediately instead of waiting for the end-of-pass save: a
	// pass that dies before it (crash, kill, failing disk) must not leave
	// the state file without the deployments that already happened. The
	// next pass would otherwise redeploy unchanged stacks ("Recreate"
	// despite an identical hash) and forget stacks that were since removed
	// from the configuration — no prune, no compose down (phase-6c bugs).
	if err := r.state.Save(); err != nil {
		return errors.Join(postErr, fmt.Errorf("stack %s: persist state: %w", stack.Name, err))
	}
	if postErr == nil {
		// Fully successful deployment: drop the dedupe key, so the next
		// failure of this stack alarms immediately again.
		pn.recovery(config.NotificationDeployFailed, stack.Name)
	}
	return postErr
}

// markFailed records a failed deployment attempt so the next pass retries
// the stack even when its hash is unchanged (state.Changed treats every
// non-deployed status as changed), and persists the record immediately (see
// the per-stack save in deployStack). The bookkeeping itself must never mask
// the original failure, so Set and Save errors are only logged.
func (r *Reconciler) markFailed(stack config.Stack, newHash string) {
	if err := r.state.Set(stack.Name, state.State{
		Hash:         newHash,
		Status:       state.StatusFailed,
		StackDir:     stack.Dir,
		ComposeFile:  stack.ComposeFile,
		ComposeFiles: stack.ComposeFiles,
	}); err != nil {
		r.logger.Error("cannot record failed state", "stack", stack.Name, "error", err)
		return
	}
	if err := r.state.Save(); err != nil {
		r.logger.Error("cannot persist failed state", "stack", stack.Name, "error", err)
	}
}

// prune tears down every state entry whose stack no longer exists in the
// configuration and removes the entry. Disabled stacks still exist in the
// configuration and are therefore never pruned. A failed teardown keeps the
// entry so the next pass retries it.
func (r *Reconciler) prune(ctx context.Context, cfg *config.Config, pn *passNotify) []error {
	configured := make(map[string]struct{}, len(cfg.Stacks))
	for _, s := range cfg.Stacks {
		configured[s.Name] = struct{}{}
	}
	var errs []error
	for _, name := range r.state.Names() {
		if _, ok := configured[name]; ok {
			continue
		}
		st, _ := r.state.Get(name)
		if err := r.pruneStack(ctx, name, st); err != nil {
			r.logger.Error("prune failed; state entry kept for retry", "stack", name, "error", err)
			err = fmt.Errorf("prune %s: %w", name, err)
			pn.pruneFailed(ctx, name, err)
			errs = append(errs, err)
			continue
		}
		r.state.Delete(name)
		r.logger.Info("pruned removed stack", "stack", name)
	}
	return errs
}

// pruneStack tears one removed stack down, covering every shape a state
// entry can have (phase 6c: none of the cases may abort the pass):
//
//   - compose files all on disk → the full Down command line runs, with
//     every -f the stack was deployed from, so the merged configuration
//     matches the one the services were created with. An empty env file
//     stands in for the secrets, which are gone with the stack definition;
//     down only interpolates, it does not need the values.
//   - compose file recorded but gone (the checkout directory vanished with
//     the stack, or any file is missing) → the briefing's fallback runs:
//     docker compose -p <name> down --remove-orphans without -f, which may
//     fail depending on the docker compose version; the failure is returned
//     to the caller for logging and retry.
//   - no compose file recorded at all ("ohne jegliche Spur", e.g. a state
//     file written before those fields existed) → there is no evidence the
//     project ever existed; shelling out would either do nothing or — on
//     docker compose versions that insist on -f — fail forever and keep the
//     entry in an endless retry loop. The entry is dropped with a warning
//     instead.
func (r *Reconciler) pruneStack(ctx context.Context, name string, st state.State) error {
	if len(st.ComposeFiles) == 0 && st.ComposeFile == "" {
		r.logger.Warn("state entry without compose file traces; removing entry without teardown",
			"stack", name, "hint", "if containers of this stack are still running, remove them manually")
		return nil
	}
	if len(st.ComposeFiles) > 0 {
		if st.StackDir == "" || !allRegularFiles(st.ComposeFiles) {
			return r.compose.DownProject(ctx, name)
		}
		envFile, cleanup, err := env.WriteShm(map[string]string{})
		if err != nil {
			return fmt.Errorf("write empty env file: %w", err)
		}
		defer cleanup()
		ghost := config.Stack{Name: name, ComposeFiles: st.ComposeFiles}
		return r.compose.Down(ctx, ghost, st.StackDir, envFile)
	}
	if st.ComposeFile != "" && st.StackDir != "" && regularFileExists(st.ComposeFile) {
		envFile, cleanup, err := env.WriteShm(map[string]string{})
		if err != nil {
			return fmt.Errorf("write empty env file: %w", err)
		}
		defer cleanup()
		ghost := config.Stack{Name: name, ComposeFile: st.ComposeFile}
		return r.compose.Down(ctx, ghost, st.StackDir, envFile)
	}
	return r.compose.DownProject(ctx, name)
}

// allRegularFiles reports whether every path exists and is a regular file.
func allRegularFiles(paths []string) bool {
	for _, p := range paths {
		if !regularFileExists(p) {
			return false
		}
	}
	return true
}

// validate checks the invariant requirements at the start of every pass so
// miswired dependencies fail with a clear message instead of a panic.
func (r *Reconciler) validate() error {
	if r.repoURL == "" {
		return fmt.Errorf("reconciler: repo url must not be empty (flag -repo or env DOCKOPS_REPO_URL)")
	}
	if r.branch == "" {
		return fmt.Errorf("reconciler: branch must not be empty (flag -branch or env DOCKOPS_BRANCH)")
	}
	if r.state == nil {
		return fmt.Errorf("reconciler: state store must not be nil")
	}
	return nil
}

// vaultSession builds and logs in the Vault client at most once per
// reconcile pass (briefing: "Login einmal pro Reconcile-Durchlauf") and
// caches the outcome: a failed construction or login is replayed for every
// later stack with vault paths or vault templates instead of hammering an
// unreachable server. The next pass starts fresh with a new session.
type vaultSession struct {
	factory VaultFactory
	cfg     config.Vault
	logger  *slog.Logger
	client  VaultClient
	err     error
	tried   bool
}

// get returns the logged-in Vault client, constructing and logging in on
// first use.
func (v *vaultSession) get(ctx context.Context) (VaultClient, error) {
	if v.tried {
		return v.client, v.err
	}
	v.tried = true
	c, err := v.factory(v.cfg, v.logger)
	if err != nil {
		v.err = fmt.Errorf("build client: %w", err)
		return nil, v.err
	}
	if err := c.Login(ctx); err != nil {
		v.err = fmt.Errorf("login: %w", err)
		return nil, v.err
	}
	v.client = c
	return c, nil
}

// stackResolver adapts the reconciler's sops function and — for stacks with
// vault paths — the logged-in Vault client to the secrets.Resolver
// interface consumed by env.MergeStackEnv. It is bound to one stack so a
// relative age key file resolves against that stack's directory, mirroring
// sops.DecryptFiles, which resolves relative key files against baseDir.
type stackResolver struct {
	stack   config.Stack
	decrypt SopsDecryptFunc
	vault   VaultClient // nil for stacks without vault paths
}

// ResolveSops implements secrets.Resolver. file.Path arrives already
// resolved to an absolute path from env.MergeStackEnv; only the age key
// file is relativized here.
func (s *stackResolver) ResolveSops(ctx context.Context, file config.SopsFile) (map[string]string, error) {
	keyfile := file.AgeKeyfile
	if keyfile != "" && !filepath.IsAbs(keyfile) {
		keyfile = filepath.Join(s.stack.Dir, keyfile)
	}
	return s.decrypt(ctx, file.Path, keyfile)
}

// ResolveVault implements secrets.Resolver.
func (s *stackResolver) ResolveVault(ctx context.Context, path string) (map[string]string, error) {
	if s.vault == nil {
		// Unreachable in the reconciler flow: the client is nil only for
		// stacks without vault paths, and those never call ResolveVault.
		return nil, fmt.Errorf("stack %s: vault path %q but no vault client was provided", s.stack.Name, path)
	}
	return s.vault.ReadKV(ctx, path)
}

// resolveWorkDir makes the configured work dir absolute exactly the way
// config.Load does: relative values resolve against the directory of the
// root configuration file. LoadRoot only applies the empty-value default;
// replicating the resolution here keeps the remotes where the loader will
// look for the imports.
func resolveWorkDir(rootFile, workDir string) string {
	if workDir == "" {
		return config.DefaultWorkDir
	}
	if filepath.IsAbs(workDir) {
		return filepath.Clean(workDir)
	}
	return filepath.Join(filepath.Dir(rootFile), workDir)
}

// recordCommit records the commit of the checkout at path. On error it logs
// a warning and leaves the checkout unrecorded — GIT_COMMIT degrades to ""
// rather than failing the deployment over missing git metadata.
func recordCommit(m GitManager, commits map[string]string, path string, logger *slog.Logger) {
	commit, err := m.HeadCommit(path)
	if err != nil {
		logger.Warn("cannot determine commit; GIT_COMMIT stays empty", "path", path, "error", err)
		return
	}
	commits[path] = commit
}

// commitFor returns the git commit of the checkout containing dir: the
// host checkout for local stacks, the owning remote checkout for imports.
// The longest matching checkout wins, so nested checkouts resolve to the
// innermost one; directories outside every checkout yield "" (unknown).
func commitFor(dir string, commits map[string]string) string {
	best, bestLen := "", -1
	for path, commit := range commits {
		if commit == "" {
			continue
		}
		if dir == path || strings.HasPrefix(dir, path+string(filepath.Separator)) {
			if len(path) > bestLen {
				best, bestLen = commit, len(path)
			}
		}
	}
	return best
}

// envList renders env as sorted KEY=VALUE entries for the hook engine
// (sorted so recorded calls and log output stay deterministic).
func envList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// sortedKeys returns the keys of m sorted for deterministic iteration
// (remotes are processed and logged in a stable order).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// regularFileExists reports whether path exists and is a regular file
// (same semantics as the config loader's helper).
func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
