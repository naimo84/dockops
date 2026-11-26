package webhook

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
)

// POST /pull runs the image update check now instead of at the next
// global.update_check_interval. The handler queues the selected stacks (see
// reconciler.Reconciler.RequestUpdateCheck) and fires the reconcile trigger;
// the check runs asynchronously in that pass, so the answer is 202 Accepted
// with what was queued, not the outcome. update_policy decides the outcome
// as for the scheduled check: apply stacks update (the self stack through its
// helper), notify stacks report, pinned stacks are skipped. Results show up
// in GET /status (updates) and the notifications.
//
// Authentication: the pull token (Deps.PullToken, Authorization: Bearer);
// while it is unset the restart token, then the webhook authentication stand
// in — a pull is the same class of operator action as a restart. With no
// credential at all the endpoint is disabled (503). The body is optional and
// has the /restart shape: {"stacks": ["name", ...]}, none = every stack.
//
// PullFunc queues stacks for the check: the queued names, a stack→reason map
// of names the check does not cover (disabled, pinned), and unknown names —
// then nothing was queued. The error return means the request cannot be
// served at all (no configuration loaded yet) and answers 500.
type PullFunc func(stacks []string) (queued []string, skipped map[string]string, unknown []string, err error)

// pullDisabledBody is answered by POST /pull without any credential.
const pullDisabledBody = "pull endpoint disabled: set DOCKOPS_PULL_TOKEN"

// pullResponse is the 202 document of POST /pull. Both arrays are always
// present (empty, never null).
type pullResponse struct {
	Queued  []string   `json:"queued"`
	Skipped []pullSkip `json:"skipped"`
}

// pullSkip is one entry of pullResponse.Skipped.
type pullSkip struct {
	Stack  string `json:"stack"`
	Reason string `json:"reason"`
}

// handlePull serves POST /pull: read body, authenticate, validate, queue,
// trigger. 401/400/503/500 as for /restart; success is 202.
func (h *Handler) handlePull(w http.ResponseWriter, r *http.Request) {
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
	token := h.pullToken
	if token == "" {
		token = h.restartToken
	}
	switch {
	case token != "":
		if !tokenMatches(r.Header.Get("Authorization"), token) {
			h.logger.Warn("pull rejected: authentication failed", "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	case h.secret != "":
		if !h.authorized(r, body) {
			h.logAuthenticationFailure(r)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	default:
		http.Error(w, pullDisabledBody, http.StatusServiceUnavailable)
		return
	}
	req, err := parseRestartRequest(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.pull == nil {
		http.Error(w, "pull not configured", http.StatusServiceUnavailable)
		return
	}
	queued, skipped, unknown, err := h.pull(req.Stacks)
	if err != nil {
		h.logger.Error("pull failed", "remote", r.RemoteAddr, "error", err)
		http.Error(w, "pull failed", http.StatusInternalServerError)
		return
	}
	if len(unknown) > 0 {
		http.Error(w, "unknown stack(s): "+strings.Join(unknown, ", "), http.StatusBadRequest)
		return
	}
	if len(queued) > 0 && h.trigger != nil {
		h.logger.Info("pull accepted; triggering reconcile", "remote", r.RemoteAddr, "stacks", queued)
		h.trigger()
	}
	resp := pullResponse{Queued: queued, Skipped: skipEntries(skipped)}
	if resp.Queued == nil {
		resp.Queued = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Error("pull: write response", "remote", r.RemoteAddr, "error", err)
	}
}

// skipEntries renders the skipped map sorted by stack name.
func skipEntries(skipped map[string]string) []pullSkip {
	out := make([]pullSkip, 0, len(skipped))
	for name, reason := range skipped {
		out = append(out, pullSkip{Stack: name, Reason: reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stack < out[j].Stack })
	return out
}
