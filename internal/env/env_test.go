package env

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/dotenv"
	"github.com/local/dockops/internal/secrets"
)

// fakeResolver implements secrets.Resolver with canned results and records
// what it was asked to resolve.
type fakeResolver struct {
	sops       map[string]map[string]string // resolved sops path -> values
	vault      map[string]map[string]string // vault path -> values
	sopsErrAt  string                       // resolve this sops path with an error
	vaultErrAt string                       // resolve this vault path with an error
	gotSops    []config.SopsFile
	gotVault   []string
}

func (f *fakeResolver) ResolveSops(_ context.Context, file config.SopsFile) (map[string]string, error) {
	f.gotSops = append(f.gotSops, file)
	if f.sopsErrAt == file.Path {
		return nil, fmt.Errorf("sops boom: %s", file.Path)
	}
	kv, ok := f.sops[file.Path]
	if !ok {
		return nil, fmt.Errorf("unexpected sops path %q", file.Path)
	}
	return kv, nil
}

func (f *fakeResolver) ResolveVault(_ context.Context, path string) (map[string]string, error) {
	f.gotVault = append(f.gotVault, path)
	if f.vaultErrAt == path {
		return nil, fmt.Errorf("vault boom: %s", path)
	}
	kv, ok := f.vault[path]
	if !ok {
		return nil, fmt.Errorf("unexpected vault path %q", path)
	}
	return kv, nil
}

// compile-time assertion that the fake satisfies the interface it tests.
var _ secrets.Resolver = (*fakeResolver)(nil)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestMergeStackEnvPriority(t *testing.T) {
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "base.env"), "SHARED=env-base\nONLY_ENV=1\nSTACK_NAME=hacked\n")
	writeFile(t, filepath.Join(baseDir, "import.env"), "SHARED=env-import\n")

	resolver := &fakeResolver{
		sops: map[string]map[string]string{
			filepath.Join(baseDir, "secrets.env"): {"SHARED": "sops", "ONLY_SOPS": "s"},
		},
		vault: map[string]map[string]string{
			"base/stack": {"SHARED": "vault-base", "ONLY_VAULT": "v"},
			"import/x":   {"SHARED": "vault-import"},
		},
	}
	stack := config.Stack{
		Name:       "web",
		Dir:        baseDir,
		EnvFiles:   []string{"base.env", "import.env"}, // second file wins over first
		SopsFiles:  []config.SopsFile{{Path: "secrets.env"}},
		VaultPaths: []string{"base/stack", "import/x"}, // import path wins over base path
	}

	got, err := MergeStackEnv(context.Background(), stack, resolver, baseDir, SystemVars{GitCommit: "abc123"})
	if err != nil {
		t.Fatalf("MergeStackEnv() error = %v", err)
	}

	want := map[string]string{
		"SHARED":     "vault-import", // vault beats sops beats env_files; later vault path wins
		"ONLY_ENV":   "1",
		"ONLY_SOPS":  "s",
		"ONLY_VAULT": "v",
		"STACK_NAME": "web", // system vars cannot be overridden by any source
		"STACK_DIR":  baseDir,
		"GIT_COMMIT": "abc123",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeStackEnv() = %v, want %v", got, want)
	}
}

