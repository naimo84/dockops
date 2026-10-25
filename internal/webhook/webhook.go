// Package webhook exposes the HTTP surface of a dockops agent: an
// authenticated POST /webhook that triggers a reconcile pass, a GET
// /healthz liveness probe and a GET /status monitoring endpoint that
// compares the desired state (state.json) against the running containers
// (see status.go) and the token-guarded log API /logs* (see logs.go):
// container inventory, bounded snapshots and live tails.
//
// Webhook authentication understands both common flavors — GitHub
// (X-Hub-Signature-256: "sha256=<hex>" HMAC-SHA256 over the raw request
// body) and GitLab (X-Gitlab-Token: plain shared secret). When a secret is
// configured, at least one of the two must match, otherwise the request is
// rejected with 401. Both comparisons run in constant time. /status is
// optionally guarded by its own Bearer token (Deps.Token).
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/logs"
	"github.com/local/dockops/internal/state"
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

// ComposeLister is the subset of compose.Executor GET /status needs: the
// running containers of one project and the running compose projects of the
// host. It is an injection point so handler tests run without docker.
type ComposeLister interface {
	// PsProject lists all containers of a compose project
	// (compose.Executor.PsProject).
	PsProject(ctx context.Context, project string) ([]compose.Container, error)

	// PsAllProjects lists the running compose projects with their
	// containers (compose.Executor.PsAllProjects).
	PsAllProjects(ctx context.Context) ([]compose.ProjectRef, error)
}

// Compile-time assertion that the real executor satisfies the injection
// interface.
var _ ComposeLister = (*compose.Executor)(nil)

// Deps are the collaborators of the Handler, mirroring reconciler.Deps: the
// first three fields existed as positional parameters of New before /status
// joined the surface; the rest feeds the status endpoint.
type Deps struct {
	// Secret authenticates POST /webhook (GitHub HMAC / GitLab token);
	// empty disables webhook authentication — callers should warn about
	// that (the CLI does).
	Secret string

	// Trigger fires a reconcile pass. It is called synchronously after
	// successful webhook authentication and must not block (the CLI passes
	// a non-blocking channel send that merges concurrent requests into one
	// pending reconcile).
	Trigger func()

	// Logger; nil falls back to slog.Default().
	Logger *slog.Logger

	// State is the deployment state store GET /status reads (nil → /status
	// answers 503, see status.go).
	State *state.Store

	// Compose lists the running containers for GET /status; nil → 503 like
	// State. Injecting a ComposeLister other than *compose.Executor is the
	// tests' lever.
	Compose ComposeLister

	// RepoURL and Branch identify the host repository; /status reports them
	// under host_repo.
	RepoURL string
	Branch  string

	// HeadCommit returns the commit currently checked out in the host
	// repository (host_repo.commit). Nil or an error degrades the commit to
	// "" — missing git metadata must not fail the endpoint.
	HeadCommit func() (string, error)

	// SelfProject is the compose project dockops itself runs under; it is
	// never reported as unmanaged. The CLI defaults it to "dockops" (the
	// project name of this repository's own compose.yaml) and lets
	// DOCKOPS_SELF_PROJECT override it.
	SelfProject string

	// Logs is the docker-side source of the log API (/logs*); nil disables
	// the endpoints with 503. *compose.Executor is the real implementation.
	Logs logs.Source

	// LogsToken guards every /logs* request (Authorization: Bearer). Unlike
	// Token it is not optional in spirit: an empty value disables the log
	// endpoints entirely (503), because log lines routinely carry secrets.
	// The CLI reads it from DOCKOPS_LOGS_TOKEN.
	LogsToken string

	// Token optionally guards GET /status: when set, requests must carry
	// "Authorization: Bearer <token>" (compared in constant time), else
	// /status answers 401. Empty keeps the endpoint open like /healthz.
	Token string
}

// Handler serves POST /webhook, GET /healthz and GET /status. It is a plain
// http.Handler, so it composes with any router or middleware.
type Handler struct {
	secret  string
	trigger func()
	logger  *slog.Logger
	// status feeds BuildStatus for GET /status (Logger pinned to the
	// handler's logger).
	status StatusDeps
	// statusToken optionally guards GET /status (Bearer); see Deps.Token.
	statusToken string
	// logSrc and logsToken feed the /logs* endpoints; see Deps.Logs and
	// Deps.LogsToken.
	logSrc    logs.Source
	logsToken string
	// streams is the semaphore bounding concurrent live tails
	// (logs.MaxConcurrentStreams slots); see takeStreamSlot.
	streams chan struct{}
}

// New creates a Handler from deps, applying the defaults documented on Deps.
func New(deps Deps) *Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		secret:  deps.Secret,
		trigger: deps.Trigger,
		logger:  logger,
		status: StatusDeps{
			State:       deps.State,
			Compose:     deps.Compose,
			RepoURL:     deps.RepoURL,
			Branch:      deps.Branch,
			HeadCommit:  deps.HeadCommit,
			SelfProject: deps.SelfProject,
			Logger:      logger,
		},
		statusToken: deps.Token,
		logSrc:      deps.Logs,
		logsToken:   deps.LogsToken,
		streams:     make(chan struct{}, logs.MaxConcurrentStreams),
	}
}

// ServeHTTP implements http.Handler. Unknown paths answer 404; /webhook,
// /healthz and /status answer 405 for other methods.
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
	case "/status":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleStatus(w, r)
	case "/logs", "/logs/containers", "/logs/stream":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleLogs(w, r)
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
