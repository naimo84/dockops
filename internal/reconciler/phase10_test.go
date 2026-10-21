package reconciler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/notify"
	"github.com/local/dockops/internal/registry"
	"github.com/local/dockops/internal/state"
)

// fakeResolver answers remote digests from a table and counts the lookups —
// the reconciler tests assert "no registry call" through calls.
type fakeResolver struct {
	mu      sync.Mutex
	calls   []string
	digests map[string]string
	errs    map[string]error
}

func (f *fakeResolver) RemoteDigest(_ context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ref)
	if err, ok := f.errs[ref]; ok {
		return "", err
	}
	if digest, ok := f.digests[ref]; ok {
		return digest, nil
	}
	return "", fmt.Errorf("no remote digest for %s", ref)
}

func (f *fakeResolver) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// phase10Reconciler wires the fixture's fakes plus an injected registry
// resolver and notifier, so a pass exercises the update check without any
// network or docker binary.
func (f *fixture) phase10Reconciler(resolver registry.Resolver, rec notify.Notifier) *Reconciler {
	return New(Deps{
		RepoURL:  "https://example.invalid/host.git",
		Branch:   "main",
		WorkDir:  f.workDir,
		State:    f.state,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Git:      f.git,
		Compose:  f.compose,
		Hooks:    f.hooks,
		Sops:     f.sops.decrypt,
		NewVault: f.vault.make,
		Notifier: rec,
		Registry: resolver,
	})
}

// updateRoot is a root configuration with a due-on-every-pass interval so a
// test can drive several checks without waiting.
const updateRoot = `global:
  work_dir: WORKDIR
  update_check_interval: 1ns
`

// imageState returns the recorded image records of a stack.
func imageState(t *testing.T, f *fixture, stack string) map[string]state.ImageRecord {
	t.Helper()
	st, ok := f.state.Get(stack)
	if !ok {
		t.Fatalf("state has no entry for %s", stack)
	}
	return st.Images
}

func TestPhase10PinnedStackSkipsRegistry(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", "name: web\nupdate_policy: pinned\n")
	f.compose.images = map[string]string{"app": "ghcr.io/x/app:latest"}
	f.compose.digests = map[string]string{"ghcr.io/x/app:latest": "sha256:old"}

	resolver := &fakeResolver{digests: map[string]string{"ghcr.io/x/app:latest": "sha256:new"}}
	rec := &recordingNotifier{}
	if err := f.phase10Reconciler(resolver, rec).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if calls := resolver.called(); len(calls) != 0 {
		t.Errorf("registry calls = %v, want none for a pinned stack", calls)
	}
	if f.compose.imagesCall != 0 {
		t.Errorf("compose Images calls = %d, want 0 for a pinned stack", f.compose.imagesCall)
	}
	if recs := imageState(t, f, "web"); len(recs) != 0 {
		t.Errorf("image records = %+v, want none for a pinned stack", recs)
	}
	if events, _ := rec.snapshot(); len(events) != 0 {
		t.Errorf("events = %+v, want none", events)
	}
}

func TestPhase10DisabledIntervalSkipsEverything(t *testing.T) {
	f := newFixture(t, `global:
  work_dir: WORKDIR
  update_check_interval: 0
`)
	f.addLocalStack("web", "name: web\n")
	f.compose.images = map[string]string{"app": "ghcr.io/x/app:latest"}
	f.compose.digests = map[string]string{"ghcr.io/x/app:latest": "sha256:old"}

	resolver := &fakeResolver{digests: map[string]string{"ghcr.io/x/app:latest": "sha256:new"}}
	if err := f.phase10Reconciler(resolver, &recordingNotifier{}).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if calls := resolver.called(); len(calls) != 0 {
		t.Errorf("registry calls = %v, want none with update_check_interval: 0", calls)
	}
	if f.compose.imagesCall != 0 {
		t.Errorf("compose Images calls = %d, want 0 with the check disabled", f.compose.imagesCall)
	}
}

func TestPhase10IntervalGatesRepeatedChecks(t *testing.T) {
	// One hour is the default: the first pass checks, the second (immediately
	// after) must not touch docker or the registry again.
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.images = map[string]string{"app": "ghcr.io/x/app:latest"}
	f.compose.digests = map[string]string{"ghcr.io/x/app:latest": "sha256:old"}

	resolver := &fakeResolver{digests: map[string]string{"ghcr.io/x/app:latest": "sha256:old"}}
	r := f.phase10Reconciler(resolver, &recordingNotifier{})
	for pass := 1; pass <= 2; pass++ {
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("pass %d: Reconcile() error = %v", pass, err)
		}
	}
	if calls := resolver.called(); len(calls) != 1 {
		t.Errorf("registry calls = %v, want exactly one (the second pass is not due)", calls)
	}
	if f.compose.imagesCall != 1 {
		t.Errorf("compose Images calls = %d, want 1", f.compose.imagesCall)
	}
}

