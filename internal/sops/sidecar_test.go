package sops

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// quietLogger returns a logger discarding everything (the handler under test
// logs one line per request).
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- client side (SidecarResolver against a fake sidecar) ----

// newFakeSidecar starts an HTTP server speaking the sidecar contract. The
// handler records every received request path and answers with the fixed
// content; serve replaces the handler for specialized tests.
func newFakeSidecar(t *testing.T, content string) (*httptest.Server, *[]string) {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req sidecarRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("fake sidecar: decode body: %v", err)
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		got = append(got, req.Path)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sidecarResponse{Content: content}); err != nil {
			t.Errorf("fake sidecar: encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestSidecarDecryptFile(t *testing.T) {
	srv, gotPaths := newFakeSidecar(t, "A=1\nB=two\n# comment\nGARBAGE\n")
	res := NewSidecar(srv.URL, nil)

	got, err := res.DecryptFile(context.Background(), "/srv/stacks/web/secrets.env")
	if err != nil {
		t.Fatalf("DecryptFile() error = %v", err)
	}
	want := map[string]string{"A": "1", "B": "two"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DecryptFile() = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(*gotPaths, []string{"/srv/stacks/web/secrets.env"}) {
		t.Errorf("sidecar received paths %v, want the requested path", *gotPaths)
	}
}

func TestSidecarDecryptFileNormalizesBaseURL(t *testing.T) {
	srv, _ := newFakeSidecar(t, "A=1\n")
	res := NewSidecar(srv.URL+"/", nil) // trailing slash must not double up

	got, err := res.DecryptFile(context.Background(), "/x/y.env")
	if err != nil {
		t.Fatalf("DecryptFile() error = %v", err)
	}
	if got["A"] != "1" {
		t.Errorf("A = %q, want 1", got["A"])
	}
}

func TestSidecarDecryptFileHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		if err := json.NewEncoder(w).Encode(sidecarErrorResponse{Error: "sops: failed to decrypt"}); err != nil {
			t.Errorf("encode error body: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	_, err := NewSidecar(srv.URL, nil).DecryptFile(context.Background(), "/x/y.env")
	if err == nil {
		t.Fatal("DecryptFile() error = nil, want sidecar error")
	}
	for _, want := range []string{"422", "sops: failed to decrypt", "/x/y.env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q misses %q", err, want)
		}
	}
}

func TestSidecarDecryptFileErrorBodySnippet(t *testing.T) {
	// A non-JSON error body degrades to a bounded snippet, not a decode error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gateway exploded", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	_, err := NewSidecar(srv.URL, nil).DecryptFile(context.Background(), "/x/y.env")
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "gateway exploded") {
		t.Errorf("DecryptFile() error = %v, want status 502 with body snippet", err)
	}
}

