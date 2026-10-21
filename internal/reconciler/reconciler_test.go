package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/dotenv"
	"github.com/local/dockops/internal/state"
)

// ---- fakes (no git/docker/sops/vault binaries needed) ----

// fakeGit "ensures" repositories without touching git: by name below
// workDir (EnsureRepo, the bootstrap path) and by absolute path
// (EnsureRepoAt, the configured checkout paths), recording both.
type fakeGit struct {
	workDir   string
	ensured   []string          // repository names in call order (EnsureRepo)
	ensuredAt []string          // absolute checkout paths in call order (EnsureRepoAt)
	failAt    string            // repository name that fails
	commits   map[string]string // checkout path -> commit; unknown paths error
}

func (f *fakeGit) EnsureRepo(name, url, branch string) (string, error) {
	f.ensured = append(f.ensured, name)
	if f.failAt == name {
		return "", fmt.Errorf("git boom: %s", name)
	}
	return filepath.Join(f.workDir, name), nil
}

func (f *fakeGit) EnsureRepoAt(path, url, branch string) (string, error) {
	f.ensuredAt = append(f.ensuredAt, path)
	return path, nil
}

func (f *fakeGit) HeadCommit(repoPath string) (string, error) {
	if commit, ok := f.commits[repoPath]; ok {
		return commit, nil
	}
	return "", fmt.Errorf("no commit recorded for %s", repoPath)
}

// upCall records one compose.Up invocation.
type upCall struct {
	stack          config.Stack
	stackDir       string
	envFile        string
	envExists      bool
	env            map[string]string // content of the env file at call time
	stackEnvExists bool              // <stackDir>/.env existed at call time
	stackEnv       map[string]string // content of <stackDir>/.env at call time
}

// downCall records one compose.Down invocation.
type downCall struct {
	stack     config.Stack
	stackDir  string
	envFile   string
	envExists bool
}

// fakeCompose records Up/Down/DownProject calls with optional failures. The
// read-only image methods of the phase-10 update check answer from canned
// tables so no docker binary is needed.
type fakeCompose struct {
	ups               []upCall
	downs             []downCall
	downProjects      []string
	failUpFor         string
	failDownFor       string
	failDownProjectAt string
	// images is the service→image map Images reports; imagesErr fails it.
	images     map[string]string
	imagesErr  error
	imagesCall int
	// digests maps an image reference to its local repository digest; a
	// missing entry means "locally built" (empty digest).
	digests   map[string]string
	digestErr error
	// imageDigestCalls counts the inspect lookups in call order.
	imageDigestCalls []string
	// onUp, when set, runs synchronously at the start of every Up with the
	// stack name — tests use it to inject mid-pass effects (e.g. making
	// the state directory unwritable to simulate an interrupted pass).
	onUp func(name string)
}

