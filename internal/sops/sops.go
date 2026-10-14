// Package sops decrypts environment files with the sops CLI
// (github.com/getsops/sops). Like internal/git it deliberately shells out to
// the locally installed binary via os/exec instead of reimplementing the
// crypto formats.
package sops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/dotenv"
)

// binary is the name of the sops executable, resolved through PATH per call.
const binary = "sops"

// maxErrorOutput bounds how much command output is embedded into an error
// message so failures stay readable while still carrying the sops
// diagnostics (same limit as internal/git).
const maxErrorOutput = 512

// DecryptFile decrypts the sops-encrypted dotenv file at path via
// `sops -d --input-type dotenv --output-type dotenv <path>` and parses the
// plaintext as dotenv (see package dotenv for the dialect).
//
// A missing sops binary yields a dedicated "sops binary not found in PATH"
// error; a failing invocation yields an error with the path and a bounded
// excerpt of sops' stderr. Decrypted values never enter error messages or
// logs — only the stderr snippet does.
func DecryptFile(ctx context.Context, path string) (map[string]string, error) {
	return decrypt(ctx, path, nil)
}

// DecryptFiles decrypts files in order and merges them: keys of later files
// override earlier ones, mirroring the env_files merge semantics. Relative
// paths (and a relative age key file) resolve against baseDir; the first
// failure aborts with the file's index and path.
//
// A per-file age key file (SopsFile.AgeKeyfile) is passed to the sops binary
// via the SOPS_AGE_KEY_FILE environment variable of the child process only —
// the process environment of dockops itself is never modified.
func DecryptFiles(ctx context.Context, files []config.SopsFile, baseDir string) (map[string]string, error) {
	merged := make(map[string]string)
	for i, f := range files {
		var extraEnv []string
		if f.AgeKeyfile != "" {
			extraEnv = []string{"SOPS_AGE_KEY_FILE=" + resolve(baseDir, f.AgeKeyfile)}
		}
		kv, err := decrypt(ctx, resolve(baseDir, f.Path), extraEnv)
		if err != nil {
			return nil, fmt.Errorf("sops_files[%d]: %w", i, err)
		}
		for k, v := range kv {
			merged[k] = v
		}
	}
	return merged, nil
}

// decrypt runs the sops CLI on path with extraEnv appended to the child
// environment and parses its stdout as dotenv.
func decrypt(ctx context.Context, path string, extraEnv []string) (map[string]string, error) {
	if _, err := exec.LookPath(binary); err != nil {
		return nil, fmt.Errorf("sops: decrypt %s: sops binary not found in PATH", path)
	}
	cmd := exec.CommandContext(ctx, binary,
		"-d", "--input-type", "dotenv", "--output-type", "dotenv", path)
	cmd.Env = append(os.Environ(), extraEnv...)
	// Output, not CombinedOutput: stdout carries the plaintext, stderr the
	// diagnostics — mixing them would leak error text into the environment.
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("sops: decrypt %s: %w%s", path, err, stderrSnippet(err))
	}
	return dotenv.Parse(out), nil
}

// stderrSnippet extracts a bounded excerpt of the stderr of a failed
// exec.ExitError; other errors (binary vanished between LookPath and start,
// context cancellation) are returned without a snippet.
func stderrSnippet(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		s := string(exitErr.Stderr)
		if len(s) > maxErrorOutput {
			s = s[:maxErrorOutput] + "..."
		}
		return ": " + strings.TrimSpace(s)
	}
	return ""
}

// resolve returns p unchanged when absolute, else joined below baseDir.
func resolve(baseDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(baseDir, p)
}
