package webhook

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/state"
)

// updatesOf returns the "updates" object of the named stack entry, or nil
// when the key is absent.
func updatesOf(t *testing.T, resp map[string]any, name string) map[string]any {
	t.Helper()
	stacks, _ := resp["stacks"].([]any)
	for _, s := range stacks {
		m, _ := s.(map[string]any)
		if m["name"] != name {
			continue
		}
		raw, ok := m["updates"]
		if !ok || raw == nil {
			return nil
		}
		updates, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("stacks[%s].updates = %T, want JSON object", name, raw)
		}
		return updates
	}
	t.Fatalf("no stacks entry named %q", name)
	return nil
}

func TestStatusReportsImageUpdates(t *testing.T) {
	older := time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)
	entry := state.State{
		Hash:     "abc123",
		Status:   state.StatusDeployed,
		StackDir: "/stacks/web",
		Images: map[string]state.ImageRecord{
			"ghcr.io/x/app:latest": {
				Service:         "app",
				CurrentDigest:   "sha256:aaaaaaaaaaaaaaaa",
				RemoteDigest:    "sha256:bbbbbbbbbbbbbbbb",
				UpdateAvailable: true,
				CheckedAt:       newer,
			},
			"ghcr.io/x/db:16": {
				Service:       "db",
				CurrentDigest: "sha256:cccccccccccccccc",
				RemoteDigest:  "sha256:cccccccccccccccc",
				CheckedAt:     older,
				Error:         "registry: rate limited",
			},
		},
	}
	fc := &fakeCompose{infos: []compose.ContainerInfo{{Project: "web", Name: "web-app-1", State: "running"}}}
	h := statusFixture(t, map[string]state.State{"web": entry}, fc, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeStatus(t, rec)

	if got := driftOf(t, resp, "web"); got != "ok" {
		t.Errorf("drift = %q, want ok (image records must not change the drift statement)", got)
	}
	updates := updatesOf(t, resp, "web")
	if updates == nil {
		t.Fatal("updates block missing although records exist")
	}
	if updates["policy"] != "notify" {
		t.Errorf("policy = %v, want notify", updates["policy"])
	}
	// CheckedAt is the newest record timestamp.
	if got, want := updates["checked_at"], "2026-10-21T20:00:00Z"; got != want {
		t.Errorf("checked_at = %v, want %v", got, want)
	}
	if got, want := updates["pending"], float64(1); got != want {
		t.Errorf("pending = %v, want %v", got, want)
	}
	services, ok := updates["services"].([]any)
	if !ok {
		t.Fatalf("services = %T, want JSON array", updates["services"])
	}
	if len(services) != 2 {
		t.Fatalf("services = %v, want two entries", services)
	}
	first, _ := services[0].(map[string]any)
	second, _ := services[1].(map[string]any)
	if first["service"] != "app" || second["service"] != "db" {
		t.Errorf("services order = %v, want app before db (sorted)", services)
	}
	if first["image"] != "ghcr.io/x/app:latest" {
		t.Errorf("image = %v, want the reference as map key", first["image"])
	}
	if first["update_available"] != true {
		t.Errorf("app update_available = %v, want true", first["update_available"])
	}
	if first["current_digest"] != "sha256:aaaaaaaaaaaaaaaa" || first["remote_digest"] != "sha256:bbbbbbbbbbbbbbbb" {
		t.Errorf("app digests = %v / %v, want the recorded ones", first["current_digest"], first["remote_digest"])
	}
	if second["update_available"] != false || second["error"] != "registry: rate limited" {
		t.Errorf("db = %v, want update_available false and the recorded error", second)
	}
	// The empty error field is part of the contract (plain field, no
	// omitempty), so consumers need no null handling.
	if got, ok := first["error"]; !ok || got != "" {
		t.Errorf("error field = %v (present %v), want an empty string", got, ok)
	}
}

func TestStatusOmitsUpdatesWithoutRecords(t *testing.T) {
	entry := state.State{Hash: "abc123", Status: state.StatusDeployed}
	fc := &fakeCompose{infos: []compose.ContainerInfo{{Project: "web", Name: "web-app-1", State: "running"}}}
	h := statusFixture(t, map[string]state.State{"web": entry}, fc, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	resp := decodeStatus(t, rec)
	if updates := updatesOf(t, resp, "web"); updates != nil {
		t.Errorf("updates = %v, want the key omitted without records", updates)
	}
}

func TestStatusKeepsUpdatesOnContainerListingError(t *testing.T) {
	entry := state.State{
		Hash:   "abc123",
		Status: state.StatusDeployed,
		Images: map[string]state.ImageRecord{
			"nginx:latest": {
				Service:         "app",
				UpdateAvailable: true,
				CheckedAt:       time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC),
			},
		},
	}
	fc := &fakeCompose{infosErr: errTest("ps boom")}
	h := statusFixture(t, map[string]state.State{"web": entry}, fc, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when ps fails", rec.Code)
	}
	resp := decodeStatus(t, rec)
	if got := driftOf(t, resp, "web"); got != "unknown" {
		t.Errorf("drift = %q, want unknown", got)
	}
	updates := updatesOf(t, resp, "web")
	if updates == nil {
		t.Fatal("updates block missing although ps failed — the two are independent")
	}
	if got, want := updates["pending"], float64(1); got != want {
		t.Errorf("pending = %v, want %v", got, want)
	}
}

// errTest is a minimal error for fixtures.
type errTest string

func (e errTest) Error() string { return string(e) }
