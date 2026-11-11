package reconciler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/state"
)

// sinkLogger discards log output — the non-portable check warns on every
// finding and the tests assert the state, not the stream.
func sinkLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestNonPortableBindsAreRecorded is issue #18's suggestion 3, end to end at
// the reconciler: an absolute bind source outside the agent's mounts is stored
// on the stack, so GET /status can report it without touching docker or the
// compose files.
func TestNonPortableBindsAreRecorded(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", "name: web\n")
	writeFile(t, filepath.Join(f.workDir, "host", "web", "compose.yaml"), `
services:
  app:
    image: ghcr.io/x/app:latest
    volumes:
      - /nowhere/app.conf:/etc/app.conf:ro
`)
	f.compose.images = map[string]string{"app": "ghcr.io/x/app:latest"}
	f.compose.digests = map[string]string{"ghcr.io/x/app:latest": "sha256:x"}

	r := f.phase10Reconciler(&fakeResolver{digests: map[string]string{"ghcr.io/x/app:latest": "sha256:x"}}, &recordingNotifier{})
	r.bindRoots = []compose.BindRoot{{Container: f.workDir, Host: "/var/lib/dockops/repos"}}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	st, _ := f.state.Get("web")
	if len(st.NonPortableBinds) != 1 || !strings.Contains(st.NonPortableBinds[0], "/nowhere/app.conf") {
		t.Errorf("NonPortableBinds = %v, want the outside-mount source", st.NonPortableBinds)
	}
}

// TestNonPortableBindsClearedWhenFixed: the marker must disappear with the
// fix, otherwise an operator learns to ignore it.
func TestNonPortableBindsClearedWhenFixed(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", "name: web\n")
	writeFile(t, filepath.Join(f.workDir, "host", "web", "compose.yaml"), `
services:
  app:
    image: ghcr.io/x/app:latest
    volumes:
      - /nowhere/app.conf:/etc/app.conf:ro
`)
	f.compose.images = map[string]string{"app": "ghcr.io/x/app:latest"}
	f.compose.digests = map[string]string{"ghcr.io/x/app:latest": "sha256:x"}
	resolver := &fakeResolver{digests: map[string]string{"ghcr.io/x/app:latest": "sha256:x"}}

	r := f.phase10Reconciler(resolver, &recordingNotifier{})
	r.bindRoots = []compose.BindRoot{{Container: f.workDir, Host: "/var/lib/dockops/repos"}}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("first pass: Reconcile() error = %v", err)
	}
	if st, _ := f.state.Get("web"); len(st.NonPortableBinds) == 0 {
		t.Fatal("first pass recorded no finding")
	}

	// The source moves inside the mount → no finding on the next pass.
	writeFile(t, filepath.Join(f.workDir, "host", "web", "compose.yaml"), `
services:
  app:
    image: ghcr.io/x/app:latest
    volumes:
      - ./app.conf:/etc/app.conf:ro
`)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second pass: Reconcile() error = %v", err)
	}
	if st, _ := f.state.Get("web"); len(st.NonPortableBinds) != 0 {
		t.Errorf("NonPortableBinds = %v after the fix, want none", st.NonPortableBinds)
	}
}

// TestNonPortableBindsUnknownMountsStaysQuiet: without a container (or with an
// unreadable mount list) there is nothing to compare against — treating that
// as "nothing is inside a mount" would flag every absolute source.
func TestNonPortableBindsUnknownMountsStaysQuiet(t *testing.T) {
	r := &Reconciler{logger: sinkLogger()}
	if got := r.resolveBindRoots(config.Stack{Name: "web"}); len(got) != 0 {
		t.Errorf("resolveBindRoots() = %v, want none without a container", got)
	}
	// A second call reuses the cached empty answer (no docker call either).
	if got := r.resolveBindRoots(config.Stack{Name: "web"}); len(got) != 0 {
		t.Errorf("resolveBindRoots() = %v on the second call, want none", got)
	}
}

// TestNonPortableBindsStateRoundTrip: the store keeps and clears the list
// (additive state field, no fileVersion bump).
func TestNonPortableBindsStateRoundTrip(t *testing.T) {
	store, err := state.New(filepath.Join(t.TempDir(), "state.json"), sinkLogger())
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	if err := store.Set("web", state.State{Status: state.StatusDeployed}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := store.SetNonPortableBinds("web", []string{"a: /x (target /y)"}); err != nil {
		t.Fatalf("SetNonPortableBinds() error = %v", err)
	}
	if st, _ := store.Get("web"); len(st.NonPortableBinds) != 1 {
		t.Fatalf("NonPortableBinds = %v, want one entry", st.NonPortableBinds)
	}
	if err := store.SetNonPortableBinds("web", nil); err != nil {
		t.Fatalf("SetNonPortableBinds(nil) error = %v", err)
	}
	if st, _ := store.Get("web"); len(st.NonPortableBinds) != 0 {
		t.Errorf("NonPortableBinds = %v after clearing, want none", st.NonPortableBinds)
	}
}
