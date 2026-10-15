package reconciler

// End-to-end integration test: complete Reconcile passes over real git
// repositories on disk, exercising the real git manager, config loader,
// stack hashing, state store, hook engine and env merge. Only the external
// binaries and services are faked:
//
//   - docker and sops are shell scripts shadowing the real binaries via a
//     PATH override; they log every invocation and always exit 0,
//   - Vault is an httptest.Server speaking the KV v2 JSON protocol.
//
// The scenario walks the whole lifecycle of a deployment:
//
//	pass 1: initial deploy of a local stack and an imported stack whose
//	        import selects a service subset
//	pass 2: no changes — no compose invocations (idempotency via the hash)
//	pass 3: a new host commit changing the stack definition — redeploy
//	pass 4: a stack whose pre_deploy hook exits 1 — no compose up, and the
//	        failure joins the pass error
//	pass 5: stacks removed from the configuration — prune tears them down
//	        with compose down (with -f while the compose file survives,
//	        by project name once the checkout directory is gone)

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/local/dockops/internal/dotenv"
	"github.com/local/dockops/internal/state"
)

// gitRemote is a bare repository (the "origin" the reconciler clones from)
// plus a seed working clone the test creates its commits in.
type gitRemote struct {
	t    *testing.T
	bare string
	seed string
}

// newGitRemote initializes an empty bare repository (branch main) and a seed
// clone of it. Commits made in the seed are published to the bare repository
// via commit, which is the only way the reconciler ever sees them.
func newGitRemote(t *testing.T, name string) *gitRemote {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, name+".git")
	runGit(t, dir, "init", "--bare", "-b", "main", bare)
	seed := filepath.Join(dir, name+"-seed")
	runGit(t, dir, "clone", bare, seed)
	return &gitRemote{t: t, bare: bare, seed: seed}
}

// commit stages and commits everything below the seed clone, pushes it to
// the bare repository and returns the new commit hash.
func (g *gitRemote) commit(msg string) string {
	g.t.Helper()
	runGit(g.t, g.seed, "add", ".")
	runGit(g.t, g.seed, "commit", "-m", msg)
	runGit(g.t, g.seed, "push", "origin", "main")
	return strings.TrimSpace(runGit(g.t, g.seed, "rev-parse", "HEAD"))
}

// write creates or replaces a file below the seed clone (not visible to the
// reconciler until the next commit).
func (g *gitRemote) write(rel, content string) {
	g.t.Helper()
	writeFile(g.t, filepath.Join(g.seed, rel), content)
}

// remove deletes a path below the seed clone.
func (g *gitRemote) remove(rel string) {
	g.t.Helper()
	if err := os.RemoveAll(filepath.Join(g.seed, rel)); err != nil {
		g.t.Fatalf("remove %s: %v", rel, err)
	}
}

// fakeToolBinaries writes shell scripts named docker and sops into a fresh
// directory prepended to PATH (shadowing any installed real binaries while
// git, sh and friends stay reachable). Both log into files given by env
// vars, which the test sets along with PATH:
//
//   - fake docker records each invocation as a "---call---" marker line
//     followed by one argument per line; when a --env-file argument is
//     present and readable it also dumps that file's content between
//     "---envfile---"/"---end---" markers, so the test sees the merged
//     environment docker compose would have interpolated. It exits 0.
//   - fake sops records its arguments (and SOPS_AGE_KEY_FILE when set) the
//     same way and "decrypts" by dumping the input file to stdout — the
//     fixtures store plaintext.
func fakeToolBinaries(t *testing.T) (dockerCalls, dockerEnv, sopsCalls string) {
	t.Helper()
	binDir := t.TempDir()
	logDir := t.TempDir()
	dockerCalls = filepath.Join(logDir, "docker.calls")
	dockerEnv = filepath.Join(logDir, "docker.env")
	sopsCalls = filepath.Join(logDir, "sops.calls")

	docker := `#!/bin/sh
{
	echo "---call---"
	for arg in "$@"; do
		printf '%s\n' "$arg"
	done
} >> "$FAKE_DOCKER_CALLS"
prev=""
for arg in "$@"; do
	if [ "$prev" = "--env-file" ] && [ -f "$arg" ]; then
		{
			echo "---envfile---"
			cat "$arg"
			echo "---end---"
		} >> "$FAKE_DOCKER_ENV"
		break
	fi
	prev="$arg"
done
exit 0
`
	sops := `#!/bin/sh
{
	echo "---call---"
	for arg in "$@"; do
		printf '%s\n' "$arg"
	done
	if [ -n "$SOPS_AGE_KEY_FILE" ]; then
		echo "SOPS_AGE_KEY_FILE=$SOPS_AGE_KEY_FILE"
	fi
} >> "$FAKE_SOPS_CALLS"
last=""
for arg in "$@"; do
	last="$arg"
done
cat "$last"
`
	for name, script := range map[string]string{"docker": docker, "sops": sops} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_CALLS", dockerCalls)
	t.Setenv("FAKE_DOCKER_ENV", dockerEnv)
	t.Setenv("FAKE_SOPS_CALLS", sopsCalls)
	return dockerCalls, dockerEnv, sopsCalls
}