func (f *fakeCompose) Up(_ context.Context, stack config.Stack, stackDir, envFile string) error {
	if f.onUp != nil {
		f.onUp(stack.Name)
	}
	call := upCall{stack: stack, stackDir: stackDir, envFile: envFile}
	if info, err := os.Stat(envFile); err == nil && info.Mode().IsRegular() {
		call.envExists = true
		if raw, err := os.ReadFile(envFile); err == nil {
			call.env = dotenv.Parse(raw)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(stackDir, ".env")); err == nil {
		call.stackEnvExists = true
		call.stackEnv = dotenv.Parse(raw)
	}
	f.ups = append(f.ups, call)
	if f.failUpFor == stack.Name {
		return fmt.Errorf("compose up boom: %s", stack.Name)
	}
	return nil
}

func (f *fakeCompose) Down(_ context.Context, stack config.Stack, stackDir, envFile string) error {
	call := downCall{stack: stack, stackDir: stackDir, envFile: envFile}
	if info, err := os.Stat(envFile); err == nil && info.Mode().IsRegular() {
		call.envExists = true
	}
	f.downs = append(f.downs, call)
	if f.failDownFor == stack.Name {
		return fmt.Errorf("compose down boom: %s", stack.Name)
	}
	return nil
}

func (f *fakeCompose) Images(_ context.Context, _ compose.Request) (map[string]string, error) {
	f.imagesCall++
	if f.imagesErr != nil {
		return nil, f.imagesErr
	}
	out := make(map[string]string, len(f.images))
	for k, v := range f.images {
		out[k] = v
	}
	return out, nil
}

func (f *fakeCompose) ImageDigest(_ context.Context, ref string) (string, error) {
	f.imageDigestCalls = append(f.imageDigestCalls, ref)
	if f.digestErr != nil {
		return "", f.digestErr
	}
	return f.digests[ref], nil
}

func (f *fakeCompose) DownProject(_ context.Context, project string) error {
	f.downProjects = append(f.downProjects, project)
	if f.failDownProjectAt == project {
		return fmt.Errorf("compose down project boom: %s", project)
	}
	return nil
}

// hookCall records one hooks.Run invocation.
type hookCall struct {
	commands []string
	dir      string
	env      []string
	timeout  time.Duration
}

// fakeHooks records every Run; a run whose first command equals failFirstCmd
// fails (tests use distinct sentinel commands for pre and post hooks).
type fakeHooks struct {
	calls        []hookCall
	failFirstCmd string
}

func (f *fakeHooks) Run(_ context.Context, commands []string, dir string, env []string, timeout time.Duration) error {
	// An empty command list is a no-op in the real engine (it starts zero
	// processes) — record nothing so call counts match observable behavior.
	if len(commands) == 0 {
		return nil
	}
	f.calls = append(f.calls, hookCall{
		commands: append([]string(nil), commands...),
		dir:      dir,
		env:      append([]string(nil), env...),
		timeout:  timeout,
	})
	if commands[0] == f.failFirstCmd {
		return fmt.Errorf("hook boom: %s", commands[0])
	}
	return nil
}

// fakeSops records decrypt calls and answers from a canned table.
type fakeSops struct {
	calls  []struct{ path, keyfile string }
	kv     map[string]map[string]string
	failAt string
}

func (f *fakeSops) decrypt(_ context.Context, path, ageKeyfile string) (map[string]string, error) {
	f.calls = append(f.calls, struct{ path, keyfile string }{path, ageKeyfile})
	if path == f.failAt {
		return nil, fmt.Errorf("sops boom: %s", path)
	}
	if kv, ok := f.kv[path]; ok {
		return kv, nil
	}
	return map[string]string{}, nil
}

// fakeVaultClient counts logins and answers ReadKV from a canned table.
type fakeVaultClient struct {
	logins     int
	reads      []string
	kv         map[string]map[string]string
	failLogin  bool
	failReadAt string
}

func (c *fakeVaultClient) Login(context.Context) error {
	c.logins++
	if c.failLogin {
		return fmt.Errorf("vault login boom")
	}
	return nil
}

func (c *fakeVaultClient) ReadKV(_ context.Context, path string) (map[string]string, error) {
	c.reads = append(c.reads, path)
	if c.failReadAt == path {
		return nil, fmt.Errorf("vault read boom: %s", path)
	}
	if kv, ok := c.kv[path]; ok {
		return kv, nil
	}
	return map[string]string{}, nil
}

// fakeVaultFactory hands out the (shared) fake client and counts calls.
type fakeVaultFactory struct {
	client *fakeVaultClient
	calls  int
}

func (f *fakeVaultFactory) make(_ config.Vault, _ *slog.Logger) (VaultClient, error) {
	f.calls++
	if f.client == nil {
		return nil, fmt.Errorf("vault factory boom")
	}
	return f.client, nil
}

// ---- fixture ----

// fixture assembles a full fake environment: a host repository layout below
// <workDir>/host, a real state store on a temp file, and injectable fakes
// for every external collaborator.
type fixture struct {
	t       *testing.T
	workDir string
	git     *fakeGit
	compose *fakeCompose
	hooks   *fakeHooks
	sops    *fakeSops
	vault   *fakeVaultFactory
	state   *state.Store
}

// newFixture writes the given rootYAML (with the placeholder WORKDIR
// replaced by the fixture work dir) into the fake host repository. The
// work_dir must match the fixture so the config loader finds the imports in
// the same place the fake git manager "clones" them.
func newFixture(t *testing.T, rootYAML string) *fixture {
	t.Helper()
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "host", rootConfigFile),
		strings.ReplaceAll(rootYAML, "WORKDIR", workDir))

	store, err := state.New(filepath.Join(t.TempDir(), "state.json"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	return &fixture{
		t:       t,
		workDir: workDir,
		git:     &fakeGit{workDir: workDir},
		compose: &fakeCompose{},
		hooks:   &fakeHooks{},
		sops:    &fakeSops{},
		vault:   &fakeVaultFactory{client: &fakeVaultClient{}},
		state:   store,
	}
}

// reconciler wires the fixture's fakes into a Reconciler.
func (f *fixture) reconciler() *Reconciler {
	return New(Deps{
		RepoURL:  "https://example.invalid/host.git",
		Branch:   "main",
		WorkDir:  f.workDir,
		State:    f.state,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Git:      f.git,
		Compose:  f.compose,
		Hooks:    f.hooks,
		Sops:     f.sops.decrypt,
		NewVault: f.vault.make,
	})
}

// addLocalStack writes a stack directory into the host repository: the
// given stack.yaml plus a minimal compose file.
func (f *fixture) addLocalStack(dirName, stackYAML string) {
	f.t.Helper()
	f.addStack(filepath.Join("host", dirName), stackYAML)
}

// addRemoteStack writes a stack directory into a remote checkout.
func (f *fixture) addRemoteStack(remote, dirName, stackYAML string) {
	f.t.Helper()
	f.addStack(filepath.Join(remote, dirName), stackYAML)
}

// addStack writes the stack definition and a minimal compose file below
// <workDir>/<rel>.
func (f *fixture) addStack(rel, stackYAML string) {
	f.t.Helper()
	writeFile(f.t, filepath.Join(f.workDir, rel, "stack.yaml"), stackYAML)
	writeFile(f.t, filepath.Join(f.workDir, rel, "compose.yaml"),
		"services:\n  app:\n    image: nginx:latest\n")
}

// writeFile creates path and parent directories with the given content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// upNames returns the names of all recorded Up calls in order.
func (f *fixture) upNames() []string {
	names := make([]string, 0, len(f.compose.ups))
	for _, up := range f.compose.ups {
		names = append(names, up.stack.Name)
	}
	return names
}

// defaultRoot is the root configuration most tests need: the work dir
// matches the fixture, no remotes, pruning off.
const defaultRoot = `global:
  work_dir: WORKDIR
`

// ---- tests ----

func TestReconcileDeploysAndSkipsUnchanged(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")
	f.addLocalStack("db", "name: db\n")

	r := f.reconciler()
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}

	if got, want := f.upNames(), []string{"db", "web"}; !reflect.DeepEqual(got, want) {
		t.Errorf("first pass Ups = %v, want %v", got, want)
	}
	for _, name := range []string{"web", "db"} {
		st, ok := f.state.Get(name)
		if !ok || st.Status != state.StatusDeployed {
			t.Errorf("state[%s] = %+v, %v; want deployed", name, st, ok)
		}
		if st.Hash == "" {
			t.Errorf("state[%s].Hash is empty", name)
		}
		dir := filepath.Join(f.workDir, "host", name)
		if st.StackDir != dir {
			t.Errorf("state[%s].StackDir = %q, want %q", name, st.StackDir, dir)
		}
		if st.ComposeFile != filepath.Join(dir, "compose.yaml") {
			t.Errorf("state[%s].ComposeFile = %q, want compose.yaml path", name, st.ComposeFile)
		}
	}
	// The env file handed to compose existed during Up and is cleaned up
	// when the stack deployment ends.
	for _, up := range f.compose.ups {
		if !up.envExists {
			t.Errorf("env file %q did not exist during Up", up.envFile)
		}
		if _, err := os.Stat(up.envFile); !os.IsNotExist(err) {
			t.Errorf("env file %q still exists after Reconcile", up.envFile)
		}
	}

	// Second pass: hashes unchanged, nothing deploys again.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if got, want := f.upNames(), []string{"db", "web"}; !reflect.DeepEqual(got, want) {
		t.Errorf("second pass Ups = %v, want %v (unchanged stacks are skipped)", got, want)
	}
	// Both passes only ever ensure the host repository (no remotes
	// configured): once per pass.
	if !reflect.DeepEqual(f.git.ensured, []string{"host", "host"}) {
		t.Errorf("EnsureRepo calls = %v, want [host host]", f.git.ensured)
	}
}

