package reconciler

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/local/dockops/internal/state"
)

// One broken stack definition must not stop the others: a typo in one
// stack.yaml — possibly in a remote somebody else maintains — used to fail
// the whole configuration load and with it every deploy of the host.
//
// And it must not prune anything: the broken stack is missing from the
// loaded configuration, so to the prune it would look removed.
func TestBrokenStackDefinitionIsIsolated(t *testing.T) {
	f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
	f.addLocalStack("web", "name: web\n")
	f.addLocalStack("broken", "name: broken\nunknown_field: true\n")
	// A deployed stack that is no longer configured: prune would remove it.
	gone := filepath.Join(f.workDir, "gone")
	if err := f.state.Set("gone", state.State{
		Hash: "h-gone", Status: state.StatusDeployed,
		StackDir: gone, ComposeFile: filepath.Join(gone, "compose.yaml"),
	}); err != nil {
		t.Fatalf("state.Set() error = %v", err)
	}

	err := f.reconciler().Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), filepath.Join("broken", "stack.yaml")) {
		t.Errorf("Reconcile() error = %v, want the broken stack file named", err)
	}
	if got, want := f.upNames(), []string{"web"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Ups = %v, want %v (the healthy stack deploys anyway)", got, want)
	}
	if len(f.compose.downProjects) != 0 || len(f.compose.downs) != 0 {
		t.Errorf("prune ran (DownProject %v, Down %d) although a stack definition could not be loaded",
			f.compose.downProjects, len(f.compose.downs))
	}
	if _, ok := f.state.Get("gone"); !ok {
		t.Error("state entry of gone was pruned in a pass with a broken stack definition")
	}
}

// A removed stack whose compose file still exists is torn down with -f. When
// that fails — a ${VAR:?} the environment cannot fill, a file that no longer
// parses — the prune must not stay stuck retrying the same command forever:
// it falls back to the project name, like for a vanished directory.
func TestPruneFallsBackToProjectWhenDownFails(t *testing.T) {
	f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
	f.addLocalStack("web", "name: web\n")
	gone := filepath.Join(f.workDir, "gone")
	composeFile := filepath.Join(gone, "compose.yaml")
	writeFile(t, composeFile, "services:\n  old:\n    image: nginx:${TAG:?TAG required}\n")
	if err := f.state.Set("gone", state.State{
		Hash: "h-gone", Status: state.StatusDeployed, StackDir: gone, ComposeFile: composeFile,
	}); err != nil {
		t.Fatalf("state.Set() error = %v", err)
	}
	f.compose.failDownFor = "gone"

	if err := f.reconciler().Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !reflect.DeepEqual(f.compose.downProjects, []string{"gone"}) {
		t.Errorf("DownProject calls = %v, want the fallback for gone", f.compose.downProjects)
	}
	if _, ok := f.state.Get("gone"); ok {
		t.Error("state entry for gone still exists after the fallback teardown")
	}
}

// The prune interpolates with the stack's generated .env when it is still
// there: a required variable then resolves and the full down works.
func TestPruneUsesTheGeneratedStackEnv(t *testing.T) {
	f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
	f.addLocalStack("web", "name: web\n")
	gone := filepath.Join(f.workDir, "gone")
	composeFile := filepath.Join(gone, "compose.yaml")
	writeFile(t, composeFile, "services:\n  old:\n    image: nginx\n")
	writeFile(t, filepath.Join(gone, ".env"), "TAG=1.27\n")
	if err := f.state.Set("gone", state.State{
		Hash: "h-gone", Status: state.StatusDeployed, StackDir: gone, ComposeFile: composeFile,
	}); err != nil {
		t.Fatalf("state.Set() error = %v", err)
	}

	if err := f.reconciler().Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(f.compose.downs) != 1 || f.compose.downs[0].envFile != filepath.Join(gone, ".env") {
		t.Errorf("Down calls = %+v, want one with the stack's generated .env", f.compose.downs)
	}
}

// Editing a plaintext env file (an image tag, a flag) redeploys the stack —
// the same class as the vault template edit of #22: before, the hash only
// covered the path, and the change sat in the checkout.
func TestEnvFileEditRedeploys(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\nenv_files:\n  - config.env\n")
	envFile := filepath.Join(f.workDir, "host", "web", "config.env")
	writeFile(t, envFile, "TAG=1.0\n")
	r := f.reconciler()
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	writeFile(t, envFile, "TAG=1.1\n")
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if got := f.upNames(); !reflect.DeepEqual(got, []string{"web", "web"}) {
		t.Errorf("Ups = %v, want a redeploy after the env file edit", got)
	}
}
