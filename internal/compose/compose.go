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
	services, err := upServices(stack, stackDir)
	if err != nil {
		return err
	}
	args, err := upSpec{
		project:  stack.Name,
		fileArgs: composeFileArgs(stack, stackDir),
		stackDir: stackDir,
		profiles: stack.Profiles,
		envFile:  envFile,
	}.args()
	if err != nil {
		return err
	}
	return e.run(ctx, append(args, services...))
}

// Pull fetches the images of the named services (the whole project for an
// empty list):
//
//	<binary> compose -p <project> [-f <file> ...] pull [service ...]
//
// It only fills the local image cache — nothing is recreated, so a pull
// cannot break a running stack. The pull is the first step of the phase-11
// apply path; the services are passed positionally so a 20-service stack does
// not pull 20 images to update one.
func (e *Executor) Pull(ctx context.Context, req Request, services []string) error {
	if req.Project == "" {
		return fmt.Errorf("docker compose pull: project name must not be empty")
	}
	args := []string{"compose", "-p", req.Project}
	args = append(args, req.fileArgs()...)
	args = append(args, "pull")
	args = append(args, services...)
	return e.run(ctx, args)
}

// UpOverride brings selected services up with additional compose files
// appended after the stack's own files (compose merge rules: the later file
// wins), recreating those services only:
//
//	<binary> compose -p <project> [-f <file> ...] [-f <override> ...]
//	         up -d --remove-orphans --no-deps [service ...]
//
// It is the write path of the phase-11 apply step, where Up's inputs are not
// available: the caller has a Request (project, stack dir, compose files) and
// a generated override file rather than a full stack. Decisions where the
// briefing is silent, all conservative and documented here:
//   - --no-deps is always part of this path. The apply step recreates exactly
//     the services whose image moved; without --no-deps compose would also
//     restart their dependencies, turning a one-image update into a stack
//     restart (and a rollback into two).
//   - no --profile flags: profiles widen the deploy set (Up needs them for
//     ignore_services' "all but these"), while this path always names its
//     services explicitly.
//   - no --env-file: the caller has no shm env file outside a deployment.
//     compose reads the stack directory's generated .env instead, which the
//     deployment wrote from the same merged environment (env.WriteStackEnv)
//     and which deliberately outlives a successful deploy.
func (e *Executor) UpOverride(ctx context.Context, req Request, extraFiles []string, services []string) error {
	args, err := upSpec{
		project:       req.Project,
		fileArgs:      req.fileArgs(),
		overrideFiles: extraFiles,
		stackDir:      req.StackDir,
		noDeps:        true,
	}.args()
	if err != nil {
		return err
	}
	return e.run(ctx, append(args, services...))
}

// upSpec is the argument recipe of one compose up invocation. Up and
// UpOverride fill it from their respective inputs and share args(), so the
// two command lines cannot drift apart (same flags, same order).
type upSpec struct {
	// project is the compose project name (-p).
	project string
	// fileArgs are the resolved -f pairs of the stack's compose files, in
	// configured order.
	fileArgs []string
	// overrideFiles are additional compose files, appended after the stack
	// files so their values win; relative entries resolve against stackDir.
	overrideFiles []string
	stackDir      string
	// profiles are passed as --profile in their configured order.
	profiles []string
	// envFile is passed as --env-file when set.
	envFile string
	// noDeps adds --no-deps (targeted recreation, see UpOverride).
	noDeps bool
}

// args assembles the command line up to (not including) the positional
// services.
func (s upSpec) args() ([]string, error) {
	if s.project == "" {
		return nil, fmt.Errorf("docker compose up: project name must not be empty")
	}
	args := []string{"compose", "-p", s.project}
	args = append(args, s.fileArgs...)
	for _, file := range s.overrideFiles {
		if file == "" {
			return nil, fmt.Errorf("docker compose up: override file of project %s must not be empty", s.project)
		}
		if !filepath.IsAbs(file) {
			file = filepath.Join(s.stackDir, file)
		}
		args = append(args, "-f", file)
	}
	for _, profile := range s.profiles {
		args = append(args, "--profile", profile)
	}
	if s.envFile != "" {
		args = append(args, "--env-file", s.envFile)
	}
	args = append(args, "up", "-d", "--remove-orphans")
	if s.noDeps {
		args = append(args, "--no-deps")
	}
	return args, nil
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
