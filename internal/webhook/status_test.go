package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/state"
)

// fakeCompose implements ComposeLister with one canned host inventory: the
// whole document comes from this single call, so there is no per-stack
// fixture anymore — only an optional host-wide error.
type fakeCompose struct {
	infos    []compose.ContainerInfo
	infosErr error
}

func (f *fakeCompose) PsContainers(_ context.Context) ([]compose.ContainerInfo, error) {
	return f.infos, f.infosErr
}

// statusFixture is a fully wired handler fixture: a state store with the given
// entries and a fake compose runner.
func statusFixture(t *testing.T, entries map[string]state.State, fc *fakeCompose, extra func(*Deps)) *Handler {
	t.Helper()
	store, err := state.New(filepath.Join(t.TempDir(), "state.json"), quietLogger())
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	for name, st := range entries {
		if err := store.Set(name, st); err != nil {
			t.Fatalf("state.Set(%s) error = %v", name, err)
		}
	}
	if err := store.Save(); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}
	deps := Deps{
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
	}
	if extra != nil {
		extra(&deps)
	}
	return New(deps)
}

// decodeStatus decodes a /status response body into a generic map, so the
// assertions check the shipped wire format (field names), not the structs.
func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /status body %q: %v", rec.Body.String(), err)
	}
	return resp
}

// statusStack returns the stacks[i] object of a decoded response.
func statusStack(t *testing.T, resp map[string]any, i int) map[string]any {
	t.Helper()
	stacks, ok := resp["stacks"].([]any)
	if !ok {
		t.Fatalf("response stacks = %T, want JSON array", resp["stacks"])
	}
	if i >= len(stacks) {
		t.Fatalf("stacks index %d out of range (%d entries)", i, len(stacks))
	}
	m, ok := stacks[i].(map[string]any)
	if !ok {
		t.Fatalf("stacks[%d] = %T, want JSON object", i, stacks[i])
	}
	return m
}

// driftOf returns the drift of the stacks entry with the given name.
func driftOf(t *testing.T, resp map[string]any, name string) string {
	t.Helper()
	stacks, _ := resp["stacks"].([]any)
	for _, s := range stacks {
		m, _ := s.(map[string]any)
		if m["name"] == name {
			drift, _ := m["drift"].(string)
			return drift
		}
	}
	t.Fatalf("no stacks entry named %q", name)
	return ""
}

