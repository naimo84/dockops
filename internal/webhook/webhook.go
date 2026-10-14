// Package webhook exposes the HTTP surface of a dockops agent: an
// authenticated POST /webhook that triggers a reconcile pass and a GET
// /healthz liveness probe.
//
// Authentication understands both common webhook flavors — GitHub
// (X-Hub-Signature-256: "sha256=<hex>" HMAC-SHA256 over the raw request
// body) and GitLab (X-Gitlab-Token: plain shared secret). When a secret is
// configured, at least one of the two must match, otherwise the request is
// rejected with 401. Both comparisons run in constant time.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// maxBody bounds the request body: webhook payloads are small JSON
// documents, and rejecting oversized bodies early keeps the authentication
// work (HMAC over the body) cheap.
const maxBody = 1 << 20 // 1 MiB

const (
	// githubSignatureHeader carries the GitHub HMAC as "sha256=<hex>".
	githubSignatureHeader = "X-Hub-Signature-256"
	// githubSignaturePrefix separates the algorithm from the hex digest.
	githubSignaturePrefix = "sha256="
	// gitlabTokenHeader carries the GitLab shared secret in plain text.
	gitlabTokenHeader = "X-Gitlab-Token"
)

// Handler serves POST /webhook and GET /healthz. It is a plain
// http.Handler, so it composes with any router or middleware. trigger is
// called synchronously after successful authentication; it must not block
// (the CLI passes a non-blocking channel send that merges concurrent
// requests into one pending reconcile).
type Handler struct {
	secret  string
	trigger func()
	logger  *slog.Logger
}

// New creates a Handler for the given shared secret and trigger callback.
// An empty secret disables authentication — every POST /webhook then
// triggers — which callers should warn about (the CLI does); a nil logger
// falls back to slog.Default().
func New(secret string, trigger func(), logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{secret: secret, trigger: trigger, logger: logger}
}

// ServeHTTP implements http.Handler. Unknown paths answer 404; /webhook
// and /healthz answer 405 for other methods.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	case "/webhook":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleWebhook(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handleWebhook reads and authenticates the request body, then fires the
// trigger. The HMAC covers the exact raw bytes received, so the body is
// read once up front instead of streamed.
func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}
	if !h.authorized(r, body) {
		// Which headers were present, not their values — secrets must not
		// leak into logs.
		h.logger.Warn("webhook rejected: authentication failed",
			"remote", r.RemoteAddr,
			"github_header", r.Header.Get(githubSignatureHeader) != "",
			"gitlab_header", r.Header.Get(gitlabTokenHeader) != "")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.logger.Info("webhook accepted; triggering reconcile", "remote", r.RemoteAddr)
	if h.trigger != nil {
		h.trigger()
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// authorized reports whether at least one authentication method matches.
// With no configured secret every request is accepted (documented behavior;
// callers warn about it). A malformed header simply does not match.
func (h *Handler) authorized(r *http.Request, body []byte) bool {
	if h.secret == "" {
		return true
	}
	return h.matchGitHub(r, body) || h.matchGitLab(r)
}

// matchGitHub verifies X-Hub-Signature-256: the header value after the
// "sha256=" prefix must be the hex-encoded HMAC-SHA256 of the body under
// the secret. hmac.Equal compares in constant time; wrong-length digests
// fail without leaking which bytes differ.
func (h *Handler) matchGitHub(r *http.Request, body []byte) bool {
	header := r.Header.Get(githubSignatureHeader)
	if !strings.HasPrefix(header, githubSignaturePrefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, githubSignaturePrefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), got)
}

// matchGitLab verifies X-Gitlab-Token against the configured secret with a
// constant-time comparison. An absent header never matches.
func (h *Handler) matchGitLab(r *http.Request) bool {
	token := r.Header.Get(gitlabTokenHeader)
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.secret)) == 1
}
