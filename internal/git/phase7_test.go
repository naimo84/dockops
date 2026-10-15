package git

// Phase-7-Regressionstests: WIP-Auto-Stash vor dem deterministischen Reset.
// Nutzer editieren gelegentlich direkt auf den Hosts im Checkout; der
// Update-Pfad (fetch + reset --hard + clean -fd) muss diese Änderungen vorher
// auf den Stash-Regal legen, statt sie kommentarlos zu vernichten.

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wipStashMessagePrefix is the spec-required prefix of auto-created WIP
// stash messages ("dockops-wip: <RFC3339-Timestamp>"). Deliberately a test
// literal instead of the implementation constant, so the tests pin the
// message format from the briefing.
const wipStashMessagePrefix = "dockops-wip: "

// newLoggedManager creates a Manager whose log output is captured in the
// returned buffer, so tests can assert on INFO/WARN messages.
func newLoggedManager(t *testing.T) (*Manager, *strings.Builder, string) {
	t.Helper()
	var logs strings.Builder
	workDir := filepath.Join(t.TempDir(), "repos")
	m, err := New(workDir, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return m, &logs, workDir
}

// stashEntries returns the lines of git stash list in dir (empty if none).
func stashEntries(t *testing.T, dir string) []string {
	t.Helper()
	out := runGit(t, dir, "stash", "list")
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// requireWipEntry asserts that a single git stash list line is a dockops WIP
// entry with a parseable RFC3339 timestamp and returns that timestamp.
func requireWipEntry(t *testing.T, entry string) time.Time {
	t.Helper()
	idx := strings.Index(entry, wipStashMessagePrefix)
	if idx < 0 {
		t.Fatalf("stash entry %q does not carry the %q message", entry, wipStashMessagePrefix)
	}
	stamp := strings.TrimSpace(entry[idx+len(wipStashMessagePrefix):])
	ts, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("stash timestamp %q in entry %q is not RFC3339: %v", stamp, entry, err)
	}
	return ts
}

func TestEnsureRepoUpdateStashesModifiedTrackedFile(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{
		"stack.yaml": "version: 1\n",
		"notes.txt":  "upstream\n",
	})
	m, logs, _ := newLoggedManager(t)

	path, err := m.EnsureRepo("infra", origin, "main")
	if err != nil {
		t.Fatalf("first EnsureRepo() error = %v", err)
	}

	// Host-side edit on a tracked file; upstream moves a DIFFERENT file so
	// the stashed edit stays conflict-free against the new origin/main.
	writeFile(t, filepath.Join(path, "notes.txt"), "local-edit\n")
	writeFile(t, filepath.Join(origin, "stack.yaml"), "version: 2\n")
	commitAll(t, origin, "bump")

	if _, err := m.EnsureRepo("infra", origin, "main"); err != nil {
		t.Fatalf("second EnsureRepo() error = %v", err)
	}

	// Reset auf origin/main durchgeführt, Worktree clean.
	if got, _ := os.ReadFile(filepath.Join(path, "stack.yaml")); string(got) != "version: 2\n" {
		t.Errorf("stack.yaml = %q after update, want upstream version 2", got)
	}
	if status := runGit(t, path, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("working tree not clean after update:\n%s", status)
	}

	// Genau ein Stash-Eintrag mit dockops-wip-Message und RFC3339-Stempel.
	entries := stashEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("git stash list has %d entries (%q), want exactly 1", len(entries), entries)
	}
	requireWipEntry(t, entries[0])

	// Der lokale Edit liegt wirklich im Stash.
	if patch := runGit(t, path, "stash", "show", "-p", "stash@{0}"); !strings.Contains(patch, "local-edit") {
		t.Errorf("stash diff does not contain the local edit:\n%s", patch)
	}

	// INFO-Log mit Repo-Kontext und Wiederherstellungs-Hinweis (der
	// TextHandler filtert Debug, also belegt Vorhandensein >= INFO).
	logOut := logs.String()
	if !strings.Contains(logOut, "infra") {
		t.Errorf("stash log lacks repo context:\n%s", logOut)
	}
	if !strings.Contains(logOut, "git stash pop") || !strings.Contains(logOut, "git stash list") {
		t.Errorf("stash log lacks restore hint (git stash list / git stash pop):\n%s", logOut)
	}
}

