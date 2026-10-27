// Command dockops is the agent that reconciles docker compose stacks from a
// declarative host repository: it clones the host repo and all remotes,
// resolves secrets from sops files and Vault, runs hooks and brings stacks
// up via docker compose. It runs one pass (-once), on a poll timer and on
// authenticated webhooks, until SIGINT/SIGTERM shuts it down.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/env"
	"github.com/local/dockops/internal/git"
	"github.com/local/dockops/internal/reconciler"
	"github.com/local/dockops/internal/selfupdate"
	"github.com/local/dockops/internal/state"
	"github.com/local/dockops/internal/webhook"
)

// Environment variables backing the flags; see parseFlags. The status
// token and self project variables have no flag of their own (briefing
// phase 8 names the env only).
const (
	envRepoURL        = "DOCKOPS_REPO_URL"
	envBranch         = "DOCKOPS_BRANCH"
	envWebhookSecret  = "DOCKOPS_WEBHOOK_SECRET"
	envSopsSidecarURL = "DOCKOPS_SOPS_SIDECAR_URL"
	envPrintStatus    = "DOCKOPS_PRINT_STATUS"
	envStatusToken    = "DOCKOPS_STATUS_TOKEN"
	envLogsToken      = "DOCKOPS_LOGS_TOKEN"
	envSelfProject    = "DOCKOPS_SELF_PROJECT"
	envSelfUpdate     = "DOCKOPS_SELF_UPDATE"
	envSelfImage      = "DOCKOPS_SELF_IMAGE"
	envSelfMounts     = "DOCKOPS_SELF_MOUNTS"
	envSelfUser       = "DOCKOPS_SELF_USER"
	envSelfGroups     = "DOCKOPS_SELF_GROUPS"
)

// Defaults for the flags and the poll loop.
const (
	defaultBranch       = "main"
	defaultStatePath    = "/var/lib/dockops/state.json"
	defaultAddr         = ":8080"
	defaultPollInterval = 5 * time.Minute // same value the config loader defaults to
	shutdownTimeout     = 10 * time.Second
	// defaultSelfProject is the compose project dockops itself runs under
	// when deployed from this repository's compose.yaml (no top-level
	// `name:`, so the project defaults to the directory name). GET /status
	// never reports it as unmanaged; DOCKOPS_SELF_PROJECT overrides.
	defaultSelfProject = "dockops"
)

// collaborators are the state-reading collaborators shared between the
// reconcile passes and the HTTP surface: the compose executor is stateless,
// and the git manager answers HeadCommit for GET /status (the reconciler
// drives the checkouts itself). git may be nil — see run.
type collaborators struct {
	store   *state.Store
	compose *compose.Executor
	git     *git.Manager
}

// options is the parsed command line (with environment fallbacks).
type options struct {
	RepoURL        string
	Branch         string
	StatePath      string
	WebhookSecret  string
	Addr           string
	SopsSidecarURL string
	Once           bool
	// PrintStatus selects the one-shot status mode (-print-status): write
	// the /status JSON document to stdout and exit — no repository, no
	// reconcile pass, no server. It wins over Once.
	PrintStatus bool
	// Interval is the -interval override; 0 means unset, in which case
	// global.poll_interval from the loaded config applies.
	Interval time.Duration
	// SelfUpdateHelper selects the phase-12 helper mode
	// (-selfupdate-helper): execute the plan named by -selfupdate-plan and
	// exit — no repository, no reconcile pass, no HTTP server. It is how the
	// agent starts its own detached replacement.
	SelfUpdateHelper bool
	// SelfUpdatePlan is the plan file path of the helper mode.
	SelfUpdatePlan string
}

// boolFromEnv parses an optional boolean environment variable: empty counts
// as unset (false), anything strconv.ParseBool understands is honored, every
// other value is a configuration error naming the variable.
func boolFromEnv(getenv func(string) string, name string) (bool, error) {
	v := getenv(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("env %s: %w", name, err)
	}
	return b, nil
}

