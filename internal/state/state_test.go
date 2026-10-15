package state

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// quietLogger keeps test output clean.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// statePath returns a state file path below a fresh temp dir that does not
// exist yet (proving New creates parent directories).
func statePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "var", "lib", "dockops", "state.json")
}

// stamp is a fixed timestamp for deterministic assertions.
var stamp = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// TestConcurrentAccess pins the concurrency contract phase 8 relies on: the
// webhook server reads the store from its request goroutines while the poll
// loop mutates it. The test only fails deterministically under -race, which
// the CI checks should run with.
func TestConcurrentAccess(t *testing.T) {
	s, err := New(statePath(t), quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				name := fmt.Sprintf("stack-%d", j%4)
				_ = s.Set(name, State{Hash: fmt.Sprintf("h-%d", i), Status: StatusDeployed})
				_, _ = s.Get(name)
				_ = s.Changed(name, "h-x")
				_ = s.Names()
			}
		}(i)
	}
	wg.Wait()

	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if got := len(s.Names()); got != 4 {
		t.Errorf("Names() = %v entries, want 4", got)
	}
}

func TestNewMissingFileYieldsEmptyStore(t *testing.T) {
	s, err := New(statePath(t), quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if names := s.Names(); len(names) != 0 {
		t.Errorf("Names() = %v, want empty", names)
	}
	if _, ok := s.Get("web"); ok {
		t.Error("Get() on empty store returned ok, want false")
	}
}

func TestSetGetDeleteNames(t *testing.T) {
	s, err := New(statePath(t), quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("web", State{Hash: "h1", Status: StatusDeployed, UpdatedAt: stamp}); err != nil {
		t.Fatalf("Set(web) error = %v", err)
	}
	if err := s.Set("db", State{Hash: "h2", Status: StatusFailed, UpdatedAt: stamp}); err != nil {
		t.Fatalf("Set(db) error = %v", err)
	}
	if got := s.Names(); !reflect.DeepEqual(got, []string{"db", "web"}) {
		t.Errorf("Names() = %v, want [db web]", got)
	}
	st, ok := s.Get("web")
	if !ok || st.Hash != "h1" || st.Status != StatusDeployed {
		t.Errorf("Get(web) = %+v, %v; want h1/deployed", st, ok)
	}
	if !s.Delete("web") {
		t.Error("Delete(web) = false, want true")
	}
	if s.Delete("web") {
		t.Error("second Delete(web) = true, want false")
	}
	if got := s.Names(); !reflect.DeepEqual(got, []string{"db"}) {
		t.Errorf("Names() after delete = %v, want [db]", got)
	}
}

func TestSetValidation(t *testing.T) {
	s, err := New(statePath(t), quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("", State{Hash: "h", Status: StatusDeployed}); err == nil {
		t.Error("Set(\"\") succeeded, want error")
	}
	err = s.Set("web", State{Hash: "h", Status: "sucess"})
	if err == nil || !strings.Contains(err.Error(), `invalid status "sucess"`) {
		t.Errorf("Set() with typo status error = %v, want invalid status error", err)
	}
	if err := s.Set("web", State{Hash: "h", Status: StatusDeployed}); err != nil {
		t.Fatalf("Set() with zero UpdatedAt error = %v", err)
	}
	st, _ := s.Get("web")
	if st.UpdatedAt.IsZero() {
		t.Error("zero UpdatedAt was not stamped with the current time")
	}
}

func TestChanged(t *testing.T) {
	s, err := New(statePath(t), quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !s.Changed("web", "h1") {
		t.Error("Changed() without entry = false, want true")
	}
	if err := s.Set("web", State{Hash: "h1", Status: StatusDeployed, UpdatedAt: stamp}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if s.Changed("web", "h1") {
		t.Error("Changed() with same hash and deployed status = true, want false")
	}
	if !s.Changed("web", "h2") {
		t.Error("Changed() with different hash = false, want true")
	}
	if err := s.Set("web", State{Hash: "h1", Status: StatusFailed, UpdatedAt: stamp}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if !s.Changed("web", "h1") {
		t.Error("Changed() with failed status = false, want true (failed deploys are retried)")
	}
}

func TestSaveReloadRoundTrip(t *testing.T) {
	path := statePath(t)
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	want := map[string]State{
		"web": {Hash: "h1", Status: StatusDeployed, UpdatedAt: stamp},
		"db":  {Hash: "h2", Status: StatusPruned, UpdatedAt: stamp.Add(time.Hour)},
	}
	for name, st := range want {
		if err := s.Set(name, st); err != nil {
			t.Fatalf("Set(%s) error = %v", name, err)
		}
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() after Save error = %v", err)
	}
	got := make(map[string]State, len(want))
	for _, name := range reloaded.Names() {
		st, _ := reloaded.Get(name)
		got[name] = st
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip states = %+v, want %+v", got, want)
	}

	// Deletes survive a round trip too.
	reloaded.Delete("web")
	if err := reloaded.Save(); err != nil {
		t.Fatalf("Save() after delete error = %v", err)
	}
	after, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() after second Save error = %v", err)
	}
	if names := after.Names(); !reflect.DeepEqual(names, []string{"db"}) {
		t.Errorf("Names() after delete round-trip = %v, want [db]", names)
	}
}

func TestSaveFileFormatAndPerms(t *testing.T) {
	path := statePath(t)
	s, err := New(path, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Set("web", State{Hash: "h1", Status: StatusDeployed, UpdatedAt: stamp}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file permission = %o, want 600", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var f fileModel
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
	if f.Version != fileVersion {
		t.Errorf("state file version = %d, want %d", f.Version, fileVersion)
	}
	if _, ok := f.Stacks["web"]; !ok {
		t.Errorf("state file = %s, want an entry for web", raw)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("state file does not end with a newline")
	}
}

func TestNewRejectsCorruptFile(t *testing.T) {
	path := statePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	_, err := New(path, quietLogger())
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "parse") {
		t.Errorf("New() with corrupt file error = %v, want parse error naming %s", err, path)
	}
}

func TestNewRejectsUnknownVersion(t *testing.T) {
	path := statePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"version":99,"stacks":{}}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	_, err := New(path, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "unsupported version 99") {
		t.Errorf("New() with unknown version error = %v, want unsupported version error", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New("", quietLogger()); err == nil {
		t.Error("New(\"\") succeeded, want error")
	}
	if _, err := New(filepath.Join(t.TempDir(), "state.json"), nil); err != nil {
		t.Errorf("New() with nil logger error = %v, want fallback to slog.Default()", err)
	}
}