func TestMergeStackEnvResolvesRelativePaths(t *testing.T) {
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "plain.env"), "A=1\n")
	resolver := &fakeResolver{
		sops: map[string]map[string]string{
			filepath.Join(baseDir, "sub", "enc.env"): {"B": "2"},
		},
		vault: map[string]map[string]string{
			"kv/web": {},
		},
	}
	stack := config.Stack{
		Name:       "web",
		Dir:        baseDir,
		EnvFiles:   []string{"plain.env"},
		SopsFiles:  []config.SopsFile{{Path: "sub/enc.env", AgeKeyfile: "keys/age.key"}},
		VaultPaths: []string{"kv/web"},
	}

	got, err := MergeStackEnv(context.Background(), stack, resolver, baseDir, SystemVars{})
	if err != nil {
		t.Fatalf("MergeStackEnv() error = %v", err)
	}
	if got["B"] != "2" {
		t.Errorf("B = %q, want 2", got["B"])
	}
	if len(resolver.gotSops) != 1 {
		t.Fatalf("ResolveSops called %d times, want 1", len(resolver.gotSops))
	}
	if gotPath := resolver.gotSops[0].Path; gotPath != filepath.Join(baseDir, "sub", "enc.env") {
		t.Errorf("ResolveSops path = %q, want resolved absolute path", gotPath)
	}
	if gotKey := resolver.gotSops[0].AgeKeyfile; gotKey != "keys/age.key" {
		t.Errorf("AgeKeyfile = %q, want passed through unchanged", gotKey)
	}
	if !reflect.DeepEqual(resolver.gotVault, []string{"kv/web"}) {
		t.Errorf("ResolveVault calls = %v, want [kv/web]", resolver.gotVault)
	}
}

func TestMergeStackEnvSystemVarsFallback(t *testing.T) {
	stack := config.Stack{Name: "web", Dir: "/srv/stacks/web"}
	got, err := MergeStackEnv(context.Background(), stack, &fakeResolver{}, t.TempDir(), SystemVars{})
	if err != nil {
		t.Fatalf("MergeStackEnv() error = %v", err)
	}
	if got["STACK_NAME"] != "web" || got["STACK_DIR"] != "/srv/stacks/web" || got["GIT_COMMIT"] != "" {
		t.Errorf("system vars = %q/%q/%q, want stack fallbacks", got["STACK_NAME"], got["STACK_DIR"], got["GIT_COMMIT"])
	}
}

func TestMergeStackEnvSystemVarsExplicit(t *testing.T) {
	stack := config.Stack{Name: "base-name", Dir: "/old"}
	got, err := MergeStackEnv(context.Background(), stack, &fakeResolver{}, t.TempDir(),
		SystemVars{StackName: "override", StackDir: "/new", GitCommit: "deadbeef"})
	if err != nil {
		t.Fatalf("MergeStackEnv() error = %v", err)
	}
	if got["STACK_NAME"] != "override" || got["STACK_DIR"] != "/new" || got["GIT_COMMIT"] != "deadbeef" {
		t.Errorf("system vars = %q/%q/%q, want explicit values", got["STACK_NAME"], got["STACK_DIR"], got["GIT_COMMIT"])
	}
}

func TestMergeStackEnvMissingEnvFile(t *testing.T) {
	stack := config.Stack{Name: "web", EnvFiles: []string{"nope.env"}}
	_, err := MergeStackEnv(context.Background(), stack, &fakeResolver{}, t.TempDir(), SystemVars{})
	if err == nil || !strings.Contains(err.Error(), "env_files[0]") {
		t.Errorf("MergeStackEnv() with missing file error = %v, want env_files[0] context", err)
	}
}

func TestMergeStackEnvSkipsStackDirDotEnv(t *testing.T) {
	baseDir := t.TempDir()
	// A stale generated .env from a previous deployment (or one committed to
	// the repository) must never be read back as a source.
	writeFile(t, filepath.Join(baseDir, ".env"), "STALE=old-secret\nKEEP_OUT=nope\n")
	writeFile(t, filepath.Join(baseDir, "real.env"), "REAL=1\n")

	stack := config.Stack{
		Name:     "web",
		EnvFiles: []string{".env", "./.env", "real.env", filepath.Join(baseDir, ".env")},
	}
	got, err := MergeStackEnv(context.Background(), stack, &fakeResolver{}, baseDir, SystemVars{})
	if err != nil {
		t.Fatalf("MergeStackEnv() error = %v", err)
	}
	for _, stale := range []string{"STALE", "KEEP_OUT"} {
		if v, ok := got[stale]; ok && v != "" {
			t.Errorf("merged env[%s] = %q, want absent (<stackDir>/.env must be skipped in every spelling)", stale, v)
		}
	}
	if got["REAL"] != "1" {
		t.Errorf("merged env[REAL] = %q, want 1 (regular env_files are still read)", got["REAL"])
	}
}