func TestReconcileRetriesAfterFailedAttempt(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.failUpFor = "web"

	r := f.reconciler()
	err := r.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("first Reconcile() error = %v, want failure naming web", err)
	}
	st, ok := f.state.Get("web")
	if !ok || st.Status != state.StatusFailed {
		t.Fatalf("state[web] after failed attempt = %+v, %v; want failed", st, ok)
	}

	// The failure is transient; the hash is unchanged but the stack must be
	// retried because the last attempt did not end deployed.
	f.compose.failUpFor = ""
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if got := f.upNames(); !reflect.DeepEqual(got, []string{"web", "web"}) {
		t.Errorf("Ups = %v, want web twice (retry after failure)", got)
	}
	if st, _ := f.state.Get("web"); st.Status != state.StatusDeployed {
		t.Errorf("state[web] = %+v, want deployed after retry", st)
	}
}

func TestReconcileWritesStackEnvBeforeUp(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.git.commits = map[string]string{filepath.Join(f.workDir, "host"): "abc123def"}
	f.sops.kv = map[string]map[string]string{
		filepath.Join(f.workDir, "host", "web", "enc.env"): {"SECRET": "hunter2"},
	}
	stackDir := filepath.Join(f.workDir, "host", "web")
	f.addLocalStack("web", "name: web\nenv_files:\n  - base.env\nsops_files:\n  - path: enc.env\n")
	writeFile(t, filepath.Join(stackDir, "base.env"), "BASE=from-base\n")
	// A stale .env (previous deployment or committed artifact) is neither
	// read as a source nor left behind.
	writeFile(t, filepath.Join(stackDir, ".env"), "STALE=old-value\n")

	r := f.reconciler()
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if len(f.compose.ups) != 1 {
		t.Fatalf("Ups = %d, want 1", len(f.compose.ups))
	}
	up := f.compose.ups[0]
	// The stack-dir .env exists when compose Up runs and carries the whole
	// merged environment — including the secrets.
	if !up.stackEnvExists {
		t.Errorf("<stackDir>/.env did not exist during Up")
	}
	for k, want := range map[string]string{
		"BASE":       "from-base",
		"SECRET":     "hunter2",
		"STACK_NAME": "web",
		"GIT_COMMIT": "abc123def",
	} {
		if got := up.stackEnv[k]; got != want {
			t.Errorf("stack .env[%s] = %q, want %q", k, got, want)
		}
	}
	if stale, ok := up.stackEnv["STALE"]; ok && stale != "" {
		t.Errorf("stack .env[STALE] = %q, want absent (old .env is not a source)", stale)
	}
	// The shm env file handed to compose carries the same environment.
	if got := up.env["SECRET"]; got != "hunter2" {
		t.Errorf("shm env file[SECRET] = %q, want hunter2 (WriteShm is kept)", got)
	}

	// Success: the generated .env stays in the stack dir after the pass.
	raw, err := os.ReadFile(filepath.Join(stackDir, ".env"))
	if err != nil {
		t.Fatalf("read generated stack .env after Reconcile: %v", err)
	}
	if got := dotenv.Parse(raw)["SECRET"]; got != "hunter2" {
		t.Errorf("stack .env after Reconcile[SECRET] = %q, want hunter2", got)
	}

	// Second pass: hash unchanged → still exactly one Up (the skip).
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(f.compose.ups) != 1 {
		t.Errorf("Ups after second pass = %d, want 1 (unchanged hash skips)", len(f.compose.ups))
	}
}

func TestReconcileUpFailureRollsBackStackEnv(t *testing.T) {
	t.Run("no previous .env is removed", func(t *testing.T) {
		f := newFixture(t, defaultRoot)
		f.addLocalStack("web", "name: web\n")
		f.compose.failUpFor = "web"

		if err := f.reconciler().Reconcile(context.Background()); err == nil {
			t.Fatal("Reconcile() error = nil, want up failure")
		}
		if _, err := os.Stat(filepath.Join(f.workDir, "host", "web", ".env")); !os.IsNotExist(err) {
			t.Errorf("generated .env still exists after failed up: %v", err)
		}
	})

	t.Run("previous .env is restored", func(t *testing.T) {
		f := newFixture(t, defaultRoot)
		f.addLocalStack("web", "name: web\n")
		envPath := filepath.Join(f.workDir, "host", "web", ".env")
		writeFile(t, envPath, "OLD=previous\n")
		f.compose.failUpFor = "web"

		if err := f.reconciler().Reconcile(context.Background()); err == nil {
			t.Fatal("Reconcile() error = nil, want up failure")
		}
		raw, err := os.ReadFile(envPath)
		if err != nil {
			t.Fatalf("read .env after failed up: %v", err)
		}
		if want := "OLD=previous\n"; string(raw) != want {
			t.Errorf(".env after failed up = %q, want restored %q", raw, want)
		}
	})
}

