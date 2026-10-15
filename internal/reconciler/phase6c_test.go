package reconciler

// Phase 6c regression tests: the prune and skip invariants observed broken in
// the devbox livetest (06:44 run).
//
// Root cause of both live symptoms: Reconcile persisted the state only once
// at the end of a pass. A pass that ended before that save (crash, kill,
// failing save) left the state file stale, and the next pass then (a)
// redeployed stacks whose hash was identical (bug 2: "Recreate") and (b)
// knew nothing about stacks that had since disappeared from the
// configuration — no compose down, no DownProject, and the state file ended
// up holding only the stacks redeployed in that pass (bug 1: "State wird auf
// 1 Stack gekürzt"). The fix persists every recorded state change
// immediately; the tests below simulate the interrupted pass by making the
// state directory unwritable during the second stack's compose up.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/local/dockops/internal/state"
)

// discardLogger returns a logger that writes nowhere, matching the fixture.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestPhase6CPruneRobustCases pins the three prune cases from the phase-6c
// briefing: (a) compose file intact → full down with -f, (b) file gone →
// DownProject by name only, (c) state entry without any trace → state remove
// plus warning, no compose call. None of the cases may abort the pass (the
// surviving stack still deploys).
func TestPhase6CPruneRobustCases(t *testing.T) {
	t.Run("a: intact compose file tears down with -f", func(t *testing.T) {
		f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
		f.addLocalStack("web", "name: web\n")
		gone := filepath.Join(f.workDir, "host", "apps", "gone")
		composeFile := filepath.Join(gone, "compose.yaml")
		writeFile(t, composeFile, "services:\n  old:\n    image: nginx\n")
		seedState(t, f, "gone", gone, composeFile, nil)

		if err := f.reconciler().Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile() error = %v (prune must not abort the pass)", err)
		}
		if len(f.compose.downs) != 1 || f.compose.downs[0].stack.Name != "gone" {
			t.Errorf("Down calls = %+v, want one for gone (case a: -f with intact file)", f.compose.downs)
		}
		if len(f.compose.downProjects) != 0 {
			t.Errorf("DownProject calls = %v, want none in case a", f.compose.downProjects)
		}
		if _, ok := f.state.Get("gone"); ok {
			t.Error("state entry for gone still exists after prune")
		}
		if got := f.upNames(); !reflect.DeepEqual(got, []string{"web"}) {
			t.Errorf("Ups = %v, want [web] (pass continued)", got)
		}
	})

	t.Run("b: deleted stack directory falls back to DownProject", func(t *testing.T) {
		f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
		f.addLocalStack("web", "name: web\n")
		// The stack folder is gone entirely — compose file and directory,
		// exactly the livetest situation (apps/selftest removed from the
		// host repo, checkout updated by reset --hard + clean -fd).
		gone := filepath.Join(f.workDir, "host", "apps", "gone")
		seedState(t, f, "gone", gone, filepath.Join(gone, "compose.yaml"), nil)

		if err := f.reconciler().Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile() error = %v (prune must not abort the pass)", err)
		}
		if !reflect.DeepEqual(f.compose.downProjects, []string{"gone"}) {
			t.Errorf("DownProject calls = %v, want [gone] (case b: -p only)", f.compose.downProjects)
		}
		if len(f.compose.downs) != 0 {
			t.Errorf("Down calls = %d, want 0 in case b", len(f.compose.downs))
		}
		if _, ok := f.state.Get("gone"); ok {
			t.Error("state entry for gone still exists after prune")
		}
	})

	t.Run("c: traceless entry is removed with a warning, no compose call", func(t *testing.T) {
		f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
		f.addLocalStack("web", "name: web\n")
		// A state entry without stack_dir / compose_file / compose_files —
		// e.g. written by an older dockops version. There is no evidence
		// the project ever existed, so the entry is dropped with a warning
		// instead of shelling out (a DownProject against docker compose
		// versions that insist on -f would otherwise fail forever and keep
		// the entry in an endless retry loop).
		seedState(t, f, "gone", "", "", nil)

		if err := f.reconciler().Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile() error = %v (prune must not abort the pass)", err)
		}
		if len(f.compose.downs) != 0 || len(f.compose.downProjects) != 0 {
			t.Errorf("compose calls = %d downs, %d downProjects; want none in case c",
				len(f.compose.downs), len(f.compose.downProjects))
		}
		if _, ok := f.state.Get("gone"); ok {
			t.Error("state entry for gone still exists after prune")
		}
		if got := f.upNames(); !reflect.DeepEqual(got, []string{"web"}) {
			t.Errorf("Ups = %v, want [web] (pass continued)", got)
		}
	})
}