// readInvocations parses a calls log written by the fake binaries: one
// invocation per "---call---" marker, one argument per line.
func readInvocations(t *testing.T, path string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var calls [][]string
	for _, chunk := range strings.Split(string(raw), "---call---") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		calls = append(calls, strings.Split(chunk, "\n"))
	}
	return calls
}

// invocationsWith returns every recorded invocation carrying want as an
// exact argument ("up"/"down" classifies the compose subcommand).
func invocationsWith(calls [][]string, want string) [][]string {
	var out [][]string
	for _, c := range calls {
		for _, a := range c {
			if a == want {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// readEnvDumps returns the content of every --env-file the fake docker
// dumped, in invocation order, parsed as dotenv.
func readEnvDumps(t *testing.T, path string) []map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var dumps []map[string]string
	for _, chunk := range strings.Split(string(raw), "---envfile---") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		content, _, _ := strings.Cut(chunk, "---end---")
		dumps = append(dumps, dotenv.Parse([]byte(content)))
	}
	return dumps
}

// withEnvFilePlaceholder replaces the value after every --env-file argument
// with "<ENVFILE>" so whole command lines compare equal without knowing the
// random tmpfs file name. It first asserts the argument looks like one of
// dockops' generated env files.
func withEnvFilePlaceholder(t *testing.T, args []string) []string {
	t.Helper()
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] != "--env-file" {
			continue
		}
		f := out[i+1]
		if !filepath.IsAbs(f) || !strings.HasPrefix(filepath.Base(f), "dockops-") || !strings.HasSuffix(f, ".env") {
			t.Errorf("env file argument %q is not an absolute dockops-*.env path", f)
		}
		out[i+1] = "<ENVFILE>"
	}
	return out
}