func TestReconcileComposeFiles(t *testing.T) {
	f := newFixture(t, defaultRoot)
	stackDir := filepath.Join(f.workDir, "host", "multi")
	writeFile(t, filepath.Join(stackDir, "stack.yaml"),
		"name: multi\ncompose_files: [compose.yaml, override.yaml]\n")
	writeFile(t, filepath.Join(stackDir, "compose.yaml"), "services:\n  app:\n    image: nginx:1\n")
	writeFile(t, filepath.Join(stackDir, "override.yaml"), "services:\n  app:\n    image: nginx:2\n")

	r := f.reconciler()
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if len(f.compose.ups) != 1 {
		t.Fatalf("Ups = %d, want 1", len(f.compose.ups))
	}
	wantFiles := []string{
		filepath.Join(stackDir, "compose.yaml"),
		filepath.Join(stackDir, "override.yaml"),
	}
	if got := f.compose.ups[0].stack.ComposeFiles; !reflect.DeepEqual(got, wantFiles) {
		t.Errorf("Up stack.ComposeFiles = %v, want %v", got, wantFiles)
	}
	st, ok := f.state.Get("multi")
	if !ok {
		t.Fatal("state has no entry for multi")
	}
	if !reflect.DeepEqual(st.ComposeFiles, wantFiles) {
		t.Errorf("state.ComposeFiles = %v, want %v (recorded for pruning)", st.ComposeFiles, wantFiles)
	}
	if st.ComposeFile != "" {
		t.Errorf("state.ComposeFile = %q, want empty in the multi-file case", st.ComposeFile)
	}

	// Changing only the override file changes the hash and redeploys.
	writeFile(t, filepath.Join(stackDir, "override.yaml"), "services:\n  app:\n    image: nginx:3\n")
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(f.compose.ups) != 2 {
		t.Errorf("Ups = %d, want 2 (override change redeploys)", len(f.compose.ups))
	}
}

func TestReconcilePreDeployFailureSkipsStackButContinues(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.hooks.failFirstCmd = "pre-boom"
	f.addLocalStack("bad", "name: bad\nhooks:\n  pre_deploy: [pre-boom]\n  post_deploy: [never]\n")
	f.addLocalStack("good", "name: good\n")

	err := f.reconciler().Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "pre_deploy") {
		t.Fatalf("Reconcile() error = %v, want pre_deploy failure naming bad", err)
	}
	if got, want := f.upNames(), []string{"good"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Ups = %v, want %v (bad skipped, good deployed)", got, want)
	}
	// The post hook of the failed stack never ran.
	for _, call := range f.hooks.calls {
		if reflect.DeepEqual(call.commands, []string{"never"}) {
			t.Error("post_deploy of the failed stack ran")
		}
	}
	if st, _ := f.state.Get("bad"); st.Status != state.StatusFailed {
		t.Errorf("state[bad] = %+v, want failed", st)
	}
	if st, _ := f.state.Get("good"); st.Status != state.StatusDeployed {
		t.Errorf("state[good] = %+v, want deployed", st)
	}
}

func TestReconcilePostDeployFailureStillDeployed(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.hooks.failFirstCmd = "post-boom"
	f.addLocalStack("web", "name: web\nhooks:\n  post_deploy: [post-boom]\n")

	err := f.reconciler().Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "post_deploy") {
		t.Fatalf("Reconcile() error = %v, want post_deploy failure to join the pass error", err)
	}
	if got := f.upNames(); !reflect.DeepEqual(got, []string{"web"}) {
		t.Errorf("Ups = %v, want [web]", got)
	}
	// The briefing: the stack counts as deployed regardless.
	if st, _ := f.state.Get("web"); st.Status != state.StatusDeployed {
		t.Errorf("state[web] = %+v, want deployed despite post_deploy failure", st)
	}
}

func TestReconcileHookEnvAndSecrets(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.git.commits = map[string]string{filepath.Join(f.workDir, "host"): "abc123def"}
	f.sops.kv = map[string]map[string]string{
		filepath.Join(f.workDir, "host", "web", "enc.env"): {"SECRET": "hunter2"},
	}
	f.vault.client.kv = map[string]map[string]string{
		"kv/web": {"VKEY": "vval"},
	}
	f.addLocalStack("web", "name: web\nsops_files:\n  - path: enc.env\nvault_paths: [kv/web]\nhooks:\n  post_deploy: [true]\n")

	if err := f.reconciler().Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if len(f.hooks.calls) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(f.hooks.calls))
	}
	hookEnv := f.hooks.calls[0].env
	stackDir := filepath.Join(f.workDir, "host", "web")
	for _, want := range []string{
		"STACK_NAME=web",
		"STACK_DIR=" + stackDir,
		"GIT_COMMIT=abc123def",
		"SECRET=hunter2", // sops secret reaches the hooks
		"VKEY=vval",      // vault secret reaches the hooks
	} {
		if !contains(hookEnv, want) {
			t.Errorf("hook env %v misses %q", hookEnv, want)
		}
	}

	// The same environment (secrets included) was written to the env file
	// handed to docker compose.
	if len(f.compose.ups) != 1 {
		t.Fatalf("Ups = %d, want 1", len(f.compose.ups))
	}
	upEnv := f.compose.ups[0].env
	for k, want := range map[string]string{"SECRET": "hunter2", "VKEY": "vval", "STACK_NAME": "web", "GIT_COMMIT": "abc123def"} {
		if got := upEnv[k]; got != want {
			t.Errorf("env file[%s] = %q, want %q", k, got, want)
		}
	}

	// Vault login happens exactly once per pass.
	if f.vault.calls != 1 {
		t.Errorf("vault factory calls = %d, want 1", f.vault.calls)
	}
	if f.vault.client.logins != 1 {
		t.Errorf("vault logins = %d, want 1", f.vault.client.logins)
	}
	if !reflect.DeepEqual(f.vault.client.reads, []string{"kv/web"}) {
		t.Errorf("vault reads = %v, want [kv/web]", f.vault.client.reads)
	}
	// The sops file was resolved against the stack dir.
	if len(f.sops.calls) != 1 || f.sops.calls[0].path != filepath.Join(stackDir, "enc.env") {
		t.Errorf("sops calls = %+v, want one resolved path", f.sops.calls)
	}
}

