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
