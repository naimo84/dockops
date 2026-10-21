package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/local/dockops/internal/config"
)

// request is one captured delivery attempt.
type request struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

// recorder is a test destination: an httptest.Server that records every
// request and can answer with a status or hang for a while.
type recorder struct {
	mu       sync.Mutex
	requests []request
	status   int
	delay    time.Duration
	server   *httptest.Server
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{status: http.StatusOK}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, request{
			method: req.Method,
			path:   req.URL.Path,
			query:  req.URL.RawQuery,
			header: req.Header.Clone(),
			body:   body,
		})
		status, delay := r.status, r.delay
		r.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-req.Context().Done():
				return
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *recorder) last(t *testing.T) request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		t.Fatal("no request recorded")
	}
	return r.requests[len(r.requests)-1]
}

// testNotifier builds a notifier for one destination with sane timings.
func testNotifier(t *testing.T, logger *slog.Logger, d config.NotificationDestination) Notifier {
	t.Helper()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return New(config.Notifications{
		Enabled:      true,
		On:           []string{config.NotificationDeployFailed, config.NotificationPruneFailed, config.NotificationRepoFailed},
		RepeatAfter:  config.Duration(time.Hour),
		Timeout:      config.Duration(2 * time.Second),
		Destinations: []config.NotificationDestination{d},
	}, logger, nil)
}

// logBuffer returns a debug-level logger writing into buf.
func logBuffer(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestNotifyWebhook(t *testing.T) {
	rec := newRecorder(t)
	n := testNotifier(t, nil, config.NotificationDestination{
		Type:    "webhook",
		URL:     rec.server.URL + "/hook",
		Headers: map[string]string{"X-Source": "dockops"},
	})
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	n.Notify(context.Background(), Event{
		Kind:  "deploy_failed",
		Stack: "immich",
		Stage: "compose_up",
		Err:   "compose up boom",
		Hash:  "abc123",
		At:    at,
	})

	got := rec.last(t)
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.path != "/hook" {
		t.Errorf("path = %s, want /hook", got.path)
	}
	if got := got.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := got.header.Get("X-Source"); got != "dockops" {
		t.Errorf("X-Source header = %q, want dockops", got)
	}
	var payload struct {
		Kind   string    `json:"kind"`
		Stack  string    `json:"stack"`
		Remote string    `json:"remote"`
		Stage  string    `json:"stage"`
		Error  string    `json:"error"`
		Hash   string    `json:"hash"`
		At     time.Time `json:"at"`
		Host   string    `json:"host"`
	}
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("decode payload %q: %v", got.body, err)
	}
	if payload.Kind != "deploy_failed" || payload.Stack != "immich" || payload.Stage != "compose_up" ||
		payload.Error != "compose up boom" || payload.Hash != "abc123" {
		t.Errorf("payload = %+v, want the event fields", payload)
	}
	if !payload.At.Equal(at) {
		t.Errorf("payload.At = %v, want %v", payload.At, at)
	}
	var raw map[string]any
	if err := json.Unmarshal(got.body, &raw); err != nil {
		t.Fatalf("decode payload into map: %v", err)
	}
	if _, ok := raw["host"]; !ok {
		t.Errorf("payload %v has no host field", raw)
	}
}

func TestNotifyNtfy(t *testing.T) {
	rec := newRecorder(t)
	n := testNotifier(t, nil, config.NotificationDestination{
		Type:     "ntfy",
		URL:      rec.server.URL + "/dockops",
		Priority: "high",
		Tags:     []string{"warning", "docker"},
	})
	n.Notify(context.Background(), Event{Kind: "prune_failed", Stack: "old", Err: "down boom"})

	got := rec.last(t)
	if got.method != http.MethodPost || got.path != "/dockops" {
		t.Errorf("request = %s %s, want POST /dockops", got.method, got.path)
	}
	if want := "[prune_failed] old: down boom"; string(got.body) != want {
		t.Errorf("body = %q, want %q", got.body, want)
	}
	for name, want := range map[string]string{
		"Priority":     "high",
		"Tags":         "warning,docker",
		"Content-Type": "text/plain; charset=utf-8",
	} {
		if got := got.header.Get(name); got != want {
			t.Errorf("%s header = %q, want %q", name, got, want)
		}
	}
	if got := got.header.Get("Title"); !strings.Contains(got, "prune_failed") {
		t.Errorf("Title header = %q, want the event kind", got)
	}
}

