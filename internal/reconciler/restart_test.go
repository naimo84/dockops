package reconciler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/state"
)

// restartCall records one compose restart of the fake restarter.
type restartCall struct {
	stack   string
	dir     string
	envFile string
}

// fakeRestarter is the Deps.Restarter test double: it records every
// invocation and fails the stacks named in failFor.
type fakeRestarter struct {
	calls   []restartCall
	failFor map[string]bool
}

func (f *fakeRestarter) Restart(_ context.Context, stack config.Stack, stackDir, envFile string) error {
	f.calls = append(f.calls, restartCall{stack: stack.Name, dir: stackDir, envFile: envFile})
	if f.failFor[stack.Name] {
		return errors.New("compose restart boom: " + stack.Name)
	}
	return nil
}

// newRestartReconciler builds a reconciler whose restart endpoint works from
// the given stack set — the same cache a reconcile pass fills.
func newRestartReconciler(t *testing.T, stacks []config.Stack, restarter Restarter, store *state.Store) *Reconciler {
	t.Helper()
	rec := New(Deps{
		RepoURL:   "https://example.com/host.git",
		Branch:    "main",
		State:     store,
		Logger:    discardLogger(),
		Restarter: restarter,
	})
	rec.storeStacks(stacks)
	return rec
}

// stackWithDir returns a stack definition whose directory exists and —
// optional — carries the generated .env a deployment would have written.
func stackWithDir(t *testing.T, name string, withEnv bool) config.Stack {
	t.Helper()
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	if withEnv {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("A=1\n"), 0o600); err != nil {
			t.Fatalf("write stack env: %v", err)
		}
	}
	return config.Stack{Name: name, Enabled: true, ComposeFile: composeFile, Dir: dir}
}

// TestRestartAllStacks: an empty selection restarts every configured stack in
// configuration order, with the stack directory and the generated .env of
// each — and only "--env-file" when that file exists.
func TestRestartAllStacks(t *testing.T) {
	web := stackWithDir(t, "web", true)
	db := stackWithDir(t, "db", false)
	cr := &fakeRestarter{}
	rec := newRestartReconciler(t, []config.Stack{web, db}, cr, nil)

	restarted, failed, unknown, err := rec.Restart(context.Background(), nil)
	if err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if len(unknown) != 0 || len(failed) != 0 {
		t.Fatalf("Restart() unknown = %v, failed = %v, want none", unknown, failed)
	}
	if want := []string{"web", "db"}; !reflect.DeepEqual(restarted, want) {
		t.Errorf("restarted = %v, want %v", restarted, want)
	}
	wantCalls := []restartCall{
		{stack: "web", dir: web.Dir, envFile: filepath.Join(web.Dir, ".env")},
		{stack: "db", dir: db.Dir, envFile: ""},
	}
	if !reflect.DeepEqual(cr.calls, wantCalls) {
		t.Errorf("compose restarts = %+v, want %+v", cr.calls, wantCalls)
	}
}

// TestRestartSelectedStacks: explicit names keep request order, duplicates
// collapse.
func TestRestartSelectedStacks(t *testing.T) {
	web := stackWithDir(t, "web", true)
	db := stackWithDir(t, "db", true)
	cr := &fakeRestarter{}
	rec := newRestartReconciler(t, []config.Stack{web, db}, cr, nil)

	restarted, _, _, err := rec.Restart(context.Background(), []string{"db", "web", "db"})
	if err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if want := []string{"db", "web"}; !reflect.DeepEqual(restarted, want) {
		t.Errorf("restarted = %v, want %v", restarted, want)
	}
	var names []string
	for _, call := range cr.calls {
		names = append(names, call.stack)
	}
	if want := []string{"db", "web"}; !reflect.DeepEqual(names, want) {
		t.Errorf("compose restart order = %v, want %v", names, want)
	}
}

// TestRestartUnknownStackValidatesFirst: one unknown name rejects the whole
// request before any command runs — no partial success.
func TestRestartUnknownStackValidatesFirst(t *testing.T) {
	web := stackWithDir(t, "web", true)
	cr := &fakeRestarter{}
	rec := newRestartReconciler(t, []config.Stack{web}, cr, nil)

	restarted, failed, unknown, err := rec.Restart(context.Background(), []string{"web", "ghost"})
	if err != nil {
		t.Fatalf("Restart() error = %v, want nil (unknown names are a 400, not a 500)", err)
	}
	if want := []string{"ghost"}; !reflect.DeepEqual(unknown, want) {
		t.Errorf("unknown = %v, want %v", unknown, want)
	}
	if len(restarted) != 0 || len(failed) != 0 {
		t.Errorf("restarted = %v, failed = %v, want nothing restarted", restarted, failed)
	}
	if len(cr.calls) != 0 {
		t.Errorf("compose restarts = %+v, want none", cr.calls)
	}
}

