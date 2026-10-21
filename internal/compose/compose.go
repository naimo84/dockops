// Package compose drives the docker compose CLI to bring stacks up and
// down. It builds explicit, deterministic command lines — project name from
// the stack name, one -f per compose file, one --env-file — and never lets
// running state drive deployments; change detection is the stack hash's job
// (internal/hash). Two exceptions read instead of write: the service filter,
// where the compose files' own service names (desired, not running, state)
// materialize "all but ignored" as positional arguments, and the Ps* methods
// (ps.go), which inspect running containers solely for the GET /status
// monitoring endpoint (internal/webhook).
package compose

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

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
//	<binary> compose -p <stack.Name> [-f <file> ...] [--profile p ...]
//	         --env-file <envFile> up -d --remove-orphans [service ...]
//
// The explicit -p makes project names deterministic (stack name instead of
// a directory-derived default). One -f is passed per compose file — either
// the multi-file compose_files configuration or the single compose_file —
// in their configured order (compose merge rules apply, later files win).
// Profiles are appended in their configured order; the service filter
// (services minus ignore_services, see upServices) yields the positional
// service arguments — docker compose takes them without a preceding "--".
func (e *Executor) Up(ctx context.Context, stack config.Stack, stackDir, envFile string) error {
	args := []string{"compose", "-p", stack.Name}
	args = append(args, composeFileArgs(stack, stackDir)...)
	for _, profile := range stack.Profiles {
		args = append(args, "--profile", profile)
	}
	args = append(args, "--env-file", envFile)
	args = append(args, "up", "-d", "--remove-orphans")
	services, err := upServices(stack, stackDir)
	if err != nil {
		return err
	}
	args = append(args, services...)
	return e.run(ctx, args)
}

// Down stops and removes the stack (used for pruning removed stacks):
//
//	<binary> compose -p <stack.Name> [-f <file> ...] --env-file <envFile>
//	         down --remove-orphans
//
// envFile is passed so variable interpolation in the compose file resolves
// identically to Up; profiles and services are irrelevant for teardown.
func (e *Executor) Down(ctx context.Context, stack config.Stack, stackDir, envFile string) error {
	args := []string{"compose", "-p", stack.Name}
	args = append(args, composeFileArgs(stack, stackDir)...)
	args = append(args, "--env-file", envFile, "down", "--remove-orphans")
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

// composeFileArgs returns the -f arguments for every compose file of the
// stack, in order: the effective file list (multi-file compose_files
// configuration, resolved absolute by the config loader) or the single
// ComposeFile, each joined below stackDir when relative (hand-constructed
// stacks may carry relative paths).
func composeFileArgs(stack config.Stack, stackDir string) []string {
	return resolveComposeFileArgs(stack.EffectiveComposeFiles(), stackDir)
}

// resolveComposeFileArgs is the file-list half of composeFileArgs, shared
// with the read-only Request of the image update check (images.go).
func resolveComposeFileArgs(files []string, stackDir string) []string {
	args := make([]string, 0, len(files)*2)
	for _, f := range files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(stackDir, f)
		}
		args = append(args, "-f", f)
	}
	return args
}

// upServices returns the positional service arguments for compose up:
// docker compose has no native exclude flag, so ignore_services is
// materialized as a set difference here.
//
//	services set       → services minus ignore_services
//	services unset, no ignore → nil (deploy every service)
//	services unset, ignore set → every service name of the compose files
//	                           minus ignore_services
//
// A result that would deploy nothing is an error — passing no positional
// argument would silently deploy everything, the exact opposite of the
// configured filter.
func upServices(stack config.Stack, stackDir string) ([]string, error) {
	if len(stack.Services) > 0 {
		effective := config.ExcludeServices(stack.Services, stack.IgnoreServices)
		if len(effective) == 0 {
			return nil, fmt.Errorf("stack %s: every configured service is in ignore_services: %v", stack.Name, stack.Services)
		}
		return effective, nil
	}
	if len(stack.IgnoreServices) == 0 {
		return nil, nil
	}
	all, err := composeServiceNames(stack, stackDir)
	if err != nil {
		return nil, fmt.Errorf("stack %s: resolve ignore_services: %w", stack.Name, err)
	}
	effective := config.ExcludeServices(all, stack.IgnoreServices)
	if len(effective) == 0 {
		return nil, fmt.Errorf("stack %s: ignore_services covers every service of the compose files: %v", stack.Name, all)
	}
	return effective, nil
}

// composeServiceNames parses the top-level services mapping of every
// compose file and returns the union of the service names, sorted. docker
// compose itself merges the files; only the names are needed here.
//
// Conservative decisions: "all services" means every top-level services key
// across all files — whether a service actually starts also depends on its
// profiles, which stays compose's business — and a file whose parsing fails
// is an error (docker compose would reject it too, with a worse message).
func composeServiceNames(stack config.Stack, stackDir string) ([]string, error) {
	seen := make(map[string]struct{})
	var names []string
	for _, f := range stack.EffectiveComposeFiles() {
		if !filepath.IsAbs(f) {
			f = filepath.Join(stackDir, f)
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		var doc struct {
			Services map[string]any `yaml:"services"`
		}
		// Non-strict decoding on purpose: only the top-level service names
		// are of interest, the rest of the document is compose's to parse.
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		for name := range doc.Services {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
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
