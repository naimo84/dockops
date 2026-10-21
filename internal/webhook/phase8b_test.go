// Phase-8b regression tests: BuildStatus is the shared document assembly of
// GET /status and the CLI's -print-status mode — these tests pin that the
// direct call produces the identical document the endpoint serves.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/state"
)

// TestBuildStatusMatchesStatusEndpoint runs the handler and the shared
// BuildStatus over the same collaborators and compares the decoded
// documents: generated_at necessarily differs (two snapshots), everything
// else must be byte-equivalent — otherwise -print-status and /status would
// drift apart. The fixture includes both failure modes (per-stack ps error,
// failed project listing) so the tolerance contract is part of the
// comparison.
func TestBuildStatusMatchesStatusEndpoint(t *testing.T) {
	deployed := time.Date(2026, 10, 2, 8, 30, 0, 0, time.UTC)
	fc := &fakeCompose{
		containers: map[string][]compose.Container{
			"alpha": {{Name: "alpha-a", State: "running", Health: "healthy"}},
			"beta":  {{Name: "beta-1", State: "exited"}},
		},
		psErrs: map[string]error{
			"gamma": errors.New("docker daemon unreachable"),
		},
		refs: []compose.ProjectRef{
			{Project: "alpha", Containers: []string{"alpha-a"}},     // managed
			{Project: "dockops", Containers: []string{"dockops-1"}}, // self
			{Project: "legacy", Containers: []string{"legacy-1"}},   // unmanaged
		},
		refsErr: errors.New("docker ps blew up"),
	}

	store, err := state.New(filepath.Join(t.TempDir(), "state.json"), quietLogger())
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	entries := map[string]state.State{
		"alpha": {Hash: "h-alpha", Status: state.StatusDeployed, UpdatedAt: deployed},
		"beta":  {Hash: "h-beta", Status: state.StatusDeployed, UpdatedAt: deployed},
		"gamma": {Hash: "h-gamma", Status: state.StatusFailed, UpdatedAt: deployed},
	}
	for name, st := range entries {
		if err := store.Set(name, st); err != nil {
			t.Fatalf("state.Set(%s) error = %v", name, err)
		}
	}
	if err := store.Save(); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}

	// Endpoint side: the handler wired like the daemon would wire it.
	h := New(Deps{
		Secret:  "s3cr3t",
		Trigger: func() {},
		Logger:  quietLogger(),
		State:   store,
		Compose: fc,
		RepoURL: "git@example.com:homelab/host.git",
		Branch:  "main",
		HeadCommit: func() (string, error) {
			return "abc123def456", nil
		},
		SelfProject: "dockops",
	})
	rec := do(h, http.MethodGet, "/status", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status status = %d, want 200", rec.Code)
	}
	var endpoint map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &endpoint); err != nil {
		t.Fatalf("decode /status body %q: %v", rec.Body.String(), err)
	}

	// One-shot side: the same collaborators through the shared function.
	resp, err := BuildStatus(context.Background(), StatusDeps{
		State:   store,
		Compose: fc,
		RepoURL: "git@example.com:homelab/host.git",
		Branch:  "main",
		HeadCommit: func() (string, error) {
			return "abc123def456", nil
		},
		SelfProject: "dockops",
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("BuildStatus() error = %v", err)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal BuildStatus response: %v", err)
	}
	var direct map[string]any
	if err := json.Unmarshal(raw, &direct); err != nil {
		t.Fatalf("decode BuildStatus document %s: %v", raw, err)
	}

	// The tolerance contract holds on the direct document as well: the
	// failed stack degrades to unknown with its error, the failed project
	// listing to the top-level error, and neither fails the call.
	if got := driftOf(t, direct, "gamma"); got != "unknown" {
		t.Errorf("gamma drift = %q, want unknown (ps failed)", got)
	}
	if got := driftOf(t, direct, "alpha"); got != "ok" {
		t.Errorf("alpha drift = %q, want ok", got)
	}
	if msg, _ := direct["error"].(string); msg == "" {
		t.Errorf("top-level error = %v, want the listing failure message", direct["error"])
	}

	delete(endpoint, "generated_at")
	delete(direct, "generated_at")
	if !reflect.DeepEqual(endpoint, direct) {
		t.Errorf("BuildStatus document differs from /status response:\n/status:   %s\nBuildStatus: %s", rec.Body.String(), raw)
	}
}

// TestBuildStatusRequiresCollaborators pins the only error BuildStatus
// returns: missing configuration. Docker failures never do (see above).
func TestBuildStatusRequiresCollaborators(t *testing.T) {
	if _, err := BuildStatus(context.Background(), StatusDeps{Compose: &fakeCompose{}}); err == nil {
		t.Errorf("BuildStatus() without state store error = nil, want error")
	}
	store, err := state.New(filepath.Join(t.TempDir(), "state.json"), quietLogger())
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	if _, err := BuildStatus(context.Background(), StatusDeps{State: store}); err == nil {
		t.Errorf("BuildStatus() without compose runner error = nil, want error")
	}
}
