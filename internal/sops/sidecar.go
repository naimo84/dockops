// The sops decrypt sidecar: a small HTTP service that runs `sops -d` on
// files shared through a volume, plus the client dockops talks to it with.
// Fleet reality: 18 stacks decrypt via Vault-Transit, and the sops binary
// with its .sops.yaml and Vault access deliberately does NOT ship in the
// dockops image — the sidecar container holds the credentials instead.
//
// HTTP contract (defined here, served by NewSidecarHandler and
// cmd/sops-sidecar):
//
//	POST /decrypt
//	  request:  {"path": "/abs/path/file.env"}
//	  response: 200 {"content": "<dotenv plaintext>"}
//	          | 4xx/5xx {"error": "..."} (400 malformed body or path,
//	            403 path outside the allowed roots, 500 decrypt failure)
//
//	GET /healthz → 200 {"status": "ok"} (liveness, no side effects)
//
// There is no authentication: the sidecar is reached only from dockops
// inside the shared compose network and publishes no port. The defenses are
// the request limits below and the path allow-list on the server side.
package sops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/dockops/internal/dotenv"
)

// Client-side limits protecting dockops against a misbehaving or malicious
// sidecar. Package variables (not constants) so tests can tighten them;
// production always runs with the defaults below.
var (
	// sidecarRequestTimeout bounds one decrypt request end to end.
	sidecarRequestTimeout = 60 * time.Second
	// sidecarMaxResponse bounds the accepted response body; a legit dotenv
	// plaintext never comes close.
	sidecarMaxResponse = 10 << 20 // 10 MB
)

// sidecarMaxRequest bounds the request body the server accepts (the request
// carries a single path — 64 KiB is generous).
const sidecarMaxRequest = 64 << 10

// SidecarResolver decrypts sops files through the decrypt sidecar service
// (see the package comment for the HTTP contract) instead of running the
// sops binary locally. It is the second resolver of internal/sops: same
// result — a dotenv map — different transport.
type SidecarResolver struct {
	baseURL string // sidecar root, e.g. http://sops-sidecar:8081, no trailing slash
	client  *http.Client
}

// NewSidecar returns a resolver for the sidecar at baseURL. A nil
// httpClient selects a default client; every request additionally carries
// the 60-second deadline and the 10 MB response limit documented above.
func NewSidecar(baseURL string, httpClient *http.Client) *SidecarResolver {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &SidecarResolver{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  httpClient,
	}
}

// DecryptFile decrypts the sops-encrypted dotenv file at path via the
// sidecar and parses the returned plaintext as dotenv. Error responses of
// the sidecar surface with their status code and the sidecar's error
// message; decrypted values never enter error messages.
func (s *SidecarResolver) DecryptFile(ctx context.Context, path string) (map[string]string, error) {
	return s.DecryptFileKeyfile(ctx, path, "")
}

// DecryptFileKeyfile decrypts like DecryptFile and exists so the resolver
// drop-in replaces sops.DecryptFileKeyfile (the reconciler's SopsDecryptFunc).
// A non-empty ageKeyfile is rejected: the HTTP contract carries the path
// only, and silently dropping a configured key file would turn a credential
// problem into a confusing decrypt failure. With the sidecar, age keys live
// in the sidecar container (SOPS_AGE_KEY_FILE there); locally decrypted
// stacks keep using the local mode.
func (s *SidecarResolver) DecryptFileKeyfile(ctx context.Context, path, ageKeyfile string) (map[string]string, error) {
	if ageKeyfile != "" {
		return nil, fmt.Errorf("sops sidecar: decrypt %s: age_keyfile is not supported through the sidecar (mount the key into the sidecar or use local sops mode)", path)
	}

	payload, err := json.Marshal(sidecarRequest{Path: path})
	if err != nil {
		return nil, fmt.Errorf("sops sidecar: encode request for %s: %w", path, err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, sidecarRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, s.baseURL+"/decrypt", strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("sops sidecar: build request for %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sops sidecar: decrypt %s: %w", path, err)
	}
	defer resp.Body.Close()

	// The size limit applies to every response, errors included, so an
	// endless error body cannot exhaust dockops either.
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(sidecarMaxResponse)+1))
	if err != nil {
		return nil, fmt.Errorf("sops sidecar: decrypt %s: read response: %w", path, err)
	}
	if len(body) > sidecarMaxResponse {
		return nil, fmt.Errorf("sops sidecar: decrypt %s: response exceeds the %d byte limit", path, sidecarMaxResponse)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sops sidecar: decrypt %s: unexpected status %d: %s", path, resp.StatusCode, sidecarErrorMessage(body))
	}

	var decoded sidecarResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("sops sidecar: decrypt %s: decode response: %w", path, err)
	}
	return dotenv.Parse([]byte(decoded.Content)), nil
}

