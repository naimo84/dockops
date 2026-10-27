package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// restartStub is a Deps.Restart test double: it records every selection and
// answers with canned results.
type restartStub struct {
	calls     [][]string
	restarted []string
	failed    map[string]string
	unknown   []string
	err       error
}

func (s *restartStub) restart(_ context.Context, stacks []string) ([]string, map[string]string, []string, error) {
	s.calls = append(s.calls, append([]string(nil), stacks...))
	return s.restarted, s.failed, s.unknown, s.err
}

// restartBody mirrors the 200 document of POST /restart.
type restartBody struct {
	Restarted []string `json:"restarted"`
	Failed    []struct {
		Stack string `json:"stack"`
		Error string `json:"error"`
	} `json:"failed"`
}

// newRestartHandler builds a handler whose restart endpoint uses stub.
func newRestartHandler(secret string, stub *restartStub) *Handler {
	return New(Deps{
		Secret:  secret,
		Trigger: func() {},
		Restart: stub.restart,
		Logger:  quietLogger(),
	})
}

func TestRestartMethodNotAllowed(t *testing.T) {
	stub := &restartStub{}
	h := newRestartHandler("", stub)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := do(h, method, "/restart", nil, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /restart status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Errorf("%s /restart Allow = %q, want POST", method, allow)
		}
	}
	if len(stub.calls) != 0 {
		t.Errorf("restart called %d times on rejected methods, want 0", len(stub.calls))
	}
}

func TestRestartAuthentication(t *testing.T) {
	const secret = "s3cr3t"
	body := []byte(`{"stacks":["web"]}`)

	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{
			name:    "gitlab token accepted",
			headers: map[string]string{gitlabTokenHeader: secret},
			want:    http.StatusOK,
		},
		{
			name:    "gitea signature accepted",
			headers: map[string]string{giteaSignatureHeader: giteaSig(secret, body)},
			want:    http.StatusOK,
		},
		{
			name:    "github signature accepted",
			headers: map[string]string{githubSignatureHeader: githubSig(secret, body)},
			want:    http.StatusOK,
		},
		{
			name:    "wrong gitlab token",
			headers: map[string]string{gitlabTokenHeader: "wrong"},
			want:    http.StatusUnauthorized,
		},
		{
			name:    "wrong gitea signature",
			headers: map[string]string{giteaSignatureHeader: giteaSig("other", body)},
			want:    http.StatusUnauthorized,
		},
		{
			name:    "no credentials",
			headers: nil,
			want:    http.StatusUnauthorized,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &restartStub{restarted: []string{"web"}}
			h := newRestartHandler(secret, stub)
			rec := do(h, http.MethodPost, "/restart", body, tc.headers)
			if rec.Code != tc.want {
				t.Fatalf("POST /restart status = %d, want %d", rec.Code, tc.want)
			}
			wantCalls := 0
			if tc.want == http.StatusOK {
				wantCalls = 1
			}
			if len(stub.calls) != wantCalls {
				t.Errorf("restart calls = %d, want %d", len(stub.calls), wantCalls)
			}
		})
	}
}

func TestRestartSuccess(t *testing.T) {
	stub := &restartStub{restarted: []string{"web", "db"}}
	h := newRestartHandler("", stub)

	rec := do(h, http.MethodPost, "/restart", []byte(`{"stacks":["web","db"]}`), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /restart status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var doc restartBody
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, rec.Body.String())
	}
	if want := []string{"web", "db"}; !reflect.DeepEqual(doc.Restarted, want) {
		t.Errorf("restarted = %v, want %v", doc.Restarted, want)
	}
	if len(doc.Failed) != 0 {
		t.Errorf("failed = %v, want empty", doc.Failed)
	}
	if !strings.Contains(rec.Body.String(), `"failed":[]`) {
		t.Errorf("empty failed list must be [] not null: %q", rec.Body.String())
	}
	if want := [][]string{{"web", "db"}}; !reflect.DeepEqual(stub.calls, want) {
		t.Errorf("restart selections = %v, want %v", stub.calls, want)
	}
}