func TestPhase10UpdateNotifiesOncePerNewDigest(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.images = map[string]string{"app": "ghcr.io/x/app:latest"}
	f.compose.digests = map[string]string{"ghcr.io/x/app:latest": "sha256:local"}

	resolver := &fakeResolver{digests: map[string]string{"ghcr.io/x/app:latest": "sha256:remote"}}
	rec := &recordingNotifier{}
	r := f.phase10Reconciler(resolver, rec)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("first pass Reconcile() error = %v", err)
	}
	events, _ := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one update_available", events)
	}
	ev := events[0]
	if ev.Kind != config.NotificationUpdateAvailable {
		t.Errorf("Kind = %q, want update_available", ev.Kind)
	}
	if ev.Stack != "web" || ev.Stage != "update_check" {
		t.Errorf("event = %+v, want stack web at stage update_check", ev)
	}
	if ev.Hash == "" {
		t.Error("Hash is empty, want the deployed stack hash")
	}
	for _, want := range []string{"app:", "ghcr.io/x/app:latest", "->"} {
		if !strings.Contains(ev.Err, want) {
			t.Errorf("Err = %q, want it to contain %q", ev.Err, want)
		}
	}

	// The record is persisted and says "update available".
	recs := imageState(t, f, "web")
	got, ok := recs["ghcr.io/x/app:latest"]
	if !ok {
		t.Fatalf("records = %+v, want the app image", recs)
	}
	if !got.UpdateAvailable || got.CurrentDigest != "sha256:local" || got.RemoteDigest != "sha256:remote" {
		t.Errorf("record = %+v, want the digest comparison", got)
	}
	if got.Service != "app" || got.CheckedAt.IsZero() {
		t.Errorf("record = %+v, want service and check time", got)
	}

	// Second pass, same remote digest: record comparison keeps it silent
	// even though the notifier's window is bypassed by the 1ns interval.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second pass Reconcile() error = %v", err)
	}
	if events, _ := rec.snapshot(); len(events) != 1 {
		t.Errorf("events after second pass = %+v, want still exactly one", events)
	}

	// Third pass with a third digest: a new update is news again.
	resolver.mu.Lock()
	resolver.digests["ghcr.io/x/app:latest"] = "sha256:newer"
	resolver.mu.Unlock()
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("third pass Reconcile() error = %v", err)
	}
	events, _ = rec.snapshot()
	if len(events) != 2 {
		t.Fatalf("events after digest change = %+v, want two", events)
	}
	if !strings.Contains(events[1].Err, "sha256:newer") {
		t.Errorf("second event Err = %q, want the new digest", events[1].Err)
	}
}

func TestPhase10RegistryErrorIsRecordedNotFatal(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.images = map[string]string{"app": "ghcr.io/x/app:latest"}
	f.compose.digests = map[string]string{"ghcr.io/x/app:latest": "sha256:local"}

	resolver := &fakeResolver{errs: map[string]error{
		"ghcr.io/x/app:latest": fmt.Errorf("%w: too many requests", registry.ErrRateLimited),
	}}
	rec := &recordingNotifier{}
	if err := f.phase10Reconciler(resolver, rec).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v, want nil (registry problems stay in the record)", err)
	}

	got := imageState(t, f, "web")["ghcr.io/x/app:latest"]
	if !strings.Contains(got.Error, "rate limited") {
		t.Errorf("record error = %q, want the registry error", got.Error)
	}
	if got.UpdateAvailable {
		t.Error("UpdateAvailable = true, want false on a failed lookup")
	}
	if got.CurrentDigest != "sha256:local" {
		t.Errorf("CurrentDigest = %q, want the local digest recorded anyway", got.CurrentDigest)
	}
	if events, _ := rec.snapshot(); len(events) != 0 {
		t.Errorf("events = %+v, want none on a registry error", events)
	}
}

func TestPhase10LocallyBuiltImageDoesNotAlarm(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.images = map[string]string{"app": "myapp:dev"}
	// No local digest for that reference: a locally built image without a
	// registry identity.
	f.compose.digests = map[string]string{}

	resolver := &fakeResolver{digests: map[string]string{"myapp:dev": "sha256:remote"}}
	rec := &recordingNotifier{}
	if err := f.phase10Reconciler(resolver, rec).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := imageState(t, f, "web")["myapp:dev"]
	if got.Error != "locally built image" {
		t.Errorf("record error = %q, want %q", got.Error, "locally built image")
	}
	if got.UpdateAvailable {
		t.Error("UpdateAvailable = true, want false for a locally built image")
	}
	if got.RemoteDigest != "" {
		t.Errorf("RemoteDigest = %q, want no registry lookup for a local build", got.RemoteDigest)
	}
	if calls := resolver.called(); len(calls) != 0 {
		t.Errorf("registry calls = %v, want none for a locally built image", calls)
	}
	if events, _ := rec.snapshot(); len(events) != 0 {
		t.Errorf("events = %+v, want none for a locally built image", events)
	}
}

