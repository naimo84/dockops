package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/local/dockops/internal/compose"
)

// fakeLogSource serves canned log output and a canned inventory.
type fakeLogSource struct {
	output string
	err    error
	list   []compose.ContainerInfo
	got    compose.LogsOptions
}

func (f *fakeLogSource) Logs(ctx context.Context, opts compose.LogsOptions, w io.Writer) error {
	f.got = opts
	if f.err != nil {
		return f.err
	}
	_, err := io.WriteString(w, f.output)
	return err
}

func (f *fakeLogSource) PsContainers(ctx context.Context) ([]compose.ContainerInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.list, nil
}

const logsTestToken = "s3cr3t-log-token"

func newLogsHandler(src *fakeLogSource, token string) *Handler {
	return New(Deps{
		Logger:    quietLogger(),
		Logs:      src,
		LogsToken: token,
		Trigger:   func() {},
	})
}

func authHeader(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// TestLogsDisabledWithoutToken pins the fail-closed switch: no
// DOCKOPS_LOGS_TOKEN means no log access at all, not open access.
func TestLogsDisabledWithoutToken(t *testing.T) {
	h := newLogsHandler(&fakeLogSource{output: "2026-10-25T08:00:01Z x\n"}, "")
	for _, target := range []string{"/logs", "/logs/containers", "/logs/stream?container=a"} {
		rec := do(h, http.MethodGet, target, nil, authHeader("anything"))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d, want 503", target, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "DOCKOPS_LOGS_TOKEN") {
			t.Errorf("%s body = %q, want the disabled hint", target, rec.Body.String())
		}
	}
}

func TestLogsRejectsMissingOrWrongToken(t *testing.T) {
	h := newLogsHandler(&fakeLogSource{output: "2026-10-25T08:00:01Z x\n"}, logsTestToken)
	cases := map[string]map[string]string{
		"missing": {},
		"wrong":   authHeader("nope"),
		"no bearer": {
			"Authorization": logsTestToken,
		},
		"empty bearer": authHeader(""),
	}
	for name, headers := range cases {
		rec := do(h, http.MethodGet, "/logs?container=a", nil, headers)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s token = %d, want 401", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "x\n") {
			t.Errorf("%s token leaked log output", name)
		}
	}
}

