package reconciler

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func (f *fixture) forceFlags() []bool {
	out := make([]bool, 0, len(f.compose.ups))
	for _, up := range f.compose.ups {
		out = append(out, up.forceRecreate)
	}
	return out
}

// The full life cycle of a restart file: the first deploy recreates (the
// containers may still run an older config), an unchanged file skips, a
// changed file redeploys with --force-recreate, and a compose-only change
// redeploys without it.
func TestRestartFilesForceRecreate(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("traefik", "name: traefik\nrestart_files:\n  - config.yaml\n")
	cfg := filepath.Join(f.workDir, "host", "traefik", "config.yaml")
	writeFile(t, cfg, "metrics: {}\n")
	r := f.reconciler()
	ctx := context.Background()

	pass := func(label string) {
		t.Helper()
		if err := r.Reconcile(ctx); err != nil {
			t.Fatalf("%s: Reconcile() error = %v", label, err)
		}
	}

	pass("first")
	if got := f.forceFlags(); len(got) != 1 || !got[0] {
		t.Fatalf("first pass force flags = %v, want [true]", got)
	}
	st, _ := f.state.Get("traefik")
	if st.RestartFilesDigest == "" {
		t.Error("state.RestartFilesDigest empty after deploy")
	}

	pass("unchanged")
	if got := len(f.compose.ups); got != 1 {
		t.Fatalf("unchanged pass: %d ups, want 1 (skip)", got)
	}

	writeFile(t, cfg, "metrics:\n  prometheus:\n    addRoutersLabels: true\n")
	pass("restart file changed")
	if got := f.forceFlags(); len(got) != 2 || !got[1] {
		t.Fatalf("after restart file edit force flags = %v, want [... true]", got)
	}

	writeFile(t, filepath.Join(f.workDir, "host", "traefik", "compose.yaml"),
		"services:\n  app:\n    image: nginx:1.27\n")
	pass("compose changed")
	if got := f.forceFlags(); len(got) != 3 || got[2] {
		t.Fatalf("after compose-only edit force flags = %v, want [... false]", got)
	}
}

// Stacks without restart_files never force a recreate.
func TestNoRestartFilesNoForce(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")
	if err := f.reconciler().Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := f.forceFlags(); len(got) != 1 || got[0] {
		t.Errorf("force flags = %v, want [false]", got)
	}
}

// A missing restart file fails the stack instead of silently disabling the
// restart it was configured for.
func TestRestartFileMissingFails(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("traefik", "name: traefik\nrestart_files:\n  - nope.yaml\n")
	err := f.reconciler().Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "restart file") {
		t.Fatalf("Reconcile() error = %v, want a restart file error", err)
	}
	if got := len(f.compose.ups); got != 0 {
		t.Errorf("%d ups, want none", got)
	}
}
