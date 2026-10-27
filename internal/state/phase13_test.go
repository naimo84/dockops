package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deployStamp is the hand-over time of the phase-13 state tests.
var deployStamp = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

// TestDeployPendingRoundTrip: the deploy pending marker survives a save/load
// cycle, keeps its UTC timestamp and does not bump the file version.
func TestDeployPendingRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("dockops", State{
		Hash:              "h1",
		Status:            StatusDeployed,
		StackDir:          "/stacks/dockops",
		DeployPendingHash: "h2",
		DeployPendingAt:   deployStamp,
	}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	for _, want := range []string{
		`"version": 1`,
		`"deploy_pending_hash": "h2"`,
		`"deploy_pending_at": "2026-10-27T09:30:00Z"`,
		// The deployed hash stays untouched while the deploy is in flight:
		// the pending hash is the claim, not the truth.
		`"hash": "h1"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state file does not contain %s:\n%s", want, raw)
		}
	}

	reloaded, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New(reload) error = %v", err)
	}
	st, ok := reloaded.Get("dockops")
	if !ok {
		t.Fatal("reloaded store has no entry for dockops")
	}
	if st.DeployPendingHash != "h2" {
		t.Errorf("DeployPendingHash = %q, want h2", st.DeployPendingHash)
	}
	if !st.DeployPendingAt.Equal(deployStamp) {
		t.Errorf("DeployPendingAt = %s, want %s", st.DeployPendingAt, deployStamp)
	}
	if st.Hash != "h1" || st.Status != StatusDeployed {
		t.Errorf("entry = %+v, want the previous deployment untouched", st)
	}
	// The pending marker is not a change the store hides: the hash differs
	// from the pending one, so a redeploy would be due once the marker is
	// cleared (deployStack's guard is what keeps it untouched meanwhile).
	if !reloaded.Changed("dockops", "h2") {
		t.Error("Changed(h2) = false, want true while the recorded hash is still h1")
	}
}

// TestDeployPendingOmitted: without a hand-over the hash key stays out of the
// document. DeployPendingAt behaves like the other timestamps: encoding/json
// does not omit a zero time.Time, so the key remains with the zero stamp and
// readers treat that as "never happened" (the documented wart of AppliedAt and
// PendingAt — the field shape is prescribed, not invented here).
func TestDeployPendingOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("web", State{Hash: "h1", Status: StatusDeployed}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if strings.Contains(string(raw), "deploy_pending_hash") {
		t.Errorf("state file contains deploy_pending_hash although no deploy is pending:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"deploy_pending_at": "0001-01-01T00:00:00Z"`) {
		t.Errorf("zero DeployPendingAt should serialize like the other zero timestamps:\n%s", raw)
	}
}

// TestDeployPendingOldFileLoads: a state file written before phase 13 loads
// unchanged — no pending, version 1.
func TestDeployPendingOldFileLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{
  "version": 1,
  "stacks": {
    "dockops": {"hash": "h1", "status": "deployed", "updated_at": "2026-10-26T10:00:00Z",
      "stack_dir": "/stacks/dockops", "update_policy": "apply"}
  }
}
`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	st, ok := s.Get("dockops")
	if !ok {
		t.Fatal("old state file lost its entry")
	}
	if st.DeployPendingHash != "" || !st.DeployPendingAt.IsZero() {
		t.Errorf("deploy pending = %q/%s, want none", st.DeployPendingHash, st.DeployPendingAt)
	}
	if st.Hash != "h1" || st.UpdatePolicy != "apply" {
		t.Errorf("entry = %+v, want the phase-12 fields intact", st)
	}
}
