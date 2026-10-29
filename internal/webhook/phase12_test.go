package webhook

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/state"
)

// TestStatusReportsSelfUpdatePending: a record with an in-flight self update
// carries pending: true on its service entry, while the aggregate pending
// count keeps meaning "images whose tag moved" (phase 10).
func TestStatusReportsSelfUpdatePending(t *testing.T) {
	checked := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	pendingAt := time.Date(2026, 10, 26, 10, 5, 0, 0, time.UTC)
	entry := state.State{
		Hash:         "abc123",
		Status:       state.StatusDeployed,
		StackDir:     "/stacks/dockops",
		UpdatePolicy: "apply",
		Images: map[string]state.ImageRecord{
			"ghcr.io/x/dockops:latest": {
				Service:            "dockops",
				CurrentDigest:      "sha256:old",
				RemoteDigest:       "sha256:new",
				UpdateAvailable:    true,
				CheckedAt:          checked,
				PreviousDigest:     "sha256:old",
				ApplyPendingDigest: "sha256:new",
				PendingAt:          pendingAt,
			},
			"ghcr.io/x/db:16": {
				Service:       "db",
				CurrentDigest: "sha256:same",
				RemoteDigest:  "sha256:same",
				CheckedAt:     checked,
			},
		},
	}
	fc := &fakeCompose{infos: []compose.ContainerInfo{{Project: "dockops", Name: "dockops-1", State: "running"}}}
	h := statusFixture(t, map[string]state.State{"dockops": entry}, fc, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeStatus(t, rec)
	updates := updatesOf(t, resp, "dockops")
	if updates == nil {
		t.Fatal("updates block missing although records exist")
	}
	if got, want := updates["pending"], float64(1); got != want {
		t.Errorf("updates.pending = %v, want %v (the count of moved tags is unchanged)", got, want)
	}
	services, ok := updates["services"].([]any)
	if !ok || len(services) != 2 {
		t.Fatalf("services = %v, want two entries", updates["services"])
	}
	first, _ := services[0].(map[string]any)
	second, _ := services[1].(map[string]any)
	if first["service"] != "db" || second["service"] != "dockops" {
		t.Fatalf("services order = %v, want db before dockops", services)
	}
	if _, ok := first["pending"]; ok {
		t.Errorf("db entry carries pending = %v, want the key omitted", first["pending"])
	}
	if got := second["pending"]; got != true {
		t.Errorf("dockops pending = %v, want true", got)
	}
	if got := second["apply_error"]; got != nil {
		t.Errorf("apply_error = %v, want omitted", got)
	}
}

// TestStatusOmitsSelfUpdatePendingWhenIdle: without a pending digest the key
// stays out of the document (additive field, phase-10/11 documents unchanged).
func TestStatusOmitsSelfUpdatePendingWhenIdle(t *testing.T) {
	entry := state.State{
		Hash:         "abc123",
		Status:       state.StatusDeployed,
		StackDir:     "/stacks/web",
		UpdatePolicy: "apply",
		Images: map[string]state.ImageRecord{
			"ghcr.io/x/app:latest": {
				Service:            "app",
				CurrentDigest:      "sha256:same",
				RemoteDigest:       "sha256:same",
				CheckedAt:          time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC),
				AppliedAt:          time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC),
				PreviousDigest:     "sha256:old",
				ApplyBlockedDigest: "",
			},
		},
	}
	fc := &fakeCompose{infos: []compose.ContainerInfo{{Project: "web", Name: "web-app-1", State: "running"}}}
	h := statusFixture(t, map[string]state.State{"web": entry}, fc, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	resp := decodeStatus(t, rec)
	services, ok := updatesOf(t, resp, "web")["services"].([]any)
	if !ok || len(services) != 1 {
		t.Fatalf("services = %v, want one entry", services)
	}
	svc, _ := services[0].(map[string]any)
	if got, ok := svc["pending"]; ok {
		t.Errorf("pending = %v, want the key omitted without a pending digest", got)
	}
}
