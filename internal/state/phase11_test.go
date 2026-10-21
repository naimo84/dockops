package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// applyStamps are the timestamps of the phase-11 state tests.
var (
	appliedStamp    = time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)
	rolledBackStamp = time.Date(2026, 10, 22, 9, 45, 0, 0, time.UTC)
)

// TestApplyFieldsRoundTrip: the phase-11 bookkeeping of ImageRecord and the
// persisted update policy survive a save/load cycle unchanged (fileVersion
// stays 1 — the fields are additive).
func TestApplyFieldsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("web", State{
		Hash:         "h1",
		Status:       StatusDeployed,
		StackDir:     "/stacks/web",
		UpdatePolicy: "apply",
	}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	recs := map[string]ImageRecord{
		"ghcr.io/x/app:latest": {
			Service:            "app",
			CurrentDigest:      "sha256:old",
			RemoteDigest:       "sha256:new",
			UpdateAvailable:    true,
			CheckedAt:          imageStamp,
			PreviousDigest:     "sha256:before",
			AppliedAt:          appliedStamp,
			RolledBackAt:       rolledBackStamp,
			ApplyBlockedDigest: "sha256:new",
			ApplyError:         "service app: container web-app-1 is unhealthy",
		},
	}
	if err := s.SetImages("web", recs); err != nil {
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
		`"update_policy": "apply"`,
		`"previous_digest": "sha256:before"`,
		`"applied_at": "2026-10-22T09:30:00Z"`,
		`"rolled_back_at": "2026-10-22T09:45:00Z"`,
		`"apply_blocked_digest": "sha256:new"`,
		`"apply_error": "service app: container web-app-1 is unhealthy"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state file does not contain %s:\n%s", want, raw)
		}
	}

	reloaded, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New(reload) error = %v", err)
	}
	st, ok := reloaded.Get("web")
	if !ok {
		t.Fatal("reloaded store has no entry for web")
	}
	if st.UpdatePolicy != "apply" {
		t.Errorf("UpdatePolicy = %q, want apply", st.UpdatePolicy)
	}
	if !reflect.DeepEqual(st.Images, recs) {
		t.Errorf("Images = %+v, want %+v", st.Images, recs)
	}
}

// TestApplyFieldsOmittedWhenEmpty keeps the wire format additive: a notify
// stack's record carries no apply keys at all.
func TestApplyFieldsOmittedWhenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("web", State{Hash: "h1", Status: StatusDeployed}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := s.SetImages("web", map[string]ImageRecord{
		"nginx:latest": {Service: "app", CheckedAt: imageStamp},
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
	// The string fields stay out of the document. The two timestamps keep
	// their keys as the zero time: encoding/json's omitempty does not apply
	// to a time.Time struct, and the briefing prescribes exactly this field
	// shape — readers treat the zero value as "never happened" (see
	// webhook.formatTime).
	for _, unwanted := range []string{"previous_digest", "apply_blocked_digest", "apply_error", "update_policy"} {
		if strings.Contains(string(raw), unwanted) {
			t.Errorf("state file contains %q although nothing was applied:\n%s", unwanted, raw)
		}
	}
}

// TestOldStateFileLoadsWithApplyFieldsZero: a phase-10 state file (no apply
// keys, no update_policy) loads and reads as "nothing applied, policy
// default".
func TestOldStateFileLoadsWithApplyFieldsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{
  "version": 1,
  "stacks": {
    "web": {
      "hash": "h1",
      "status": "deployed",
      "updated_at": "2026-10-21T20:00:00Z",
      "images": {
        "ghcr.io/x/app:latest": {
          "service": "app",
          "current_digest": "sha256:old",
          "remote_digest": "sha256:new",
          "update_available": true,
          "checked_at": "2026-10-21T20:00:00Z"
        }
      }
    }
  }
}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatalf("write old state file: %v", err)
	}
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v, want the old file to load", err)
	}
	st, ok := s.Get("web")
	if !ok {
		t.Fatal("no entry for web")
	}
	if st.UpdatePolicy != "" {
		t.Errorf("UpdatePolicy = %q, want empty for an old file", st.UpdatePolicy)
	}
	rec := st.Images["ghcr.io/x/app:latest"]
	if rec.PreviousDigest != "" || !rec.AppliedAt.IsZero() || !rec.RolledBackAt.IsZero() || rec.ApplyBlockedDigest != "" || rec.ApplyError != "" {
		t.Errorf("record = %+v, want zero apply fields", rec)
	}
}
