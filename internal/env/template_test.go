package env

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/vault"
)

// fakeVaultReader implements secrets.VaultReader from a canned table and
// records every read.
type fakeVaultReader struct {
	kv      map[string]map[string]string
	failAt  string
	reads   []string
	readErr map[string]int // path -> permanent failure
}

func (f *fakeVaultReader) ReadKV(_ context.Context, path string) (map[string]string, error) {
	f.reads = append(f.reads, path)
	if path == f.failAt {
		return nil, fmt.Errorf("vault boom: %s", path)
	}
	return f.kv[path], nil
}

// newFakeVaultServer starts a fake Vault KV v2 server serving kv below the
// default "secret" mount and returns a logged-in client for it — the same
// wiring the reconciler uses, so the template rendering is tested against
// the real HTTP client (URL escaping included).
func newFakeVaultServer(t *testing.T, kv map[string]map[string]string) *vault.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/secret/data/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
		if got := r.Header.Get("X-Vault-Token"); got != "tok-static" {
			t.Errorf("read %s: X-Vault-Token = %q, want the login token", path, got)
		}
		data, ok := kv[path]
		if !ok {
			http.Error(w, `{"errors":["not found"]}`, http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"data":{"data":%s}}`, mustJSON(t, data))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	t.Setenv("VAULT_TOKEN", "tok-static")
	client, err := vault.New(config.Vault{Address: srv.URL, AuthMethod: "token"}, quietVaultLogger())
	if err != nil {
		t.Fatalf("vault.New() error = %v", err)
	}
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("vault Login() error = %v", err)
	}
	return client
}

func quietVaultLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal vault data: %v", err)
	}
	return string(raw)
}

func TestRenderVaultTemplate(t *testing.T) {
	reader := &fakeVaultReader{kv: map[string]map[string]string{
		"hosts/host-01/traefik": {"db_password": "pg-secret", "api_key": "key-1"},
		"shared/tls":            {"cert": "CERTDATA"},
	}}
	template := `# vault-rendered environment
DB_PASSWORD=${VAULT:hosts/host-01/traefik#db_password}
API_KEY=${VAULT:hosts/host-01/traefik:api_key}
URL=https://${VAULT:shared/tls#cert}/endpoint?x=${VAULT:shared/tls#cert}
PLAIN=literal-value
NO-REF-LINE
`
	got, err := RenderVaultTemplate(context.Background(), template, reader)
	if err != nil {
		t.Fatalf("RenderVaultTemplate() error = %v", err)
	}
	want := map[string]string{
		"DB_PASSWORD": "pg-secret",                            // # separator
		"API_KEY":     "key-1",                                // : separator
		"URL":         "https://CERTDATA/endpoint?x=CERTDATA", // two refs per line, literal text around
		"PLAIN":       "literal-value",                        // untouched line
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RenderVaultTemplate() = %v, want %v", got, want)
	}
}

func TestRenderVaultTemplateWithVaultClient(t *testing.T) {
	client := newFakeVaultServer(t, map[string]map[string]string{
		"hosts/host-01/traefik": {"db_password": "pg-secret"},
	})
	template := "MY_VAR=${VAULT:hosts/host-01/traefik#db_password}\n"

	got, err := RenderVaultTemplate(context.Background(), template, client)
	if err != nil {
		t.Fatalf("RenderVaultTemplate() with real client error = %v", err)
	}
	if got["MY_VAR"] != "pg-secret" {
		t.Errorf("MY_VAR = %q, want pg-secret", got["MY_VAR"])
	}
}

func TestRenderVaultTemplateCachesPathsPerCall(t *testing.T) {
	reader := &fakeVaultReader{kv: map[string]map[string]string{
		"p": {"k": "v"},
	}}
	template := "A=${VAULT:p#k}\nB=${VAULT:p#k}\nC=${VAULT:p:k}\n"

	if _, err := RenderVaultTemplate(context.Background(), template, reader); err != nil {
		t.Fatalf("RenderVaultTemplate() error = %v", err)
	}
	if len(reader.reads) != 1 {
		t.Errorf("ReadKV calls = %d (%v), want 1 per distinct path per template", len(reader.reads), reader.reads)
	}
}

func TestRenderVaultTemplateMissingKey(t *testing.T) {
	reader := &fakeVaultReader{kv: map[string]map[string]string{
		"hosts/host-01/traefik": {"db_password": "pg-secret"},
	}}
	template := "OK=${VAULT:hosts/host-01/traefik#db_password}\nMY_VAR=${VAULT:hosts/host-01/traefik#missing_key}\n"

	_, err := RenderVaultTemplate(context.Background(), template, reader)
	if err == nil {
		t.Fatal("RenderVaultTemplate() with missing key error = nil, want error")
	}
	for _, want := range []string{"line 2", "${VAULT:hosts/host-01/traefik#missing_key}", `"missing_key"`, "hosts/host-01/traefik"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q misses %q", err, want)
		}
	}
}

func TestRenderVaultTemplateMissingPath(t *testing.T) {
	client := newFakeVaultServer(t, map[string]map[string]string{
		"known": {"k": "v"},
	})
	template := "MY_VAR=${VAULT:does/not/exist#k}\n"

	_, err := RenderVaultTemplate(context.Background(), template, client)
	if err == nil || !strings.Contains(err.Error(), "does/not/exist") {
		t.Errorf("RenderVaultTemplate() with missing path error = %v, want error naming the path", err)
	}
}

func TestRenderVaultTemplateReadFailure(t *testing.T) {
	reader := &fakeVaultReader{
		kv:     map[string]map[string]string{"broken": {"k": "v"}},
		failAt: "broken",
	}
	_, err := RenderVaultTemplate(context.Background(), "A=${VAULT:broken#k}\n", reader)
	if err == nil || !strings.Contains(err.Error(), "vault boom") || !strings.Contains(err.Error(), "${VAULT:broken#k}") {
		t.Errorf("RenderVaultTemplate() read failure error = %v, want reference and cause", err)
	}
}

func TestRenderVaultTemplateMalformedReferences(t *testing.T) {
	reader := &fakeVaultReader{kv: map[string]map[string]string{"p": {"k": "v"}}}
	cases := map[string]string{
		"no separator":     "A=${VAULT:only-a-path}\n",
		"empty key":        "A=${VAULT:p#}\n",
		"empty path":       "A=${VAULT:#k}\n",
		"empty with colon": "A=${VAULT::k}\n",
	}
	for name, template := range cases {
		_, err := RenderVaultTemplate(context.Background(), template, reader)
		if err == nil || !strings.Contains(err.Error(), "${VAULT:") {
			t.Errorf("RenderVaultTemplate() (%s) error = %v, want error naming the reference", name, err)
		}
	}
}

func TestMergeStackEnvVaultTemplates(t *testing.T) {
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "plain.env"), "SHARED=env\nONLY_ENV=1\n")
	writeFile(t, filepath.Join(baseDir, "tpl-a.env"), "SHARED=tpl-a\nONLY_TPL=a\nTWO=${VAULT:shared/x#a}\n")
	writeFile(t, filepath.Join(baseDir, "tpl-b.env"), "SHARED=tpl-b\nONLY_TPL=b\n")

	resolver := &fakeResolver{
		sops: map[string]map[string]string{
			filepath.Join(baseDir, "enc.env"): {"SHARED": "sops", "ONLY_SOPS": "s"},
		},
		vault: map[string]map[string]string{
			"shared/x": {"SHARED": "vault", "ONLY_VAULT": "v", "a": "a-from-vault"},
		},
	}
	stack := config.Stack{
		Name:              "web",
		Dir:               baseDir,
		EnvFiles:          []string{"plain.env"},
		SopsFiles:         []config.SopsFile{{Path: "enc.env"}},
		VaultPaths:        []string{"shared/x"},
		VaultEnvTemplates: []string{"tpl-a.env", "tpl-b.env"}, // later template wins
	}

	got, err := MergeStackEnv(context.Background(), stack, resolver, baseDir, SystemVars{})
	if err != nil {
		t.Fatalf("MergeStackEnv() error = %v", err)
	}
	want := map[string]string{
		"SHARED":     "tpl-b", // templates sit on the vault level, above sops and vault_paths; later template wins
		"ONLY_ENV":   "1",
		"ONLY_SOPS":  "s",
		"ONLY_VAULT": "v",
		"ONLY_TPL":   "b",
		// vault_paths merges every key of the path — including "a" — and
		// the template reads the same value through a reference.
		"a":          "a-from-vault",
		"TWO":        "a-from-vault",
		"STACK_NAME": "web",
		"STACK_DIR":  baseDir,
		"GIT_COMMIT": "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeStackEnv() = %v, want %v", got, want)
	}
}

func TestMergeStackEnvVaultTemplatesSystemVarsWin(t *testing.T) {
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "tpl.env"), "STACK_NAME=hacked\nT=${VAULT:p#k}\n")
	resolver := &fakeResolver{vault: map[string]map[string]string{"p": {"k": "v"}}}
	stack := config.Stack{Name: "web", Dir: baseDir, VaultEnvTemplates: []string{"tpl.env"}}

	got, err := MergeStackEnv(context.Background(), stack, resolver, baseDir, SystemVars{})
	if err != nil {
		t.Fatalf("MergeStackEnv() error = %v", err)
	}
	if got["STACK_NAME"] != "web" {
		t.Errorf("STACK_NAME = %q, want web (system vars cannot be overridden)", got["STACK_NAME"])
	}
}

func TestMergeStackEnvVaultTemplatesMissingFile(t *testing.T) {
	stack := config.Stack{Name: "web", VaultEnvTemplates: []string{"nope.env"}}
	_, err := MergeStackEnv(context.Background(), stack, &fakeResolver{}, t.TempDir(), SystemVars{})
	if err == nil || !strings.Contains(err.Error(), "vault_env_templates[0]") || !strings.Contains(err.Error(), "nope.env") {
		t.Errorf("MergeStackEnv() with missing template error = %v, want vault_env_templates[0] context", err)
	}
}

func TestMergeStackEnvVaultTemplatesRenderError(t *testing.T) {
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "tpl.env"), "A=${VAULT:p#missing}\n")
	stack := config.Stack{Name: "web", VaultEnvTemplates: []string{"tpl.env"}}
	resolver := &fakeResolver{vault: map[string]map[string]string{"p": {"k": "v"}}}

	_, err := MergeStackEnv(context.Background(), stack, resolver, baseDir, SystemVars{})
	if err == nil {
		t.Fatal("MergeStackEnv() with unresolvable template error = nil, want error")
	}
	// The error names the template file (added by the merge) and the
	// reference (added by the renderer).
	for _, want := range []string{"vault_env_templates[0]", filepath.Join(baseDir, "tpl.env"), "${VAULT:p#missing}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q misses %q", err, want)
		}
	}
}

func TestMergeStackEnvVaultTemplatesSkipStackDirDotEnv(t *testing.T) {
	baseDir := t.TempDir()
	// A stale generated .env must not be read as a template either.
	writeFile(t, filepath.Join(baseDir, ".env"), "STALE=${VAULT:p#k}\n")
	stack := config.Stack{Name: "web", Dir: baseDir, VaultEnvTemplates: []string{".env"}}

	got, err := MergeStackEnv(context.Background(), stack, &fakeResolver{vault: map[string]map[string]string{"p": {"k": "v"}}}, baseDir, SystemVars{})
	if err != nil {
		t.Fatalf("MergeStackEnv() error = %v, err; the generated .env must be skipped, not rendered", err)
	}
	if _, ok := got["STALE"]; ok {
		t.Errorf("merged env contains STALE = %q; <stackDir>/.env must be skipped as a template", got["STALE"])
	}
}
