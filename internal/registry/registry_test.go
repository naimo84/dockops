package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// digestOf builds a syntactically valid digest with a repeated hex rune, so
// table cases stay readable.
func digestOf(c byte) string {
	return "sha256:" + strings.Repeat(string(c), 64)
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		ref      string
		registry string
		repo     string
		tag      string
		pinned   bool
		digest   string
	}{
		{ref: "nginx", registry: "docker.io", repo: "library/nginx", tag: "latest"},
		{ref: "nginx:1.25", registry: "docker.io", repo: "library/nginx", tag: "1.25"},
		{ref: "library/nginx:latest", registry: "docker.io", repo: "library/nginx", tag: "latest"},
		{ref: "grafana/grafana:10", registry: "docker.io", repo: "grafana/grafana", tag: "10"},
		{ref: "ghcr.io/x/app", registry: "ghcr.io", repo: "x/app", tag: "latest"},
		{ref: "ghcr.io/x/app:v1.2.3", registry: "ghcr.io", repo: "x/app", tag: "v1.2.3"},
		{ref: "gitea.naimo84.dev:3000/loki/app", registry: "gitea.naimo84.dev:3000", repo: "loki/app", tag: "latest"},
		{ref: "localhost:5000/dev/app:dev", registry: "localhost:5000", repo: "dev/app", tag: "dev"},
		{ref: "index.docker.io/library/nginx:1.25", registry: "index.docker.io", repo: "library/nginx", tag: "1.25"},
		{ref: "quay.io/org/app:edge", registry: "quay.io", repo: "org/app", tag: "edge"},
		{
			ref:      "ghcr.io/x/app@" + digestOf('a'),
			registry: "ghcr.io",
			repo:     "x/app",
			pinned:   true,
			digest:   digestOf('a'),
		},
		{
			// A tag next to a digest pin is legal in docker and irrelevant:
			// the digest decides.
			ref:      "ghcr.io/x/app:latest@" + digestOf('b'),
			registry: "ghcr.io",
			repo:     "x/app",
			pinned:   true,
			digest:   digestOf('b'),
		},
		{
			ref:      "nginx@" + digestOf('c'),
			registry: "docker.io",
			repo:     "library/nginx",
			pinned:   true,
			digest:   digestOf('c'),
		},
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			got, err := ParseRef(tc.ref)
			if err != nil {
				t.Fatalf("ParseRef(%q) error = %v", tc.ref, err)
			}
			if got.Registry != tc.registry {
				t.Errorf("Registry = %q, want %q", got.Registry, tc.registry)
			}
			if got.Repository != tc.repo {
				t.Errorf("Repository = %q, want %q", got.Repository, tc.repo)
			}
			if got.Tag != tc.tag {
				t.Errorf("Tag = %q, want %q", got.Tag, tc.tag)
			}
			if got.DigestPinned != tc.pinned {
				t.Errorf("DigestPinned = %v, want %v", got.DigestPinned, tc.pinned)
			}
			if got.Digest != tc.digest {
				t.Errorf("Digest = %q, want %q", got.Digest, tc.digest)
			}
		})
	}
}

func TestParseRefErrors(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"blank":            "   ",
		"whitespace":       "nginx latest",
		"empty repository": "ghcr.io/",
		"empty tag":        "nginx:",
		"empty segment":    "ghcr.io//app",
		"bad digest":       "nginx@sha256:xyz",
		"wrong algo":       "nginx@md5:" + strings.Repeat("a", 64),
		"digest only":      "@" + digestOf('d'),
	}
	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := ParseRef(ref); err == nil {
				t.Fatalf("ParseRef(%q) = %+v, want error", ref, got)
			}
		})
	}
}

func TestParseRefStringRoundTrip(t *testing.T) {
	ref, err := ParseRef("ghcr.io/x/app:1.0")
	if err != nil {
		t.Fatalf("ParseRef() error = %v", err)
	}
	if want := "ghcr.io/x/app:1.0"; ref.String() != want {
		t.Errorf("String() = %q, want %q", ref.String(), want)
	}
	pinned, err := ParseRef("ghcr.io/x/app@" + digestOf('e'))
	if err != nil {
		t.Fatalf("ParseRef() error = %v", err)
	}
	if want := "ghcr.io/x/app@" + digestOf('e'); pinned.String() != want {
		t.Errorf("String() = %q, want %q", pinned.String(), want)
	}
}

