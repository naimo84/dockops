// Command dockops is the agent that reconciles docker compose stacks from a
// declarative host repository: it clones the host repo and all remotes,
// resolves secrets from sops files and Vault, runs hooks and brings stacks
// up via docker compose. It runs one pass (-once), on a poll timer and on
// authenticated webhooks, until SIGINT/SIGTERM shuts it down.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/git"
	"github.com/local/dockops/internal/reconciler"
	"github.com/local/dockops/internal/state"
	"github.com/local/dockops/internal/webhook"
)

// Environment variables backing the flags; see parseFlags. The two status
// variables have no flag of their own (briefing phase 8 names the env only).
const (
	envRepoURL        = "DOCKOPS_REPO_URL"
	envBranch         = "DOCKOPS_BRANCH"
	envWebhookSecret  = "DOCKOPS_WEBHOOK_SECRET"
	envSopsSidecarURL = "DOCKOPS_SOPS_SIDECAR_URL"
	envStatusToken    = "DOCKOPS_STATUS_TOKEN"
	envSelfProject    = "DOCKOPS_SELF_PROJECT"
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
	// Interval is the -interval override; 0 means unset, in which case
	// global.poll_interval from the loaded config applies.
	Interval time.Duration
}

// parseFlags parses args (without the program name) with environment
// fallbacks from getenv. It is a separate function so tests can exercise it
// with a fake environment instead of the process environment. -repo wins
// over DOCKOPS_REPO_URL, -branch over DOCKOPS_BRANCH (which wins over
// "main"), -webhook-secret over DOCKOPS_WEBHOOK_SECRET.
func parseFlags(args []string, getenv func(string) string) (options, error) {
	var opts options
	var interval string

	fs := flag.NewFlagSet("dockops", flag.ContinueOnError)
	fs.StringVar(&opts.RepoURL, "repo", getenv(envRepoURL), "host repository URL (env "+envRepoURL+")")
	fs.StringVar(&opts.Branch, "branch", getenv(envBranch), "host repository branch (env "+envBranch+", default "+defaultBranch+")")
	fs.StringVar(&opts.StatePath, "state", defaultStatePath, "path of the JSON state file")
	fs.StringVar(&opts.WebhookSecret, "webhook-secret", getenv(envWebhookSecret), "shared secret for webhook authentication (env "+envWebhookSecret+")")
	fs.StringVar(&opts.Addr, "addr", defaultAddr, "listen address of the webhook server")
	fs.StringVar(&opts.SopsSidecarURL, "sops-sidecar", getenv(envSopsSidecarURL),
		"URL of the sops decrypt sidecar, e.g. http://sops-sidecar:8081 (env "+envSopsSidecarURL+"; empty: decrypt via the local sops binary)")
	fs.BoolVar(&opts.Once, "once", false, "run a single reconcile pass and exit")
	fs.StringVar(&interval, "interval", "", "poll interval override like \"5m\" (default: global.poll_interval from the config)")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if opts.RepoURL == "" {
		return opts, fmt.Errorf("no host repository configured: set -repo or %s", envRepoURL)
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
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(opts, logger); err != nil {
		logger.Error("dockops exiting", "error", err)
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

	store, err := state.New(opts.StatePath, logger)
	if err != nil {
		return err
	}

	// global.poll_interval is only known after the first pass loaded the
	// host repository; the callback hands it over to the poll loop.
	intervals := &intervalSource{}

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
		OnConfigLoaded: func(cfg *config.Config) {
			intervals.set(time.Duration(cfg.Global.PollInterval))
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
			Secret:  opts.WebhookSecret,
			Trigger: notify,
			Logger:  logger,
			State:   collab.store,
			Compose: collab.compose,
			RepoURL: opts.RepoURL,
			Branch:  opts.Branch,
			HeadCommit: func() (string, error) {
				if collab.git == nil {
					return "", errors.New("git manager unavailable (see startup log)")
				}
				return collab.git.HeadCommit(filepath.Join(config.DefaultWorkDir, reconciler.HostRepoName))
			},
			SelfProject: selfProject(),
			Token:       os.Getenv(envStatusToken),
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
