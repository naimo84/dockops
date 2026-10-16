package git

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestMain isolates all git invocations from the user's global and system
// git config (insteadOf rewrites, signing, default branch, ...) and provides
// a commit identity, so tests behave the same on every machine. This
// environment is inherited by both the test helper below and the git
// commands spawned by Manager.
func TestMain(m *testing.M) {
	for _, kv := range []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=dockops test",
		"GIT_AUTHOR_EMAIL=test@dockops.invalid",
		"GIT_COMMITTER_NAME=dockops test",
		"GIT_COMMITTER_EMAIL=test@dockops.invalid",
	} {
		os.Setenv(strings.SplitN(kv, "=", 2)[0], strings.SplitN(kv, "=", 2)[1])
	}
	os.Exit(m.Run())
}

// runGit runs git in dir and fails the test on error, returning the output.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir %s): %v\n%s", args, dir, err, out)
	}
	return string(out)
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

// newManager creates a Manager with a fresh, initially non-existent work
// directory (also proving that New creates it).
func newManager(t *testing.T) (*Manager, string) {
	t.Helper()
	workDir := filepath.Join(t.TempDir(), "repos")
	m, err := New(workDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return m, workDir
}

// newOrigin creates a git repository at <tmp>/origin with one commit of the
// given files on the given branch and returns its path.
func newOrigin(t *testing.T, branch string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	runGit(t, dir, "init", "-b", branch)
	for _, name := range sortedKeys(files) {
		writeFile(t, filepath.Join(dir, name), files[name])
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")
	return dir
}

// commitAll records all current content in the origin repository as a commit.
func commitAll(t *testing.T, dir, message string) {
	t.Helper()
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", message)
}

// sortedKeys returns the map keys in sorted order for deterministic commits.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestNewCreatesWorkDir(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "nested", "repos")
	if _, err := New(workDir, nil); err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		t.Fatalf("work dir %s not created: %v", workDir, err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New("", nil); err == nil {
		t.Error("New(\"\") succeeded, want error")
	}

	t.Run("git binary not found", func(t *testing.T) {
		emptyPath := t.TempDir()
		t.Setenv("PATH", emptyPath)
		if _, err := New(filepath.Join(t.TempDir(), "repos"), nil); err == nil {
			t.Error("New() succeeded without git in PATH, want error")
		}
	})
}

func TestEnsureRepoClone(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: infra-stack\n"})
	m, workDir := newManager(t)

	path, err := m.EnsureRepo("infra", origin, "main")
	if err != nil {
		t.Fatalf("EnsureRepo() error = %v", err)
	}
	if want := filepath.Join(workDir, "infra"); path != want {
		t.Errorf("localPath = %q, want %q", path, want)
	}
	got, err := os.ReadFile(filepath.Join(path, "stack.yaml"))
	if err != nil || string(got) != "name: infra-stack\n" {
		t.Errorf("cloned stack.yaml = %q, %v; want committed content", got, err)
	}
	if branch := runGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); strings.TrimSpace(branch) != "main" {
		t.Errorf("cloned branch = %q, want main", branch)
	}
}

func TestEnsureRepoCloneNonDefaultBranch(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: base\n"})
	runGit(t, origin, "checkout", "-b", "release")
	writeFile(t, filepath.Join(origin, "stack.yaml"), "name: release-stack\n")
	commitAll(t, origin, "release state")
	m, _ := newManager(t)

	path, err := m.EnsureRepo("infra", origin, "release")
	if err != nil {
		t.Fatalf("EnsureRepo() error = %v", err)
	}
	if branch := runGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); strings.TrimSpace(branch) != "release" {
		t.Errorf("cloned branch = %q, want release", branch)
	}
	if got, _ := os.ReadFile(filepath.Join(path, "stack.yaml")); string(got) != "name: release-stack\n" {
		t.Errorf("stack.yaml = %q, want release content", got)
	}
}

func TestEnsureRepoCloneIntoEmptyDir(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: infra-stack\n"})
	m, workDir := newManager(t)

	empty := filepath.Join(workDir, "infra")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", empty, err)
	}
	path, err := m.EnsureRepo("infra", origin, "main")
	if err != nil {
		t.Fatalf("EnsureRepo() into empty dir error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "stack.yaml")); err != nil {
		t.Errorf("stack.yaml missing after clone into empty dir: %v", err)
	}
}