func TestNotifyTelegram(t *testing.T) {
	rec := newRecorder(t)
	old := telegramAPIBase
	telegramAPIBase = rec.server.URL
	t.Cleanup(func() { telegramAPIBase = old })

	n := testNotifier(t, nil, config.NotificationDestination{
		Type:   "telegram",
		Token:  "123:abc",
		ChatID: "42",
	})
	n.Notify(context.Background(), Event{Kind: "deploy_failed", Stack: "web", Err: "boom"})

	got := rec.last(t)
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if want := "/bot123:abc/sendMessage"; got.path != want {
		t.Errorf("path = %s, want %s", got.path, want)
	}
	var payload map[string]string
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("decode payload %q: %v", got.body, err)
	}
	if payload["chat_id"] != "42" {
		t.Errorf("chat_id = %q, want 42", payload["chat_id"])
	}
	if want := "[deploy_failed] web: boom"; payload["text"] != want {
		t.Errorf("text = %q, want %q", payload["text"], want)
	}
	// No parse_mode: plain text avoids the Markdown/HTML escaping mess.
	if _, ok := payload["parse_mode"]; ok {
		t.Errorf("payload sets parse_mode: %v", payload)
	}
}

func TestNotifyDiscordAndSlack(t *testing.T) {
	tests := []struct {
		kind string
		body string
	}{
		{kind: "discord", body: "content"},
		{kind: "slack", body: "text"},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			rec := newRecorder(t)
			n := testNotifier(t, nil, config.NotificationDestination{
				Type:       tc.kind,
				WebhookURL: rec.server.URL + "/" + tc.kind,
			})
			n.Notify(context.Background(), Event{Kind: "deploy_failed", Stack: "web", Err: "boom"})

			got := rec.last(t)
			if got.method != http.MethodPost || got.path != "/"+tc.kind {
				t.Errorf("request = %s %s, want POST /%s", got.method, got.path, tc.kind)
			}
			if ct := got.header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var payload map[string]string
			if err := json.Unmarshal(got.body, &payload); err != nil {
				t.Fatalf("decode payload %q: %v", got.body, err)
			}
			if want := "[deploy_failed] web: boom"; payload[tc.body] != want {
				t.Errorf("%s = %q, want %q", tc.body, payload[tc.body], want)
			}
		})
	}
}

func TestNotifyRepoFailedTextHasNoStack(t *testing.T) {
	rec := newRecorder(t)
	n := testNotifier(t, nil, config.NotificationDestination{Type: "ntfy", URL: rec.server.URL})
	n.Notify(context.Background(), Event{Kind: "repo_failed", Remote: "shared", Stage: "repo", Err: "clone boom"})

	if want := "[repo_failed] clone boom"; string(rec.last(t).body) != want {
		t.Errorf("body = %q, want %q", rec.last(t).body, want)
	}
}