// TestRestartFailuresCollected: a failing stack lands in failed while the
// others still restart.
func TestRestartFailuresCollected(t *testing.T) {
	web := stackWithDir(t, "web", true)
	db := stackWithDir(t, "db", true)
	cr := &fakeRestarter{failFor: map[string]bool{"db": true}}
	rec := newRestartReconciler(t, []config.Stack{web, db}, cr, nil)

	restarted, failed, unknown, err := rec.Restart(context.Background(), nil)
	if err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %v, want none", unknown)
	}
	if want := []string{"web"}; !reflect.DeepEqual(restarted, want) {
		t.Errorf("restarted = %v, want %v", restarted, want)
	}
	if len(failed) != 1 || !strings.Contains(failed["db"], "boom") {
		t.Errorf("failed = %v, want an entry for db", failed)
	}
	if len(cr.calls) != 2 {
		t.Errorf("compose restarts = %d, want 2 (a failing stack must not stop the rest)", len(cr.calls))
	}
}

// TestRestartAllFailed: every stack failing still returns the (empty) result
// instead of an error, so the endpoint can answer 200.
func TestRestartAllFailed(t *testing.T) {
	web := stackWithDir(t, "web", false)
	db := stackWithDir(t, "db", false)
	cr := &fakeRestarter{failFor: map[string]bool{"web": true, "db": true}}
	rec := newRestartReconciler(t, []config.Stack{web, db}, cr, nil)

	restarted, failed, _, err := rec.Restart(context.Background(), nil)
	if err != nil {
		t.Fatalf("Restart() error = %v, want nil", err)
	}
	if restarted == nil || len(restarted) != 0 {
		t.Errorf("restarted = %v, want an empty non-nil slice", restarted)
	}
	if len(failed) != 2 {
		t.Errorf("failed = %v, want both stacks", failed)
	}
}

// TestRestartWithoutConfiguration: before the first pass there is no stack
// set; the request is unservable (500) instead of silently restarting
// nothing.
func TestRestartWithoutConfiguration(t *testing.T) {
	rec := New(Deps{Logger: discardLogger(), Restarter: &fakeRestarter{}})
	if _, _, _, err := rec.Restart(context.Background(), nil); err == nil {
		t.Fatal("Restart() without a loaded configuration error = nil, want an error")
	}
}

// TestReconcileCachesStacksForRestart is the wiring test: a reconcile pass
// caches its configuration for POST /restart, and a restart then runs against
// the deployed stack's directory and the generated <stackDir>/.env the pass
// wrote — no second configuration load, no git.
func TestReconcileCachesStacksForRestart(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")

	cr := &fakeRestarter{}
	rec := New(Deps{
		RepoURL:   "https://example.invalid/host.git",
		Branch:    "main",
		WorkDir:   f.workDir,
		State:     f.state,
		Logger:    discardLogger(),
		Git:       f.git,
		Compose:   f.compose,
		Hooks:     f.hooks,
		Sops:      f.sops.decrypt,
		NewVault:  f.vault.make,
		Restarter: cr,
	})
	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	restarted, failed, unknown, err := rec.Restart(context.Background(), nil)
	if err != nil {
		t.Fatalf("Restart() after a pass error = %v", err)
	}
	if len(unknown) != 0 || len(failed) != 0 {
		t.Fatalf("Restart() unknown = %v, failed = %v, want none", unknown, failed)
	}
	if want := []string{"web"}; !reflect.DeepEqual(restarted, want) {
		t.Errorf("restarted = %v, want %v", restarted, want)
	}
	stackDir := filepath.Join(f.workDir, "host", "web")
	wantCall := restartCall{stack: "web", dir: stackDir, envFile: filepath.Join(stackDir, ".env")}
	if len(cr.calls) != 1 || cr.calls[0] != wantCall {
		t.Errorf("compose restarts = %+v, want [%+v]", cr.calls, wantCall)
	}
	if _, statErr := os.Stat(wantCall.envFile); statErr != nil {
		t.Errorf("the restart's --env-file %q does not exist: %v", wantCall.envFile, statErr)
	}
}

// TestRestartLeavesStateAlone pins the "no deployment" contract: a restart
// neither rewrites the state entry nor persists anything.
func TestRestartLeavesStateAlone(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "state.json"), discardLogger())
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	if err := store.Set("web", state.State{Hash: "abc123", Status: state.StatusDeployed}); err != nil {
		t.Fatalf("state.Set() error = %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}
	before, _ := store.Get("web")

	web := stackWithDir(t, "web", true)
	cr := &fakeRestarter{failFor: map[string]bool{"web": true}}
	rec := newRestartReconciler(t, []config.Stack{web}, cr, store)
	if _, _, _, err := rec.Restart(context.Background(), nil); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}

	after, ok := store.Get("web")
	if !ok {
		t.Fatal("state entry disappeared after a restart")
	}
	if after.Hash != before.Hash || after.Status != before.Status || after.StackDir != before.StackDir {
		t.Errorf("state changed after restart: before %+v, after %+v", before, after)
	}
	if names := store.Names(); !reflect.DeepEqual(names, []string{"web"}) {
		t.Errorf("state names = %v, want [web]", names)
	}
}
