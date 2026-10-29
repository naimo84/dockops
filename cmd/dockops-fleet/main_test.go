package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// quiet swallows the aggregator's warnings.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// statusDoc is a minimal but complete /status document.
const statusDoc = `{"generated_at":"2026-10-28T05:00:00Z",` +
	`"host_repo":{"url":"git@example.com:host.git","branch":"main","commit":"abc1234"},` +
	`"stacks":[{"name":"web","deployed_hash":"h","drift":"ok","containers":[]}],"unmanaged":[]}`

func TestLoadHostsValid(t *testing.T) {
	hosts, err := loadHosts(
		"hvaler=https://dockops.hvaler.example,netcup2=http://10.0.0.2:8180/",
		"hvaler=tok-a,netcup2=tok=b==", // '=' in a token must survive
	)
	if err != nil {
		t.Fatalf("loadHosts() error = %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %v, want 2 entries", hosts)
	}
	if hosts[0].Name != "hvaler" || hosts[0].URL != "https://dockops.hvaler.example" {
		t.Errorf("hosts[0] = %+v, want the hvaler entry unchanged", hosts[0])
	}
	if hosts[1].Token != "tok=b==" {
		t.Errorf("hosts[1].Token = %q, want the token including its '=' padding", hosts[1].Token)
	}
}

func TestLoadHostsRejects(t *testing.T) {
	for name, tc := range map[string]struct{ hosts, tokens, want string }{
		"empty host list":   {"", "hvaler=tok", "is empty"},
		"no separator":      {"hvaler", "hvaler=tok", "is not name=url"},
		"no url":            {"hvaler=", "hvaler=tok", "is not name=url"},
		"no scheme":         {"hvaler=dockops.hvaler.example", "hvaler=tok", "no http(s) scheme"},
		"duplicate host":    {"hvaler=https://a,hvaler=https://b", "hvaler=tok", "duplicate host"},
		"missing token":     {"hvaler=https://a,omv=https://b", "hvaler=tok", `no token for host "omv"`},
		"malformed tokens":  {"hvaler=https://a", "hvaler", "is not name=token"},
		"empty token value": {"hvaler=https://a", "hvaler=", "is not name=token"},
	} {
		if _, err := loadHosts(tc.hosts, tc.tokens); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

// TestFleetParallelOrderAndDegradation is the core contract: config order,
// one broken host never damages the others, a dead host costs its timeout.
func TestFleetParallelOrderAndDegradation(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			t.Errorf("requested path = %q, want /status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, statusDoc)
	}))
	defer ok.Close()

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daemon kaputt", http.StatusInternalServerError)
	}))
	defer broken.Close()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, statusDoc)
	}))
	defer slow.Close()

	hosts := []host{
		{Name: "alpha", URL: ok.URL, Token: "t1"},
		{Name: "beta", URL: broken.URL, Token: "t2"},
		{Name: "gamma", URL: slow.URL, Token: "t3"},
	}
	a := newAggregator(hosts, nil, quiet())
	a.timeout = 100 * time.Millisecond

	started := time.Now()
	doc := a.fleet(t.Context())
	elapsed := time.Since(started)

	if len(doc.Hosts) != 3 {
		t.Fatalf("hosts = %d, want 3", len(doc.Hosts))
	}
	for i, want := range []string{"alpha", "beta", "gamma"} {
		if doc.Hosts[i].Name != want {
			t.Errorf("hosts[%d].Name = %q, want %q (configuration order)", i, doc.Hosts[i].Name, want)
		}
	}
	if !doc.Hosts[0].OK || len(doc.Hosts[0].Status) == 0 {
		t.Errorf("alpha = %+v, want ok with the status document", doc.Hosts[0])
	}
	if doc.Hosts[1].OK || !strings.Contains(doc.Hosts[1].Error, "status 500") ||
		!strings.Contains(doc.Hosts[1].Error, "daemon kaputt") {
		t.Errorf("beta = %+v, want the 500 with its body in the error", doc.Hosts[1])
	}
	if doc.Hosts[2].OK || !strings.Contains(doc.Hosts[2].Error, "timeout") {
		t.Errorf("gamma = %+v, want a timeout error", doc.Hosts[2])
	}
	// Parallel, not serial: one timeout total, not the sum of three queries.
	if elapsed > 300*time.Millisecond {
		t.Errorf("fleet() took %s, want the hosts queried in parallel", elapsed)
	}
	// generated_at is RFC3339 and parseable.
	if _, err := time.Parse(time.RFC3339, doc.GeneratedAt); err != nil {
		t.Errorf("generated_at = %q, want RFC3339: %v", doc.GeneratedAt, err)
	}
	// The raw status survives untouched: the page reads the agent's own words.
	var parsed struct {
		HostRepo struct{ Commit string } `json:"host_repo"`
		Stacks   []struct{ Name string } `json:"stacks"`
	}
	if err := json.Unmarshal(doc.Hosts[0].Status, &parsed); err != nil {
		t.Fatalf("status is not the forwarded document: %v", err)
	}
	if parsed.HostRepo.Commit != "abc1234" || len(parsed.Stacks) != 1 || parsed.Stacks[0].Name != "web" {
		t.Errorf("forwarded status = %+v, want the agent document unchanged", parsed)
	}
}