func TestMergeStackEnvResolverErrors(t *testing.T) {
	baseDir := t.TempDir()
	resolver := &fakeResolver{
		sops:       map[string]map[string]string{filepath.Join(baseDir, "a.env"): {}},
		vault:      map[string]map[string]string{"ok": {}},
		sopsErrAt:  filepath.Join(baseDir, "bad.env"),
		vaultErrAt: "boom",
	}
	ctx := context.Background()

	_, err := MergeStackEnv(ctx, config.Stack{Name: "web", SopsFiles: []config.SopsFile{{Path: "bad.env"}}}, resolver, baseDir, SystemVars{})
	if err == nil || !strings.Contains(err.Error(), "sops_files[0]") || !strings.Contains(err.Error(), "sops boom") {
		t.Errorf("sops resolver error = %v, want sops_files[0] context with cause", err)
	}

	_, err = MergeStackEnv(ctx, config.Stack{Name: "web", VaultPaths: []string{"ok", "boom"}}, resolver, baseDir, SystemVars{})
	if err == nil || !strings.Contains(err.Error(), "vault_paths[1]") || !strings.Contains(err.Error(), "vault boom") {
		t.Errorf("vault resolver error = %v, want vault_paths[1] context with cause", err)
	}
}

func TestWriteShm(t *testing.T) {
	// /dev/shm is not guaranteed on every host (containers without tmpfs
	// mount); the fallback has its own test.
	if _, err := os.Stat(shmDir); err != nil {
		t.Skipf("%s not available: %v", shmDir, err)
	}
	env := map[string]string{"SECRET": "hunter2", "A": "1"}

	path, cleanup, err := WriteShm(env)
	if err != nil {
		t.Fatalf("WriteShm() error = %v", err)
	}
	if dir := filepath.Dir(path); dir != shmDir {
		t.Errorf("file written to %s, want %s", dir, shmDir)
	}
	if matched, _ := regexp.MatchString(`^dockops-[0-9a-f]{8}-[0-9a-f]{12}\.env$`, filepath.Base(path)); !matched {
		t.Errorf("file name %q does not match dockops-<random>-<hash>.env", filepath.Base(path))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got := dotenv.Parse(raw); !reflect.DeepEqual(got, env) {
		t.Errorf("content = %v, want %v", got, env)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still exists after cleanup: %v", err)
	}
	cleanup() // idempotent: must not fail or re-remove anything
}

func TestWriteShmFallbackToTempDir(t *testing.T) {
	// Simulate a host without /dev/shm and capture the warning log.
	var buf bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(oldDefault)

	oldShm := shmDir
	shmDir = filepath.Join(t.TempDir(), "does-not-exist")
	defer func() { shmDir = oldShm }()

	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	path, cleanup, err := WriteShm(map[string]string{"A": "1"})
	if err != nil {
		t.Fatalf("WriteShm() error = %v", err)
	}
	if dir := filepath.Dir(path); dir != tmp {
		t.Errorf("file written to %s, want TMPDIR %s", dir, tmp)
	}
	if !strings.Contains(buf.String(), "falling back to temp dir") {
		t.Errorf("warning log = %q, want fallback mention", buf.String())
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still exists after cleanup: %v", err)
	}
}

func TestWriteShmEmptyEnv(t *testing.T) {
	oldShm := shmDir
	shmDir = t.TempDir()
	defer func() { shmDir = oldShm }()

	path, cleanup, err := WriteShm(map[string]string{})
	if err != nil {
		t.Fatalf("WriteShm() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
	cleanup()
}

func TestWriteStackEnv(t *testing.T) {
	stackDir := t.TempDir()

	path, rollback, err := WriteStackEnv(stackDir, map[string]string{"SECRET": "hunter2", "A": "1"}, Options{})
	if err != nil {
		t.Fatalf("WriteStackEnv() error = %v", err)
	}
	if want := filepath.Join(stackDir, ".env"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}

	// Deterministic content: sorted keys, plain KEY=value lines.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if want := "A=1\nSECRET=hunter2\n"; string(raw) != want {
		t.Errorf("content = %q, want %q", raw, want)
	}

	// Atomic write: no temporary files are left behind in the stack dir.
	entries, err := os.ReadDir(stackDir)
	if err != nil {
		t.Fatalf("read dir %s: %v", stackDir, err)
	}
	if len(entries) != 1 || entries[0].Name() != ".env" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("stack dir contains %v, want exactly [.env] (no temp leftovers)", names)
	}

	// Success case: rollback exists but is not called — the file stays. Its
	// no-op on success is verified by not invoking it here; the removal and
	// restore paths have their own tests.
	_ = rollback
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file must stay after a successful write: %v", err)
	}
}

