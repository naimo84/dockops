package webhook

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/state"
)

// TestStatusReportsApplyState: the phase-11 fields of a stack with
// update_policy: apply come from the state alone — policy, rollback target,
// apply/rollback times, the error and the blocked flag.
func TestStatusReportsApplyState(t *testing.T) {
	checked := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	applied := time.Date(2026, 10, 22, 8, 30, 0, 0, time.UTC)
	rolled := time.Date(2026, 10, 22, 8, 45, 0, 0, time.UTC)
	entry := state.State{
		Hash:         "abc123",
		Status:       state.StatusDeployed,
		StackDir:     "/stacks/web",
		UpdatePolicy: "apply",
		Images: map[string]state.ImageRecord{
			"ghcr.io/x/app:latest": {
				Service:            "app",
				CurrentDigest:      "sha256:old",
				RemoteDigest:       "sha256:new",
				UpdateAvailable:    true,
				CheckedAt:          checked,
				PreviousDigest:     "sha256:previous",
				AppliedAt:          applied,
				RolledBackAt:       rolled,
				ApplyBlockedDigest: "sha256:new",
				ApplyError:         "service app: container web-app-1 is unhealthy",
			},
			"ghcr.io/x/db:16": {
				Service:       "db",
				CurrentDigest: "sha256:same",
				RemoteDigest:  "sha256:same",
				CheckedAt:     checked,
				AppliedAt:     applied,
			},
		},
	}
	fc := &fakeCompose{containers: map[string][]compose.Container{
		"web": {{Name: "web-app-1", State: "running"}},
	}}
	h := statusFixture(t, map[string]state.State{"web": entry}, fc, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeStatus(t, rec)
	updates := updatesOf(t, resp, "web")
	if updates == nil {
		t.Fatal("updates block missing although records exist")
	}
	if got := updates["policy"]; got != "apply" {
		t.Errorf("policy = %v, want apply", got)
	}
	services, _ := updates["services"].([]any)
	if len(services) != 2 {
		t.Fatalf("services = %v, want two entries", services)
	}
	app, _ := services[0].(map[string]any)
	if app["service"] != "app" {
		t.Fatalf("first service = %v, want app", app)
	}
	if got := app["previous_digest"]; got != "sha256:previous" {
		t.Errorf("previous_digest = %v, want sha256:previous", got)
	}
	if got := app["applied_at"]; got != "2026-10-22T08:30:00Z" {
		t.Errorf("applied_at = %v, want RFC3339 of the apply time", got)
	}
	if got := app["rolled_back_at"]; got != "2026-10-22T08:45:00Z" {
		t.Errorf("rolled_back_at = %v, want RFC3339 of the rollback", got)
	}
	if got := app["apply_error"]; got != "service app: container web-app-1 is unhealthy" {
		t.Errorf("apply_error = %v, want the recorded failure", got)
	}
	if got := app["blocked"]; got != true {
		t.Errorf("blocked = %v, want true (the failed digest is the remote one)", got)
	}

	db, _ := services[1].(map[string]any)
	if got := db["blocked"]; got != false {
		t.Errorf("blocked = %v for a healthy image, want false", got)
	}
	if _, ok := db["previous_digest"]; ok {
		t.Errorf("db entry = %v, want no previous_digest key", db)
	}
}

// TestStatusPolicyDefaultsToNotify: an entry without a persisted policy (a
// phase-10 state file) reads as notify, the documented default.
func TestStatusPolicyDefaultsToNotify(t *testing.T) {
	entry := state.State{
		Hash:   "abc123",
		Status: state.StatusDeployed,
		Images: map[string]state.ImageRecord{
			"nginx:latest": {Service: "app", CheckedAt: time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)},
		},
	}
	fc := &fakeCompose{containers: map[string][]compose.Container{
		"web": {{Name: "web-app-1", State: "running"}},
	}}
	h := statusFixture(t, map[string]state.State{"web": entry}, fc, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	resp := decodeStatus(t, rec)
	updates := updatesOf(t, resp, "web")
	if updates == nil {
		t.Fatal("updates block missing")
	}
	if got := updates["policy"]; got != "notify" {
		t.Errorf("policy = %v, want notify for an entry without a policy", got)
	}
}