func TestLogsContainersReturnsInventory(t *testing.T) {
	src := &fakeLogSource{list: []compose.ContainerInfo{
		{Name: "n8n-1", Project: "n8n", Service: "n8n", Image: "n8nio/n8n", State: "running", Health: "healthy"},
		{Name: "db-1", Project: "n8n", Service: "db", Image: "postgres:16", State: "exited"},
	}}
	h := newLogsHandler(src, logsTestToken)
	rec := do(h, http.MethodGet, "/logs/containers", nil, authHeader(logsTestToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`"container":"n8n-1"`, `"project":"n8n"`, `"health":"healthy"`, `"state":"exited"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s: %s", want, body)
		}
	}
}

func TestLogsContainerInventoryErrorIsBadGateway(t *testing.T) {
	h := newLogsHandler(&fakeLogSource{err: errors.New("Cannot connect to the Docker daemon")}, logsTestToken)
	rec := do(h, http.MethodGet, "/logs/containers", nil, authHeader(logsTestToken))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestLogSnapshotTextAndHeaders(t *testing.T) {
	src := &fakeLogSource{output: "2026-10-25T08:00:01Z first\n2026-10-25T08:00:02Z second\n"}
	h := newLogsHandler(src, logsTestToken)
	rec := do(h, http.MethodGet, "/logs?container=n8n-1&tail=5", nil, authHeader(logsTestToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if got := rec.Header().Get("X-Dockops-Lines"); got != "2" {
		t.Errorf("X-Dockops-Lines = %q, want 2", got)
	}
	if got := rec.Header().Get("X-Dockops-Truncated"); got != "" {
		t.Errorf("X-Dockops-Truncated = %q, want empty", got)
	}
	if want := "2026-10-25T08:00:01Z first\n2026-10-25T08:00:02Z second\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
	if src.got.Container != "n8n-1" || src.got.Tail != 5 {
		t.Errorf("source saw %+v, want container n8n-1 tail 5", src.got)
	}
}

func TestLogSnapshotJSONContentType(t *testing.T) {
	src := &fakeLogSource{output: "2026-10-25T08:00:01Z hi\n"}
	h := newLogsHandler(src, logsTestToken)
	rec := do(h, http.MethodGet, "/logs?project=n8n&format=json", nil, authHeader(logsTestToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
	if want := "{\"t\":\"2026-10-25T08:00:01Z\",\"c\":\"\",\"m\":\"hi\"}\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
	if src.got.Project != "n8n" {
		t.Errorf("source saw %+v, want project n8n", src.got)
	}
}

func TestLogSnapshotRejectsBadQuery(t *testing.T) {
	h := newLogsHandler(&fakeLogSource{}, logsTestToken)
	cases := []string{
		"/logs",
		"/logs?container=a&project=b",
		"/logs?container=--help",
		"/logs?container=a&tail=999999",
		"/logs?container=a&since=nonsense",
		"/logs?container=a&format=xml",
	}
	for _, target := range cases {
		rec := do(h, http.MethodGet, target, nil, authHeader(logsTestToken))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", target, rec.Code)
		}
	}
}

// TestLogSnapshotErrorMapping pins the 404/422/502 split documented in the
// spec: missing container, unreadable logging driver, everything else.
func TestLogSnapshotErrorMapping(t *testing.T) {
	cases := map[string]int{
		"No such container: nope": http.StatusNotFound,
		"no such project: nope":   http.StatusNotFound,
		"Error response from daemon: configured logging driver does not support reading": http.StatusUnprocessableEntity,
		"Cannot connect to the Docker daemon":                                            http.StatusBadGateway,
	}
	for message, want := range cases {
		h := newLogsHandler(&fakeLogSource{err: errors.New(message)}, logsTestToken)
		rec := do(h, http.MethodGet, "/logs?container=a", nil, authHeader(logsTestToken))
		if rec.Code != want {
			t.Errorf("%q = %d, want %d", message, rec.Code, want)
		}
	}
}

func TestLogStreamFramesEvents(t *testing.T) {
	src := &fakeLogSource{output: "2026-10-25T08:00:01Z live\n"}
	h := newLogsHandler(src, logsTestToken)
	rec := do(h, http.MethodGet, "/logs/stream?container=n8n-1", nil, authHeader(logsTestToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: log\ndata: 2026-10-25T08:00:01Z live\n\n",
		"event: end\ndata: {\"reason\":\"docker_exit\"}\n\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if !src.got.Follow || src.got.Tail != 0 {
		t.Errorf("source saw %+v, want follow with tail 0", src.got)
	}
}

func TestLogStreamConcurrencyLimit(t *testing.T) {
	h := newLogsHandler(&fakeLogSource{output: "2026-10-25T08:00:01Z live\n"}, logsTestToken)
	h.streams = make(chan struct{}, 1)
	h.streams <- struct{}{} // occupy the only slot
	rec := do(h, http.MethodGet, "/logs/stream?container=a", nil, authHeader(logsTestToken))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
}

func TestLogsRoutingAndMethods(t *testing.T) {
	h := newLogsHandler(&fakeLogSource{}, logsTestToken)
	for _, target := range []string{"/logs", "/logs/containers", "/logs/stream?container=a"} {
		rec := do(h, http.MethodPost, target, []byte("{}"), authHeader(logsTestToken))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", target, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Errorf("POST %s Allow = %q, want GET", target, allow)
		}
	}
	// Routing is exact-match: unknown sub-paths under /logs stay 404 instead
	// of falling into the snapshot handler.
	rec := do(h, http.MethodGet, "/logs/bogus/path", nil, authHeader(logsTestToken))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /logs/bogus/path = %d, want 404", rec.Code)
	}
}

func TestLogsStreamUntilLimitEndsWithReason(t *testing.T) {
	line := "2026-10-25T08:00:01Z " + strings.Repeat("z", 500) + "\n"
	src := &fakeLogSource{output: strings.Repeat(line, 25000)}
	h := newLogsHandler(src, logsTestToken)
	rec := do(h, http.MethodGet, "/logs/stream?container=a", nil, authHeader(logsTestToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `{"reason":"limit"}`) {
		t.Error("stream did not close with end{limit}")
	}
}