func TestWriteStackEnvRollbackRemovesNewFile(t *testing.T) {
	stackDir := t.TempDir()

	path, rollback, err := WriteStackEnv(stackDir, map[string]string{"A": "1"}, Options{})
	if err != nil {
		t.Fatalf("WriteStackEnv() error = %v", err)
	}
	rollback()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still exists after rollback: %v", err)
	}
	rollback() // idempotent
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("second rollback changed anything: %v", err)
	}
}

func TestWriteStackEnvRollbackRestoresPreviousFile(t *testing.T) {
	stackDir := t.TempDir()
	path := filepath.Join(stackDir, ".env")
	if err := os.WriteFile(path, []byte("OLD=previous\n"), 0o644); err != nil {
		t.Fatalf("write previous .env: %v", err)
	}
	// Chmod (not the WriteFile mode) fixes the permission regardless of the
	// process umask, so the assertion below is host-independent.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod previous .env: %v", err)
	}

	gotPath, rollback, err := WriteStackEnv(stackDir, map[string]string{"NEW": "value"}, Options{})
	if err != nil {
		t.Fatalf("WriteStackEnv() error = %v", err)
	}
	if gotPath != path {
		t.Fatalf("path = %q, want %q", gotPath, path)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "NEW=value\n" {
		t.Fatalf("content before rollback = %q, want the new content", raw)
	}

	rollback()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored %s: %v", path, err)
	}
	if want := "OLD=previous\n"; string(raw) != want {
		t.Errorf("restored content = %q, want %q", raw, want)
	}
	// The previous permissions are restored with the content.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("restored permissions = %o, want 644", perm)
	}
	rollback() // idempotent: still the restored state
	if raw, _ := os.ReadFile(path); string(raw) != "OLD=previous\n" {
		t.Errorf("second rollback changed the content: %q", raw)
	}
}

func TestWriteStackEnvMissingDir(t *testing.T) {
	_, _, err := WriteStackEnv(filepath.Join(t.TempDir(), "does-not-exist"), map[string]string{"A": "1"}, Options{})
	if err == nil || !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("WriteStackEnv() into missing dir error = %v, want error naming the dir", err)
	}
}

func TestWriteStackEnvEmptyEnv(t *testing.T) {
	stackDir := t.TempDir()
	path, rollback, err := WriteStackEnv(stackDir, map[string]string{}, Options{})
	if err != nil {
		t.Fatalf("WriteStackEnv() error = %v", err)
	}
	if raw, _ := os.ReadFile(path); len(raw) != 0 {
		t.Errorf("content = %q, want empty", raw)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("permissions = %v, %v; want 600", info, err)
	}
	rollback()
}