func TestReconcileVaultEnvTemplates(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.git.commits = map[string]string{filepath.Join(f.workDir, "host"): "abc123def"}
	f.vault.client.kv = map[string]map[string]string{
		"hosts/host-01/traefik": {"db_password": "pg-secret", "spare": "unused"},
	}
	stackDir := filepath.Join(f.workDir, "host", "web")
	f.addLocalStack("web", "name: web\nvault_env_templates: [tpl.env]\nhooks:\n  post_deploy: [true]\n")
	// Both separator spellings; the KEY names live in the template.
	writeFile(t, filepath.Join(stackDir, "tpl.env"),
		"DB_PASSWORD=${VAULT:hosts/host-01/traefik#db_password}\nAPI_KEY=${VAULT:hosts/host-01/traefik:spare}\n")

	if err := f.reconciler().Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if len(f.compose.ups) != 1 {
		t.Fatalf("Ups = %d, want 1", len(f.compose.ups))
	}
	upEnv := f.compose.ups[0].env
	for k, want := range map[string]string{
		"DB_PASSWORD": "pg-secret",
		"API_KEY":     "unused",
	} {
		if got := upEnv[k]; got != want {
			t.Errorf("env file[%s] = %q, want %q (template rendered into the stack environment)", k, got, want)
		}
	}
	// The vault login happened although the stack has no vault_paths — the
	// templates resolve through the same client — and each distinct path
	// was read exactly once.
	if f.vault.client.logins != 1 {
		t.Errorf("vault logins = %d, want 1", f.vault.client.logins)
	}
	if !reflect.DeepEqual(f.vault.client.reads, []string{"hosts/host-01/traefik"}) {
		t.Errorf("vault reads = %v, want [hosts/host-01/traefik]", f.vault.client.reads)
	}
}

func TestReconcileVaultEnvTemplateFailureFailsStack(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.vault.client.kv = map[string]map[string]string{
		"hosts/host-01/traefik": {"db_password": "pg-secret"},
	}
	stackDir := filepath.Join(f.workDir, "host", "web")
	f.addLocalStack("web", "name: web\nvault_env_templates: [tpl.env]\n")
	writeFile(t, filepath.Join(stackDir, "tpl.env"), "MY_VAR=${VAULT:hosts/host-01/traefik#missing}\n")

	err := f.reconciler().Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "vault_env_templates[0]") || !strings.Contains(err.Error(), "tpl.env") {
		t.Fatalf("Reconcile() error = %v, want template failure with file context", err)
	}
	if got := f.upNames(); len(got) != 0 {
		t.Errorf("Ups = %v, want none (unresolvable template must not deploy)", got)
	}
	if st, _ := f.state.Get("web"); st.Status != state.StatusFailed {
		t.Errorf("state[web] = %+v, want failed", st)
	}
}