func TestStatusDriftMatrixAndUnmanaged(t *testing.T) {
	deployed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fc := &fakeCompose{
		infos: []compose.ContainerInfo{
			// alpha: two running → ok (output arrives unsorted, response sorts).
			{Project: "alpha", Name: "alpha-b", Service: "web", State: "running", Health: "healthy"},
			{Project: "alpha", Name: "alpha-a", Service: "db", State: "running"},
			// beta: everything exited → stopped.
			{Project: "beta", Name: "beta-1", Service: "app", State: "exited"},
			// gamma: one running, one exited → partial.
			{Project: "gamma", Name: "gamma-1", Service: "app", State: "running", Health: "healthy"},
			{Project: "gamma", Name: "gamma-2", Service: "worker", State: "exited"},
			// delta: no containers at all → stopped as well (0 running).
			// Legacy projects: two unmanaged with running containers, one
			// unmanaged that is fully stopped, one plain docker run.
			{Project: "legacy", Name: "legacy-1", State: "running"},
			{Project: "other", Name: "other-1", State: "running"},
			{Project: "other", Name: "other-2", State: "running"},
			{Project: "ghost", Name: "ghost-1", State: "exited"},
			// The agent's own project, never reported as unmanaged.
			{Project: "dockops", Name: "dockops-1", State: "running"},
			// No compose project: belongs to no stack and no project listing.
			{Name: "plain-container", State: "running"},
		},
	}
	h := statusFixture(t, map[string]state.State{
		"alpha": {Hash: "h-alpha", Status: state.StatusDeployed, UpdatedAt: deployed},
		"beta":  {Hash: "h-beta", Status: state.StatusDeployed, UpdatedAt: deployed},
		"gamma": {Hash: "h-gamma", Status: state.StatusDeployed, UpdatedAt: deployed},
		"delta": {Hash: "h-delta", Status: state.StatusDeployed, UpdatedAt: deployed},
	}, fc, nil)

	rec := do(h, http.MethodGet, "/status", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	resp := decodeStatus(t, rec)

	// generated_at: present and RFC3339.
	generated, _ := resp["generated_at"].(string)
	if _, err := time.Parse(time.RFC3339, generated); err != nil {
		t.Errorf("generated_at = %q, want RFC3339 (parse error: %v)", generated, err)
	}

	// host_repo: url, branch and the commit from the injected callback.
	hostRepo, _ := resp["host_repo"].(map[string]any)
	if hostRepo["url"] != "git@example.com:homelab/host.git" {
		t.Errorf("host_repo.url = %v, want repo url", hostRepo["url"])
	}
	if hostRepo["branch"] != "main" {
		t.Errorf("host_repo.branch = %v, want main", hostRepo["branch"])
	}
	if hostRepo["commit"] != "abc123def456" {
		t.Errorf("host_repo.commit = %v, want HEAD sha from callback", hostRepo["commit"])
	}

	// Drift matrix, stacks sorted by name (alpha..gamma).
	for name, want := range map[string]string{
		"alpha": "ok",
		"beta":  "stopped",
		"gamma": "partial",
		"delta": "stopped",
	} {
		if got := driftOf(t, resp, name); got != want {
			t.Errorf("stack %s drift = %q, want %q", name, got, want)
		}
	}
	// A healthy host reports no top-level error.
	if msg, _ := resp["error"].(string); msg != "" {
		t.Errorf("top-level error = %q, want empty", msg)
	}

	// The ok stack: hash, last_deployed and the sorted container list.
	alpha := statusStack(t, resp, 0)
	if alpha["name"] != "alpha" {
		t.Fatalf("stacks[0].name = %v, want alpha (sorted)", alpha["name"])
	}
	if alpha["deployed_hash"] != "h-alpha" {
		t.Errorf("alpha deployed_hash = %v, want h-alpha", alpha["deployed_hash"])
	}
	if got, want := alpha["last_deployed"], "2026-10-01T12:00:00Z"; got != want {
		t.Errorf("alpha last_deployed = %v, want %q (RFC3339 of the state entry)", got, want)
	}
	containers, _ := alpha["containers"].([]any)
	if len(containers) != 2 {
		t.Fatalf("alpha containers = %v, want 2 entries", alpha["containers"])
	}
	first, _ := containers[0].(map[string]any)
	if first["name"] != "alpha-a" || first["state"] != "running" || first["service"] != "db" {
		t.Errorf("alpha containers[0] = %v, want alpha-a/running/db (sorted by name)", first)
	}
	second, _ := containers[1].(map[string]any)
	if second["name"] != "alpha-b" || second["health"] != "healthy" {
		t.Errorf("alpha containers[1] = %v, want alpha-b/healthy", second)
	}

	// Unmanaged: legacy and other (alpha is managed, dockops is the agent's
	// own project, ghost is fully stopped), sorted by project with their
	// running container names.
	unmanaged, _ := resp["unmanaged"].([]any)
	if len(unmanaged) != 2 {
		t.Fatalf("unmanaged = %v, want 2 entries (legacy, other)", resp["unmanaged"])
	}
	legacy, _ := unmanaged[0].(map[string]any)
	if legacy["project"] != "legacy" {
		t.Errorf("unmanaged[0].project = %v, want legacy", legacy["project"])
	}
	if names, _ := legacy["containers"].([]any); len(names) != 1 || names[0] != "legacy-1" {
		t.Errorf("unmanaged[0].containers = %v, want [legacy-1]", legacy["containers"])
	}
	other, _ := unmanaged[1].(map[string]any)
	if other["project"] != "other" {
		t.Errorf("unmanaged[1].project = %v, want other", other["project"])
	}
	if names, _ := other["containers"].([]any); len(names) != 2 {
		t.Errorf("unmanaged[1].containers = %v, want 2 entries", other["containers"])
	}
}

func TestStatusEmptyStateYieldsEmptyArrays(t *testing.T) {
	h := statusFixture(t, nil, &fakeCompose{}, nil)

	rec := do(h, http.MethodGet, "/status", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status status = %d, want 200", rec.Code)
	}
	resp := decodeStatus(t, rec)
	// JSON arrays, not null: monitoring consumers should not need null checks.
	for _, key := range []string{"stacks", "unmanaged"} {
		arr, ok := resp[key].([]any)
		if !ok || len(arr) != 0 {
			t.Errorf("%s = %v, want empty JSON array", key, resp[key])
		}
	}
}

// TestStatusListingErrorMarksEveryStackUnknown pins the coarser degradation
// the single-call design buys: one failed docker ps is host-wide, so every
// stack says "unknown" with the error and the document still answers 200.
func TestStatusListingErrorMarksEveryStackUnknown(t *testing.T) {
	fc := &fakeCompose{
		infos:    []compose.ContainerInfo{{Project: "alpha", Name: "alpha-1", State: "running"}},
		infosErr: errors.New("docker ps blew up"),
	}
	h := statusFixture(t, map[string]state.State{
		"alpha": {Hash: "h", Status: state.StatusDeployed, UpdatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)},
		"beta":  {Hash: "h", Status: state.StatusDeployed, UpdatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)},
	}, fc, nil)

	rec := do(h, http.MethodGet, "/status", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status status = %d, want 200 even when the container listing fails", rec.Code)
	}
	resp := decodeStatus(t, rec)
	for _, name := range []string{"alpha", "beta"} {
		if got := driftOf(t, resp, name); got != "unknown" {
			t.Errorf("%s drift = %q, want unknown (host-wide failure)", name, got)
		}
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "docker ps blew up") {
		t.Errorf("top-level error = %v, want the listing failure", resp["error"])
	}
	if arr, _ := resp["unmanaged"].([]any); len(arr) != 0 {
		t.Errorf("unmanaged = %v, want empty array on listing failure", resp["unmanaged"])
	}
	stack := statusStack(t, resp, 0)
	if msg, _ := stack["error"].(string); !strings.Contains(msg, "docker ps blew up") {
		t.Errorf("stacks[0].error = %v, want the listing failure", stack["error"])
	}
}