// TestFleetSendsPerHostToken pins the security property the split token list
// buys: each request carries that host's token, never a shared secret.
func TestFleetSendsPerHostToken(t *testing.T) {
	received := make(chan string, 2)
	fake := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			received <- r.Header.Get("Authorization")
			_, _ = io.WriteString(w, statusDoc)
		}))
	}
	first, second := fake(), fake()
	defer first.Close()
	defer second.Close()

	a := newAggregator([]host{
		{Name: "first", URL: first.URL, Token: "token-first"},
		{Name: "second", URL: second.URL, Token: "token-second"},
	}, nil, quiet())
	doc := a.fleet(t.Context())
	if !doc.Hosts[0].OK || !doc.Hosts[1].OK {
		t.Fatalf("hosts = %+v, want both ok", doc.Hosts)
	}

	got := map[string]bool{}
	for range 2 {
		select {
		case header := <-received:
			got[header] = true
		case <-time.After(time.Second):
			t.Fatal("host server did not receive a request")
		}
	}
	for _, want := range []string{"Bearer token-first", "Bearer token-second"} {
		if !got[want] {
			t.Errorf("Authorization headers = %v, want %q among them", got, want)
		}
	}
}

// TestFleetNonJSONAndEmptyBody keeps the error paths honest: a 200 with
// garbage is not a status document.
func TestFleetNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>login</html>")
	}))
	defer srv.Close()

	a := newAggregator([]host{{Name: "h", URL: srv.URL, Token: "t"}}, nil, quiet())
	doc := a.fleet(t.Context())
	if doc.Hosts[0].OK || !strings.Contains(doc.Hosts[0].Error, "not JSON") {
		t.Errorf("host = %+v, want a not-JSON error", doc.Hosts[0])
	}
}

// TestPageAndAPI covers the two routes without real hosts.
func TestPageAndAPI(t *testing.T) {
	a := newAggregator([]host{{Name: "h", URL: "http://127.0.0.1:1", Token: "t"}}, nil, quiet())
	a.timeout = 20 * time.Millisecond
	srv := httptest.NewServer(newMux(a))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer res.Body.Close()
	page, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(page), "dockops fleet") {
		t.Errorf("GET / = %d, want the embedded page", res.StatusCode)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("GET / Cache-Control = %q, want no-store", got)
	}

	res, err = http.Get(srv.URL + "/api/fleet")
	if err != nil {
		t.Fatalf("GET /api/fleet: %v", err)
	}
	defer res.Body.Close()
	var doc fleetResponse
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatalf("decode /api/fleet: %v", err)
	}
	// Unreachable host: still a complete document with the error on the entry.
	if len(doc.Hosts) != 1 || doc.Hosts[0].OK || doc.Hosts[0].Error == "" {
		t.Errorf("doc.Hosts = %+v, want one entry with an error", doc.Hosts)
	}

	for _, path := range []string{"/nope"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, res.StatusCode)
		}
	}

	post, err := http.Post(srv.URL+"/api/fleet", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/fleet: %v", err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/fleet = %d, want 405", post.StatusCode)
	}
}