func TestSchemeFor(t *testing.T) {
	cases := map[string]string{
		"localhost:5000":         "http",
		"127.0.0.1:8080":         "http",
		"[::1]:5000":             "http",
		"ghcr.io":                "https",
		"gitea.naimo84.dev:3000": "https",
		"docker.io":              "https",
	}
	for host, want := range cases {
		if got := schemeFor(host); got != want {
			t.Errorf("schemeFor(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	ch, ok := parseChallenge(`Bearer realm="https://auth.example/token",service="registry.example",scope="repository:a/b:pull,push"`)
	if !ok {
		t.Fatal("parseChallenge() ok = false, want true")
	}
	if ch.Realm != "https://auth.example/token" || ch.Service != "registry.example" {
		t.Errorf("challenge = %+v, want realm and service", ch)
	}
	// The comma inside the quoted scope must not split the parameter list.
	if ch.Scope != "repository:a/b:pull,push" {
		t.Errorf("Scope = %q, want the full quoted scope", ch.Scope)
	}
	if _, ok := parseChallenge(`Basic realm="x"`); ok {
		t.Error("parseChallenge() accepted a non-bearer challenge")
	}
	// Unquoted values are tolerated (some registries omit the quotes).
	ch, ok = parseChallenge("bearer realm=https://auth.example/token")
	if !ok || ch.Realm != "https://auth.example/token" {
		t.Errorf("parseChallenge(unquoted) = %+v, %v", ch, ok)
	}
}

// registryFixture starts a fake registry on loopback and returns its host
// plus the client pointing at it.
type registryFixture struct {
	server  *httptest.Server
	host    string
	server2 *httptest.Server
}

func newRegistryFixture(t *testing.T, handler http.HandlerFunc, creds []Credentials) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New(creds, nil)
	return srv, c
}

func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestRemoteDigestBearerChallenge(t *testing.T) {
	const want = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	var srv *httptest.Server
	var tokenRequests, manifestRequests int32
	var gotAccept, gotScope, gotService string
	var basicSeen bool

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			atomic.AddInt32(&tokenRequests, 1)
			if user, pass, ok := r.BasicAuth(); ok {
				basicSeen = true
				if user != "loki" || pass != "s3cr3t" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
			}
			gotService = r.URL.Query().Get("service")
			gotScope = r.URL.Query().Get("scope")
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "good-token", "expires_in": 300})
		case r.URL.Path == "/v2/loki/app/manifests/1.2.3":
			atomic.AddInt32(&manifestRequests, 1)
			gotAccept = r.Header.Get("Accept")
			if r.Header.Get("Authorization") != "Bearer good-token" {
				w.Header().Set("WWW-Authenticate",
					fmt.Sprintf(`Bearer realm="%s/token",service="registry.test",scope="repository:loki/app:pull"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", want)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	c := New([]Credentials{{Host: host, Username: "loki", Password: "s3cr3t"}}, nil)

	got, err := c.RemoteDigest(context.Background(), host+"/loki/app:1.2.3")
	if err != nil {
		t.Fatalf("RemoteDigest() error = %v", err)
	}
	if got != want {
		t.Errorf("RemoteDigest() = %q, want %q", got, want)
	}
	if n := atomic.LoadInt32(&manifestRequests); n != 2 {
		t.Errorf("manifest requests = %d, want 2 (401, then authorized retry)", n)
	}
	if n := atomic.LoadInt32(&tokenRequests); n != 1 {
		t.Errorf("token requests = %d, want 1", n)
	}
	if !basicSeen {
		t.Error("token request carried no Basic credentials although they are configured")
	}
	if gotService != "registry.test" {
		t.Errorf("token service = %q, want registry.test", gotService)
	}
	if gotScope != "repository:loki/app:pull" {
		t.Errorf("token scope = %q, want repository:loki/app:pull", gotScope)
	}
	for _, mediaType := range []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	} {
		if !strings.Contains(gotAccept, mediaType) {
			t.Errorf("Accept = %q, want %q", gotAccept, mediaType)
		}
	}

	// The token is cached per repository: a second lookup goes straight to
	// the manifest with the bearer token.
	if _, err := c.RemoteDigest(context.Background(), host+"/loki/app:1.2.3"); err != nil {
		t.Fatalf("second RemoteDigest() error = %v", err)
	}
	if n := atomic.LoadInt32(&tokenRequests); n != 1 {
		t.Errorf("token requests after second lookup = %d, want 1 (cached per repository)", n)
	}
	// Each lookup probes unauthenticated first (the token is only sent when
	// the registry asks for it), so the second lookup adds one 401 plus one
	// authorized HEAD — but no second token request.
	if n := atomic.LoadInt32(&manifestRequests); n != 4 {
		t.Errorf("manifest requests after second lookup = %d, want 4", n)
	}
}

func TestRemoteDigestTokenIsScopedPerRepository(t *testing.T) {
	// Registry tokens are scoped per repository. A cache keyed on the host
	// alone hands repository A's token to repository B, which answers 401 —
	// with more than one image per registry every image after the first
	// looks unauthorized (live symptom on the fleet).
	var srv *httptest.Server
	tokens := map[string]string{}
	var tokenRequests int32
	repos := []string{"loki/app", "loki/other"}

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			atomic.AddInt32(&tokenRequests, 1)
			scope := r.URL.Query().Get("scope")
			token := "token-" + strings.TrimSuffix(strings.TrimPrefix(scope, "repository:"), ":pull")
			tokens[token] = scope
			_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_in": 300})
			return
		}
		for _, repo := range repos {
			if r.URL.Path != "/v2/"+repo+"/manifests/1.0" {
				continue
			}
			scope := tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
			if scope != "repository:"+repo+":pull" {
				w.Header().Set("WWW-Authenticate",
					fmt.Sprintf(`Bearer realm="%s/token",service="registry.test",scope="repository:%s:pull"`, srv.URL, repo))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("2", 64))
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	c := New(nil, nil)
	for _, repo := range repos {
		if _, err := c.RemoteDigest(context.Background(), host+"/"+repo+":1.0"); err != nil {
			t.Fatalf("RemoteDigest(%s) error = %v", repo, err)
		}
	}
	if n := atomic.LoadInt32(&tokenRequests); n != 2 {
		t.Errorf("token requests = %d, want 2 (one per repository)", n)
	}
}

func TestRemoteDigestAnonymousTokenHasNoBasicAuth(t *testing.T) {
	var srv *httptest.Server
	var sawBasic int32
	const want = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if _, _, ok := r.BasicAuth(); ok {
				atomic.AddInt32(&sawBasic, 1)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "anon-token"})
		case "/v2/app/manifests/latest":
			if r.Header.Get("Authorization") != "Bearer anon-token" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", want)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	c := New(nil, nil)
	got, err := c.RemoteDigest(context.Background(), host+"/app:latest")
	if err != nil {
		t.Fatalf("RemoteDigest() error = %v", err)
	}
	if got != want {
		t.Errorf("RemoteDigest() = %q, want %q", got, want)
	}
	if n := atomic.LoadInt32(&sawBasic); n != 0 {
		t.Errorf("token requests with Basic credentials = %d, want 0 without configured credentials", n)
	}
}

func TestRemoteDigestRejectedToken(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "bad-token"})
			return
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token"`, srv.URL))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := New(nil, nil)
	_, err := c.RemoteDigest(context.Background(), strings.TrimPrefix(srv.URL, "http://")+"/app:latest")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("RemoteDigest() error = %v, want ErrUnauthorized", err)
	}
}