// TestPhase6CSkipUnchangedAcrossPrunePass reproduces the live topology of
// the 06:44 run in the healthy case: two deployed stacks, one disappears
// from the host repo, and the next pass must prune it while SKIPPING the
// unchanged survivor (bug 2: identical hash must not redeploy).
func TestPhase6CSkipUnchangedAcrossPrunePass(t *testing.T) {
	f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
	f.addLocalStack("apps/e2e", "name: dockops-e2e\n")
	f.addLocalStack("apps/selftest", "name: dockops-selftest\n")

	r := f.reconciler()
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 1 Reconcile() error = %v", err)
	}
	if got, want := f.upNames(), []string{"dockops-e2e", "dockops-selftest"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pass 1 Ups = %v, want %v", got, want)
	}

	// The host repo deletes all files of the selftest stack; the checkout
	// update removes the directory (tracked files via reset, the generated
	// .env via clean).
	selftestDir := filepath.Join(f.workDir, "host", "apps", "selftest")
	if err := os.RemoveAll(selftestDir); err != nil {
		t.Fatalf("remove selftest dir: %v", err)
	}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 2 Reconcile() error = %v", err)
	}
	if got, want := f.upNames(), []string{"dockops-e2e", "dockops-selftest"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pass 2 Ups = %v, want %v (identical hash must skip, prune must not redeploy)",
			got, want)
	}
	if !reflect.DeepEqual(f.compose.downProjects, []string{"dockops-selftest"}) {
		t.Errorf("DownProject calls = %v, want [dockops-selftest]", f.compose.downProjects)
	}
	if _, ok := f.state.Get("dockops-selftest"); ok {
		t.Error("state entry for dockops-selftest still exists after prune")
	}
}

