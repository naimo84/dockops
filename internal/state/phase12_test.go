package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pendingStamp is the hand-over time of the phase-12 state tests.
var pendingStamp = time.Date(2026, 10, 26, 10, 5, 0, 0, time.UTC)

// TestSelfUpdateFieldsRoundTrip: the phase-12 pending marker survives a
// save/load cycle and stays optional — fileVersion remains 1, and an empty
// marker keeps the keys out of the document.
func TestSelfUpdateFieldsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("dockops", State{Hash: "h1", Status: StatusDeployed, StackDir: "/stacks/dockops"}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := s.SetImages("dockops", map[string]ImageRecord{
		"ghcr.io/x/dockops:latest": {
			Service:            "dockops",
			CurrentDigest:      "sha256:old",
			RemoteDigest:       "sha256:new",
			UpdateAvailable:    true,
			CheckedAt:          pendingStamp,
			PreviousDigest:     "sha256:old",
			ApplyPendingDigest: "sha256:new",
			PendingAt:          pendingStamp,
		},
	}); err != nil {
		t.Fatalf("SetImages() error = %v", err)
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
		`"apply_pending_digest": "sha256:new"`,
		`"pending_at": "2026-10-26T10:05:00Z"`,
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
	rec, ok := st.Images["ghcr.io/x/dockops:latest"]
	if !ok {
		t.Fatalf("reloaded records = %+v, want the dockops image", st.Images)
	}
	if rec.ApplyPendingDigest != "sha256:new" {
		t.Errorf("ApplyPendingDigest = %q, want sha256:new", rec.ApplyPendingDigest)
	}
	if !rec.PendingAt.Equal(pendingStamp) {
		t.Errorf("PendingAt = %s, want %s", rec.PendingAt, pendingStamp)
	}
}

// TestSelfUpdateFieldsOmitted: a record without the pending marker keeps the
// digest key out of the document. PendingAt behaves like AppliedAt: a zero
// time.Time is not omitted by encoding/json, and readers treat the zero stamp
// as "never happened" — the documented wart of the phase-11 timestamps.
func TestSelfUpdateFieldsOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("web", State{Hash: "h1", Status: StatusDeployed}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := s.SetImages("web", map[string]ImageRecord{
		"ghcr.io/x/app:latest": {Service: "app", CurrentDigest: "sha256:old", CheckedAt: pendingStamp},
	}); err != nil {
		t.Fatalf("SetImages() error = %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if strings.Contains(string(raw), "apply_pending_digest") {
		t.Errorf("state file contains apply_pending_digest although no self update is pending:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"pending_at": "0001-01-01T00:00:00Z"`) {
		t.Errorf("zero PendingAt should serialize like the other zero timestamps:\n%s", raw)
	}
}

// TestStorePath: the store exposes the path it persists to, so the
// self-update files can live next to it without a second configuration
// source.
func TestStorePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := s.Path(); got != path {
		t.Errorf("Path() = %q, want %q", got, path)
	}
}