func TestRemoteDigestMissingDigestHeader(t *testing.T) {
	srv, c := newRegistryFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // no Docker-Content-Digest
	}, nil)
	_, err := c.RemoteDigest(context.Background(), hostOf(t, srv)+"/app:latest")
	if err == nil || !strings.Contains(err.Error(), "Docker-Content-Digest") {
		t.Fatalf("RemoteDigest() error = %v, want missing-header error", err)
	}
}

func TestRemoteDigestErrorClasses(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, ErrNotFound},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusForbidden, ErrUnauthorized},
		{http.StatusInternalServerError, nil},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status %d", tc.status), func(t *testing.T) {
			srv, c := newRegistryFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}, nil)
			_, err := c.RemoteDigest(context.Background(), hostOf(t, srv)+"/app:latest")
			if err == nil {
				t.Fatalf("RemoteDigest() error = nil, want an error for status %d", tc.status)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("errors.Is(%v, %v) = false, want true", err, tc.want)
			}
			if tc.want == nil && (errors.Is(err, ErrNotFound) || errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUnauthorized)) {
				t.Errorf("error %v was classified although status %d has no class", err, tc.status)
			}
		})
	}
}

func TestRemoteDigestTimeout(t *testing.T) {
	release := make(chan struct{})
	srv, c := newRegistryFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Docker-Content-Digest", digestOf('f'))
	}, nil)
	t.Cleanup(func() { close(release) })
	c.timeout = 50 * time.Millisecond

	start := time.Now()
	_, err := c.RemoteDigest(context.Background(), hostOf(t, srv)+"/app:latest")
	if err == nil {
		t.Fatal("RemoteDigest() error = nil, want a timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RemoteDigest() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("RemoteDigest() took %v, want the 50ms request timeout", elapsed)
	}
}