func TestSidecarDecryptFileTimeout(t *testing.T) {
	old := sidecarRequestTimeout
	sidecarRequestTimeout = 100 * time.Millisecond
	defer func() { sidecarRequestTimeout = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // comfortably past the shortened deadline
		w.Write([]byte(`{"content":"A=1"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := NewSidecar(srv.URL, nil).DecryptFile(context.Background(), "/x/y.env")
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("DecryptFile() against slow sidecar error = %v, want deadline exceeded", err)
	}
}

func TestSidecarDecryptFileResponseLimit(t *testing.T) {
	old := sidecarMaxResponse
	sidecarMaxResponse = 64
	defer func() { sidecarMaxResponse = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(make([]byte, 128)) // double the shortened limit
	}))
	t.Cleanup(srv.Close)

	_, err := NewSidecar(srv.URL, nil).DecryptFile(context.Background(), "/x/y.env")
	if err == nil || !strings.Contains(err.Error(), "exceeds the 64 byte limit") {
		t.Errorf("DecryptFile() against oversized response error = %v, want size limit error", err)
	}
}

func TestSidecarDecryptFileAgeKeyfileRejected(t *testing.T) {
	// The contract carries the path only; a configured age key file must
	// fail loudly instead of being silently ignored — and without an HTTP
	// round trip.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("sidecar was contacted although ageKeyfile made the request invalid")
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	_, err := NewSidecar(srv.URL, nil).DecryptFileKeyfile(context.Background(), "/x/y.env", "/keys/age.key")
	if err == nil || !strings.Contains(err.Error(), "age_keyfile") {
		t.Errorf("DecryptFileKeyfile() with ageKeyfile error = %v, want age_keyfile rejection", err)
	}
}

func TestSidecarDecryptFileEmptyKeyfileOK(t *testing.T) {
	srv, _ := newFakeSidecar(t, "A=1\n")
	got, err := NewSidecar(srv.URL, nil).DecryptFileKeyfile(context.Background(), "/x/y.env", "")
	if err != nil {
		t.Fatalf("DecryptFileKeyfile() with empty keyfile error = %v", err)
	}
	if got["A"] != "1" {
		t.Errorf("A = %q, want 1", got["A"])
	}
}

// ---- server side (the real handler, the fake sops binary) ----

// newSidecarTestHandler starts the real sidecar handler with allowedRoot as
// the only allowed root and a quiet logger.
func newSidecarTestHandler(t *testing.T, allowedRoot string) http.Handler {
	t.Helper()
	return NewSidecarHandler([]string{allowedRoot}, quietLogger())
}

// post sends body against handler and returns status code and decoded body.
func post(t *testing.T, handler http.Handler, target, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestSidecarHandlerDecrypt(t *testing.T) {
	_, callsFile := fakeSops(t)
	root := t.TempDir()
	enc := filepath.Join(root, "stacks", "web", "secrets.env")
	writeFile(t, enc, "SECRET=hunter2\n# note\n")

	handler := newSidecarTestHandler(t, root)
	status, body := post(t, handler, "/decrypt", `{"path": "`+enc+`"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /decrypt status = %d, body %s, want 200", status, body)
	}
	var resp sidecarResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	if resp.Content != "SECRET=hunter2\n# note\n" {
		t.Errorf("content = %q, want the raw sops stdout (comments survive the hop)", resp.Content)
	}
	// The sops binary saw the exact requested path with the dotenv flags.
	calls := readCalls(t, callsFile)
	want := "args: -d --input-type dotenv --output-type dotenv " + enc
	if len(calls) != 1 || calls[0] != want {
		t.Errorf("sops calls = %v, want exactly [%s]", calls, want)
	}
}

func TestSidecarHandlerDecryptCleansPath(t *testing.T) {
	_, callsFile := fakeSops(t)
	root := t.TempDir()
	enc := filepath.Join(root, "secrets.env")
	writeFile(t, enc, "A=1\n")

	handler := newSidecarTestHandler(t, root)
	// Redundant but harmless segments stay inside the root and are handed
	// to sops cleaned.
	status, body := post(t, handler, "/decrypt", `{"path": "`+root+`/./sub/../secrets.env"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /decrypt status = %d, body %s, want 200", status, body)
	}
	calls := readCalls(t, callsFile)
	if len(calls) != 1 || !strings.HasSuffix(calls[0], enc) {
		t.Errorf("sops calls = %v, want the cleaned path %s", calls, enc)
	}
}

func TestSidecarHandlerRejectsTraversal(t *testing.T) {
	_, callsFile := fakeSops(t)
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "escape.env"), "SECRET=nope\n")

	handler := newSidecarTestHandler(t, root)
	// filepath.Clean resolves the .. chain to a path outside the root; the
	// prefix check must reject it before sops runs.
	status, body := post(t, handler, "/decrypt",
		`{"path": "`+root+`/sub/../../`+filepath.Base(outside)+`/escape.env"}`)
	if status != http.StatusForbidden {
		t.Fatalf("POST /decrypt status = %d, body %s, want 403", status, body)
	}
	var resp sidecarErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil || resp.Error == "" {
		t.Errorf("error body %q, want JSON with a non-empty error", body)
	}
	if calls := readCalls(t, callsFile); len(calls) != 0 {
		t.Errorf("sops ran for a rejected path: %v", calls)
	}
}

func TestSidecarHandlerRejectsOutsideRoot(t *testing.T) {
	_, callsFile := fakeSops(t)
	root := t.TempDir()
	other := t.TempDir()
	enc := filepath.Join(other, "secrets.env")
	writeFile(t, enc, "A=1\n")

	handler := newSidecarTestHandler(t, root)
	status, _ := post(t, handler, "/decrypt", `{"path": "`+enc+`"}`)
	if status != http.StatusForbidden {
		t.Fatalf("POST /decrypt status = %d, want 403 (path outside the allowed root)", status)
	}
	if calls := readCalls(t, callsFile); len(calls) != 0 {
		t.Errorf("sops ran for a rejected path: %v", calls)
	}
}

func TestSidecarHandlerRejectsMalformedPaths(t *testing.T) {
	_, callsFile := fakeSops(t)
	handler := newSidecarTestHandler(t, t.TempDir())

	cases := []struct {
		name string
		body string
	}{
		{"relative path", `{"path": "secrets.env"}`},
		{"empty path", `{"path": ""}`},
		{"missing path key", `{}`},
		{"malformed JSON", `{"path": `},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := post(t, handler, "/decrypt", tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("POST /decrypt (%s) status = %d, body %s, want 400", tc.name, status, body)
			}
		})
	}
	if calls := readCalls(t, callsFile); len(calls) != 0 {
		t.Errorf("sops ran for rejected requests: %v", calls)
	}
}

func TestSidecarHandlerDecryptFailure(t *testing.T) {
	fakeSops(t)
	root := t.TempDir()

	handler := newSidecarTestHandler(t, root)
	// The fake sops' cat fails on the missing file, like the real sops.
	status, body := post(t, handler, "/decrypt", `{"path": "`+filepath.Join(root, "missing.env")+`"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("POST /decrypt status = %d, body %s, want 500", status, body)
	}
	var resp sidecarErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil || !strings.Contains(resp.Error, "missing.env") {
		t.Errorf("error body %q, want JSON error naming the file", body)
	}
}

func TestSidecarHandlerMethodNotAllowed(t *testing.T) {
	handler := newSidecarTestHandler(t, t.TempDir())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/decrypt", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /decrypt status = %d, want 405", rec.Code)
	}
}

func TestSidecarHandlerHealthz(t *testing.T) {
	handler := newSidecarTestHandler(t, t.TempDir())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", rec.Code)
	}
	var resp sidecarHealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status != "ok" {
		t.Errorf("healthz body = %q, want {\"status\":\"ok\"}", rec.Body.String())
	}
}

func TestValidateSidecarPath(t *testing.T) {
	const root = "/srv/repos"
	cases := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{"file below root", "/srv/repos/web/secrets.env", "/srv/repos/web/secrets.env", false},
		{"redundant segments", "/srv/repos/web/./sub/../secrets.env", "/srv/repos/web/secrets.env", false},
		{"traversal escapes", "/srv/repos/web/../../etc/passwd", "", true},
		{"sibling prefix", "/srv/repos-other/x.env", "", true},
		{"relative", "srv/repos/x.env", "", true},
		{"empty", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateSidecarPath(tc.path, []string{root})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateSidecarPath(%q) = %q, nil; want error", tc.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateSidecarPath(%q) error = %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("validateSidecarPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
