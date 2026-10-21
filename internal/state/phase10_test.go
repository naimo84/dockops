package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// imageStamp is the check time used by the phase-10 tests.
var imageStamp = time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)

func TestImagesRoundTrip(t *testing.T) {
	path := statePath(t)
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("web", State{Hash: "h1", Status: StatusDeployed, StackDir: "/stacks/web"}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	recs := map[string]ImageRecord{
		"ghcr.io/x/app:latest": {
			Service:         "app",
			CurrentDigest:   "sha256:old",
			RemoteDigest:    "sha256:new",
			UpdateAvailable: true,
			CheckedAt:       imageStamp,
		},
		"ghcr.io/x/db:16": {
			Service:       "db",
			CurrentDigest: "sha256:same",
			RemoteDigest:  "sha256:same",
			CheckedAt:     imageStamp,
			Error:         "registry: rate limited",
		},
	}
	if err := s.SetImages("web", recs); err != nil {
		t.Fatalf("SetImages() error = %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New(reload) error = %v", err)
	}
	st, ok := reloaded.Get("web")
	if !ok {
		t.Fatal("reloaded store has no entry for web")
	}
	if !reflect.DeepEqual(st.Images, recs) {
		t.Errorf("Images = %+v, want %+v", st.Images, recs)
	}
	// The record must not have disturbed the deployment fields.
	if st.Hash != "h1" || st.Status != StatusDeployed || st.StackDir != "/stacks/web" {
		t.Errorf("state after SetImages = %+v, want the deployment fields unchanged", st)
	}
}

func TestImagesAreOmittedWhenEmpty(t *testing.T) {
	path := statePath(t)
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
		t.Fatalf("read state: %v", err)
	}
	if strings.Contains(string(raw), `"images"`) {
		t.Errorf("state file %s contains an images key although none were recorded", raw)
	}
}

// TestOldStateFileWithoutImagesLoads pins the additive contract: a state file
// written before phase 10 (version 1, no images key) keeps loading, and saving
// it again keeps version 1.
func TestOldStateFileWithoutImagesLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	legacy := `{
  "version": 1,
  "stacks": {
    "web": {
      "hash": "legacy-hash",
      "status": "deployed",
      "updated_at": "2026-10-14T12:00:00Z",
      "stack_dir": "/stacks/web",
      "compose_file": "/stacks/web/compose.yaml"
    }
  }
}
`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New(legacy) error = %v", err)
	}
	st, ok := s.Get("web")
	if !ok {
		t.Fatal("legacy entry missing")
	}
	if st.Hash != "legacy-hash" || st.Status != StatusDeployed {
		t.Errorf("legacy entry = %+v, want hash and status preserved", st)
	}
	if st.Images != nil {
		t.Errorf("Images = %+v, want nil for a legacy entry", st.Images)
	}

	if err := s.SetImages("web", map[string]ImageRecord{
		"nginx:latest": {Service: "app", CurrentDigest: "sha256:a", RemoteDigest: "sha256:b", UpdateAvailable: true, CheckedAt: imageStamp},
	}); err != nil {
		t.Fatalf("SetImages() error = %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var doc struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse saved state: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("saved version = %d, want 1 (additive field, no schema bump)", doc.Version)
	}
	if !strings.Contains(string(raw), `"images"`) {
		t.Errorf("saved state %s has no images key after SetImages", raw)
	}
}

func TestSetImagesRequiresStackEntry(t *testing.T) {
	s, err := New(statePath(t), quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = s.SetImages("ghost", map[string]ImageRecord{"nginx:latest": {CheckedAt: imageStamp}})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("SetImages(ghost) error = %v, want an error naming the stack", err)
	}
	if err := s.SetImages("", nil); err == nil {
		t.Error("SetImages(\"\") error = nil, want error")
	}
}

func TestSetImagesNormalizesCheckTime(t *testing.T) {
	s, err := New(statePath(t), quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("web", State{Hash: "h1", Status: StatusDeployed}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	zone := time.FixedZone("CEST", 2*60*60)
	checkedAt := time.Date(2026, 10, 21, 22, 0, 0, 0, zone)
	if err := s.SetImages("web", map[string]ImageRecord{
		"nginx:latest": {Service: "app", CheckedAt: checkedAt},
	}); err != nil {
		t.Fatalf("SetImages() error = %v", err)
	}
	st, _ := s.Get("web")
	if got := st.Images["nginx:latest"].CheckedAt; !got.Equal(checkedAt) || got.Location() != time.UTC {
		t.Errorf("CheckedAt = %v (%v), want the same instant in UTC", got, got.Location())
	}
}

func TestSetImagesReplacesPreviousRecords(t *testing.T) {
	s, err := New(statePath(t), quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("web", State{Hash: "h1", Status: StatusDeployed}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := s.SetImages("web", map[string]ImageRecord{"a:1": {CheckedAt: imageStamp}}); err != nil {
		t.Fatalf("SetImages() error = %v", err)
	}
	if err := s.SetImages("web", map[string]ImageRecord{"b:2": {CheckedAt: imageStamp}}); err != nil {
		t.Fatalf("SetImages() error = %v", err)
	}
	st, _ := s.Get("web")
	if _, ok := st.Images["a:1"]; ok {
		t.Errorf("Images = %+v, want the previous records replaced", st.Images)
	}
	if _, ok := st.Images["b:2"]; !ok {
		t.Errorf("Images = %+v, want the new record", st.Images)
	}
	// A copy is stored: mutating the caller's map afterwards must not leak.
	recs := map[string]ImageRecord{"c:3": {CheckedAt: imageStamp}}
	if err := s.SetImages("web", recs); err != nil {
		t.Fatalf("SetImages() error = %v", err)
	}
	delete(recs, "c:3")
	st, _ = s.Get("web")
	if _, ok := st.Images["c:3"]; !ok {
		t.Error("SetImages() kept a reference to the caller's map")
	}
}