func TestRemoteDigestPinnedSkipsHTTP(t *testing.T) {
	var requests int32
	srv, c := newRegistryFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		http.NotFound(w, nil)
	}, nil)

	want := digestOf('a')
	got, err := c.RemoteDigest(context.Background(), hostOf(t, srv)+"/app@"+want)
	if err != nil {
		t.Fatalf("RemoteDigest() error = %v", err)
	}
	if got != want {
		t.Errorf("RemoteDigest() = %q, want the pinned digest", got)
	}
	if n := atomic.LoadInt32(&requests); n != 0 {
		t.Errorf("HTTP requests = %d, want 0 for a digest-pinned reference", n)
	}
}

func TestDockerHubAliasesResolveToAPIHost(t *testing.T) {
	// Every Docker Hub spelling must end up on registry-1.docker.io: the
	// other two hosts answer 401 without a bearer challenge, so the token
	// walk can never start there (live symptom: "registry lookup failed
	// ... unauthorized" for henrygd/beszel-agent).
	for _, ref := range []string{"nginx:1.25", "docker.io/library/nginx:1.25", "index.docker.io/library/nginx:1.25"} {
		parsed, err := ParseRef(ref)
		if err != nil {
			t.Fatalf("ParseRef(%q) error = %v", ref, err)
		}
		c := New(nil, nil)
		if want := "https://registry-1.docker.io/v2/library/nginx/manifests/1.25"; c.manifestURL(parsed.Registry, parsed.Repository, parsed.Tag) != want {
			t.Errorf("manifestURL(%q) = %q, want %q", ref, c.manifestURL(parsed.Registry, parsed.Repository, parsed.Tag), want)
		}
	}
}

func TestDockerHubCredentialsMatchEveryAlias(t *testing.T) {
	// A registries: entry may name any spelling; the lookup must still
	// find it, whichever spelling the image reference uses.
	for _, configured := range []string{"docker.io", "index.docker.io", "registry-1.docker.io"} {
		for _, referenced := range []string{"docker.io", "index.docker.io", "registry-1.docker.io"} {
			c := New([]Credentials{{Host: configured, Username: "u", Password: "p"}}, nil)
			if _, ok := c.credentials(referenced); !ok {
				t.Errorf("credentials(configured %q, referenced %q) not found", configured, referenced)
			}
		}
	}
}

func TestRemoteDigestDockerHubCallsAPIHost(t *testing.T) {
	// The Docker Hub alias must be rewritten to the API host before a
	// request leaves the process; asserted via the URL builder (no network,
	// docker.io is never contacted in tests).
	c := New(nil, nil)
	got := c.manifestURL(DefaultRegistry, "library/nginx", "latest")
	if want := "https://registry-1.docker.io/v2/library/nginx/manifests/latest"; got != want {
		t.Errorf("manifestURL() = %q, want %q", got, want)
	}
	if got := c.manifestURL("localhost:5000", "a/b", "edge"); got != "http://localhost:5000/v2/a/b/manifests/edge" {
		t.Errorf("manifestURL(localhost) = %q, want plain http", got)
	}
}