func TestEnsureRepoUpdateStashesUntrackedFile(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "version: 1\n"})
	m, _ := newManager(t)

	path, err := m.EnsureRepo("infra", origin, "main")
	if err != nil {
		t.Fatalf("first EnsureRepo() error = %v", err)
	}

	// Untracked Host-Edit plus Upstream-Bump für den Update-Pfad.
	writeFile(t, filepath.Join(path, "hotfix.txt"), "precious local work\n")
	writeFile(t, filepath.Join(origin, "stack.yaml"), "version: 2\n")
	commitAll(t, origin, "bump")

	if _, err := m.EnsureRepo("infra", origin, "main"); err != nil {
		t.Fatalf("second EnsureRepo() error = %v", err)
	}

	// Datei ist aus dem Worktree verschwunden (im Stash, nicht gelöscht).
	if _, err := os.Stat(filepath.Join(path, "hotfix.txt")); !os.IsNotExist(err) {
		t.Errorf("untracked hotfix.txt still in worktree after update (err = %v)", err)
	}
	entries := stashEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("git stash list has %d entries (%q), want exactly 1", len(entries), entries)
	}
	requireWipEntry(t, entries[0])

	// Wiederherstellung beweist, dass der Stash-Eintrag die Datei enthält.
	runGit(t, path, "stash", "pop")
	got, err := os.ReadFile(filepath.Join(path, "hotfix.txt"))
	if err != nil || string(got) != "precious local work\n" {
		t.Errorf("hotfix.txt after stash pop = %q, %v; want stashed content", got, err)
	}
}

func TestEnsureRepoUpdateCleanWorktreeNoStash(t *testing.T) {
	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "version: 1\n"})
	m, logs, _ := newLoggedManager(t)

	path, err := m.EnsureRepo("infra", origin, "main")
	if err != nil {
		t.Fatalf("first EnsureRepo() error = %v", err)
	}

	writeFile(t, filepath.Join(origin, "stack.yaml"), "version: 2\n")
	commitAll(t, origin, "bump")

	if _, err := m.EnsureRepo("infra", origin, "main"); err != nil {
		t.Fatalf("second EnsureRepo() error = %v", err)
	}

	if entries := stashEntries(t, path); len(entries) != 0 {
		t.Errorf("git stash list = %q on a clean worktree, want no entries", entries)
	}
	// Kein Stash-INFO-Log (Debug wäre laut Briefing okay und wird vom
	// TextHandler nicht ausgegeben).
	if logOut := logs.String(); strings.Contains(logOut, wipStashMessagePrefix) {
		t.Errorf("clean worktree produced stash log output:\n%s", logOut)
	}
}

func TestEnsureRepoUpdateStashFailureContinues(t *testing.T) {
	// Echt-git lokieren, BEVOR der Wrapper es verschattet.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("locating real git binary: %v", err)
	}

	// git-Wrapper: nur "stash push" schlägt fehl, alles andere delegiert an
	// das echte Binary (Muster wie die Fake-Binaries im E2E-Test).
	binDir := t.TempDir()
	wrapper := "#!/bin/sh\n" +
		"if [ \"$1\" = \"stash\" ] && [ \"$2\" = \"push\" ]; then\n" +
		"  echo \"simulated stash push failure\" >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatalf("writing git wrapper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	origin := newOrigin(t, "main", map[string]string{"stack.yaml": "version: 1\n"})
	m, logs, _ := newLoggedManager(t)

	path, err := m.EnsureRepo("infra", origin, "main")
	if err != nil {
		t.Fatalf("first EnsureRepo() error = %v", err)
	}

	writeFile(t, filepath.Join(path, "stack.yaml"), "version: 1-local-edit\n")
	writeFile(t, filepath.Join(origin, "stack.yaml"), "version: 2\n")
	commitAll(t, origin, "bump")

	// Stash-Fehler darf den Reconcile NICHT abbrechen.
	if _, err := m.EnsureRepo("infra", origin, "main"); err != nil {
		t.Fatalf("second EnsureRepo() error = %v, want nil despite stash failure", err)
	}

	// Reset läuft wie vor Phase 7: Upstream-Inhalt, Worktree clean.
	if got, _ := os.ReadFile(filepath.Join(path, "stack.yaml")); string(got) != "version: 2\n" {
		t.Errorf("stack.yaml = %q after update, want upstream version 2", got)
	}
	if status := runGit(t, path, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("working tree not clean after update:\n%s", status)
	}
	if entries := stashEntries(t, path); len(entries) != 0 {
		t.Errorf("git stash list = %q after failed stash push, want no entries", entries)
	}
	if logOut := logs.String(); !strings.Contains(logOut, "WARN") {
		t.Errorf("no WARN logged for the failed stash:\n%s", logOut)
	}
}