func TestStatusToken(t *testing.T) {
	h := statusFixture(t, nil, &fakeCompose{}, func(d *Deps) { d.Token = "status-t0ken" })

	// Without header: 401.
	if rec := do(h, http.MethodGet, "/status", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no Authorization header status = %d, want 401", rec.Code)
	}
	// Wrong token and non-bearer scheme: 401.
	for name, auth := range map[string]string{
		"wrong token":   "Bearer wrong",
		"not a bearer":  "Basic status-t0ken",
		"empty bearer":  "Bearer ",
		"token as raw":  "status-t0ken",
		"prefix of key": "Bearer status-t0ke",
	} {
		if rec := do(h, http.MethodGet, "/status", nil, map[string]string{"Authorization": auth}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
	}
	// Correct token: 200.
	if rec := do(h, http.MethodGet, "/status", nil, map[string]string{"Authorization": "Bearer status-t0ken"}); rec.Code != http.StatusOK {
		t.Errorf("correct bearer status = %d, want 200", rec.Code)
	}

	// The token guards /status only; /healthz stays open and /webhook keeps
	// its own webhook authentication.
	if rec := do(h, http.MethodGet, "/healthz", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz with status token set status = %d, want 200 (unaffected)", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/webhook", []byte(`{}`), map[string]string{
		gitlabTokenHeader: "s3cr3t",
	}); rec.Code != http.StatusOK {
		t.Errorf("POST /webhook with webhook secret status = %d, want 200 (own auth, no bearer)", rec.Code)
	}
}

func TestStatusWithoutTokenIsOpen(t *testing.T) {
	h := statusFixture(t, nil, &fakeCompose{}, nil) // no Token set

	if rec := do(h, http.MethodGet, "/status", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("GET /status without token configured status = %d, want 200 (open like /healthz)", rec.Code)
	}
}

func TestStatusHeadCommitFailureYieldsEmptyCommit(t *testing.T) {
	h := statusFixture(t, nil, &fakeCompose{}, func(d *Deps) {
		d.HeadCommit = func() (string, error) { return "", errors.New("no git metadata") }
	})

	rec := do(h, http.MethodGet, "/status", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status status = %d, want 200 despite commit failure", rec.Code)
	}
	resp := decodeStatus(t, rec)
	hostRepo, _ := resp["host_repo"].(map[string]any)
	if hostRepo["commit"] != "" {
		t.Errorf("host_repo.commit = %v, want empty string on failure", hostRepo["commit"])
	}
}

func TestStatusRoutingAndMethods(t *testing.T) {
	h := statusFixture(t, nil, &fakeCompose{}, nil)

	rec := do(h, http.MethodPost, "/status", []byte("x"), nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /status status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("POST /status Allow = %q, want GET", allow)
	}
}

func TestStatusNotConfigured(t *testing.T) {
	// No state store and no compose runner injected: the endpoint exists but
	// cannot answer → 503 instead of a panic or a half-empty payload.
	h := New(Deps{Secret: "s3cr3t", Trigger: func() {}, Logger: quietLogger()})

	if rec := do(h, http.MethodGet, "/status", nil, nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /status without status dependencies status = %d, want 503", rec.Code)
	}
}
