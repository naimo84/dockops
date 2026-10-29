// dockops-fleet — read-only fleet overview over the /status endpoint of
// every dockops agent.
//
// One binary, one embedded page, no build step, no cache, no state on disk:
// after the single-call /status the per-host query costs ~150 ms, so a cache
// would only buy a second truth. Configuration comes from the environment
// (vault-env renders it), one status token per host — a leaked token opens
// one host, not the fleet.
//
// The aggregator only ever reads. No restart, no deploy, no write path: the
// UI must never become a second opinion about when something is deployed.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

const (
	// envHosts is the host list: "name=url,name=url,…".
	envHosts = "FLEET_HOSTS"
	// envTokens maps every host to its own status token:
	// "name=token,name=token,…".
	envTokens = "FLEET_STATUS_TOKENS"
	// envListen is the listen address (default :9090).
	envListen = "LISTEN"

	// statusTimeout bounds one host query. A dead host costs this much and
	// never blocks the others.
	statusTimeout = 5 * time.Second
	// maxStatusBytes bounds one /status response; a status document with
	// 100 stacks is well below 1 MiB, so 4 MiB is a sanity limit, not a
	// capacity plan.
	maxStatusBytes = 4 << 20
	// snippetBytes is how much of an unexpected body reaches the error text
	// (the page shows it, so it must stay readable).
	snippetBytes = 200
)

// host is one configured agent.
type host struct {
	Name  string
	URL   string
	Token string
}

// hostStatus is the per-host result: the raw /status document when the query
// succeeded, an error text otherwise. Status stays raw JSON so the page sees
// exactly what the agent reported — the aggregator parses as little as
// possible and therefore cannot invent drift.
type hostStatus struct {
	Name   string          `json:"name"`
	URL    string          `json:"url"`
	OK     bool            `json:"ok"`
	Status json.RawMessage `json:"status,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// fleetResponse is the document of GET /api/fleet, hosts in configuration
// order (the page sorts for display; the API stays predictable).
type fleetResponse struct {
	GeneratedAt string       `json:"generated_at"`
	Hosts       []hostStatus `json:"hosts"`
}

// aggregator queries the fleet. Client and timeout are injectable so tests
// need no real network and no five-second waits.
type aggregator struct {
	hosts   []host
	client  *http.Client
	logger  *slog.Logger
	timeout time.Duration
}

func newAggregator(hosts []host, client *http.Client, logger *slog.Logger) *aggregator {
	if client == nil {
		client = &http.Client{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &aggregator{hosts: hosts, client: client, logger: logger, timeout: statusTimeout}
}

// fleet queries every host in parallel and merges the results. There is no
// shared deadline on purpose: each host gets its own timeout, so the slowest
// possible answer is one timeout, not the sum.
func (a *aggregator) fleet(ctx context.Context) fleetResponse {
	resp := fleetResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Hosts:       make([]hostStatus, len(a.hosts)),
	}
	var wg sync.WaitGroup
	for i, h := range a.hosts {
		wg.Add(1)
		go func(i int, h host) {
			defer wg.Done()
			resp.Hosts[i] = a.fetch(ctx, h)
		}(i, h)
	}
	wg.Wait()
	return resp
}

// fetch reads one agent's /status. Every failure becomes an error text on
// that host entry: a broken host must never fail the fleet document.
func (a *aggregator) fetch(ctx context.Context, h host) hostStatus {
	out := hostStatus{Name: h.Name, URL: h.URL}
	reqCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	url := strings.TrimRight(h.URL, "/") + "/status"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)

	res, err := a.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			out.Error = fmt.Sprintf("timeout after %s", a.timeout)
		} else {
			out.Error = err.Error()
		}
		a.logger.Warn("fleet: host query failed", "host", h.Name, "error", out.Error)
		return out
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxStatusBytes))
	if err != nil {
		out.Error = fmt.Sprintf("read response: %v", err)
		return out
	}
	if res.StatusCode != http.StatusOK {
		out.Error = fmt.Sprintf("status %d: %s", res.StatusCode, snippet(body))
		a.logger.Warn("fleet: host answered non-200", "host", h.Name, "status", res.StatusCode)
		return out
	}
	if !json.Valid(body) {
		out.Error = "response is not JSON: " + snippet(body)
		return out
	}
	out.OK = true
	out.Status = json.RawMessage(body)
	return out
}

// snippet renders at most snippetBytes of a body as one line, for error texts.
func snippet(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > snippetBytes {
		text = text[:snippetBytes] + "…"
	}
	if text == "" {
		return "(empty body)"
	}
	return text
}

// newMux wires the two routes: the API and the embedded page. Anything else
// is a 404 — there is no static directory to serve.
func newMux(a *aggregator) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/fleet", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(a.fleet(r.Context())); err != nil {
			a.logger.Error("fleet: write response", "error", err)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if _, err := w.Write(indexHTML); err != nil {
			a.logger.Error("fleet: write page", "error", err)
		}
	})
	return mux
}

// loadHosts builds the configuration and enforces the one-token-per-host
// invariant: a host without a token stops the process at startup instead of
// querying an open endpoint with an empty bearer and hiding the mistake
// behind a 401 card.
func loadHosts(rawHosts, rawTokens string) ([]host, error) {
	entries := splitList(rawHosts)
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s is empty", envHosts)
	}
	tokens, err := parseTokens(rawTokens)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envTokens, err)
	}
	hosts := make([]host, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name, url, ok := strings.Cut(entry, "=")
		name, url = strings.TrimSpace(name), strings.TrimSpace(url)
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("%s: entry %q is not name=url", envHosts, entry)
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return nil, fmt.Errorf("%s: %s has no http(s) scheme: %q", envHosts, name, url)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("%s: duplicate host %q", envHosts, name)
		}
		seen[name] = struct{}{}
		token, ok := tokens[name]
		if !ok || token == "" {
			return nil, fmt.Errorf("%s: no token for host %q", envTokens, name)
		}
		hosts = append(hosts, host{Name: name, URL: url, Token: token})
	}
	return hosts, nil
}

// splitList splits a comma-separated configuration value, trimming entries
// and dropping empties so a trailing comma is not a configuration error.
func splitList(raw string) []string {
	entries := make([]string, 0, 8)
	for _, entry := range strings.Split(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

// parseTokens turns "name=token,…" into name→token. Tokens may contain '='
// (base64 padding), so only the first separator counts.
func parseTokens(raw string) (map[string]string, error) {
	tokens := make(map[string]string, 8)
	for _, entry := range splitList(raw) {
		name, token, ok := strings.Cut(entry, "=")
		name, token = strings.TrimSpace(name), strings.TrimSpace(token)
		if !ok || name == "" || token == "" {
			return nil, fmt.Errorf("entry %q is not name=token", entry)
		}
		tokens[name] = token
	}
	return tokens, nil
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	hosts, err := loadHosts(os.Getenv(envHosts), os.Getenv(envTokens))
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}
	addr := os.Getenv(envListen)
	if addr == "" {
		addr = ":9090"
	}
	logger.Info("dockops-fleet starting", "addr", addr, "hosts", len(hosts))
	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(newAggregator(hosts, nil, logger)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("listen failed", "addr", addr, "error", err)
		os.Exit(1)
	}
}