func TestPhase10IgnoreServicesAreNotChecked(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", "name: web\nignore_services:\n  - sidecar\n")
	f.compose.images = map[string]string{
		"app":     "ghcr.io/x/app:latest",
		"sidecar": "ghcr.io/x/sidecar:latest",
	}
	f.compose.digests = map[string]string{
		"ghcr.io/x/app:latest":     "sha256:local",
		"ghcr.io/x/sidecar:latest": "sha256:local",
	}
	resolver := &fakeResolver{digests: map[string]string{
		"ghcr.io/x/app:latest":     "sha256:local",
		"ghcr.io/x/sidecar:latest": "sha256:remote",
	}}

	if err := f.phase10Reconciler(resolver, &recordingNotifier{}).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	recs := imageState(t, f, "web")
	if _, ok := recs["ghcr.io/x/sidecar:latest"]; ok {
		t.Errorf("records = %+v, want the ignored service left out", recs)
	}
	if _, ok := recs["ghcr.io/x/app:latest"]; !ok {
		t.Errorf("records = %+v, want the checked service", recs)
	}
	for _, ref := range f.compose.imageDigestCalls {
		if strings.Contains(ref, "sidecar") {
			t.Errorf("image inspect called for ignored service image %q", ref)
		}
	}
	if calls := resolver.called(); len(calls) != 1 || calls[0] != "ghcr.io/x/app:latest" {
		t.Errorf("registry calls = %v, want only the app image", calls)
	}
}

func TestPhase10ConfigErrorsDegrade(t *testing.T) {
	// A stack whose compose configuration cannot be resolved must not
	// fabricate records and must not fail the pass.
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.imagesErr = fmt.Errorf("compose config boom")
	resolver := &fakeResolver{}

	if err := f.phase10Reconciler(resolver, &recordingNotifier{}).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	if recs := imageState(t, f, "web"); len(recs) != 0 {
		t.Errorf("records = %+v, want none when the image map is unavailable", recs)
	}
	if calls := resolver.called(); len(calls) != 0 {
		t.Errorf("registry calls = %v, want none", calls)
	}
}

// TestPhase10Helpers covers the pure helpers of the update check: which
// services are looked at, when a stack is due, and how the notification text
// is shortened.
func TestPhase10Helpers(t *testing.T) {
	images := map[string]string{
		"app":     "ghcr.io/x/app:1",
		"db":      "ghcr.io/x/db:2",
		"sidecar": "ghcr.io/x/sidecar:3",
		"extra":   "ghcr.io/x/extra:4",
	}

	t.Run("ignore and services filter", func(t *testing.T) {
		stack := config.Stack{IgnoreServices: []string{"sidecar"}}
		if got, want := checkableServices(stack, images), []string{"app", "db", "extra"}; !reflect.DeepEqual(got, want) {
			t.Errorf("checkableServices() = %v, want %v", got, want)
		}
		stack = config.Stack{Services: []string{"app", "db"}, IgnoreServices: []string{"db"}}
		if got, want := checkableServices(stack, images), []string{"app"}; !reflect.DeepEqual(got, want) {
			t.Errorf("checkableServices() = %v, want %v", got, want)
		}
		// The compose file may contain services the stack filter excludes.
		stack = config.Stack{Services: []string{"app"}}
		if got, want := checkableServices(stack, images), []string{"app"}; !reflect.DeepEqual(got, want) {
			t.Errorf("checkableServices() = %v, want %v", got, want)
		}
	})

	t.Run("due", func(t *testing.T) {
		now := time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)
		if !updateDue(state.State{}, time.Hour, now) {
			t.Error("updateDue(no records) = false, want true")
		}
		fresh := state.State{Images: map[string]state.ImageRecord{
			"a:1": {CheckedAt: now.Add(-30 * time.Minute)},
			"b:1": {CheckedAt: now.Add(-10 * time.Minute)},
		}}
		if updateDue(fresh, time.Hour, now) {
			t.Error("updateDue(fresh records) = true, want false")
		}
		if !updateDue(fresh, 5*time.Minute, now) {
			t.Error("updateDue with a shorter interval = false, want true")
		}
	})

	t.Run("notification text", func(t *testing.T) {
		long := "sha256:" + strings.Repeat("a", 64)
		other := "sha256:" + strings.Repeat("b", 64)
		got := updateText("app", "ghcr.io/x/app:latest", long, other)
		want := "app: ghcr.io/x/app:latest sha256:aaaaaaaaaaaa -> sha256:bbbbbbbbbbbb"
		if got != want {
			t.Errorf("updateText() = %q, want %q", got, want)
		}
		if short := shortDigest("sha256:abc"); short != "sha256:abc" {
			t.Errorf("shortDigest(short) = %q, want unchanged", short)
		}
		if short := shortDigest("not-a-digest"); short != "not-a-digest" {
			t.Errorf("shortDigest(no colon) = %q, want unchanged", short)
		}
	})
}
