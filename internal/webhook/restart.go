package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
)

// POST /restart restarts stacks of the host configuration without touching
// the deployment state: the operator's answer to "the containers are wedged,
// bring them back" that needs no repository change — no pull, no up, no hash
// and no state update, exactly one `docker compose restart` per selected
// stack (see reconciler.Reconciler.Restart and compose.Executor.Restart).
// Authentication is the webhook's (GitHub HMAC, Gitea HMAC or GitLab token,
// see authorized), and the body is optional: without one every configured
// stack is restarted.
//
// RestartFunc restarts stacks of the host configuration and reports the
// outcome: the names restarted (request order for an explicit selection,
// configuration order for "all"), a stack→error map of the stacks whose
// restart failed, and the names the configuration does not know — in that
// case nothing was restarted, because the implementation validates before it
// runs the first command. The error return is reserved for requests that
// cannot be served at all (no configuration loaded yet, cancelled context)
// and answers 500.
//
// Implemented by reconciler.Reconciler.Restart, which resolves each stack's
// directory, compose files and generated .env from the configuration of the
// last reconcile pass — the /restart endpoint therefore knows no deployment
// details of its own.
type RestartFunc func(ctx context.Context, stacks []string) (restarted []string, failed map[string]string, unknown []string, err error)

// restartRequest is the optional POST /restart body. A missing body, an empty
// object and an empty "stacks" array all mean "every configured stack".
type restartRequest struct {
	Stacks []string `json:"stacks"`
}

// restartResponse is the 200 document of POST /restart. Both arrays are
// always present (empty, never null), so clients can iterate without a nil
// check.
type restartResponse struct {
	Restarted []string         `json:"restarted"`
	Failed    []restartFailure `json:"failed"`
}

// restartFailure is one entry of restartResponse.Failed.
type restartFailure struct {
	Stack string `json:"stack"`
	Error string `json:"error"`
}

// handleRestart serves POST /restart: read body, authenticate, validate the
// selection before the first command, then report per-stack results. Only
// authentication (401), an unusable body or selection (400), a missing
// dependency (503) and an unservable request (500) are HTTP errors — a failed
// restart of one stack is a 200 with that stack in "failed", and a request
// whose every stack failed is still a 200 with an empty "restarted".
func (h *Handler) handleRestart(w http.ResponseWriter, r *http.Request) {
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
	// Same authentication as /webhook, over the same raw bytes: a restart
	// reuses the sender's existing HMAC/token setup unchanged.
	if !h.authorized(r, body) {
		h.logAuthenticationFailure(r)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	req, err := parseRestartRequest(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.restart == nil {
		// No reconciler wired (embedders, tests): the endpoint exists but
		// cannot act — 503 like /status without its dependencies.
		http.Error(w, "restart not configured", http.StatusServiceUnavailable)
		return
	}
	restarted, failed, unknown, err := h.restart(r.Context(), req.Stacks)
	if err != nil {
		// The error may carry command output, so only the log sees it.
		h.logger.Error("restart failed", "remote", r.RemoteAddr, "error", err)
		http.Error(w, "restart failed", http.StatusInternalServerError)
		return
	}
	if len(unknown) > 0 {
		// Validation happened before the first command, so this is never a
		// partial success: name every unknown stack and restart nothing.
		http.Error(w, "unknown stack(s): "+strings.Join(unknown, ", "), http.StatusBadRequest)
		return
	}
	resp := restartResponse{Restarted: restarted, Failed: failedEntries(failed)}
	if resp.Restarted == nil {
		resp.Restarted = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// The status line is already on the wire; only log what happened.
		h.logger.Error("restart: write response", "remote", r.RemoteAddr, "error", err)
	}
}

// parseRestartRequest decodes the optional body. An absent or blank body is
// a valid request meaning "all stacks"; anything that is not a JSON object
// with a string array in "stacks" is rejected before any command runs.
func parseRestartRequest(body []byte) (restartRequest, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return restartRequest{}, nil
	}
	var req restartRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return restartRequest{}, errors.New(`invalid request body: expected {"stacks":["name", ...]}`)
	}
	for _, name := range req.Stacks {
		if strings.TrimSpace(name) == "" {
			return restartRequest{}, errors.New("invalid request body: stack names must not be empty")
		}
	}
	return req, nil
}

// failedEntries renders the failed map as a JSON array sorted by stack name,
// so the response is deterministic. An empty map yields an empty (non-nil)
// slice.
func failedEntries(failed map[string]string) []restartFailure {
	out := make([]restartFailure, 0, len(failed))
	for name, msg := range failed {
		out = append(out, restartFailure{Stack: name, Error: msg})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stack < out[j].Stack })
	return out
}
