package reconciler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/notify"
)

// recordingNotifier captures what the reconciler reports, without any HTTP.
type recordingNotifier struct {
	mu         sync.Mutex
	events     []notify.Event
	recoveries []string
}

func (n *recordingNotifier) Notify(_ context.Context, ev notify.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, ev)
}

func (n *recordingNotifier) Recovery(kind, stack string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.recoveries = append(n.recoveries, kind+"/"+stack)
}

func (n *recordingNotifier) snapshot() ([]notify.Event, []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notify.Event(nil), n.events...), append([]string(nil), n.recoveries...)
}

// p9opts override single collaborators of the fixture's reconciler.
type p9opts struct {
	logger   *slog.Logger
	notifier notify.Notifier
	factory  func(config.Notifications, *slog.Logger) notify.Notifier
	compose  ComposeExecutor
}

// reconcilerFor builds a reconciler on the fixture's fakes with the given
// notification collaborators.
func (f *fixture) reconcilerFor(opts p9opts) *Reconciler {
	logger := opts.logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	composeExec := opts.compose
	if composeExec == nil {
		composeExec = f.compose
	}
	return New(Deps{
		RepoURL:     "https://example.invalid/host.git",
		Branch:      "main",
		WorkDir:     f.workDir,
		State:       f.state,
		Logger:      logger,
		Git:         f.git,
		Compose:     composeExec,
		Hooks:       f.hooks,
		Sops:        f.sops.decrypt,
		NewVault:    f.vault.make,
		Notifier:    opts.notifier,
		NewNotifier: opts.factory,
	})
}

const notificationsRoot = `global:
  work_dir: WORKDIR
notifications:
  repeat_after: 1h
  timeout: 2s
  destinations:
    - type: webhook
      url: %s
`

func TestPhase9DeployFailedOnComposeUp(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.failUpFor = "web"

	rec := &recordingNotifier{}
	if err := f.reconcilerFor(p9opts{notifier: rec}).Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want the compose up failure")
	}

	events, _ := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one", events)
	}
	ev := events[0]
	if ev.Kind != config.NotificationDeployFailed {
		t.Errorf("Kind = %q, want deploy_failed", ev.Kind)
	}
	if ev.Stack != "web" {
		t.Errorf("Stack = %q, want web", ev.Stack)
	}
	if ev.Stage != "compose_up" {
		t.Errorf("Stage = %q, want compose_up", ev.Stage)
	}
	if ev.Hash == "" {
		t.Error("Hash is empty, want the hash of the attempted stack")
	}
	if !strings.Contains(ev.Err, "compose up boom") {
		t.Errorf("Err = %q, want the compose failure text", ev.Err)
	}
}

func TestPhase9SuccessfulDeploySendsNoEventAndRecovers(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")

	rec := &recordingNotifier{}
	if err := f.reconcilerFor(p9opts{notifier: rec}).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	events, recoveries := rec.snapshot()
	if len(events) != 0 {
		t.Errorf("events = %+v, want none for a successful pass", events)
	}
	if want := "deploy_failed/web"; len(recoveries) != 1 || recoveries[0] != want {
		t.Errorf("recoveries = %v, want [%s] (success clears the dedupe key)", recoveries, want)
	}
}

func TestPhase9PreDeployHookFailure(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\nhooks:\n  pre_deploy:\n    - boom-pre\n")
	f.hooks.failFirstCmd = "boom-pre"

	rec := &recordingNotifier{}
	if err := f.reconcilerFor(p9opts{notifier: rec}).Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want the pre_deploy failure")
	}
	events, _ := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one", events)
	}
	if events[0].Stage != "pre_deploy" {
		t.Errorf("Stage = %q, want pre_deploy", events[0].Stage)
	}
	if len(f.compose.ups) != 0 {
		t.Errorf("Ups = %d, want 0 (the hook failed before compose)", len(f.compose.ups))
	}
}