// sidecarErrorMessage renders a non-OK response body for an error message:
// the sidecar's {"error": "..."} field when present and non-empty, else a
// bounded snippet of the raw body (distinguishable from a missing one).
func sidecarErrorMessage(body []byte) string {
	var errResp sidecarErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil && strings.TrimSpace(errResp.Error) != "" {
		return errResp.Error
	}
	snippet := strings.TrimSpace(string(body))
	if snippet == "" {
		return "(empty body)"
	}
	if len(snippet) > maxErrorOutput {
		snippet = snippet[:maxErrorOutput] + "..."
	}
	return snippet
}

// JSON bodies of the HTTP contract.
type (
	sidecarRequest struct {
		Path string `json:"path"`
	}
	sidecarResponse struct {
		Content string `json:"content"`
	}
	sidecarErrorResponse struct {
		Error string `json:"error"`
	}
	sidecarHealthResponse struct {
		Status string `json:"status"`
	}
)

// NewSidecarHandler returns the http.Handler serving the decrypt sidecar
// contract (see the package comment): POST /decrypt runs
// `sops -d --input-type dotenv --output-type dotenv <path>` locally — inside
// the sidecar container — and returns its stdout; GET /healthz answers
// liveness probes.
//
// Path validation is the security boundary of the service: the request path
// must be absolute and, after filepath.Clean (which resolves ".."
// traversal), must live below one of allowedRoots — anything else is
// rejected with 403 before the sops binary is ever invoked. Validation is
// purely lexical; symlinks inside an allowed root resolve as the mount
// defines them (the read-only volume is the operator's boundary). A nil
// logger falls back to slog.Default().
func NewSidecarHandler(allowedRoots []string, logger *slog.Logger) http.Handler {
	h := &sidecarHandler{
		roots:  cleanRoots(allowedRoots),
		logger: logger,
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	mux := http.NewServeMux()
	// Method-specific patterns: a GET /decrypt answers 405 by itself.
	mux.HandleFunc("POST /decrypt", h.handleDecrypt)
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	return mux
}

// sidecarHandler serves the decrypt endpoint for one set of allowed roots.
type sidecarHandler struct {
	roots  []string
	logger *slog.Logger
}

// handleDecrypt validates the requested path and runs the sops binary on it.
func (h *sidecarHandler) handleDecrypt(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	status, path := h.decrypt(w, r)
	// The decrypted content is never logged — only method, path, status and
	// duration (the path names an encrypted file, it is not a secret).
	h.logger.Info("decrypt request",
		"method", r.Method, "path", path, "status", status,
		"duration", time.Since(started).String())
}

// decrypt performs the work of one request and returns the status code and
// requested path for logging.
func (h *sidecarHandler) decrypt(w http.ResponseWriter, r *http.Request) (int, string) {
	var req sidecarRequest
	r.Body = http.MaxBytesReader(w, r.Body, sidecarMaxRequest)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return h.fail(w, http.StatusBadRequest, fmt.Sprintf("decode request body: %v", err)), ""
	}
	cleaned, err := validateSidecarPath(req.Path, h.roots)
	if err != nil {
		// Malformed (empty, relative) paths are client errors; absolute
		// paths outside the allowed roots are forbidden — same JSON body
		// shape, different status codes.
		status := http.StatusForbidden
		if req.Path == "" || !filepath.IsAbs(req.Path) {
			status = http.StatusBadRequest
		}
		return h.fail(w, status, err.Error()), req.Path
	}
	content, err := decryptContent(r.Context(), cleaned, nil)
	if err != nil {
		return h.fail(w, http.StatusInternalServerError, err.Error()), cleaned
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(sidecarResponse{Content: string(content)}); err != nil {
		// The status line is already out; nothing left but to log the
		// truncated response.
		h.logger.Error("write decrypt response", "path", cleaned, "error", err)
	}
	return http.StatusOK, cleaned
}

// handleHealthz answers liveness probes without touching the sops binary.
func (h *sidecarHandler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(sidecarHealthResponse{Status: "ok"}); err != nil {
		h.logger.Error("write healthz response", "error", err)
	}
}

// fail writes one JSON error response with the given status and message and
// returns the status for logging.
func (h *sidecarHandler) fail(w http.ResponseWriter, status int, message string) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(sidecarErrorResponse{Error: message}); err != nil {
		h.logger.Error("write error response", "status", status, "error", err)
	}
	return status
}

// cleanRoots normalizes the allowed roots for prefix checks: absolute,
// filepath.Clean'ed, lexically sorted for deterministic behavior.
func cleanRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		out = append(out, filepath.Clean(root))
	}
	return out
}

// validateSidecarPath enforces the service's path allow-list: p must be
// absolute, is cleaned (which collapses ".." segments — a traversal like
// /root/sub/../../etc/passwd cannot survive as a path below root), and must
// then lie below one of the roots. The cleaned path is returned.
func validateSidecarPath(p string, roots []string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be absolute", p)
	}
	cleaned := filepath.Clean(p)
	for _, root := range roots {
		if cleaned == root || strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
			return cleaned, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the allowed roots", cleaned)
}