// parseFlags parses args (without the program name) with environment
// fallbacks from getenv. It is a separate function so tests can exercise it
// with a fake environment instead of the process environment. -repo wins
// over DOCKOPS_REPO_URL, -branch over DOCKOPS_BRANCH (which wins over
// "main"), -webhook-secret over DOCKOPS_WEBHOOK_SECRET. -print-status is
// the one exception to the -repo requirement: the status document is built
// from the state file and docker alone.
func parseFlags(args []string, getenv func(string) string) (options, error) {
	var opts options
	var interval string

	printStatus, err := boolFromEnv(getenv, envPrintStatus)
	if err != nil {
		return opts, err
	}

	fs := flag.NewFlagSet("dockops", flag.ContinueOnError)
	fs.StringVar(&opts.RepoURL, "repo", getenv(envRepoURL), "host repository URL (env "+envRepoURL+")")
	fs.StringVar(&opts.Branch, "branch", getenv(envBranch), "host repository branch (env "+envBranch+", default "+defaultBranch+")")
	fs.StringVar(&opts.StatePath, "state", defaultStatePath, "path of the JSON state file")
	fs.StringVar(&opts.WebhookSecret, "webhook-secret", getenv(envWebhookSecret), "shared secret for webhook authentication (env "+envWebhookSecret+")")
	fs.StringVar(&opts.Addr, "addr", defaultAddr, "listen address of the webhook server")
	fs.StringVar(&opts.SopsSidecarURL, "sops-sidecar", getenv(envSopsSidecarURL),
		"URL of the sops decrypt sidecar, e.g. http://sops-sidecar:8081 (env "+envSopsSidecarURL+"; empty: decrypt via the local sops binary)")
	fs.BoolVar(&opts.Once, "once", false, "run a single reconcile pass and exit")
	fs.BoolVar(&opts.PrintStatus, "print-status", printStatus,
		"write the /status JSON document to stdout and exit (env "+envPrintStatus+"; no -repo needed; wins over -once and server mode)")
	fs.StringVar(&interval, "interval", "", "poll interval override like \"5m\" (default: global.poll_interval from the config)")
	fs.BoolVar(&opts.SelfUpdateHelper, "selfupdate-helper", false,
		"run as the self-update helper: execute the plan given by -selfupdate-plan and exit (no repository, no server)")
	fs.StringVar(&opts.SelfUpdatePlan, "selfupdate-plan", "",
		"path of the self-update plan (-selfupdate-helper only)")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	// The helper and the status mode read everything they need from the
	// state directory and docker; only the daemon modes need a repository.
	if !opts.PrintStatus && !opts.SelfUpdateHelper && opts.RepoURL == "" {
		return opts, fmt.Errorf("no host repository configured: set -repo or %s", envRepoURL)
	}
	if opts.SelfUpdateHelper && opts.SelfUpdatePlan == "" {
		return opts, fmt.Errorf("-selfupdate-helper requires -selfupdate-plan")
	}
	if opts.Branch == "" {
		opts.Branch = defaultBranch
	}
	if interval != "" {
		d, err := time.ParseDuration(interval)
		if err != nil {
			return opts, fmt.Errorf("flag -interval: %w", err)
		}
		if d < 0 {
			return opts, fmt.Errorf("flag -interval: must not be negative")
		}
		opts.Interval = d
	}
	return opts, nil
}

// intervalSource carries the poll interval from the loaded configuration
// (written by the reconciler callback, read by the poll loop) safely across
// goroutines.
type intervalSource struct {
	mu      sync.Mutex
	fromCfg time.Duration
}

// set records the poll interval of a freshly loaded configuration.
func (s *intervalSource) set(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fromCfg = d
}

// get returns the last recorded poll interval.
func (s *intervalSource) get() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fromCfg
}

// effective returns the poll interval to arm the ticker with: the
// -interval override wins, then global.poll_interval from the loaded
// config, then the same 5m default the config loader applies (before any
// config could be loaded at all).
func (s *intervalSource) effective(opts options) time.Duration {
	if opts.Interval > 0 {
		return opts.Interval
	}
	if d := s.get(); d > 0 {
		return d
	}
	return defaultPollInterval
}

func main() {
	opts, err := parseFlags(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dockops:", err)
		os.Exit(2)
	}
	// -print-status carries the JSON document on stdout; its diagnostics
	// (slog included) move to stderr so pipes and jq see nothing but the
	// document.
	logOut := io.Writer(os.Stdout)
	if opts.PrintStatus {
		logOut = os.Stderr
	}
	logger := slog.New(slog.NewJSONHandler(logOut, nil))
	if err := run(opts, logger); err != nil {
		if opts.PrintStatus {
			// Same contract as the flag errors: plain stderr, stdout stays
			// reserved for the document that never got written.
			fmt.Fprintln(os.Stderr, "dockops:", err)
		} else {
			logger.Error("dockops exiting", "error", err)
		}
		os.Exit(1)
	}
}