// TestReconcileSopsSidecar drives sops_files through the sidecar resolver:
// Deps.SopsSidecarURL set and no injected Sops → decryption goes over HTTP
// against the fake sidecar, which must receive the resolved absolute path.
func TestReconcileSopsSidecar(t *testing.T) {
	f := newFixture(t, defaultRoot)
	stackDir := filepath.Join(f.workDir, "host", "web")

	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/decrypt" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		gotPaths = append(gotPaths, req.Path)
		w.Write([]byte(`{"content":"SECRET=sidecar-value\n"}`))
	}))
	t.Cleanup(srv.Close)

	f.addLocalStack("web", "name: web\nsops_files:\n  - path: secrets.env.sops\n")
	writeFile(t, filepath.Join(stackDir, "secrets.env.sops"), "ENCRYPTED\n")

	r := New(Deps{
		RepoURL:        "https://example.invalid/host.git",
		Branch:         "main",
		WorkDir:        f.workDir,
		State:          f.state,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Git:            f.git,
		Compose:        f.compose,
		Hooks:          f.hooks,
		SopsSidecarURL: srv.URL,
		NewVault:       f.vault.make,
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if want := []string{filepath.Join(stackDir, "secrets.env.sops")}; !reflect.DeepEqual(gotPaths, want) {
		t.Errorf("sidecar received paths = %v, want %v (resolved absolute path)", gotPaths, want)
	}
	if len(f.compose.ups) != 1 {
		t.Fatalf("Ups = %d, want 1", len(f.compose.ups))
	}
	if got := f.compose.ups[0].env["SECRET"]; got != "sidecar-value" {
		t.Errorf("env file[SECRET] = %q, want sidecar-value (decrypted via the sidecar)", got)
	}
}

// TestReconcileInjectedSopsWinsOverSidecarURL pins the Deps precedence: an
// injected Sops function (tests, special hosts) is never re-routed through
// the sidecar URL.
func TestReconcileInjectedSopsWinsOverSidecarURL(t *testing.T) {
	f := newFixture(t, defaultRoot)
	stackDir := filepath.Join(f.workDir, "host", "web")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("sidecar was contacted although Sops was injected")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	f.sops.kv = map[string]map[string]string{
		filepath.Join(stackDir, "enc.env"): {"SECRET": "local-value"},
	}
	f.addLocalStack("web", "name: web\nsops_files:\n  - path: enc.env\n")

	r := New(Deps{
		RepoURL:        "https://example.invalid/host.git",
		Branch:         "main",
		WorkDir:        f.workDir,
		State:          f.state,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Git:            f.git,
		Compose:        f.compose,
		Hooks:          f.hooks,
		Sops:           f.sops.decrypt,
		SopsSidecarURL: srv.URL,
		NewVault:       f.vault.make,
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := f.compose.ups[0].env["SECRET"]; got != "local-value" {
		t.Errorf("env file[SECRET] = %q, want local-value (injected decrypt wins)", got)
	}
}

func TestReconcileVaultLoginFailureFailsAllVaultStacks(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.vault.client.failLogin = true
	f.addLocalStack("web", "name: web\nvault_paths: [kv/web]\n")
	f.addLocalStack("db", "name: db\nvault_paths: [kv/db]\n")
	f.addLocalStack("plain", "name: plain\n")

	err := f.reconciler().Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile() error = nil, want vault failures")
	}
	for _, stack := range []string{"web", "db"} {
		if !strings.Contains(err.Error(), "stack "+stack+": vault:") {
			t.Errorf("error %q misses vault failure for %s", err, stack)
		}
	}
	// One login attempt per pass, not one per stack.
	if f.vault.client.logins != 1 {
		t.Errorf("vault logins = %d, want 1", f.vault.client.logins)
	}
	// The stack without vault paths deploys anyway.
	if got, want := f.upNames(), []string{"plain"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Ups = %v, want %v", got, want)
	}
}

func TestReconcilePrune(t *testing.T) {
	seed := func(f *fixture, name, stackDir, composeFile string) {
		f.t.Helper()
		if err := f.state.Set(name, state.State{
			Hash: "h-" + name, Status: state.StatusDeployed,
			StackDir: stackDir, ComposeFile: composeFile,
		}); err != nil {
			f.t.Fatalf("state.Set(%s) error = %v", name, err)
		}
	}

	t.Run("vanished directory falls back to down without -f", func(t *testing.T) {
		f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
		f.addLocalStack("web", "name: web\n")
		gone := filepath.Join(f.workDir, "gone")
		seed(f, "gone", gone, filepath.Join(gone, "compose.yaml"))

		if err := f.reconciler().Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if !reflect.DeepEqual(f.compose.downProjects, []string{"gone"}) {
			t.Errorf("DownProject calls = %v, want [gone]", f.compose.downProjects)
		}
		if len(f.compose.downs) != 0 {
			t.Errorf("Down calls = %d, want 0", len(f.compose.downs))
		}
		if _, ok := f.state.Get("gone"); ok {
			t.Error("state entry for gone still exists after prune")
		}
		if st, _ := f.state.Get("web"); st.Status != state.StatusDeployed {
			t.Errorf("state[web] = %+v, want deployed", st)
		}
	})

	t.Run("existing compose file tears down with -f", func(t *testing.T) {
		f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
		f.addLocalStack("web", "name: web\n")
		gone := filepath.Join(f.workDir, "gone")
		composeFile := filepath.Join(gone, "compose.yaml")
		writeFile(t, composeFile, "services:\n  old:\n    image: nginx\n")
		seed(f, "gone", gone, composeFile)

		if err := f.reconciler().Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if len(f.compose.downs) != 1 {
			t.Fatalf("Down calls = %d, want 1", len(f.compose.downs))
		}
		down := f.compose.downs[0]
		if down.stack.Name != "gone" || down.stack.ComposeFile != composeFile || down.stackDir != gone {
			t.Errorf("Down call = %+v, want gone with persisted compose file and dir", down)
		}
		if !down.envExists {
			t.Errorf("Down env file %q did not exist", down.envFile)
		}
		if len(f.compose.downProjects) != 0 {
			t.Errorf("DownProject calls = %v, want none", f.compose.downProjects)
		}
		if _, ok := f.state.Get("gone"); ok {
			t.Error("state entry for gone still exists after prune")
		}
	})

	t.Run("failed teardown keeps the state entry", func(t *testing.T) {
		f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
		f.addLocalStack("web", "name: web\n")
		f.compose.failDownProjectAt = "gone"
		seed(f, "gone", filepath.Join(f.workDir, "gone"), filepath.Join(f.workDir, "gone", "compose.yaml"))

		err := f.reconciler().Reconcile(context.Background())
		if err == nil || !strings.Contains(err.Error(), "prune gone") {
			t.Fatalf("Reconcile() error = %v, want prune failure for gone", err)
		}
		if _, ok := f.state.Get("gone"); !ok {
			t.Error("state entry for gone was removed despite failed teardown")
		}
	})

	t.Run("disabled stacks exist and are not pruned", func(t *testing.T) {
		f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
		f.addLocalStack("off", "name: off\nenabled: false\n")
		f.addLocalStack("web", "name: web\n")
		seed(f, "off", "", "")

		if err := f.reconciler().Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if _, ok := f.state.Get("off"); !ok {
			t.Error("state entry of the disabled stack was pruned")
		}
		if got := f.upNames(); !reflect.DeepEqual(got, []string{"web"}) {
			t.Errorf("Ups = %v, want [web] (disabled stacks are not deployed)", got)
		}
	})

	t.Run("prune disabled keeps entries", func(t *testing.T) {
		f := newFixture(t, defaultRoot) // prune_removed_stacks defaults to false
		f.addLocalStack("web", "name: web\n")
		seed(f, "gone", filepath.Join(f.workDir, "gone"), filepath.Join(f.workDir, "gone", "compose.yaml"))

		if err := f.reconciler().Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if _, ok := f.state.Get("gone"); !ok {
			t.Error("state entry for gone was pruned although pruning is disabled")
		}
		if len(f.compose.downProjects) != 0 || len(f.compose.downs) != 0 {
			t.Error("teardown ran although pruning is disabled")
		}
	})
}

func TestReconcilePruneMultiFileStack(t *testing.T) {
	seed := func(f *fixture, stackDir string, files []string) {
		f.t.Helper()
		if err := f.state.Set("gone", state.State{
			Hash: "h-gone", Status: state.StatusDeployed,
			StackDir: stackDir, ComposeFiles: files,
		}); err != nil {
			f.t.Fatalf("state.Set(gone) error = %v", err)
		}
	}

	t.Run("all files exist tears down with every -f", func(t *testing.T) {
		f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
		f.addLocalStack("web", "name: web\n")
		gone := filepath.Join(f.workDir, "gone")
		files := []string{filepath.Join(gone, "compose.yaml"), filepath.Join(gone, "override.yaml")}
		for _, file := range files {
			writeFile(t, file, "services:\n  old:\n    image: nginx\n")
		}
		seed(f, gone, files)

		if err := f.reconciler().Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if len(f.compose.downs) != 1 {
			t.Fatalf("Down calls = %d, want 1", len(f.compose.downs))
		}
		if got := f.compose.downs[0].stack.ComposeFiles; !reflect.DeepEqual(got, files) {
			t.Errorf("Down stack.ComposeFiles = %v, want %v (teardown needs the full merged config)", got, files)
		}
		if len(f.compose.downProjects) != 0 {
			t.Errorf("DownProject calls = %v, want none", f.compose.downProjects)
		}
	})

	t.Run("one file missing falls back to down without -f", func(t *testing.T) {
		f := newFixture(t, "global:\n  work_dir: WORKDIR\n  prune_removed_stacks: true\n")
		f.addLocalStack("web", "name: web\n")
		gone := filepath.Join(f.workDir, "gone")
		writeFile(t, filepath.Join(gone, "compose.yaml"), "services:\n  old:\n    image: nginx\n")
		// override.yaml recorded but not on disk anymore.
		seed(f, gone, []string{filepath.Join(gone, "compose.yaml"), filepath.Join(gone, "override.yaml")})

		if err := f.reconciler().Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if !reflect.DeepEqual(f.compose.downProjects, []string{"gone"}) {
			t.Errorf("DownProject calls = %v, want [gone]", f.compose.downProjects)
		}
		if len(f.compose.downs) != 0 {
			t.Errorf("Down calls = %d, want 0 (incomplete file set)", len(f.compose.downs))
		}
	})
}

func TestReconcileImportsDeployWithRemoteCommit(t *testing.T) {
	f := newFixture(t, `global:
  work_dir: WORKDIR
remotes:
  origin1:
    url: https://example.invalid/origin1.git
imports:
  - remote: origin1
    path: svc
    name: imported
`)
	f.git.commits = map[string]string{
		filepath.Join(f.workDir, "host"):    "host123",
		filepath.Join(f.workDir, "origin1"): "remote789",
	}
	f.addRemoteStack("origin1", "svc", "name: base\nhooks:\n  post_deploy: [true]\n")

	if err := f.reconciler().Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if want := []string{"host"}; !reflect.DeepEqual(f.git.ensured, want) {
		t.Errorf("EnsureRepo calls = %v, want %v (bootstrap by name)", f.git.ensured, want)
	}
	if want := []string{filepath.Join(f.workDir, "origin1")}; !reflect.DeepEqual(f.git.ensuredAt, want) {
		t.Errorf("EnsureRepoAt calls = %v, want %v (remote without checkout below work_dir)", f.git.ensuredAt, want)
	}
	if got := f.upNames(); !reflect.DeepEqual(got, []string{"imported"}) {
		t.Fatalf("Ups = %v, want [imported] (import name overrides base name)", got)
	}
	if len(f.hooks.calls) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(f.hooks.calls))
	}
	env := f.hooks.calls[0].env
	if !contains(env, "GIT_COMMIT=remote789") {
		t.Errorf("hook env %v misses GIT_COMMIT of the remote checkout", env)
	}
	if !contains(env, "STACK_NAME=imported") || !contains(env, "STACK_DIR="+filepath.Join(f.workDir, "origin1", "svc")) {
		t.Errorf("hook env %v misses stack identity of the import", env)
	}
}

