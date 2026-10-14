// Package git manages the local checkouts of the remote git repositories
// that back dockops import stacks. It deliberately shells out to the locally
// installed git binary via os/exec instead of using an external git library.
//
// Credential handling is intentionally out of scope: HTTP credentials must be
// provided by the caller through the environment (e.g. GIT_ASKPASS) or as
// tokens embedded in the URL; SSH simply uses the agent and keys of the
// surrounding environment. SSH known-hosts pinning is likewise the caller's
// concern (GIT_SSH_COMMAND), not this package's.
package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitBinary is the name of the git executable used for every command. It is
// resolved through PATH once, when a Manager is created.
const gitBinary = "git"

// maxErrorOutput bounds how much command output is embedded into an error
// message so failures stay readable while still carrying the essential git
// diagnostics.
const maxErrorOutput = 512

// Manager clones and deterministically updates remote repositories below a
// shared work directory (global.work_dir of the root configuration).
type Manager struct {
	workDir string
	logger  *slog.Logger
}

// New creates a Manager managing checkouts below workDir. It verifies that
// the git binary is reachable via exec.LookPath, resolves workDir to an
// absolute path and creates the directory if needed. A nil logger falls back
// to slog.Default().
func New(workDir string, logger *slog.Logger) (*Manager, error) {
	if workDir == "" {
		return nil, fmt.Errorf("git: workDir must not be empty")
	}
	if _, err := exec.LookPath(gitBinary); err != nil {
		return nil, fmt.Errorf("git: binary %q not found in PATH: %w", gitBinary, err)
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("git: resolve workDir %s: %w", workDir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("git: create workDir %s: %w", abs, err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{workDir: abs, logger: logger}, nil
}

// EnsureRepo guarantees that <workDir>/<name> is a checkout of url at the
// latest commit of branch (clone-or-update) and returns the local path.
//
// Clone when the directory does not exist or is empty; otherwise the
// checkout is made deterministic with exactly the sequence prescribed by the
// briefing: git fetch origin <branch>, git reset --hard origin/<branch>,
// git clean -fd. Local modifications and untracked files are discarded on
// purpose — the checkout is machine-managed state.
//
// A non-empty directory without a .git entry is refused instead of
// overwritten (conservative choice: never destroy data we did not create).
// A checkout whose recorded origin differs from url is rejected as well, so
// a name collision cannot silently serve the wrong repository.
func (m *Manager) EnsureRepo(name, url, branch string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	if url == "" {
		return "", fmt.Errorf("git %s: url must not be empty", name)
	}
	if branch == "" {
		return "", fmt.Errorf("git %s: branch must not be empty", name)
	}
	path := filepath.Join(m.workDir, name)

	state, err := dirState(path)
	if err != nil {
		return "", err
	}

	switch state {
	case dirAbsent, dirEmpty:
		m.logger.Info("cloning repository", "name", name, "url", url, "branch", branch, "path", path)
		// exec passes arguments directly (no shell), so tokens embedded in
		// url cannot be misinterpreted as shell syntax.
		if _, err := m.runGit("", "clone", "--branch", branch, "--single-branch", "--", url, path); err != nil {
			return "", err
		}
	case dirRepo:
		if err := m.checkOrigin(path, name, url); err != nil {
			return "", err
		}
		m.logger.Info("updating repository", "name", name, "branch", branch, "path", path)
		if _, err := m.runGit(path, "fetch", "origin", branch); err != nil {
			return "", err
		}
		if _, err := m.runGit(path, "reset", "--hard", "origin/"+branch); err != nil {
			return "", err
		}
		if _, err := m.runGit(path, "clean", "-fd"); err != nil {
			return "", err
		}
	default: // dirForeign
		return "", fmt.Errorf("git %s: %s exists but is not a git repository (and not empty); refusing to overwrite it", name, path)
	}
	return path, nil
}

// validateName rejects repository names that would escape the work directory
// or are unusable as a single path segment. The briefing is silent on name
// validation; rejecting is the conservative choice.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("git: name must not be empty")
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("git: invalid repository name %q (must be a single path segment below the work dir)", name)
	}
	return nil
}

// dirKind classifies the target directory of EnsureRepo.
type dirKind int

const (
	dirAbsent  dirKind = iota // does not exist
	dirEmpty                  // exists but has no entries
	dirRepo                   // contains a .git entry
	dirForeign                // non-empty without .git
)

// dirState classifies path for the clone-or-update decision.
func dirState(path string) (dirKind, error) {
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return dirAbsent, nil
	}
	if err != nil {
		return 0, fmt.Errorf("git: inspect %s: %w", path, err)
	}
	if len(entries) == 0 {
		return dirEmpty, nil
	}
	// .git is a directory for normal clones and a file for worktrees; both
	// mark a git-managed checkout.
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return dirRepo, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("git: inspect %s: %w", path, err)
	}
	return dirForeign, nil
}

// checkOrigin verifies that the existing checkout at path was cloned from
// url. A mismatch is an error instead of a silent fetch from the old origin.
func (m *Manager) checkOrigin(path, name, url string) error {
	out, err := m.runGit(path, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != url {
		return fmt.Errorf("git %s: %s was cloned from %q, not %q (remove the checkout or fix the configuration)", name, path, got, url)
	}
	return nil
}

// runGit executes the git binary with args, optionally with dir as working
// directory, and returns its combined output. Errors embed the full command
// and a bounded excerpt of the output, per the project convention that
// errors carry context.
//
// The public API has no context parameters (matching the briefing's
// signatures); commands still run through exec.CommandContext with
// context.Background so a context can be plumbed through Manager later
// without touching call sites.
func (m *Manager) runGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), gitBinary, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w%s", strings.Join(args, " "), err, outputSuffix(out))
	}
	return out, nil
}

// outputSuffix renders command output for an error message, bounded to
// maxErrorOutput bytes; empty output yields an empty suffix.
func outputSuffix(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	if len(s) > maxErrorOutput {
		s = s[:maxErrorOutput] + "..."
	}
	return ": " + s
}