// run executes the selected mode and returns the process exit error. The
// context is cancelled by SIGINT/SIGTERM, which a running reconcile pass
// receives (aborting hooks and compose commands) and the shutdown path
// uses to stop the webhook server.
func run(opts options, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// -selfupdate-helper wins over every other mode: the helper is a
	// short-lived container whose only job is the plan (phase 12). No
	// reconciler, no webhook server, no repository access.
	if opts.SelfUpdateHelper {
		if opts.PrintStatus || opts.Once {
			logger.Info("-selfupdate-helper wins over -print-status/-once; running the helper plan only")
		}
		return runSelfUpdateHelper(ctx, opts.SelfUpdatePlan, logger)
	}

	// -print-status wins over every other mode: no reconcile pass, no
	// server, just the one-shot document. -repo is not required here (see
	// parseFlags); an unreadable state file is the only fatal error — docker
	// problems degrade into the document exactly like the endpoint.
	if opts.PrintStatus {
		if opts.Once {
			logger.Info("-print-status wins over -once; printing the status without a reconcile pass")
		}
		store, err := state.New(opts.StatePath, logger)
		if err != nil {
			return err
		}
		composeExec := compose.New(logger, "")
		gitMgr, gitErr := git.New(config.DefaultWorkDir, logger)
		if gitErr != nil {
			logger.Warn("git manager unavailable; the status reports an empty host commit",
				"error", gitErr)
		}
		return writeStatus(ctx, webhook.StatusDeps{
			State:       store,
			Compose:     composeExec,
			RepoURL:     opts.RepoURL,
			Branch:      opts.Branch,
			HeadCommit:  hostCommitFn(gitMgr),
			SelfProject: selfProject(),
			Logger:      logger,
		}, os.Stdout)
	}

	store, err := state.New(opts.StatePath, logger)
	if err != nil {
		return err
	}

	// global.poll_interval is only known after the first pass loaded the
	// host repository; the callback hands it over to the poll loop.
	intervals := &intervalSource{}

	// Phase 14: mode and group of the generated <stackDir>/.env follow the
	// root config's global.env_file block. That block is only readable
	// inside a pass (the host repository is bootstrapped there), so the
	// reconciler keeps this pointer and the OnConfigLoaded callback below
	// fills it from the freshly loaded configuration — before the pass
	// deploys anything. cmd/dockops owns the mapping from Global; the
	// loader owns defaults, validation and the DOCKOPS_ENV_FILE_*
	// fallbacks (an explicit YAML value wins).
	envFileOpts := &env.Options{}

	// Shared collaborators of the reconcile passes and GET /status: one
	// compose executor (stateless) and one git manager over the bootstrap
	// work dir (the same default the reconciler applies when Deps.WorkDir
	// is unset). An unavailable git binary degrades exactly as before
	// phase 8 — the daemon starts, every pass fails with the git error —
	// and /status additionally reports an empty host commit.
	composeExec := compose.New(logger, "")
	gitMgr, gitErr := git.New(config.DefaultWorkDir, logger)
	if gitErr != nil {
		logger.Warn("git manager unavailable; reconcile passes will fail and /status reports an empty host commit",
			"error", gitErr)
	}
	recDeps := reconciler.Deps{
		RepoURL:        opts.RepoURL,
		Branch:         opts.Branch,
		State:          store,
		Logger:         logger,
		Compose:        composeExec,
		SopsSidecarURL: opts.SopsSidecarURL,
		SelfProject:    selfProject(),
		SelfUpdate:     selfUpdateEnabled(os.Getenv),
		SelfImage:      selfImage(os.Getenv, logger),
		SelfMounts:     selfMounts(os.Getenv, logger),
		SelfUser:       selfUser(os.Getenv, logger),
		SelfGroups:     selfGroups(os.Getenv, logger),
		// The stack .env permissions of every pass, from the just-loaded
		// root configuration (phase 14).
		EnvFileOptions: envFileOpts,
		OnConfigLoaded: func(cfg *config.Config) {
			intervals.set(time.Duration(cfg.Global.PollInterval))
			*envFileOpts = env.OptionsFromEnvFile(cfg.Global.EnvFile)
		},
	}
	if gitMgr != nil { // nil *git.Manager would be a non-nil interface and panic on use
		recDeps.Git = gitMgr
	}
	rec := reconciler.New(recDeps)

	if opts.Once {
		logger.Info("running single reconcile pass")
		return rec.Reconcile(ctx)
	}
	return serve(ctx, opts, logger, rec, intervals, collaborators{
		store:   store,
		compose: composeExec,
		git:     gitMgr,
	})
}