// TestReconcileConfiguredCheckoutPaths pins the configurable checkout paths
// (global.host_checkout, remotes.<name>.checkout): the pass bootstraps below
// the work dir by name, then ensures the configured host and remote
// checkouts at their absolute paths, reads the full configuration from the
// configured host checkout (local stacks included) and deploys every stack
// where it actually lives.
func TestReconcileConfiguredCheckoutPaths(t *testing.T) {
	base := t.TempDir()
	hostCheckout := filepath.Join(base, "apps")
	infraCheckout := filepath.Join(base, "infrastructure")

	// The adopted clones carry the same root config as the bootstrap
	// checkout (in reality all of them are clones of the same repository).
	rootYAML := fmt.Sprintf(`global:
  work_dir: WORKDIR
  host_checkout: %s
remotes:
  infra:
    url: https://example.invalid/infra.git
    checkout: %s
imports:
  - remote: infra
    path: svc
    name: imported
`, hostCheckout, infraCheckout)
	f := newFixture(t, rootYAML)
	writeFile(t, filepath.Join(hostCheckout, rootConfigFile),
		strings.ReplaceAll(rootYAML, "WORKDIR", f.workDir))

	// Stacks live below the CONFIGURED paths, not below the work dir.
	writeFile(t, filepath.Join(hostCheckout, "web", "stack.yaml"), "name: web\n")
	writeFile(t, filepath.Join(hostCheckout, "web", "compose.yaml"), "services:\n  app:\n    image: nginx:latest\n")
	writeFile(t, filepath.Join(infraCheckout, "svc", "stack.yaml"), "name: base\n")
	writeFile(t, filepath.Join(infraCheckout, "svc", "compose.yaml"), "services:\n  app:\n    image: nginx:latest\n")

	f.git.commits = map[string]string{
		hostCheckout:  "host123",
		infraCheckout: "remote789",
	}

	if err := f.reconciler().Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// Bootstrap by name only; both configured paths go through EnsureRepoAt.
	if want := []string{"host"}; !reflect.DeepEqual(f.git.ensured, want) {
		t.Errorf("EnsureRepo calls = %v, want %v (bootstrap)", f.git.ensured, want)
	}
	if want := []string{hostCheckout, infraCheckout}; !reflect.DeepEqual(f.git.ensuredAt, want) {
		t.Errorf("EnsureRepoAt calls = %v, want %v (configured host and remote paths)", f.git.ensuredAt, want)
	}

	// Local stacks are discovered in the configured host checkout, the
	// import in the configured remote checkout.
	if got, want := f.upNames(), []string{"web", "imported"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Ups = %v, want %v", got, want)
	}
	if got, want := f.compose.ups[0].stackDir, filepath.Join(hostCheckout, "web"); got != want {
		t.Errorf("web stackDir = %q, want %q (deployed from the configured host checkout)", got, want)
	}
	if got := f.compose.ups[0].env["GIT_COMMIT"]; got != "host123" {
		t.Errorf("web GIT_COMMIT = %q, want the configured host checkout's commit", got)
	}
	if got, want := f.compose.ups[1].stackDir, filepath.Join(infraCheckout, "svc"); got != want {
		t.Errorf("imported stackDir = %q, want %q (deployed from the configured remote checkout)", got, want)
	}
	if got := f.compose.ups[1].env["GIT_COMMIT"]; got != "remote789" {
		t.Errorf("imported GIT_COMMIT = %q, want the configured remote checkout's commit", got)
	}

	// State records the configured locations.
	st, ok := f.state.Get("web")
	if !ok {
		t.Fatal("state has no entry for web")
	}
	if st.StackDir != filepath.Join(hostCheckout, "web") {
		t.Errorf("state[web].StackDir = %q, want the configured host checkout", st.StackDir)
	}
}