func TestEnsureRepoUpdateIsDeterministic(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "version: 1\n"})
	m, _ := newManager(t)

	path, err := m.EnsureRepo("infra", origin, "main")
	if err != nil {
		t.Fatalf("first EnsureRepo() error = %v", err)
	}

	// Dirty the checkout: modify a tracked file and add untracked files and
	// a directory. EnsureRepo must discard all of them.
	writeFile(t, filepath.Join(path, "stack.yaml"), "version: 1-local-edit\n")
	writeFile(t, filepath.Join(path, "untracked.txt"), "junk\n")
	writeFile(t, filepath.Join(path, "untracked-dir", "nested.txt"), "junk\n")

	writeFile(t, filepath.Join(origin, "stack.yaml"), "version: 2\n")
	commitAll(t, origin, "bump")

	if _, err := m.EnsureRepo("infra", origin, "main"); err != nil {
		t.Fatalf("second EnsureRepo() error = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(path, "stack.yaml")); string(got) != "version: 2\n" {
		t.Errorf("stack.yaml = %q after update, want upstream version 2", got)
	}
	if _, err := os.Stat(filepath.Join(path, "untracked.txt")); !os.IsNotExist(err) {
		t.Errorf("untracked.txt survived git clean -fd (err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(path, "untracked-dir")); !os.IsNotExist(err) {
		t.Errorf("untracked-dir/ survived git clean -fd (err = %v)", err)
	}
	if status := runGit(t, path, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("working tree not clean after update:\n%s", status)
	}
}

func TestEnsureRepoRefusesForeignDirectory(t *testing.T) {
	m, workDir := newManager(t)
	foreign := filepath.Join(workDir, "infra")
	writeFile(t, filepath.Join(foreign, "precious.txt"), "not a repo\n")

	_, err := m.EnsureRepo("infra", t.TempDir(), "main")
	if err == nil {
		t.Fatal("EnsureRepo() on non-repo directory succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error = %v, want mention of missing git repository", err)
	}
	if _, err := os.Stat(filepath.Join(foreign, "precious.txt")); err != nil {
		t.Errorf("foreign directory was modified: %v", err)
	}
}

func TestEnsureRepoRejectsOriginMismatch(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: infra-stack\n"})
	m, _ := newManager(t)
	if _, err := m.EnsureRepo("infra", origin, "main"); err != nil {
		t.Fatalf("first EnsureRepo() error = %v", err)
	}

	otherOrigin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: other\n"})
	_, err := m.EnsureRepo("infra", otherOrigin, "main")
	if err == nil || !strings.Contains(err.Error(), "was cloned from") {
		t.Errorf("EnsureRepo() with different url error = %v, want origin mismatch error", err)
	}
}

func TestEnsureRepoCloneFailure(t *testing.T) {
	m, _ := newManager(t)

	_, err := m.EnsureRepo("infra", filepath.Join(t.TempDir(), "does-not-exist"), "main")
	if err == nil || !strings.Contains(err.Error(), "git clone") {
		t.Errorf("clone of missing url error = %v, want error mentioning git clone", err)
	}

	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: infra-stack\n"})
	_, err = m.EnsureRepo("infra", origin, "no-such-branch")
	if err == nil || !strings.Contains(err.Error(), "git clone") {
		t.Errorf("clone of missing branch error = %v, want error mentioning git clone", err)
	}
}

func TestEnsureRepoValidation(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: infra-stack\n"})
	m, _ := newManager(t)

	cases := []struct {
		name, url, branch, wantErr string
	}{
		{"", origin, "main", "name must not be empty"},
		{"../escape", origin, "main", "invalid repository name"},
		{"a/b", origin, "main", "invalid repository name"},
		{".", origin, "main", "invalid repository name"},
		{"infra", "", "main", "url must not be empty"},
		{"infra", origin, "", "branch must not be empty"},
	}
	for _, tc := range cases {
		_, err := m.EnsureRepo(tc.name, tc.url, tc.branch)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("EnsureRepo(%q, %q, %q) error = %v, want containing %q", tc.name, tc.url, tc.branch, err, tc.wantErr)
		}
	}
}

// ---- configurable checkout paths (EnsureRepoAt) ----

// TestEnsureRepoAtCloneOutsideWorkDir: (a) a configured path outside the
// manager's work directory is cloned into directly.
func TestEnsureRepoAtCloneOutsideWorkDir(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: infra-stack\n"})
	m, workDir := newManager(t)
	path := filepath.Join(t.TempDir(), "home-docker", "infrastructure")

	got, err := m.EnsureRepoAt(path, origin, "main")
	if err != nil {
		t.Fatalf("EnsureRepoAt() error = %v", err)
	}
	if got != path {
		t.Errorf("path = %q, want %q", got, path)
	}
	if data, err := os.ReadFile(filepath.Join(path, "stack.yaml")); err != nil || string(data) != "name: infra-stack\n" {
		t.Errorf("stack.yaml = %q, %v; want cloned content", data, err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "infrastructure")); !os.IsNotExist(err) {
		t.Errorf("the work dir was used although the checkout path is configured (err = %v)", err)
	}
}