// assertStateFile reads the persisted state JSON from disk and checks that
// it holds exactly one entry per name in want, each with the wanted status
// and a non-empty hash (the empty want map asserts a fully pruned store).
func assertStateFile(t *testing.T, path string, want map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file %s: %v", path, err)
	}
	var f struct {
		Version int `json:"version"`
		Stacks  map[string]struct {
			Hash   string `json:"hash"`
			Status string `json:"status"`
		} `json:"stacks"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse state file %s: %v\n%s", path, err, raw)
	}
	if f.Version != 1 {
		t.Errorf("state file version = %d, want 1", f.Version)
	}
	if len(f.Stacks) != len(want) {
		t.Errorf("state file holds %d stacks (%v), want exactly %d", len(f.Stacks), f.Stacks, len(want))
	}
	for name, status := range want {
		st, ok := f.Stacks[name]
		if !ok {
			t.Errorf("state file has no entry for %q", name)
			continue
		}
		if st.Status != status {
			t.Errorf("state[%s].Status = %q, want %q", name, st.Status, status)
		}
		if st.Hash == "" {
			t.Errorf("state[%s].Hash is empty", name)
		}
	}
}

// TestReconcileEndToEnd drives the full lifecycle described in the file
// comment through one Reconciler instance, as the daemon loop would.
func TestReconcileEndToEnd(t *testing.T) {
	// Commit identity and hermetic git configuration for the repositories
	// the test creates; the reconciler's own git children inherit both.
	t.Setenv("GIT_AUTHOR_NAME", "dockops e2e")
	t.Setenv("GIT_AUTHOR_EMAIL", "e2e@dockops.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "dockops e2e")
	t.Setenv("GIT_COMMITTER_EMAIL", "e2e@dockops.invalid")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	// Fake docker + sops binaries and the fake Vault server; the token is
	// what the configured token auth method reads from the environment.
	dockerCallsFile, dockerEnvFile, sopsCallsFile := fakeToolBinaries(t)
	t.Setenv("VAULT_TOKEN", "e2e-token")
	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Vault-Token"); got != "e2e-token" {
			t.Errorf("vault request %s: X-Vault-Token = %q, want e2e-token", r.URL.Path, got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/secret/data/stacks/web" {
			t.Errorf("vault request: unexpected path %q, want /v1/secret/data/stacks/web", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"data":{"data":{"VAULT_SECRET":"vault-value"}}}`)
	}))
	defer vaultSrv.Close()

	// Host repository with the root config, one local stack ("web") and the
	// import of a service subset from the shared remote.
	workDir := t.TempDir()
	host := newGitRemote(t, "host")
	shared := newGitRemote(t, "shared")

	rootYAML := func(withImport bool) string {
		yaml := fmt.Sprintf(`global:
  work_dir: %q
  prune_removed_stacks: true
  vault:
    address: %q
    auth_method: token
remotes:
  shared:
    url: %q
    branch: main
`, workDir, vaultSrv.URL, shared.bare)
		if withImport {
			yaml += `imports:
  - remote: shared
    path: monitoring
    name: shared-monitoring
    services:
      - prometheus
`
		}
		return yaml
	}

	webStack := func(extraEnvFile bool) string {
		yaml := `name: web
profiles:
  - edge
services:
  - app
env_files:
  - base.env
`
		if extraEnvFile {
			yaml += "  - extra.env\n"
		}
		yaml += `sops_files:
  - path: secrets.env
vault_paths:
  - stacks/web
hooks:
  pre_deploy:
    - echo pre-web
  post_deploy:
    - echo post-web
`
		return yaml
	}

	host.write("stacks.yaml", rootYAML(true))
	host.write("web/stack.yaml", webStack(false))
	host.write("web/compose.yaml", "services:\n  app:\n    image: nginx:1.27\n    environment:\n      FROM_ENV: ${FROM_ENV}\n")
	host.write("web/base.env", "FROM_ENV=plain-value\n")
	// A committed .env (a stale artifact of an earlier tool or deployment)
	// must NOT be read as an env_files source — dockops regenerates it.
	host.write("web/.env", "STALE=stale-value\n")
	host.write("web/secrets.env", "SOPS_SECRET=sops-value\n")

	shared.write("monitoring/stack.yaml", "name: monitoring\nprofiles:\n  - full\nservices:\n  - prometheus\n  - grafana\n")
	shared.write("monitoring/compose.yaml", "services:\n  prometheus:\n    image: prom/prometheus\n  grafana:\n    image: grafana/grafana\n")

	hostCommit1 := host.commit("initial host config")
	sharedCommit := shared.commit("shared stacks")

	statePath := filepath.Join(t.TempDir(), "state.json")
	store, err := state.New(statePath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	r := New(Deps{
		RepoURL: host.bare,
		Branch:  "main",
		WorkDir: workDir,
		State:   store,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Git, Compose, Hooks, Sops and NewVault stay nil: the real
		// implementations run against the fake binaries on PATH and the
		// httptest Vault.
	})

	// ---- pass 1: initial deploy --------------------------------------

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 1 Reconcile() error = %v", err)
	}

	ups := invocationsWith(readInvocations(t, dockerCallsFile), "up")
	if len(ups) != 2 {
		t.Fatalf("pass 1: %d compose up invocations, want 2 (web, shared-monitoring): %v", len(ups), ups)
	}
	webDir := filepath.Join(workDir, "host", "web")
	wantWebUp := []string{
		"compose", "-p", "web", "-f", filepath.Join(webDir, "compose.yaml"),
		"--profile", "edge", "--env-file", "<ENVFILE>",
		"up", "-d", "--remove-orphans", "app",
	}
	if got := withEnvFilePlaceholder(t, ups[0]); !reflect.DeepEqual(got, wantWebUp) {
		t.Errorf("pass 1 web up args = %v, want %v", got, wantWebUp)
	}
	monDir := filepath.Join(workDir, "shared", "monitoring")
	wantMonUp := []string{
		"compose", "-p", "shared-monitoring", "-f", filepath.Join(monDir, "compose.yaml"),
		"--profile", "full", "--env-file", "<ENVFILE>",
		"up", "-d", "--remove-orphans", "prometheus",
	}
	if got := withEnvFilePlaceholder(t, ups[1]); !reflect.DeepEqual(got, wantMonUp) {
		t.Errorf("pass 1 import up args = %v, want %v (base profile, import service subset)", got, wantMonUp)
	}

	// The env file docker compose received carries the whole merged
	// environment: env_files, sops, vault and the system variables.
	dumps := readEnvDumps(t, dockerEnvFile)
	if len(dumps) != 2 {
		t.Fatalf("pass 1: %d env file dumps, want 2", len(dumps))
	}
	for k, want := range map[string]string{
		"FROM_ENV":     "plain-value",
		"SOPS_SECRET":  "sops-value",
		"VAULT_SECRET": "vault-value",
		"STACK_NAME":   "web",
		"GIT_COMMIT":   hostCommit1,
	} {
		if got := dumps[0][k]; got != want {
			t.Errorf("pass 1 web env[%s] = %q, want %q", k, got, want)
		}
	}
	// The committed .env was skipped as a source (it is regenerated), so
	// its stale value never reaches the merge.
	if stale, ok := dumps[0]["STALE"]; ok && stale != "" {
		t.Errorf("pass 1 web env[STALE] = %q, want absent (stack-dir .env is skipped as a source)", stale)
	}

	// The generated stack-dir .env exists right after the pass and carries
	// the same merged environment — the file compose's env_file: ./.env
	// references and ${...} interpolation resolve against.
	stackEnvRaw, err := os.ReadFile(filepath.Join(webDir, ".env"))
	if err != nil {
		t.Fatalf("read generated stack .env: %v", err)
	}
	stackEnv := dotenv.Parse(stackEnvRaw)
	for k, want := range map[string]string{
		"FROM_ENV":     "plain-value",
		"SOPS_SECRET":  "sops-value",
		"VAULT_SECRET": "vault-value",
		"STACK_NAME":   "web",
		"GIT_COMMIT":   hostCommit1,
	} {
		if got := stackEnv[k]; got != want {
			t.Errorf("pass 1 generated stack .env[%s] = %q, want %q", k, got, want)
		}
	}
	if info, err := os.Stat(filepath.Join(webDir, ".env")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("generated stack .env permissions = %v, %v; want 0600", info, err)
	}
	if got := dumps[1]["GIT_COMMIT"]; got != sharedCommit {
		t.Errorf("pass 1 import env[GIT_COMMIT] = %q, want %q (commit of the shared checkout)", got, sharedCommit)
	}
	if got := dumps[1]["STACK_NAME"]; got != "shared-monitoring" {
		t.Errorf("pass 1 import env[STACK_NAME] = %q, want shared-monitoring (import name override)", got)
	}

	// The fake sops saw the expected command line exactly once.
	sopsCalls := readInvocations(t, sopsCallsFile)
	wantSops := []string{"-d", "--input-type", "dotenv", "--output-type", "dotenv", filepath.Join(webDir, "secrets.env")}
	if len(sopsCalls) != 1 || !reflect.DeepEqual(sopsCalls[0], wantSops) {
		t.Errorf("pass 1 sops invocations = %v, want exactly %v", sopsCalls, wantSops)
	}

	assertStateFile(t, statePath, map[string]string{
		"web":               state.StatusDeployed,
		"shared-monitoring": state.StatusDeployed,
	})

	// ---- pass 2: unchanged → idempotent ------------------------------

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 2 Reconcile() error = %v", err)
	}
	if ups := invocationsWith(readInvocations(t, dockerCallsFile), "up"); len(ups) != 2 {
		t.Errorf("pass 2: %d compose up invocations, want still 2 (unchanged hashes skip)", len(ups))
	}
	if downs := invocationsWith(readInvocations(t, dockerCallsFile), "down"); len(downs) != 0 {
		t.Errorf("pass 2: %d compose down invocations, want 0: %v", len(downs), downs)
	}

	// ---- pass 3: changed stack definition → redeploy ------------------

	host.write("web/extra.env", "EXTRA=extra-value\n")
	host.write("web/stack.yaml", webStack(true))
	host.commit("extend web env_files")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 3 Reconcile() error = %v", err)
	}
	ups = invocationsWith(readInvocations(t, dockerCallsFile), "up")
	if len(ups) != 3 {
		t.Fatalf("pass 3: %d compose up invocations, want 3 (web redeployed): %v", len(ups), ups)
	}
	if got := ups[2][2]; got != "web" {
		t.Errorf("pass 3: redeployed project %q, want web", got)
	}
	if dumps := readEnvDumps(t, dockerEnvFile); dumps[2]["EXTRA"] != "extra-value" {
		t.Errorf("pass 3: redeployed web env[EXTRA] = %q, want extra-value (new env_file line reached compose)", dumps[2]["EXTRA"])
	}

	// ---- pass 4: failing pre_deploy hook → no deployment --------------

	host.write("hookfail/stack.yaml", "name: hookfail\nhooks:\n  pre_deploy:\n    - exit 1\n")
	host.write("hookfail/compose.yaml", "services:\n  app:\n    image: nginx\n")
	host.commit("add stack with failing pre_deploy hook")

	err = r.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "hookfail") || !strings.Contains(err.Error(), "pre_deploy") {
		t.Fatalf("pass 4 Reconcile() error = %v, want pre_deploy failure naming hookfail", err)
	}
	if ups := invocationsWith(readInvocations(t, dockerCallsFile), "up"); len(ups) != 3 {
		t.Errorf("pass 4: %d compose up invocations, want still 3 (hookfail was never deployed)", len(ups))
	}
	assertStateFile(t, statePath, map[string]string{
		"web":               state.StatusDeployed,
		"shared-monitoring": state.StatusDeployed,
		"hookfail":          state.StatusFailed,
	})

	// ---- pass 5: stacks removed → prune with compose down -------------

	// web and hookfail lose their checkout directories with the next host
	// commit (so pruning falls back to `down` without -f); the import entry
	// disappears while the shared checkout survives (so pruning takes the
	// full `down -f` path).
	host.remove("web")
	host.remove("hookfail")
	host.write("stacks.yaml", rootYAML(false))
	host.commit("remove web, hookfail and the import")
	// Phase 6c: delete the stack folders completely — all files, the
	// generated .env, the directory itself — as the livetest run showed it
	// (reset --hard + clean -fd leave nothing behind). Prune must tear the
	// stacks down by project name alone.
	for _, dir := range []string{
		filepath.Join(workDir, "host", "web"),
		filepath.Join(workDir, "host", "hookfail"),
	} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("remove checkout stack dir %s: %v", dir, err)
		}
	}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 5 Reconcile() error = %v", err)
	}
	downs := invocationsWith(readInvocations(t, dockerCallsFile), "down")
	wantDowns := [][]string{
		// Sorted state order hookfail < shared-monitoring < web; the two
		// host-repo stacks are gone from their checkouts, the import keeps
		// its compose file in the surviving shared checkout.
		{"compose", "-p", "hookfail", "down", "--remove-orphans"},
		{"compose", "-p", "shared-monitoring", "-f", filepath.Join(monDir, "compose.yaml"),
			"--env-file", "<ENVFILE>", "down", "--remove-orphans"},
		{"compose", "-p", "web", "down", "--remove-orphans"},
	}
	if len(downs) != len(wantDowns) {
		t.Fatalf("pass 5: %d compose down invocations, want %d: %v", len(downs), len(wantDowns), downs)
	}
	for i, want := range wantDowns {
		if got := withEnvFilePlaceholder(t, downs[i]); !reflect.DeepEqual(got, want) {
			t.Errorf("pass 5 down[%d] args = %v, want %v", i, got, want)
		}
	}
	if ups := invocationsWith(readInvocations(t, dockerCallsFile), "up"); len(ups) != 3 {
		t.Errorf("pass 5: %d compose up invocations, want still 3 (nothing to deploy)", len(ups))
	}
	assertStateFile(t, statePath, map[string]string{})
}