// TestReconcileRemotesBelowConfiguredWorkDir replaces the old warn path: a
// global.work_dir differing from the bootstrap dir no longer needs a second
// git manager — the remote is simply ensured at <work_dir>/<name> via
// EnsureRepoAt on the bootstrap manager.
func TestReconcileRemotesBelowConfiguredWorkDir(t *testing.T) {
	otherWork := filepath.Join(t.TempDir(), "other-work")
	f := newFixture(t, fmt.Sprintf(`global:
  work_dir: %s
remotes:
  infra:
    url: https://example.invalid/infra.git
imports:
  - remote: infra
    path: svc
    name: imported
`, otherWork))
	writeFile(t, filepath.Join(otherWork, "infra", "svc", "stack.yaml"), "name: base\n")
	writeFile(t, filepath.Join(otherWork, "infra", "svc", "compose.yaml"), "services:\n  app:\n    image: nginx:latest\n")

	if err := f.reconciler().Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if want := []string{filepath.Join(otherWork, "infra")}; !reflect.DeepEqual(f.git.ensuredAt, want) {
		t.Errorf("EnsureRepoAt calls = %v, want %v (remote below the configured work dir)", f.git.ensuredAt, want)
	}
	if got := f.upNames(); !reflect.DeepEqual(got, []string{"imported"}) {
		t.Errorf("Ups = %v, want the import below the configured work dir deployed", got)
	}
}

func TestReconcileContextCancelled(t *testing.T) {
	f := newFixture(t, defaultRoot)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := f.reconciler().Reconcile(ctx)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Reconcile() with cancelled context error = %v, want context error", err)
	}
}

func TestReconcileValidatesDeps(t *testing.T) {
	if err := New(Deps{}).Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "repo url") {
		t.Errorf("Reconcile() without repo error = %v, want repo url error", err)
	}
	err := New(Deps{RepoURL: "https://example.invalid/host.git"}).Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "branch") {
		t.Errorf("Reconcile() without branch error = %v, want branch error", err)
	}
	err = New(Deps{RepoURL: "u", Branch: "main"}).Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "state store") {
		t.Errorf("Reconcile() without state error = %v, want state store error", err)
	}
}

func TestReconcileHostRepoFailureAbortsPass(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")
	f.git.failAt = "host"

	err := f.reconciler().Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "host repo") {
		t.Fatalf("Reconcile() error = %v, want host repo failure", err)
	}
	if len(f.compose.ups) != 0 {
		t.Error("a stack was deployed although the host repo failed")
	}
	if names := f.state.Names(); len(names) != 0 {
		t.Errorf("state entries = %v, want none", names)
	}
}

// contains reports whether list has want as an element.
func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ---- integration: the real git manager, fakes only for docker ----

// runGit runs git in dir and fails the test on error, returning the output.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir %s): %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// TestReconcileWithRealGitManager drives one full pass with the real git
// manager (clone, fetch/reset, rev-parse) and the real config loader; only
// docker compose is faked. It also proves the loop reacts to a new commit.
func TestReconcileWithRealGitManager(t *testing.T) {
	// Commit identity for the origin we create (no global git config
	// assumed); the manager's own git children inherit it harmlessly.
	t.Setenv("GIT_AUTHOR_NAME", "dockops test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@dockops.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "dockops test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@dockops.invalid")

	workDir := t.TempDir()
	origin := filepath.Join(t.TempDir(), "origin-host")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", origin, err)
	}
	runGit(t, origin, "init", "-b", "main")
	writeFile(t, filepath.Join(origin, rootConfigFile), "global:\n  work_dir: "+workDir+"\n")
	writeFile(t, filepath.Join(origin, "web", "stack.yaml"), "name: web\n")
	writeFile(t, filepath.Join(origin, "web", "compose.yaml"), "services:\n  app:\n    image: nginx:1\n")
	runGit(t, origin, "add", ".")
	runGit(t, origin, "commit", "-m", "initial")
	commit := strings.TrimSpace(runGit(t, origin, "rev-parse", "HEAD"))

	store, err := state.New(filepath.Join(t.TempDir(), "state.json"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	f := &fixture{t: t, workDir: workDir, compose: &fakeCompose{}}
	r := New(Deps{
		RepoURL: origin,
		Branch:  "main",
		WorkDir: workDir,
		State:   store,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Compose: f.compose, // everything real except docker compose
	})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if got := f.upNames(); !reflect.DeepEqual(got, []string{"web"}) {
		t.Fatalf("Ups = %v, want [web]", got)
	}
	if got := f.compose.ups[0].env["GIT_COMMIT"]; got != commit {
		t.Errorf("GIT_COMMIT = %q, want the origin's commit %q", got, commit)
	}
	if st, _ := store.Get("web"); st.Status != state.StatusDeployed {
		t.Errorf("state[web] = %+v, want deployed", st)
	}

	// Unchanged repository: second pass skips.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if got := f.upNames(); !reflect.DeepEqual(got, []string{"web"}) {
		t.Errorf("Ups = %v, want [web] (unchanged commit skips)", got)
	}

	// A new commit changes the hash and triggers a redeploy.
	writeFile(t, filepath.Join(origin, "web", "compose.yaml"), "services:\n  app:\n    image: nginx:2\n")
	runGit(t, origin, "add", ".")
	runGit(t, origin, "commit", "-m", "bump image")
	newCommit := strings.TrimSpace(runGit(t, origin, "rev-parse", "HEAD"))

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("third Reconcile() error = %v", err)
	}
	if got := f.upNames(); !reflect.DeepEqual(got, []string{"web", "web"}) {
		t.Errorf("Ups = %v, want two deploys after a new commit", got)
	}
	if got := f.compose.ups[1].env["GIT_COMMIT"]; got != newCommit {
		t.Errorf("GIT_COMMIT = %q, want the new commit %q", got, newCommit)
	}
}