func TestPhase9DedupeAcrossPasses(t *testing.T) {
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newFixture(t, fmt.Sprintf(notificationsRoot, srv.URL))
	f.addLocalStack("web", "name: web\n")
	f.compose.failUpFor = "web"

	var builds int32
	factory := func(cfg config.Notifications, logger *slog.Logger) notify.Notifier {
		atomic.AddInt32(&builds, 1)
		return notify.New(cfg, logger, nil)
	}
	r := f.reconcilerFor(p9opts{factory: factory})

	for pass := 1; pass <= 2; pass++ {
		if err := r.Reconcile(context.Background()); err == nil {
			t.Fatalf("pass %d: Reconcile() error = nil, want the compose up failure", pass)
		}
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Errorf("webhook deliveries = %d, want 1 (repeat_after 1h dedupes the second pass)", got)
	}
	if got := atomic.LoadInt32(&builds); got != 1 {
		t.Errorf("notifier builds = %d, want 1 (unchanged block reuses the notifier)", got)
	}
}

func TestPhase9PruneFailed(t *testing.T) {
	f := newFixture(t, `global:
  work_dir: WORKDIR
  prune_removed_stacks: true
`)
	f.addLocalStack("ghost", "name: ghost\n")

	rec := &recordingNotifier{}
	r := f.reconcilerFor(p9opts{notifier: rec})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("first pass Reconcile() error = %v", err)
	}
	// The stack disappears from the checkout; the recorded state entry stays
	// and its teardown fails (compose files gone → DownProject).
	if err := os.RemoveAll(filepath.Join(f.workDir, "host", "ghost")); err != nil {
		t.Fatalf("remove stack dir: %v", err)
	}
	f.compose.failDownProjectAt = "ghost"

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("second pass Reconcile() error = nil, want the prune failure")
	}
	events, _ := rec.snapshot()
	// Pass 1 recovered deploy_failed (no event); pass 2 reports the prune.
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one prune_failed", events)
	}
	ev := events[0]
	if ev.Kind != config.NotificationPruneFailed || ev.Stack != "ghost" {
		t.Errorf("event = %+v, want prune_failed for ghost", ev)
	}
	if !strings.Contains(ev.Err, "down project boom") {
		t.Errorf("Err = %q, want the compose failure text", ev.Err)
	}
}

func TestPhase9RepoFailedHostRepo(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.git.failAt = "host"

	rec := &recordingNotifier{}
	if err := f.reconcilerFor(p9opts{notifier: rec}).Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want the host repo failure")
	}
	events, _ := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one repo_failed", events)
	}
	ev := events[0]
	if ev.Kind != config.NotificationRepoFailed || ev.Remote != HostRepoName || ev.Stage != "repo" {
		t.Errorf("event = %+v, want repo_failed for remote host at stage repo", ev)
	}
	if ev.Stack != "" {
		t.Errorf("Stack = %q, want empty for repo_failed", ev.Stack)
	}
}

func TestPhase9NoNotifierWithoutBlock(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.failUpFor = "web"

	rec := &recordingNotifier{}
	var builds int32
	factory := func(config.Notifications, *slog.Logger) notify.Notifier {
		atomic.AddInt32(&builds, 1)
		return rec
	}
	if err := f.reconcilerFor(p9opts{factory: factory}).Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want the compose up failure")
	}
	if got := atomic.LoadInt32(&builds); got != 0 {
		t.Errorf("notifier builds = %d, want 0 without a notifications block", got)
	}
	if events, _ := rec.snapshot(); len(events) != 0 {
		t.Errorf("events = %+v, want none without a notifications block", events)
	}
}

func TestPhase9LogsNotificationsActiveOncePerPass(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	f := newFixture(t, fmt.Sprintf(notificationsRoot, "https://example.invalid/hook"))
	f.addLocalStack("web", "name: web\n")
	r := f.reconcilerFor(p9opts{logger: logger})

	for pass := 1; pass <= 2; pass++ {
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("pass %d: Reconcile() error = %v", pass, err)
		}
	}
	if got := strings.Count(logs.String(), "notifications active"); got != 2 {
		t.Errorf("log lines with \"notifications active\" = %d, want 2 (one per pass): %s", got, logs.String())
	}
	for _, want := range []string{"destinations=1", "deploy_failed prune_failed repo_failed"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q does not contain %q", logs.String(), want)
		}
	}
}

