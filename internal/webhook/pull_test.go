package webhook

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

type pullStub struct {
	calls   [][]string
	queued  []string
	skipped map[string]string
	unknown []string
}

func (s *pullStub) pull(stacks []string) ([]string, map[string]string, []string, error) {
	s.calls = append(s.calls, append([]string(nil), stacks...))
	return s.queued, s.skipped, s.unknown, nil
}

func newPullHandler(deps Deps, stub *pullStub, triggered *int) *Handler {
	deps.Pull = stub.pull
	deps.Trigger = func() { *triggered++ }
	deps.Logger = quietLogger()
	return New(deps)
}

func TestPullQueuesAndTriggers(t *testing.T) {
	stub := &pullStub{queued: []string{"dockops"}, skipped: map[string]string{"db": "update_policy: pinned"}}
	n := 0
	h := newPullHandler(Deps{PullToken: "p"}, stub, &n)
	rr := do(h, http.MethodPost, "/pull", []byte(`{"stacks":["dockops","db"]}`), map[string]string{"Authorization": "Bearer p"})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rr.Code, rr.Body.String())
	}
	if n != 1 {
		t.Errorf("trigger fired %d times, want 1", n)
	}
	if !reflect.DeepEqual(stub.calls, [][]string{{"dockops", "db"}}) {
		t.Errorf("pull calls = %v", stub.calls)
	}
	var body pullResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(body.Queued, []string{"dockops"}) || len(body.Skipped) != 1 || body.Skipped[0].Stack != "db" {
		t.Errorf("body = %+v", body)
	}
}

func TestPullAuth(t *testing.T) {
	stub := &pullStub{queued: []string{"web"}}
	n := 0
	cases := []struct {
		name string
		deps Deps
		auth string
		want int
	}{
		{"pull token ok", Deps{PullToken: "p"}, "Bearer p", http.StatusAccepted},
		{"pull token wrong", Deps{PullToken: "p"}, "Bearer x", http.StatusUnauthorized},
		{"restart token fallback", Deps{RestartToken: "r"}, "Bearer r", http.StatusAccepted},
		{"pull token beats restart token", Deps{PullToken: "p", RestartToken: "r"}, "Bearer r", http.StatusUnauthorized},
		{"no credential", Deps{}, "Bearer p", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPullHandler(tc.deps, stub, &n)
			rr := do(h, http.MethodPost, "/pull", nil, map[string]string{"Authorization": tc.auth})
			if rr.Code != tc.want {
				t.Errorf("status = %d, want %d", rr.Code, tc.want)
			}
		})
	}
}

func TestPullUnknownAndNothingQueued(t *testing.T) {
	n := 0
	h := newPullHandler(Deps{PullToken: "p"}, &pullStub{unknown: []string{"nope"}}, &n)
	auth := map[string]string{"Authorization": "Bearer p"}
	if rr := do(h, http.MethodPost, "/pull", []byte(`{"stacks":["nope"]}`), auth); rr.Code != http.StatusBadRequest {
		t.Errorf("unknown: status = %d, want 400", rr.Code)
	}
	h = newPullHandler(Deps{PullToken: "p"}, &pullStub{skipped: map[string]string{"db": "update_policy: pinned"}}, &n)
	if rr := do(h, http.MethodPost, "/pull", nil, auth); rr.Code != http.StatusAccepted {
		t.Errorf("all skipped: status = %d, want 202", rr.Code)
	}
	if n != 0 {
		t.Errorf("trigger fired %d times, want 0 (nothing queued or rejected)", n)
	}
	if rr := do(h, http.MethodGet, "/pull", nil, auth); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", rr.Code)
	}
}