// TestRestartSelectionDefaultsToAll pins the optional body: no body, an empty
// object and an empty array all select every configured stack (the empty
// selection is the reconciler's "all").
func TestRestartSelectionDefaultsToAll(t *testing.T) {
	bodies := map[string][]byte{
		"no body":      nil,
		"empty object": []byte(`{}`),
		"empty array":  []byte(`{"stacks":[]}`),
		"blank body":   []byte("  \n"),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			stub := &restartStub{restarted: []string{"web"}}
			h := newRestartHandler("", stub)
			rec := do(h, http.MethodPost, "/restart", body, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("POST /restart status = %d, want 200", rec.Code)
			}
			if len(stub.calls) != 1 {
				t.Fatalf("restart calls = %d, want 1", len(stub.calls))
			}
			if len(stub.calls[0]) != 0 {
				t.Errorf("selection = %v, want empty (all stacks)", stub.calls[0])
			}
		})
	}
}

func TestRestartUnknownStack(t *testing.T) {
	stub := &restartStub{unknown: []string{"ghost"}}
	h := newRestartHandler("", stub)

	rec := do(h, http.MethodPost, "/restart", []byte(`{"stacks":["web","ghost"]}`), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /restart status = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "ghost") {
		t.Errorf("400 body must name the unknown stack, got %q", body)
	}
}

func TestRestartPartialFailure(t *testing.T) {
	stub := &restartStub{
		restarted: []string{"web"},
		failed:    map[string]string{"db": "compose restart: exit status 1"},
	}
	h := newRestartHandler("", stub)

	rec := do(h, http.MethodPost, "/restart", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /restart status = %d, want 200 (a failing stack is not a failed request)", rec.Code)
	}
	var doc restartBody
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if want := []string{"web"}; !reflect.DeepEqual(doc.Restarted, want) {
		t.Errorf("restarted = %v, want %v", doc.Restarted, want)
	}
	if len(doc.Failed) != 1 || doc.Failed[0].Stack != "db" || doc.Failed[0].Error != "compose restart: exit status 1" {
		t.Errorf("failed = %+v, want one entry for db", doc.Failed)
	}
}

// TestRestartAllFailed keeps the 200 contract for the worst case: every stack
// failed, so restarted is an empty (non-nil) array and failed lists them all
// sorted by name.
func TestRestartAllFailed(t *testing.T) {
	stub := &restartStub{failed: map[string]string{"web": "boom", "db": "bang"}}
	h := newRestartHandler("", stub)

	rec := do(h, http.MethodPost, "/restart", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /restart status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"restarted":[]`) {
		t.Errorf("restarted must be [] not null: %q", rec.Body.String())
	}
	var doc restartBody
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	var names []string
	for _, f := range doc.Failed {
		names = append(names, f.Stack)
	}
	if want := []string{"db", "web"}; !reflect.DeepEqual(names, want) {
		t.Errorf("failed stacks = %v, want %v (sorted)", names, want)
	}
}

func TestRestartNotConfigured(t *testing.T) {
	h := New(Deps{Secret: "", Trigger: func() {}, Logger: quietLogger()})
	if rec := do(h, http.MethodPost, "/restart", nil, nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /restart without a restart func status = %d, want 503", rec.Code)
	}
}

func TestRestartInvalidBody(t *testing.T) {
	bodies := map[string][]byte{
		"not JSON":   []byte("restart please"),
		"wrong type": []byte(`{"stacks":"web"}`),
		"empty name": []byte(`{"stacks":[""]}`),
		"blank name": []byte(`{"stacks":["  "]}`),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			stub := &restartStub{}
			h := newRestartHandler("", stub)
			rec := do(h, http.MethodPost, "/restart", body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST /restart status = %d, want 400", rec.Code)
			}
			if len(stub.calls) != 0 {
				t.Errorf("restart called with an invalid body (%v)", stub.calls)
			}
		})
	}
}

func TestRestartCallbackError(t *testing.T) {
	stub := &restartStub{err: errors.New("no configuration loaded yet")}
	h := newRestartHandler("", stub)

	rec := do(h, http.MethodPost, "/restart", nil, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /restart status = %d, want 500", rec.Code)
	}
	// Command output may carry secrets: the detail belongs into the log,
	// not into the response.
	if body := rec.Body.String(); strings.Contains(body, "no configuration") {
		t.Errorf("500 body leaks the internal error: %q", body)
	}
}