func TestNotifyDedupeWindow(t *testing.T) {
	rec := newRecorder(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	n := New(config.Notifications{
		Enabled:     true,
		On:          []string{config.NotificationDeployFailed},
		RepeatAfter: config.Duration(time.Hour),
		Timeout:     config.Duration(time.Second),
		Destinations: []config.NotificationDestination{
			{Type: "webhook", URL: rec.server.URL},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), clock)

	ev := Event{Kind: "deploy_failed", Stack: "web", Err: "boom"}
	n.Notify(context.Background(), ev)
	if got := rec.count(); got != 1 {
		t.Fatalf("requests after first event = %d, want 1", got)
	}
	n.Notify(context.Background(), ev)
	if got := rec.count(); got != 1 {
		t.Fatalf("requests after second event in window = %d, want 1 (deduped)", got)
	}
	// A different stack is a different dedupe key.
	n.Notify(context.Background(), Event{Kind: "deploy_failed", Stack: "db", Err: "boom"})
	if got := rec.count(); got != 2 {
		t.Fatalf("requests after other stack = %d, want 2", got)
	}
	// Just inside the window: still deduped.
	now = now.Add(59 * time.Minute)
	n.Notify(context.Background(), ev)
	if got := rec.count(); got != 2 {
		t.Fatalf("requests at 59m = %d, want 2 (still inside 1h window)", got)
	}
	// After the window the next failure alarms again.
	now = now.Add(2 * time.Minute)
	n.Notify(context.Background(), ev)
	if got := rec.count(); got != 3 {
		t.Fatalf("requests after window = %d, want 3", got)
	}
}

func TestNotifyRecoveryClearsDedupe(t *testing.T) {
	rec := newRecorder(t)
	n := testNotifier(t, nil, config.NotificationDestination{Type: "webhook", URL: rec.server.URL})

	ev := Event{Kind: "deploy_failed", Stack: "web", Err: "boom"}
	n.Notify(context.Background(), ev)
	n.Recovery("deploy_failed", "web")
	n.Notify(context.Background(), ev)
	if got := rec.count(); got != 2 {
		t.Fatalf("requests = %d, want 2 (Recovery clears the dedupe key)", got)
	}
}

func TestNotifyOnFilter(t *testing.T) {
	rec := newRecorder(t)
	n := New(config.Notifications{
		Enabled:     true,
		On:          []string{config.NotificationDeployFailed},
		RepeatAfter: config.Duration(time.Hour),
		Timeout:     config.Duration(time.Second),
		Destinations: []config.NotificationDestination{
			{Type: "webhook", URL: rec.server.URL},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	n.Notify(context.Background(), Event{Kind: "prune_failed", Stack: "old", Err: "boom"})
	if got := rec.count(); got != 0 {
		t.Fatalf("requests for filtered kind = %d, want 0", got)
	}
	n.Notify(context.Background(), Event{Kind: "deploy_failed", Stack: "web", Err: "boom"})
	if got := rec.count(); got != 1 {
		t.Fatalf("requests for selected kind = %d, want 1", got)
	}
}

func TestNotifyDisabledSendsNothing(t *testing.T) {
	rec := newRecorder(t)
	n := New(config.Notifications{
		Enabled:     false,
		On:          []string{config.NotificationDeployFailed},
		RepeatAfter: config.Duration(time.Hour),
		Timeout:     config.Duration(time.Second),
		Destinations: []config.NotificationDestination{
			{Type: "webhook", URL: rec.server.URL},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	n.Notify(context.Background(), Event{Kind: "deploy_failed", Stack: "web", Err: "boom"})
	if got := rec.count(); got != 0 {
		t.Fatalf("requests while disabled = %d, want 0", got)
	}
}

func TestNotifyTimeoutDoesNotBlockNextDestination(t *testing.T) {
	slow := newRecorder(t)
	slow.delay = 500 * time.Millisecond
	fast := newRecorder(t)

	var logs bytes.Buffer
	n := New(config.Notifications{
		Enabled:     true,
		On:          []string{config.NotificationDeployFailed},
		RepeatAfter: config.Duration(time.Hour),
		Timeout:     config.Duration(50 * time.Millisecond),
		Destinations: []config.NotificationDestination{
			{Type: "webhook", URL: slow.server.URL},
			{Type: "webhook", URL: fast.server.URL},
		},
	}, logBuffer(&logs), nil)

	start := time.Now()
	n.Notify(context.Background(), Event{Kind: "deploy_failed", Stack: "web", Err: "boom"})
	elapsed := time.Since(start)

	if got := fast.count(); got != 1 {
		t.Errorf("second destination received %d requests, want 1 (timeout must not stop the chain)", got)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("Notify took %v, want it bounded by the 50ms destination timeout", elapsed)
	}
	if !strings.Contains(logs.String(), "notification delivery failed") {
		t.Errorf("log %q does not warn about the failed destination", logs.String())
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("log %q has no WARN record", logs.String())
	}
}

func TestNotifyTruncatesErrorTo2000Characters(t *testing.T) {
	rec := newRecorder(t)
	n := testNotifier(t, nil, config.NotificationDestination{Type: "webhook", URL: rec.server.URL})

	long := strings.Repeat("ä", 5000)
	n.Notify(context.Background(), Event{Kind: "deploy_failed", Stack: "web", Err: long})

	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.last(t).body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got := utf8.RuneCountInString(payload.Error); got != maxErrorLength {
		t.Errorf("error length = %d characters, want %d", got, maxErrorLength)
	}
	if !strings.HasSuffix(payload.Error, "…") {
		t.Errorf("truncated error does not end with an ellipsis")
	}
	if !utf8.ValidString(payload.Error) {
		t.Errorf("truncated error is not valid UTF-8")
	}
}

func TestTelegramTruncatesTextTo4000Characters(t *testing.T) {
	rec := newRecorder(t)
	old := telegramAPIBase
	telegramAPIBase = rec.server.URL
	t.Cleanup(func() { telegramAPIBase = old })

	ch := newChannel(config.NotificationDestination{Type: "telegram", Token: "t", ChatID: "1"})
	text := strings.Repeat("x", 9000)
	if err := ch.send(context.Background(), &http.Client{}, Event{Kind: "deploy_failed"}, text, ""); err != nil {
		t.Fatalf("send() error = %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.last(t).body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got := utf8.RuneCountInString(payload["text"]); got != maxMessageLength {
		t.Errorf("telegram text length = %d characters, want %d", got, maxMessageLength)
	}
}

func TestTruncateKeepsRunesIntact(t *testing.T) {
	if got := truncate("äöü", 2); got != "ä…" {
		t.Errorf("truncate(\"äöü\", 2) = %q, want %q", got, "ä…")
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate(\"short\", 10) = %q, want unchanged", got)
	}
}

func TestNotifyLogsCarryNoSecrets(t *testing.T) {
	rec := newRecorder(t)
	rec.status = http.StatusInternalServerError

	var logs bytes.Buffer
	logger := logBuffer(&logs)
	n := testNotifier(t, logger, config.NotificationDestination{
		Type: "webhook",
		URL:  rec.server.URL + "/hook?token=querysecret",
	})
	n.Notify(context.Background(), Event{Kind: "deploy_failed", Stack: "web", Err: "boom"})

	if got := rec.count(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	log := logs.String()
	if !strings.Contains(log, "http 500") {
		t.Errorf("log %q does not mention the failing status", log)
	}
	for _, secret := range []string{"querysecret", "/hook?token"} {
		if strings.Contains(log, secret) {
			t.Errorf("log %q leaks %q", log, secret)
		}
	}
}

func TestNotifyTelegramLogHidesTokenOnTimeout(t *testing.T) {
	rec := newRecorder(t)
	rec.delay = 500 * time.Millisecond
	old := telegramAPIBase
	telegramAPIBase = rec.server.URL
	t.Cleanup(func() { telegramAPIBase = old })

	var logs bytes.Buffer
	n := New(config.Notifications{
		Enabled:     true,
		On:          []string{config.NotificationDeployFailed},
		RepeatAfter: config.Duration(time.Hour),
		Timeout:     config.Duration(30 * time.Millisecond),
		Destinations: []config.NotificationDestination{
			{Type: "telegram", Token: "s3cr3t-bot-token", ChatID: "1"},
		},
	}, logBuffer(&logs), nil)
	n.Notify(context.Background(), Event{Kind: "deploy_failed", Stack: "web", Err: "boom"})

	log := logs.String()
	if !strings.Contains(log, "notification delivery failed") {
		t.Fatalf("log %q does not warn about the timeout", log)
	}
	if strings.Contains(log, "s3cr3t-bot-token") {
		t.Errorf("log %q leaks the bot token", log)
	}
}
