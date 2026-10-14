package vault

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
)

// quietLogger returns a logger discarding everything.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestServer starts a fake Vault covering login and KV reads. It asserts
// that data reads carry the given bearer token and fails the test otherwise.
// The secret at secret/data/stacks/web mixes strings with non-string values.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/auth/approle/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("approle login: decode body: %v", err)
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if body["role_id"] != "role-1" || body["secret_id"] != "secret-1" {
			http.Error(w, `{"errors":["invalid role or secret id"]}`, http.StatusForbidden)
			return
		}
		w.Write([]byte(`{"auth":{"client_token":"tok-approle"}}`))
	})

	mux.HandleFunc("GET /v1/secret/data/stacks/web", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Vault-Token"); got != "tok-approle" && got != "tok-static" {
			t.Errorf("read stacks/web: X-Vault-Token = %q, want a token from Login", got)
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":{"data":{"password":"topsecret","retries":3,"enabled":true,"note":null}}}`))
	})
	mux.HandleFunc("GET /v1/kv/data/custom", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"data":{"k":"v"}}}`))
	})
	mux.HandleFunc("GET /v1/secret/data/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":["not found"]}`, http.StatusNotFound)
	})
	mux.HandleFunc("GET /v1/secret/data/broken", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal vault error", http.StatusInternalServerError)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// tokenClient returns a client for srv that is logged in with the static
// token auth method.
func tokenClient(t *testing.T, srv *httptest.Server, mutate func(*config.Vault)) *Client {
	t.Helper()
	t.Setenv("VAULT_TOKEN", "tok-static")
	cfg := config.Vault{Address: srv.URL, AuthMethod: "token"}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	return c
}

func TestNewMissingAddress(t *testing.T) {
	t.Setenv("VAULT_ADDR", "")
	if _, err := New(config.Vault{}, quietLogger()); err == nil || !strings.Contains(err.Error(), "no address configured") {
		t.Errorf("New() without address error = %v, want no-address error", err)
	}
}

func TestNewEnvOverrideAddress(t *testing.T) {
	srv := newTestServer(t)
	t.Setenv("VAULT_ADDR", "")
	t.Setenv("VAULT_TOKEN", "tok-static")

	// cfg.Address set: wins over the environment.
	c, err := New(config.Vault{Address: srv.URL + "/", AuthMethod: "token"}, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if strings.HasSuffix(c.address, "/") {
		t.Errorf("address %q keeps a trailing slash", c.address)
	}
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	// cfg.Address empty: VAULT_ADDR is used (with slash normalization).
	t.Setenv("VAULT_ADDR", srv.URL+"/")
	c2, err := New(config.Vault{AuthMethod: "token"}, quietLogger())
	if err != nil {
		t.Fatalf("New() with VAULT_ADDR error = %v", err)
	}
	if want := srv.URL; c2.address != want {
		t.Errorf("address = %q, want %q from VAULT_ADDR", c2.address, want)
	}
}

func TestLoginToken(t *testing.T) {
	srv := newTestServer(t)
	c := tokenClient(t, srv, nil)
	got, err := c.ReadKV(context.Background(), "stacks/web")
	if err != nil {
		t.Fatalf("ReadKV() error = %v", err)
	}
	if got["password"] != "topsecret" {
		t.Errorf("password = %q, want topsecret", got["password"])
	}
}

func TestLoginTokenEmptyIsError(t *testing.T) {
	srv := newTestServer(t)
	t.Setenv("VAULT_TOKEN", "")
	c, err := New(config.Vault{Address: srv.URL, AuthMethod: "token"}, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = c.Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "VAULT_TOKEN") {
		t.Errorf("Login() with empty token error = %v, want VAULT_TOKEN mention", err)
	}
}

func TestLoginAppRole(t *testing.T) {
	srv := newTestServer(t)
	t.Setenv("VAULT_ROLE_ID", "role-1")
	t.Setenv("VAULT_SECRET_ID", "secret-1")
	c, err := New(config.Vault{Address: srv.URL, AuthMethod: "approle"}, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	// The login token must be used for reads (the handler asserts it).
	got, err := c.ReadKV(context.Background(), "stacks/web")
	if err != nil {
		t.Fatalf("ReadKV() error = %v", err)
	}
	if got["password"] != "topsecret" {
		t.Errorf("password = %q, want topsecret", got["password"])
	}
}

func TestLoginAppRoleBadCredentials(t *testing.T) {
	srv := newTestServer(t)
	t.Setenv("VAULT_ROLE_ID", "role-1")
	t.Setenv("VAULT_SECRET_ID", "wrong")
	c, err := New(config.Vault{Address: srv.URL, AuthMethod: "approle"}, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = c.Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "invalid role or secret id") {
		t.Errorf("Login() with bad credentials error = %v, want status 403 with body snippet", err)
	}
}

func TestLoginAppRoleMissingSecretID(t *testing.T) {
	srv := newTestServer(t)
	t.Setenv("VAULT_ROLE_ID", "role-1")
	t.Setenv("VAULT_SECRET_ID", "")
	c, err := New(config.Vault{Address: srv.URL, AuthMethod: "approle"}, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = c.Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "VAULT_SECRET_ID") {
		t.Errorf("Login() without secret id error = %v, want VAULT_SECRET_ID mention", err)
	}
}

func TestLoginUnsupportedMethod(t *testing.T) {
	srv := newTestServer(t)
	c, err := New(config.Vault{Address: srv.URL, AuthMethod: "saml"}, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Login(context.Background()); err == nil || !strings.Contains(err.Error(), "auth_method") {
		t.Errorf("Login() with bogus method error = %v, want auth_method error", err)
	}
}

func TestReadKVFlattensNonStrings(t *testing.T) {
	srv := newTestServer(t)
	c := tokenClient(t, srv, nil)
	got, err := c.ReadKV(context.Background(), "stacks/web")
	if err != nil {
		t.Fatalf("ReadKV() error = %v", err)
	}
	want := map[string]string{
		"password": "topsecret",
		"retries":  "3",
		"enabled":  "true",
		"note":     "<nil>",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("ReadKV()[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestReadKVNotFound(t *testing.T) {
	srv := newTestServer(t)
	c := tokenClient(t, srv, nil)
	_, err := c.ReadKV(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("ReadKV(missing) error = %v, want not-found error", err)
	}
}

func TestReadKVOtherStatus(t *testing.T) {
	srv := newTestServer(t)
	c := tokenClient(t, srv, nil)
	_, err := c.ReadKV(context.Background(), "broken")
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "internal vault error") {
		t.Errorf("ReadKV(broken) error = %v, want status 500 with body snippet", err)
	}
}

func TestReadKVCustomMount(t *testing.T) {
	srv := newTestServer(t)
	c := tokenClient(t, srv, func(cfg *config.Vault) { cfg.MountPath = "kv" })
	got, err := c.ReadKV(context.Background(), "custom")
	if err != nil {
		t.Fatalf("ReadKV() error = %v", err)
	}
	if got["k"] != "v" {
		t.Errorf("k = %q, want v", got["k"])
	}
}

func TestReadKVNotLoggedIn(t *testing.T) {
	srv := newTestServer(t)
	t.Setenv("VAULT_TOKEN", "")
	c, err := New(config.Vault{Address: srv.URL, AuthMethod: "token"}, quietLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = c.ReadKV(context.Background(), "stacks/web")
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("ReadKV() before Login error = %v, want not-logged-in error", err)
	}
}

func TestReadKVEmptyPath(t *testing.T) {
	srv := newTestServer(t)
	c := tokenClient(t, srv, nil)
	if _, err := c.ReadKV(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "path must not be empty") {
		t.Errorf("ReadKV(\"\") error = %v, want empty-path error", err)
	}
}