func TestPhase9NotificationsDisabledIsSilent(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	root := `global:
  work_dir: WORKDIR
notifications:
  enabled: false
  destinations:
    - type: webhook
      url: https://example.invalid/hook
`
	f := newFixture(t, root)
	f.addLocalStack("web", "name: web\n")
	f.compose.failUpFor = "web"

	var builds int32
	factory := func(cfg config.Notifications, logger *slog.Logger) notify.Notifier {
		atomic.AddInt32(&builds, 1)
		return notify.New(cfg, logger, nil)
	}
	if err := f.reconcilerFor(p9opts{logger: logger, factory: factory}).Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want the compose up failure")
	}
	if got := atomic.LoadInt32(&builds); got != 1 {
		t.Errorf("notifier builds = %d, want 1 (a disabled block is still loaded)", got)
	}
	if !strings.Contains(logs.String(), "notifications inactive") {
		t.Errorf("log %q does not report the disabled block", logs.String())
	}
	if strings.Contains(logs.String(), "notifications active") {
		t.Errorf("log %q claims active notifications although enabled: false", logs.String())
	}
}

// TestPhase9ComposeUpFailureViaFakeDocker drives the real compose executor
// against a docker binary shadowed through PATH (the e2e pattern): the
// failure must surface as deploy_failed with stage compose_up.
func TestPhase9ComposeUpFailureViaFakeDocker(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")

	binDir := t.TempDir()
	calls := filepath.Join(t.TempDir(), "docker.calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_DOCKER_CALLS\"\necho 'compose up failed' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_CALLS", calls)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &recordingNotifier{}
	r := f.reconcilerFor(p9opts{logger: logger, notifier: rec, compose: compose.New(logger, "")})
	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want the docker failure")
	}

	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read fake docker calls: %v", err)
	}
	if !strings.Contains(string(raw), "compose -p web") || !strings.Contains(string(raw), "up -d") {
		t.Errorf("fake docker calls = %q, want a compose up for web", raw)
	}

	events, _ := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one", events)
	}
	if events[0].Stage != "compose_up" || events[0].Stack != "web" {
		t.Errorf("event = %+v, want deploy_failed for web at compose_up", events[0])
	}
}

// TestPhase9BrokenDestinationDoesNotChangeThePassResult makes sure a dead
// notification channel only logs and never turns a pass error into something
// else (or a success into an error).
func TestPhase9BrokenDestinationDoesNotChangeThePassResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // dead channel from the first notification on

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	f := newFixture(t, fmt.Sprintf(notificationsRoot, url))
	f.addLocalStack("web", "name: web\n")

	factory := func(cfg config.Notifications, l *slog.Logger) notify.Notifier {
		return notify.New(cfg, l, nil)
	}
	// Successful pass with a dead channel: still nil.
	if err := f.reconcilerFor(p9opts{logger: logger, factory: factory}).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v, want nil (delivery problems must not fail the pass)", err)
	}
	// Failing pass: the returned error is the deployment failure only. The
	// compose content changes so the hash differs and the stack is deployed
	// again instead of being skipped.
	writeFile(t, filepath.Join(f.workDir, "host", "web", "compose.yaml"),
		"services:\n  app:\n    image: nginx:latest\n  extra:\n    image: busybox\n")
	f.compose.failUpFor = "web"
	err := f.reconcilerFor(p9opts{logger: logger, factory: factory}).Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile() error = nil, want the compose up failure")
	}
	if strings.Contains(err.Error(), "notification") {
		t.Errorf("pass error %q mentions notifications; the notifier must stay out of the pass result", err)
	}
	if !strings.Contains(logs.String(), "notification delivery failed") {
		t.Errorf("log %q does not warn about the dead channel", logs.String())
	}
}