// selfProject returns the compose project dockops itself runs under, for
// GET /status' unmanaged detection: DOCKOPS_SELF_PROJECT overrides the
// "dockops" default (this repository's compose.yaml project name). Set the
// variable to a non-empty foreign value when running dockops outside
// compose, so a legitimate project that happens to be named "dockops" is
// still reported.
func selfProject() string {
	if p := os.Getenv(envSelfProject); p != "" {
		return p
	}
	return defaultSelfProject
}

// selfImage returns the image a phase-13 deploy helper runs. A deploy plan
// has no target image, so the helper must start the build the agent itself
// runs:
//
//   - DOCKOPS_SELF_IMAGE wins when set (the override for hosts where docker
//     reports an unusable reference, e.g. a locally built image without a
//     tag);
//   - otherwise docker answers for the agent's own container
//     (`docker inspect <container> --format '{{.Config.Image}}'`), which is
//     exactly the image this process was created from.
//
// The lookup happens once at startup, not per pass: it needs the daemon, and
// a per-pass shell-out would only add latency to a value that cannot change
// while the container lives. Failing (no docker, no container) is not fatal at
// startup — it is reported once, and a self-stack deploy then keeps the
// in-process recreate with a warning (see deployStack): a helper with a
// guessed image would deploy a build this agent does not run.
func selfImage(getenv func(string) string, logger *slog.Logger) string {
	if v := strings.TrimSpace(getenv(envSelfImage)); v != "" {
		return v
	}
	host, err := os.Hostname()
	if err != nil {
		logger.Warn("self image: cannot determine the agent's container", "error", err, "hint", "set "+envSelfImage)
		return ""
	}
	// helperDockerBinary is the same "docker" the helper's own commands use
	// (selfupdate.go); one string, one CLI.
	out, err := exec.Command(helperDockerBinary, "inspect", "--format", "{{.Config.Image}}", host).Output()
	if err != nil {
		logger.Warn("self image: docker does not know the agent's image; self-stack deploys stay in-process",
			"container", host, "error", err, "hint", "set "+envSelfImage)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// selfMounts returns the agent's own mount points, so the self-update helper
// can re-declare them. The helper inherits volumes through --volumes-from, but
// bind mounts are not inherited at all — and a bind-mount installation (state
// dir, compose checkouts, docker socket as binds) is the normal case. Without
// this the helper starts, cannot read the plan, and dies without a result.
//
// DOCKOPS_SELF_MOUNTS overrides the lookup with a comma-separated
// host:container[:ro] list for installations where docker reports nothing
// useful. The lookup mirrors selfImage: once at startup, docker inspect on the
// agent's own container, and a failure is a warning rather than a crash — the
// helper then starts with volumes only, which fails the same way every install
// without binds never does.
func selfMounts(getenv func(string) string, logger *slog.Logger) []selfupdate.Mount {
	if v := strings.TrimSpace(getenv(envSelfMounts)); v != "" {
		return parseMountList(v)
	}
	host, err := os.Hostname()
	if err != nil {
		logger.Warn("self mounts: cannot determine the agent's container", "error", err, "hint", "set "+envSelfMounts)
		return nil
	}
	out, err := exec.Command(helperDockerBinary, "inspect", "--format", "{{json .Mounts}}", host).Output()
	if err != nil {
		logger.Warn("self mounts: docker does not know the agent's mounts; the helper will not see bind mounts",
			"container", host, "error", err, "hint", "set "+envSelfMounts)
		return nil
	}
	var reported []struct {
		Type        string
		Source      string
		Destination string
		RW          bool
	}
	if err := json.Unmarshal(out, &reported); err != nil {
		logger.Warn("self mounts: cannot parse docker's mount list", "container", host, "error", err, "hint", "set "+envSelfMounts)
		return nil
	}
	mounts := make([]selfupdate.Mount, 0, len(reported))
	for _, m := range reported {
		// Volumes are covered by --volumes-from; only binds need the
		// explicit host path, which docker does not report for them.
		if m.Type != "bind" {
			continue
		}
		mounts = append(mounts, selfupdate.Mount{Host: m.Source, Path: m.Destination, ReadOnly: !m.RW})
	}
	return mounts
}

// selfUser returns the user spec the self-update helper runs as: the agent's
// own runtime user (docker inspect .Config.User, e.g. "1000:4001").
//
// Not cosmetic. The helper writes selfupdate-result.json and
// selfupdate-helper.log into the state directory with 0600. Started as the
// image default (root) it owns those files, and the agent — a non-root uid in
// its own container — cannot read the verdict it is waiting for: the log said
// "self update: unreadable result file; falling back to the digest
// comparison" and a phase-13 deploy stayed pending forever.
//
// An empty answer is legitimate (agent runs as the image default too) and
// means "no --user flag". DOCKOPS_SELF_USER overrides it for installations
// where docker cannot report the container.
func selfUser(getenv func(string) string, logger *slog.Logger) string {
	if v := strings.TrimSpace(getenv(envSelfUser)); v != "" {
		return v
	}
	host, err := os.Hostname()
	if err != nil {
		logger.Warn("self user: cannot determine the agent's container", "error", err, "hint", "set "+envSelfUser)
		return ""
	}
	out, err := exec.Command(helperDockerBinary, "inspect", "--format", "{{.Config.User}}", host).Output()
	if err != nil {
		logger.Warn("self user: docker does not know the agent's user; the helper runs as the image default",
			"container", host, "error", err, "hint", "set "+envSelfUser)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// selfGroups returns the supplementary groups the helper needs: docker's
// group_add entries of the agent's own container (docker inspect
// .HostConfig.GroupAdd), normally the host's docker gid that owns the socket.
//
// --user does not carry groups over, so a helper that inherits only the uid
// starts, reads the plan and then fails at the first daemon call:
//
//	permission denied while trying to connect to the Docker daemon socket
//
// DOCKOPS_SELF_GROUPS overrides the lookup with a comma-separated gid list.
// An empty answer is legitimate (an agent without group_add) and means "no
// --group-add flag".
func selfGroups(getenv func(string) string, logger *slog.Logger) []string {
	if v := strings.TrimSpace(getenv(envSelfGroups)); v != "" {
		groups := make([]string, 0, 2)
		for _, g := range strings.Split(v, ",") {
			if g = strings.TrimSpace(g); g != "" {
				groups = append(groups, g)
			}
		}
		return groups
	}
	host, err := os.Hostname()
	if err != nil {
		logger.Warn("self groups: cannot determine the agent's container", "error", err, "hint", "set "+envSelfGroups)
		return nil
	}
	out, err := exec.Command(helperDockerBinary, "inspect", "--format", "{{json .HostConfig.GroupAdd}}", host).Output()
	if err != nil {
		logger.Warn("self groups: docker does not know the agent's groups; the helper may not reach the docker socket",
			"container", host, "error", err, "hint", "set "+envSelfGroups)
		return nil
	}
	var groups []string
	if err := json.Unmarshal(out, &groups); err != nil {
		logger.Warn("self groups: cannot parse docker's group list", "container", host, "error", err, "hint", "set "+envSelfGroups)
		return nil
	}
	return groups
}

// parseMountList splits DOCKOPS_SELF_MOUNTS, the escape hatch of selfMounts:
// comma-separated host:container entries, a trailing :ro marks a read-only
// mount. Unparsable entries are dropped (an empty -v value would make docker
// refuse to start the helper at all).
func parseMountList(v string) []selfupdate.Mount {
	var mounts []selfupdate.Mount
	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		readOnly := false
		if strings.HasSuffix(entry, ":ro") {
			readOnly = true
			entry = strings.TrimSuffix(entry, ":ro")
		}
		host, path, ok := strings.Cut(entry, ":")
		if !ok || strings.TrimSpace(host) == "" || strings.TrimSpace(path) == "" {
			continue
		}
		mounts = append(mounts, selfupdate.Mount{Host: host, Path: path, ReadOnly: readOnly})
	}
	return mounts
}

// selfUpdateEnabled reports whether the phase-12 self-update path is active:
// DOCKOPS_SELF_UPDATE disables it only with the literal value "off" (case
// insensitive); unset, empty or anything else keeps it on. Default-on is
// deliberate — a self stack handled by the in-process apply is the bug this
// phase fixes, so an operator has to opt out explicitly (and only to debug).
func selfUpdateEnabled(getenv func(string) string) bool {
	return !strings.EqualFold(strings.TrimSpace(getenv(envSelfUpdate)), "off")
}

// hostCommitFn wires the host_repo.commit callback of GET /status and
// -print-status over the host checkout: with a git manager it resolves the
// checkout's HEAD — at DOCKOPS_HOST_CHECKOUT when set (global.host_
// checkout's env fallback), otherwise at the bootstrap default — without one
// it always fails (BuildStatus degrades both cases to an empty commit). The
// callback has no config access, so the env is the only override it can see;
// in daemon mode the bootstrap checkout is reset to the same origin/branch
// on every pass, so its commit equals the configured checkout's anyway.
func hostCommitFn(gitMgr *git.Manager) func() (string, error) {
	if gitMgr == nil { // nil *git.Manager would be a non-nil interface and panic on use
		return func() (string, error) { return "", errors.New("git manager unavailable (see startup log)") }
	}
	return func() (string, error) {
		return gitMgr.HeadCommit(hostCheckoutPath())
	}
}

// hostCheckoutPath returns the host checkout path for the status callbacks:
// DOCKOPS_HOST_CHECKOUT (config.EnvHostCheckout, global.host_checkout's env
// fallback) wins over the bootstrap default <DefaultWorkDir>/host. The
// status modes load no configuration — env fallback is all they need.
func hostCheckoutPath() string {
	if p := os.Getenv(config.EnvHostCheckout); p != "" {
		return p
	}
	return filepath.Join(config.DefaultWorkDir, reconciler.HostRepoName)
}

// writeStatus builds the status document via the shared webhook function of
// GET /status and writes it to w as JSON indented with two spaces plus a
// trailing newline. Docker failures already live inside the document (drift
// "unknown", error fields) and do not fail the call — the one-shot mode
// mirrors the endpoint, which keeps answering 200.
func writeStatus(ctx context.Context, deps webhook.StatusDeps, w io.Writer) error {
	resp, err := webhook.BuildStatus(ctx, deps)
	if err != nil {
		return fmt.Errorf("build status: %w", err)
	}
	raw, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return fmt.Errorf("encode status: %w", err)
	}
	raw = append(raw, '\n')
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	return nil
}

// serve runs the daemon: the webhook server plus the poll loop, until the
// context is cancelled. Webhook triggers are debounced and merged — the
// loop is single-goroutine, a running pass is never interrupted, and any
// number of webhook requests during a run collapse into exactly one
// subsequent pass (the trigger channel holds one pending slot and drops
// further sends).
func serve(ctx context.Context, opts options, logger *slog.Logger, rec *reconciler.Reconciler, intervals *intervalSource, collab collaborators) error {
	if opts.WebhookSecret == "" {
		logger.Warn("no webhook secret configured; every caller can trigger reconciles",
			"hint", "set -webhook-secret or "+envWebhookSecret)
	}

	logsToken := os.Getenv(envLogsToken)
	if logsToken == "" {
		logger.Info("log API disabled: no token configured", "hint", "set "+envLogsToken)
	}

	triggers := make(chan struct{}, 1)
	notify := func() {
		select {
		case triggers <- struct{}{}:
		default: // a pass is already pending or running — merged
		}
	}

	server := &http.Server{
		Addr: opts.Addr,
		Handler: webhook.New(webhook.Deps{
			Secret:      opts.WebhookSecret,
			Trigger:     notify,
			Logger:      logger,
			State:       collab.store,
			Compose:     collab.compose,
			RepoURL:     opts.RepoURL,
			Branch:      opts.Branch,
			HeadCommit:  hostCommitFn(collab.git),
			SelfProject: selfProject(),
			Token:       os.Getenv(envStatusToken),
			Logs:        collab.compose,
			LogsToken:   logsToken,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", opts.Addr, err)
	}
	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	logger.Info("webhook server listening",
		"addr", opts.Addr, "endpoints", "POST /webhook, GET /healthz, GET /status")

	// First pass immediately: deploy the current state on startup; it also
	// loads the config and thereby the poll interval.
	runPass := func() {
		if err := rec.Reconcile(ctx); err != nil {
			logger.Error("reconcile pass failed", "error", err)
		}
	}
	runPass()

	interval := intervals.effective(opts)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	logger.Info("polling for changes", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shutdown webhook server: %w", err)
			}
			return nil
		case err := <-serveErr:
			// The webhook server died (listen socket closed, ...);
			// without it the agent is deaf — surface the failure.
			if err != nil {
				return fmt.Errorf("webhook server: %w", err)
			}
			return nil
		case <-triggers:
		case <-ticker.C:
		}
		runPass()
		// Re-arm when a config reload changed the poll interval and no
		// -interval override pins it.
		if next := intervals.effective(opts); next != interval {
			interval = next
			ticker.Reset(interval)
			logger.Info("poll interval changed", "interval", interval.String())
		}
	}
}
