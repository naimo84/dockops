package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// quietLogger keeps test output clean.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// do dispatches one request against the handler and returns the recorder.
func do(h *Handler, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t := httptest.NewRecorder()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	h.ServeHTTP(t, req)
	return t
}

// githubSig computes the X-Hub-Signature-256 header value for body under
// secret, exactly like GitHub does.
func githubSig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// countingTrigger returns a trigger func that counts its calls.
func countingTrigger(counts *int) func() {
	return func() { *counts++ }
}

func TestHealthz(t *testing.T) {
	h := New(Deps{Secret: "s3cr3t", Trigger: func() {}, Logger: quietLogger()})

	rec := do(h, http.MethodGet, "/healthz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("GET /healthz body = %q, want ok", rec.Body.String())
	}

	rec = do(h, http.MethodPost, "/healthz", []byte("x"), nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz status = %d, want 405", rec.Code)
	}
}

func TestWebhookGitHubSignature(t *testing.T) {
	const secret = "s3cr3t"
	body := []byte(`{"ref":"refs/heads/main","repository":{"name":"host"}}`)
	triggers := 0
	h := New(Deps{Secret: secret, Trigger: countingTrigger(&triggers), Logger: quietLogger()})

	// Valid signature: accepted, trigger fired exactly once.
	rec := do(h, http.MethodPost, "/webhook", body, map[string]string{
		githubSignatureHeader: githubSig(secret, body),
	})
	if rec.Code != http.StatusOK {
		t.Errorf("valid GitHub signature status = %d, want 200", rec.Code)
	}
	if triggers != 1 {
		t.Errorf("trigger calls = %d, want 1", triggers)
	}

	cases := []struct {
		name    string
		sig     string
		payload []byte
	}{
		{"wrong secret signs the body", githubSig("wrong", body), body},
		{"body tampered after signing", githubSig(secret, body), []byte(`{"ref":"refs/heads/evil"}`)},
		{"malformed hex digest", "sha256=zzzznot-hex", body},
		{"missing sha256 prefix", hex.EncodeToString([]byte("deadbeef")), body},
		{"empty header value", "", body},
	}
	for _, tc := range cases {
		before := triggers
		rec := do(h, http.MethodPost, "/webhook", tc.payload, map[string]string{
			githubSignatureHeader: tc.sig,
		})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, rec.Code)
		}
		if triggers != before {
			t.Errorf("%s: trigger fired, want no trigger on rejected requests", tc.name)
		}
	}
}

func TestWebhookGitLabToken(t *testing.T) {
	triggers := 0
	h := New(Deps{Secret: "s3cr3t", Trigger: countingTrigger(&triggers), Logger: quietLogger()})
	body := []byte(`{"object_kind":"push"}`)

	rec := do(h, http.MethodPost, "/webhook", body, map[string]string{
		gitlabTokenHeader: "s3cr3t",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("valid GitLab token status = %d, want 200", rec.Code)
	}
	if triggers != 1 {
		t.Errorf("trigger calls = %d, want 1", triggers)
	}

	rec = do(h, http.MethodPost, "/webhook", body, map[string]string{
		gitlabTokenHeader: "wrong-token",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong GitLab token status = %d, want 401", rec.Code)
	}
	if triggers != 1 {
		t.Errorf("trigger calls = %d, want still 1 (rejected requests do not trigger)", triggers)
	}
}

func TestWebhookAtLeastOneMethodMustMatch(t *testing.T) {
	triggers := 0
	h := New(Deps{Secret: "s3cr3t", Trigger: countingTrigger(&triggers), Logger: quietLogger()})
	body := []byte(`{}`)

	// No authentication headers at all.
	rec := do(h, http.MethodPost, "/webhook", body, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no headers status = %d, want 401", rec.Code)
	}

	// A valid GitHub signature matches even though the GitLab header is
	// absent, and vice versa a valid GitLab token matches without the
	// GitHub header.
	rec = do(h, http.MethodPost, "/webhook", body, map[string]string{
		githubSignatureHeader: githubSig("s3cr3t", body),
	})
	if rec.Code != http.StatusOK {
		t.Errorf("only GitHub header status = %d, want 200", rec.Code)
	}
	rec = do(h, http.MethodPost, "/webhook", body, map[string]string{
		gitlabTokenHeader: "s3cr3t",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("only GitLab header status = %d, want 200", rec.Code)
	}

	// Both present, both wrong: still rejected.
	rec = do(h, http.MethodPost, "/webhook", body, map[string]string{
		githubSignatureHeader: githubSig("nope", body),
		gitlabTokenHeader:     "nope",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("both methods wrong status = %d, want 401", rec.Code)
	}
	if triggers != 2 {
		t.Errorf("trigger calls = %d, want 2 (only the accepted requests)", triggers)
	}
}

func TestWebhookWithoutSecretAcceptsEverything(t *testing.T) {
	triggers := 0
	h := New(Deps{Trigger: countingTrigger(&triggers), Logger: quietLogger()})

	rec := do(h, http.MethodPost, "/webhook", []byte(`{}`), nil)
	if rec.Code != http.StatusOK {
		t.Errorf("no secret configured status = %d, want 200 (authentication disabled)", rec.Code)
	}
	if triggers != 1 {
		t.Errorf("trigger calls = %d, want 1", triggers)
	}
}

func TestWebhookRoutingAndMethods(t *testing.T) {
	triggers := 0
	h := New(Deps{Secret: "s3cr3t", Trigger: countingTrigger(&triggers), Logger: quietLogger()})

	if rec := do(h, http.MethodGet, "/webhook", nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /webhook status = %d, want 405", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/elsewhere", []byte("x"), nil); rec.Code != http.StatusNotFound {
		t.Errorf("POST /elsewhere status = %d, want 404", rec.Code)
	}
	if triggers != 0 {
		t.Errorf("trigger calls = %d, want 0", triggers)
	}
}

func TestWebhookBodyTooLarge(t *testing.T) {
	h := New(Deps{Secret: "s3cr3t", Trigger: func() {}, Logger: quietLogger()})
	big := bytes.Repeat([]byte("x"), maxBody+1)

	rec := do(h, http.MethodPost, "/webhook", big, map[string]string{
		gitlabTokenHeader: "s3cr3t",
	})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d, want 413", rec.Code)
	}
}
