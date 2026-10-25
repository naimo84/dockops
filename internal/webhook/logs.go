// Log API: container inventory, log snapshots and live tails. These endpoints
// exist for machines (agent, curl), not for a browser — there is no UI and no
// session: every request must carry the Bearer token from DOCKOPS_LOGS_TOKEN,
// which is mandatory. An unset token disables the endpoints with 503 instead
// of opening them, because log lines routinely contain secrets.
package webhook

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/local/dockops/internal/logs"
)

// logsDisabledBody is answered when the feature is switched off.
const logsDisabledBody = "logs endpoint disabled: DOCKOPS_LOGS_TOKEN unset"

// streamPingInterval is the SSE keepalive cadence: idle log streams otherwise
// look dead to proxies and clients after a minute.
const streamPingInterval = 15 * time.Second

// handleLogs is the single entry point of the /logs* endpoints: it enforces
// the feature switch and the token, then dispatches by path.
func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	if h.logsToken == "" {
		http.Error(w, logsDisabledBody, http.StatusServiceUnavailable)
		return
	}
	if h.logSrc == nil {
		http.Error(w, "logs endpoint unavailable: no log source", http.StatusServiceUnavailable)
		return
	}
	if !tokenMatches(r.Header.Get("Authorization"), h.logsToken) {
		// The header value is never logged; the path is enough to see abuse.
		h.logger.Warn("log request rejected: authentication failed",
			"remote", r.RemoteAddr, "path", r.URL.Path)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/logs/containers":
		h.handleLogContainers(w, r)
	case "/logs/stream":
		h.handleLogStream(w, r)
	default:
		h.handleLogSnapshot(w, r)
	}
}

// tokenMatches compares "Bearer <token>" against want in constant time.
func tokenMatches(header, want string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// handleLogContainers answers the container inventory of this host.
func (h *Handler) handleLogContainers(w http.ResponseWriter, r *http.Request) {
	infos, err := h.logSrc.PsContainers(r.Context())
	if err != nil {
		h.logger.Error("container inventory failed", "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	body, err := json.Marshal(infos)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleLogSnapshot answers one bounded snapshot. The answer is buffered
// before the first byte goes out: the line count and the truncation flag must
// travel as headers, and the buffer is bounded by logs.MaxBytes — the filter
// stops writing at the limit, so the buffer cannot grow past it.
func (h *Handler) handleLogSnapshot(w http.ResponseWriter, r *http.Request) {
	opts, err := logs.ParseQuery(r.URL.Query(), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var buf bytes.Buffer
	lines, truncated, err := logs.Copy(r.Context(), h.logSrc, opts, &buf)
	if err != nil {
		status := logStatus(err)
		h.logger.Warn("log snapshot failed", "status", status, "error", err.Error())
		http.Error(w, err.Error(), status)
		return
	}
	if opts.Format == logs.FormatJSON {
		w.Header().Set("Content-Type", "application/x-ndjson")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("X-Dockops-Lines", strconv.Itoa(lines))
	if truncated {
		w.Header().Set("X-Dockops-Truncated", "true")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// handleLogStream answers a live tail as Server-Sent Events. Writes from the
// log path and the keepalive goroutine are serialized by mu — a
// ResponseWriter is not safe for concurrent use. The request context bounds
// the stream: a client disconnect cancels it, MaxStreamDuration ends it, and
// both kill the docker subprocess (internal/compose sets WaitDelay).
func (h *Handler) handleLogStream(w http.ResponseWriter, r *http.Request) {
	opts, err := logs.ParseStreamQuery(r.URL.Query(), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if !h.takeStreamSlot() {
		http.Error(w, "too many concurrent streams", http.StatusTooManyRequests)
		return
	}
	defer h.releaseStreamSlot()

	ctx, cancel := context.WithTimeout(r.Context(), logs.MaxStreamDuration)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var mu sync.Mutex
	send := func(ev logs.Event) error {
		mu.Lock()
		defer mu.Unlock()
		if _, err := io.WriteString(w, sseEvent(ev.Name, ev.Data)); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(streamPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := send(logs.Event{Name: "ping", Data: []byte(`{}`)}); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	streamErr := logs.Stream(ctx, h.logSrc, opts, send)
	cancel()
	<-pingDone
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) && !errors.Is(streamErr, context.DeadlineExceeded) {
		h.logger.Warn("log stream ended", "error", streamErr.Error())
	}
}

// takeStreamSlot claims one concurrency slot; false means the limit is
// reached. The channel is the semaphore: capacity MaxConcurrentStreams, one
// entry per running stream.
func (h *Handler) takeStreamSlot() bool {
	select {
	case h.streams <- struct{}{}:
		return true
	default:
		return false
	}
}

func (h *Handler) releaseStreamSlot() { <-h.streams }

// sseEvent renders one server-sent event. Log lines never contain newlines
// (the filter splits them), so a single data field per event suffices.
func sseEvent(name string, data []byte) string {
	return "event: " + name + "\ndata: " + string(data) + "\n\n"
}

// logStatus maps a docker-side error to an HTTP status: a missing container
// or project is a client error (404), an unreadable logging driver is a
// documented limitation of the docker CLI (422), everything else is upstream
// (502). A cancelled request is reported as a timeout rather than as a server
// fault.
func logStatus(err error) int {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "no such container"), strings.Contains(message, "no such project"):
		return http.StatusNotFound
	case strings.Contains(message, "logging driver does not support reading"):
		return http.StatusUnprocessableEntity
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}