// TestEnsureRepoAtAdoptsExistingCheckout: (b) an existing checkout with a
// matching origin is adopted in place and runs the full deterministic update
// sequence — dirty worktree included (WIP stash, fetch, reset, clean).
func TestEnsureRepoAtAdoptsExistingCheckout(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "version: 1\n"})
	m, _ := newManager(t)
	// The fleet's existing clone, created without the manager's help.
	path := filepath.Join(t.TempDir(), "infra")
	runGit(t, filepath.Dir(path), "clone", "--branch", "main", "--single-branch", origin, path)

	// Host-side edit plus upstream bump: adoption must update, not skip.
	writeFile(t, filepath.Join(path, "stack.yaml"), "version: 1-local\n")
	writeFile(t, filepath.Join(origin, "stack.yaml"), "version: 2\n")
	commitAll(t, origin, "bump")

	got, err := m.EnsureRepoAt(path, origin, "main")
	if err != nil {
		t.Fatalf("EnsureRepoAt() error = %v", err)
	}
	if got != path {
		t.Errorf("path = %q, want %q", got, path)
	}
	if data, _ := os.ReadFile(filepath.Join(path, "stack.yaml")); string(data) != "version: 2\n" {
		t.Errorf("stack.yaml = %q after adoption, want upstream version 2", data)
	}
	if status := runGit(t, path, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("working tree not clean after adoption:\n%s", status)
	}
	// The local edit was stashed, not destroyed.
	if entries := stashEntries(t, path); len(entries) != 1 {
		t.Errorf("git stash list = %q, want exactly the dockops-wip entry", entries)
	}
}

// TestEnsureRepoAtRejectsOriginMismatch: (c) adopting a checkout cloned from
// a different origin is refused — a path collision must not silently serve
// the wrong repository.
func TestEnsureRepoAtRejectsOriginMismatch(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: infra-stack\n"})
	otherOrigin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: other\n"})
	m, _ := newManager(t)
	path := filepath.Join(t.TempDir(), "infra")
	if _, err := m.EnsureRepoAt(path, origin, "main"); err != nil {
		t.Fatalf("first EnsureRepoAt() error = %v", err)
	}

	_, err := m.EnsureRepoAt(path, otherOrigin, "main")
	if err == nil || !strings.Contains(err.Error(), "was cloned from") {
		t.Errorf("EnsureRepoAt() with different url error = %v, want origin mismatch error", err)
	}
}

// TestEnsureRepoAtRefusesForeignDirectory: (d) a non-empty directory without
// .git is refused instead of overwritten.
func TestEnsureRepoAtRefusesForeignDirectory(t *testing.T) {
	m, _ := newManager(t)
	foreign := filepath.Join(t.TempDir(), "precious")
	writeFile(t, filepath.Join(foreign, "precious.txt"), "not a repo\n")

	_, err := m.EnsureRepoAt(foreign, t.TempDir(), "main")
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("EnsureRepoAt() on non-repo directory error = %v, want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(foreign, "precious.txt")); err != nil {
		t.Errorf("foreign directory was modified: %v", err)
	}
}

// TestEnsureRepoAtValidation: (e) non-absolute paths and paths outside the
// conservative character set are rejected before any git command runs.
func TestEnsureRepoAtValidation(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: infra-stack\n"})
	m, _ := newManager(t)

	cases := []struct{ path, wantErr string }{
		{"", "checkout path must not be empty"},
		{"relative/path", "must be absolute"},
		{"relative", "must be absolute"},
		{"/tmp/with space", "must only contain"},
		{"/tmp/with'quote", "must only contain"},
		{"/tmp/with\"dquote", "must only contain"},
		{"/tmp/semi;colon", "must only contain"},
		{"/tmp/dollar$sign", "must only contain"},
		{"/tmp/infrâstructure", "must only contain"},
	}
	for _, tc := range cases {
		_, err := m.EnsureRepoAt(tc.path, origin, "main")
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("EnsureRepoAt(%q) error = %v, want containing %q", tc.path, err, tc.wantErr)
		}
	}

	// url and branch requirements are unchanged.
	if _, err := m.EnsureRepoAt("/tmp/checkout", "", "main"); err == nil || !strings.Contains(err.Error(), "url must not be empty") {
		t.Errorf("EnsureRepoAt() without url error = %v, want url-required error", err)
	}
	if _, err := m.EnsureRepoAt("/tmp/checkout", origin, ""); err == nil || !strings.Contains(err.Error(), "branch must not be empty") {
		t.Errorf("EnsureRepoAt() without branch error = %v, want branch-required error", err)
	}
}

// TestHeadCommitOutsideWorkDir pins the relaxed HeadCommit: a configured
// checkout outside the work directory is answered, not refused — GIT_COMMIT
// must keep working for adopted clones.
func TestHeadCommitOutsideWorkDir(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "name: infra-stack\n"})
	m, _ := newManager(t)
	path := filepath.Join(t.TempDir(), "elsewhere", "infra")
	if _, err := m.EnsureRepoAt(path, origin, "main"); err != nil {
		t.Fatalf("EnsureRepoAt() error = %v", err)
	}
	want := strings.TrimSpace(runGit(t, path, "rev-parse", "HEAD"))

	got, err := m.HeadCommit(path)
	if err != nil {
		t.Fatalf("HeadCommit() outside the work dir error = %v", err)
	}
	if got != want {
		t.Errorf("HeadCommit() = %q, want %q", got, want)
	}
}
