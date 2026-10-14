// Package compose drives the docker compose CLI to bring stacks up and
// down. It builds explicit, deterministic command lines — project name from
// the stack name, one -f, one --env-file — and never parses running state;
// change detection is the stack hash's job (internal/hash).
package compose

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/local/dockops/internal/config"
)

// defaultBinary is used when New is called with an empty binary name.
const defaultBinary = "docker"

// maxErrorOutput bounds how much command output is embedded into an error
// message or log entry (same limit as internal/git).
const maxErrorOutput = 512

// Executor runs docker compose commands for stacks.
type Executor struct {
	binary string
	logger *slog.Logger
}

// New creates an Executor invoking binary ("docker" when empty). A nil
// logger falls back to slog.Default(). The binary is not resolved here — a
// missing binary surfaces at the first Up/Down with the exec error.
func New(logger *slog.Logger, binary string) *Executor {
	if binary == "" {
		binary = defaultBinary
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Executor{binary: binary, logger: logger}
}

// Up brings the stack up detached:
//
//	<binary> compose -p <stack.Name> -f <composeFile> [--profile p ...]
//	         --env-file <envFile> up -d --remove-orphans [service ...]
//
// The explicit -p makes project names deterministic (stack name instead of
// a directory-derived default). Profiles and services are appended in their
// configured order; services are positional arguments — docker compose takes
// them without a preceding "--".
func (e *Executor) Up(ctx context.Context, stack config.Stack, stackDir, envFile string) error {
	args := []string{
		"compose",
		"-p", stack.Name,
		"-f", composeFilePath(stack, stackDir),
	}
	for _, profile := range stack.Profiles {
		args = append(args, "--profile", profile)
	}
	args = append(args, "--env-file", envFile)
	args = append(args, "up", "-d", "--remove-orphans")
	args = append(args, stack.Services...)
	return e.run(ctx, args)
}

// Down stops and removes the stack (used for pruning removed stacks):
//
//	<binary> compose -p <stack.Name> -f <composeFile> --env-file <envFile>
//	         down --remove-orphans
//
// envFile is passed so variable interpolation in the compose file resolves
// identically to Up; profiles and services are irrelevant for teardown.
func (e *Executor) Down(ctx context.Context, stack config.Stack, stackDir, envFile string) error {
	args := []string{
		"compose",
		"-p", stack.Name,
		"-f", composeFilePath(stack, stackDir),
		"--env-file", envFile,
		"down", "--remove-orphans",
	}
	return e.run(ctx, args)
}

// DownProject tears a project down by name only, without a compose file:
//
//	<binary> compose -p <project> down --remove-orphans
//
// It is the prune fallback for stacks whose definition (and with it the
// compose file and checkout directory) has disappeared; only the project
// name — which docker compose derives its resource labels from — is still
// known. Depending on the docker compose version this may fail (some
// versions insist on a compose file even for teardown); the caller logs and
// retries on the next pass.
func (e *Executor) DownProject(ctx context.Context, project string) error {
	return e.run(ctx, []string{"compose", "-p", project, "down", "--remove-orphans"})
}

// composeFilePath returns the compose file to pass to -f: the stack's
// resolved ComposeFile when absolute (the config loader resolves it),
// otherwise ComposeFile joined below stackDir.
func composeFilePath(stack config.Stack, stackDir string) string {
	if stack.ComposeFile == "" || filepath.IsAbs(stack.ComposeFile) {
		return stack.ComposeFile
	}
	return filepath.Join(stackDir, stack.ComposeFile)
}

// run executes the composed command line, logs it with its combined output
// and returns an error embedding the command and a bounded output excerpt
// when it fails.
func (e *Executor) run(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, e.binary, args...)
	out, err := cmd.CombinedOutput()
	command := strings.Join(args, " ")
	if err != nil {
		e.logger.Error("docker compose command failed",
			"command", command, "error", err.Error(), "output", snippet(out))
		return fmt.Errorf("docker %s: %w%s", command, err, outputSuffix(out))
	}
	e.logger.Info("docker compose command finished", "command", command, "output", snippet(out))
	return nil
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

// snippet bounds log output to maxErrorOutput bytes.
func snippet(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > maxErrorOutput {
		s = s[:maxErrorOutput] + "..."
	}
	return s
}
