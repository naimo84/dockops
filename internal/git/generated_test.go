package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The files a deploy leaves in a stack directory: the merged stack
// environment (resolved secrets included) and the bind override. They belong
// to dockops, not to the repository.
const generatedEnvContent = "DB_PASSWORD=hunter2\n"

func writeDeployArtefacts(t *testing.T, stackDir string) {
	t.Helper()
	writeFile(t, filepath.Join(stackDir, ".env"), generatedEnvContent)
	writeFile(t, filepath.Join(stackDir, "dockops-binds-web.yaml"), "services: {}\n")
}

// An update pass must neither stash nor clean the generated files: the .env
// carries resolved secrets (a stash would persist them in the object store),
// and compose reads it again long after the deploy that wrote it (apply,
// restart, update check).
func TestEnsureRepoKeepsUntrackedGeneratedFiles(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"web/compose.yaml": "services: {}\n"})
	m, _ := newManager(t)

	path, err := m.EnsureRepo("host", origin, "main")
	if err != nil {
		t.Fatalf("first EnsureRepo() error = %v", err)
	}
	writeDeployArtefacts(t, filepath.Join(path, "web"))

	if _, err := m.EnsureRepo("host", origin, "main"); err != nil {
		t.Fatalf("second EnsureRepo() error = %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(path, "web", ".env")); err != nil || string(got) != generatedEnvContent {
		t.Errorf("generated .env after update = %q, %v; want it untouched", got, err)
	}
	if _, err := os.Stat(filepath.Join(path, "web", "dockops-binds-web.yaml")); err != nil {
		t.Errorf("generated bind override removed by the update: %v", err)
	}
	if entries := stashEntries(t, path); len(entries) != 0 {
		t.Errorf("git stash list = %q, want no entry (generated files are not work in progress)", entries)
	}
	if status := runGit(t, path, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("generated files still show up in git status:\n%s", status)
	}
}

// A real host-side edit is still protected — but the stash must carry only
// the edit, never the generated .env next to it.
func TestEnsureRepoStashesEditsWithoutGeneratedFiles(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{
		"web/compose.yaml": "services: {}\n",
		"notes.txt":        "upstream\n",
	})
	m, _ := newManager(t)

	path, err := m.EnsureRepo("host", origin, "main")
	if err != nil {
		t.Fatalf("first EnsureRepo() error = %v", err)
	}
	writeDeployArtefacts(t, filepath.Join(path, "web"))
	writeFile(t, filepath.Join(path, "notes.txt"), "local-edit\n")

	if _, err := m.EnsureRepo("host", origin, "main"); err != nil {
		t.Fatalf("second EnsureRepo() error = %v", err)
	}

	entries := stashEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("git stash list = %q, want exactly the WIP entry of the edit", entries)
	}
	files := runGit(t, path, "stash", "show", "--include-untracked", "--name-only", "stash@{0}")
	if !strings.Contains(files, "notes.txt") {
		t.Errorf("stash lacks the host-side edit; files:\n%s", files)
	}
	if strings.Contains(files, ".env") || strings.Contains(files, "dockops-binds") {
		t.Errorf("stash contains generated files:\n%s", files)
	}
	if got, _ := os.ReadFile(filepath.Join(path, "web", ".env")); string(got) != generatedEnvContent {
		t.Errorf("generated .env after update = %q, want it untouched", got)
	}
}

// A repository that commits a .env in a stack directory gets it overwritten
// by every deploy. The update resets it to the committed version, but the
// generated content (secrets) must not end up in the stash, and the operator
// is told why the file keeps flipping.
func TestEnsureRepoNeverStashesTrackedGeneratedFile(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{
		"web/compose.yaml": "services: {}\n",
		"web/.env":         "COMMITTED=1\n",
	})
	m, logs, _ := newLoggedManager(t)

	path, err := m.EnsureRepo("host", origin, "main")
	if err != nil {
		t.Fatalf("first EnsureRepo() error = %v", err)
	}
	writeFile(t, filepath.Join(path, "web", ".env"), generatedEnvContent)

	if _, err := m.EnsureRepo("host", origin, "main"); err != nil {
		t.Fatalf("second EnsureRepo() error = %v", err)
	}

	if entries := stashEntries(t, path); len(entries) != 0 {
		t.Errorf("git stash list = %q, want no entry (the generated secrets must not be stashed)", entries)
	}
	if got, _ := os.ReadFile(filepath.Join(path, "web", ".env")); string(got) != "COMMITTED=1\n" {
		t.Errorf("tracked .env after update = %q, want the committed version", got)
	}
	if out := logs.String(); !strings.Contains(out, "web/.env") || !strings.Contains(out, "WARN") {
		t.Errorf("no warning naming the tracked generated file:\n%s", out)
	}
}

// The exclude block is written once, even across many passes, and it keeps
// whatever the file held before.
func TestEnsureRepoExcludeBlockIsIdempotent(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"web/compose.yaml": "services: {}\n"})
	m, _ := newManager(t)

	path, err := m.EnsureRepo("host", origin, "main")
	if err != nil {
		t.Fatalf("EnsureRepo() error = %v", err)
	}
	excludeFile := filepath.Join(path, ".git", "info", "exclude")
	before, err := os.ReadFile(excludeFile)
	if err != nil {
		t.Fatalf("read exclude file after clone: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := m.EnsureRepo("host", origin, "main"); err != nil {
			t.Fatalf("EnsureRepo() pass %d error = %v", i, err)
		}
	}
	after, err := os.ReadFile(excludeFile)
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("exclude file changed on later passes:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if n := strings.Count(string(after), "dockops-rollback-*.yaml"); n != 1 {
		t.Errorf("rollback pattern appears %d times, want 1:\n%s", n, after)
	}
}