// TestPhase6CInterruptedPassRedeploysAndForgetsPrune reproduces both live
// bugs against their common root cause: pass N ends before its final state
// save (simulated by an unwritable state directory from the last stack's
// compose up on), so pass N+1 starts from a state file without the deployed
// entries.
//
// Stacks deploy in discovery order (e2e < selftest < zz, the sacrificial
// third stack whose up turns the state directory unwritable); the saves of
// dockops-e2e and dockops-selftest precede the interruption, so a correct
// implementation has both persisted when the pass dies.
func TestPhase6CInterruptedPassRedeploysAndForgetsPrune(t *testing.T) {
	newInterruptedFixture := func(t *testing.T) (f *fixture, stateDir string) {
		t.Helper()
		f = newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
		f.addLocalStack("apps/e2e", "name: dockops-e2e\n")
		f.addLocalStack("apps/selftest", "name: dockops-selftest\n")
		f.addLocalStack("apps/zz", "name: dockops-zz\n")

		// The fixture's own store writes to a throwaway path; point the
		// reconciler at a state file whose directory the test can lock
		// down mid-pass.
		stateDir = t.TempDir()
		statePath := filepath.Join(stateDir, "state.json")
		store, err := state.New(statePath, discardLogger())
		if err != nil {
			t.Fatalf("state.New() error = %v", err)
		}
		f.state = store

		// During dockops-zz's compose up (the last stack of the pass) the
		// state directory becomes unwritable: every save from that point
		// on fails, standing in for a pass that is interrupted before its
		// final state save (crash, kill, failing disk, ...).
		f.compose.onUp = func(name string) {
			if name != "dockops-zz" {
				return
			}
			if err := os.Chmod(stateDir, 0o500); err != nil {
				t.Errorf("chmod state dir: %v", err)
			}
		}
		t.Cleanup(func() { os.Chmod(stateDir, 0o700) }) // best effort
		return f, stateDir
	}
	// restart simulates the daemon coming back: the injection heals, the
	// store is rebuilt from whatever reached the disk, and the next pass
	// runs against it.
	restart := func(t *testing.T, f *fixture, stateDir string) {
		t.Helper()
		f.compose.onUp = nil
		if err := os.Chmod(stateDir, 0o700); err != nil {
			t.Fatalf("chmod state dir back: %v", err)
		}
		restarted, err := state.New(filepath.Join(stateDir, "state.json"), discardLogger())
		if err != nil {
			t.Fatalf("state.New() error = %v", err)
		}
		f.state = restarted
	}

	t.Run("bug 2: identical hash redeploys after interrupted pass", func(t *testing.T) {
		f, stateDir := newInterruptedFixture(t)

		// Pass N: all three deploy, but the state cannot be persisted from
		// dockops-zz's up on (the error is expected with and without the
		// fix; only its origin differs).
		if err := f.reconciler().Reconcile(context.Background()); err == nil {
			t.Fatal("pass 1 error = nil, want state save failure")
		}
		restart(t, f, stateDir)

		// Pass N+1: the stacks deployed before the interruption must skip
		// on their identical hashes. Without per-stack persistence the
		// file is empty and everything redeploys: the live "Recreate"
		// symptom. Only dockops-zz — whose own save was interrupted —
		// legitimately redeploys.
		if err := f.reconciler().Reconcile(context.Background()); err != nil {
			t.Fatalf("pass 2 Reconcile() error = %v", err)
		}
		want := []string{"dockops-e2e", "dockops-selftest", "dockops-zz", "dockops-zz"}
		if got := f.upNames(); !reflect.DeepEqual(got, want) {
			t.Errorf("Ups across both passes = %v, want %v (e2e and selftest skip on their identical hashes)",
				got, want)
		}
	})

	t.Run("bug 1: removed stack is not pruned after interrupted pass", func(t *testing.T) {
		f, stateDir := newInterruptedFixture(t)

		if err := f.reconciler().Reconcile(context.Background()); err == nil {
			t.Fatal("pass 1 error = nil, want state save failure")
		}
		restart(t, f, stateDir)

		// Between the passes the host repo removes the selftest stack —
		// and its files vanish from the checkout.
		if err := os.RemoveAll(filepath.Join(f.workDir, "host", "apps", "selftest")); err != nil {
			t.Fatalf("remove selftest dir: %v", err)
		}

		// Pass N+1: the persisted state knows about dockops-selftest's
		// previous deployment, so its entry must be pruned with
		// DownProject. Without per-stack persistence the state file is
		// empty: no prune, no down, and the saved state shrinks to the
		// stacks that redeployed — the live symptom.
		if err := f.reconciler().Reconcile(context.Background()); err != nil {
			t.Fatalf("pass 2 Reconcile() error = %v", err)
		}
		if !reflect.DeepEqual(f.compose.downProjects, []string{"dockops-selftest"}) {
			t.Errorf("DownProject calls = %v, want [dockops-selftest] (removed stack must be torn down even after an interrupted predecessor pass)",
				f.compose.downProjects)
		}
		if _, ok := f.state.Get("dockops-selftest"); ok {
			t.Error("state entry for dockops-selftest still exists after prune")
		}
	})
}

// seedState writes a deployed state entry for name, as a previous successful
// deployment would have.
func seedState(t *testing.T, f *fixture, name, stackDir, composeFile string, composeFiles []string) {
	t.Helper()
	if err := f.state.Set(name, state.State{
		Hash: "h-" + name, Status: state.StatusDeployed,
		StackDir: stackDir, ComposeFile: composeFile, ComposeFiles: composeFiles,
	}); err != nil {
		t.Fatalf("state.Set(%s) error = %v", name, err)
	}
}
