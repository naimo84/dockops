// Package selfupdate holds the pure logic of the phase-12 self update: the
// plan an agent leaves for its detached helper container, the result the
// helper leaves for the next agent start, and the shared helpers both sides
// need (self detection, atomic JSON persistence, the docker run argument
// list, the rollback override).
//
// Nothing in here executes a command. The agent and the helper build their
// argument lists here and run them with os/exec, which keeps every decision
// — mounts, container name, override YAML, validation — testable without a
// docker daemon.
//
// Why a plan file at all: the agent dies with its own container when the
// helper recreates the stack it runs in. Everything the helper needs must
// therefore exist on disk before the recreate, and everything the next agent
// process needs to finish the bookkeeping must exist after it. Both files are
// JSON (machine-written, machine-read) and live in the state directory — the
// one volume both containers share.
package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// Version is the schema version of plan and result. A file of another
	// version is rejected instead of guessed at: the two sides are shipped
	// in one image, so a version mismatch means a stale file from an older
	// image, not a migration.
	Version = 1

	// DefaultProject is the compose project dockops runs under when the
	// operator does not set DOCKOPS_SELF_PROJECT — the same default
	// internal/webhook/status.go uses for its unmanaged detection. There is
	// deliberately one value, not two.
	DefaultProject = "dockops"

	// PlanFileName and ResultFileName are the fixed file names below the
	// state directory. Fixed, not configurable: the agent writes the plan,
	// the helper finds it through the -selfupdate-plan argument, and the next agent
	// start looks for the result before anything else — a configuration
	// knob would only add a way to lose the file.
	PlanFileName   = "selfupdate-plan.json"
	ResultFileName = "selfupdate-result.json"

	// HelperLogFileName is the append-only helper log in the state
	// directory. The helper is the only witness of a recreate that never
	// came back; docker logs of a --rm container die with it, so the file
	// is the diagnosis source.
	HelperLogFileName = "selfupdate-helper.log"

	// RollbackFilePattern names the rollback override the helper writes
	// into the stack directory, the same pattern and shape the in-process
	// phase-11 rollback uses (internal/reconciler apply.go). Two writers,
	// one convention: an operator who sees the file knows what it is.
	RollbackFilePattern = "dockops-rollback-%s.yaml"

	// RollbackFailedTag marks the one outcome that needs a human: the stack
	// runs neither the new nor the old digest. The in-process path uses the
	// same wording, so an alert reads identically no matter which path
	// produced it.
	RollbackFailedTag = "rollback FAILED"

	// KindDeploy marks a plan (and with it a result) that is a full stack
	// deploy instead of an image move. The empty kind means "images", the
	// phase-12 behaviour — the two-valued enum is the migration rule of the
	// schema, so there is deliberately no "images" literal: nothing writes
	// it, and a third value would have no producer to keep honest.
	KindDeploy = "deploy"

	// DockerSocket is the daemon socket the helper inherits from the agent
	// via --volumes-from: docker compose inside the helper talks to the host
	// daemon exactly like the agent, with the same rights.
	DockerSocket = "/var/run/docker.sock"

	// helperNamePrefix is the fixed name prefix of the helper container.
	// The name is what a human greps for in `docker ps` after a failed
	// update; the suffix keeps successive attempts distinguishable.
	helperNamePrefix = "dockops-selfupdate-"
)

// Result statuses. Anything else is rejected when a result is read: an
// unknown status must never be interpreted as success or failure by
// accident.
const (
	// StatusOK means the new image was recreated and passed the gate.
	StatusOK = "ok"
	// StatusRolledBack means the new image failed and the previous digest is
	// running again.
	StatusRolledBack = "rolled_back"
	// StatusRollbackFailed means both digests are in doubt.
	StatusRollbackFailed = "rollback_failed"
	// StatusFailed means the helper's command failed outright. It is only
	// produced by deploy plans (phase 13): a deploy has no digest to roll
	// back to, so there is nothing to call "rolled_back". For an image plan
	// the status stays invalid on purpose — a failed image move that is not
	// a rollback would be a bug the finalization must not guess at.
	StatusFailed = "failed"
)

// Plan is the JSON document the agent writes before it starts the helper.
// The field set is the design contract (docs/specs/2026-10-26-dockops-self-
// update-design.md): the helper must be able to run its compose commands and
// its health gate from this file alone.
type Plan struct {
	// Version is Version; ReadPlan rejects anything else.
	Version int `json:"version"`
	// Project is the compose project of the stack being recreated.
	Project string `json:"project"`
	// StackDir is the stack's working directory, mounted into the helper at
	// the identical path: compose file paths and the generated .env stay
	// valid without any translation.
	StackDir string `json:"stack_dir"`
	// ComposeFiles are the stack's compose files in -f order, relative
	// entries resolved against StackDir.
	ComposeFiles []string `json:"compose_files,omitempty"`
	// EnvFile is the --env-file of the compose call. The agent passes the
	// stack directory's generated .env (the same file compose reads for
	// stack-relative env_file references); empty omits the flag.
	EnvFile string `json:"env_file,omitempty"`
	// Services are the services to recreate (compose up --no-deps targets).
	Services []string `json:"services,omitempty"`
	// Containers is the health gate target list: container names, because a
	// healthcheck belongs to the container, not the service. A missing or
	// shorter list makes the helper fall back to the service name (the
	// helper resolves it through compose ps then).
	Containers []string `json:"containers,omitempty"`
	// GraceSeconds is how long the helper waits before its first compose
	// call, so the agent can finish its pass and stop writing. 0 is legal:
	// the wait is a courtesy, not a correctness requirement.
	GraceSeconds int `json:"grace_seconds"`
	// HealthTimeoutSeconds bounds one health gate (the forward one and the
	// rollback one each). Must be > 0.
	HealthTimeoutSeconds int `json:"health_timeout_seconds"`
	// HealthPollSeconds is the gate's polling interval. Must be > 0.
	HealthPollSeconds int `json:"health_poll_seconds"`
	// Targets are the images being moved, one entry per image. The agent
	// fills repository and previous digest so the helper can render its
	// rollback override without asking a registry or the state file.
	Targets []Target `json:"targets"`

	// Kind selects what the helper does: empty (or "images") means the
	// phase-12 image move, KindDeploy a full stack deploy (phase 13). The
	// field is additive and omitempty because the version stays 1: the OLD
	// image writes the plan, the NEW image executes it — a version bump
	// would reject exactly that hand-over.
	Kind string `json:"kind,omitempty"`
	// Profiles are the stack's compose profiles (--profile, in configured
	// order). A deploy without them silently omits profile services, which
	// the in-process up would have started.
	Profiles []string `json:"profiles,omitempty"`
	// DeployHash is the stack hash the deploy installs; the finalization
	// books it as the deployed hash. Set for kind=deploy only.
	DeployHash string `json:"deploy_hash,omitempty"`
}

// Target is one image of the plan: what to install, where it came from, and
// which digest the rollback pins.
type Target struct {
	// Image is the image reference as the stack uses it (tag form), e.g.
	// "ghcr.io/x/dockops:latest". The helper image is taken from it.
	Image string `json:"image,omitempty"`
	// Repository is the reference without tag or digest; the rollback
	// override pins Repository@Previous.
	Repository string `json:"repository"`
	// Digest is the target digest the stack should run afterwards.
	Digest string `json:"digest"`
	// Previous is the digest the stack ran before, the rollback target.
	// Optional in Validate (a first apply may have none) but required by
	// RollbackOverride.
	Previous string `json:"previous,omitempty"`
	// Service is the compose service the image belongs to.
	Service string `json:"service"`
}

// Result is the JSON document the helper leaves for the next agent start.
// Exactly one of them exists per helper run; the agent reads it once,
// bookkeeps the outcome in the state and deletes it.
type Result struct {
	// Version is Version. It is optional in the document (the design's
	// example omits it): an empty value reads as version 1, any other value
	// is rejected — a result from a future image must not be guessed at.
	Version int `json:"version,omitempty"`
	// Status is one of the Status constants.
	Status string `json:"status"`
	// Digest is the digest the attempt targeted (the plan's digest), so the
	// agent can book success without re-reading the plan. Empty on a
	// rollback is tolerated: the agent then falls back to its recorded
	// ApplyPendingDigest.
	Digest string `json:"digest,omitempty"`
	// Error is the failure text of a rolled_back / rollback_failed / failed
	// run.
	Error string `json:"error,omitempty"`
	// Kind echoes the plan's kind (KindDeploy for a deploy plan, empty for
	// an image move). Additive and optional: a phase-12 result has no kind,
	// and the finalization must know which bookkeeping applies without
	// re-reading a plan that may already be overwritten.
	Kind string `json:"kind,omitempty"`
	// DeployHash echoes the plan's deploy hash, so the deploy bookkeeping
	// can cross-check what it finds in the state.
	DeployHash string `json:"deploy_hash,omitempty"`
	// At is the UTC time the helper wrote its verdict.
	At time.Time `json:"at"`
}

// IsSelf reports whether stackName is the project dockops itself runs under.
// An empty selfProject selects DefaultProject, the same default GET /status
// applies — the detection must not depend on whether the operator spelled
// the default out.
func IsSelf(stackName, selfProject string) bool {
	if selfProject == "" {
		selfProject = DefaultProject
	}
	if stackName == "" || selfProject == "" {
		return false
	}
	return stackName == selfProject
}

// Project returns the effective self project: selfProject when set,
// DefaultProject otherwise. Callers that need to name the self stack (the
// finalization looks up its state entry) use this instead of repeating the
// default.
func Project(selfProject string) string {
	if selfProject == "" {
		return DefaultProject
	}
	return selfProject
}

// PlanPath returns the plan file path below stateDir.
func PlanPath(stateDir string) string { return filepath.Join(stateDir, PlanFileName) }

// ResultPath returns the result file path below stateDir.
func ResultPath(stateDir string) string { return filepath.Join(stateDir, ResultFileName) }

// HelperLogPath returns the helper log path below stateDir.
func HelperLogPath(stateDir string) string { return filepath.Join(stateDir, HelperLogFileName) }

// WriteAtomic marshals v as indented JSON and writes it to path via a
// temporary file in the same directory plus a rename, so a crash (or the
// container dying mid-write) can never leave a truncated plan or result
// behind — the same pattern the state store uses. The file is created with
// 0o600: the documents carry image digests and paths, nothing secret, but
// restrictive defaults cost nothing.
func WriteAtomic(path string, v any) error {
	if path == "" {
		return errors.New("selfupdate: path must not be empty")
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("selfupdate: serialize %s: %w", path, err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".dockops-selfupdate-*")
	if err != nil {
		return fmt.Errorf("selfupdate: create temp file next to %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("selfupdate: chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("selfupdate: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("selfupdate: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("selfupdate: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("selfupdate: rename %s to %s: %w", tmpName, path, err)
	}
	return nil
}

// ReadPlan reads and parses a plan file. A missing file surfaces as an error
// wrapping fs.ErrNotExist (the caller decides whether that is normal); a
// wrong version is rejected, and the plan is validated before it is
// returned — a helper must never start acting on a half-written plan.
func ReadPlan(path string) (Plan, error) {
	var p Plan
	raw, err := os.ReadFile(path)
	if err != nil {
		return p, fmt.Errorf("selfupdate: read plan %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("selfupdate: parse plan %s: %w", path, err)
	}
	if p.Version != Version {
		return p, fmt.Errorf("selfupdate: plan %s: unsupported version %d (want %d)", path, p.Version, Version)
	}
	if err := p.Validate(); err != nil {
		return p, fmt.Errorf("selfupdate: plan %s: %w", path, err)
	}
	return p, nil
}

// ReadResult reads and parses a result file. A missing file surfaces as an
// error wrapping fs.ErrNotExist; an empty file, a wrong version (the field is
// optional in the document, so only a non-zero value is compared) and an
// unknown status are rejected — the finalization must never guess what a
// broken result meant.
func ReadResult(path string) (Result, error) {
	var r Result
	raw, err := os.ReadFile(path)
	if err != nil {
		return r, fmt.Errorf("selfupdate: read result %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return r, fmt.Errorf("selfupdate: result %s is empty", path)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("selfupdate: parse result %s: %w", path, err)
	}
	if r.Version != 0 && r.Version != Version {
		return r, fmt.Errorf("selfupdate: result %s: unsupported version %d (want %d)", path, r.Version, Version)
	}
	switch r.Status {
	case StatusOK, StatusRolledBack, StatusRollbackFailed, StatusFailed:
	default:
		return r, fmt.Errorf("selfupdate: result %s: unknown status %q", path, r.Status)
	}
	return r, nil
}

// Validate checks the plan for everything the helper relies on. It is the
// last line of defence between a buggy agent and a helper that recreates a
// stack with an empty service list (which compose would read as "all
// services", the exact opposite of the intent) or waits forever.
func (p Plan) Validate() error {
	if p.Version != Version {
		return fmt.Errorf("unsupported version %d (want %d)", p.Version, Version)
	}
	if strings.TrimSpace(p.Project) == "" {
		return errors.New("project must not be empty")
	}
	if strings.TrimSpace(p.StackDir) == "" {
		return errors.New("stack_dir must not be empty")
	}
	if len(p.ComposeFiles) == 0 {
		return errors.New("compose_files must not be empty")
	}
	for i, f := range p.ComposeFiles {
		if strings.TrimSpace(f) == "" {
			return fmt.Errorf("compose_files[%d] must not be empty", i)
		}
	}
	for i, profile := range p.Profiles {
		if strings.TrimSpace(profile) == "" {
			return fmt.Errorf("profiles[%d] must not be empty", i)
		}
	}
	// The service list is validated in both kinds, but only a deploy may
	// leave it empty: "all services" is a legitimate deploy (compose up
	// without positional arguments), while an image plan without services
	// would recreate nothing.
	for i, s := range p.Services {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("services[%d] must not be empty", i)
		}
	}
	if p.GraceSeconds < 0 {
		return fmt.Errorf("grace_seconds must not be negative (got %d)", p.GraceSeconds)
	}
	// The health fields stay required for both kinds: the deploy branch
	// ignores them, but a plan that carries no deadlines would be a schema
	// change dressed up as a convenience (and the wait window of the deploy
	// finalization is derived from them).
	if p.HealthTimeoutSeconds <= 0 {
		return fmt.Errorf("health_timeout_seconds must be > 0 (got %d)", p.HealthTimeoutSeconds)
	}
	if p.HealthPollSeconds <= 0 {
		return fmt.Errorf("health_poll_seconds must be > 0 (got %d)", p.HealthPollSeconds)
	}
	switch p.Kind {
	case "":
		// Phase 12: an image move needs its targets and the services they
		// live in.
		if len(p.Services) == 0 {
			return errors.New("services must not be empty")
		}
		if len(p.Targets) == 0 {
			return errors.New("targets must not be empty")
		}
		for i, t := range p.Targets {
			if strings.TrimSpace(t.Service) == "" {
				return fmt.Errorf("targets[%d].service must not be empty", i)
			}
			if strings.TrimSpace(t.Repository) == "" {
				return fmt.Errorf("targets[%d].repository must not be empty", i)
			}
			if strings.TrimSpace(t.Digest) == "" {
				return fmt.Errorf("targets[%d].digest must not be empty", i)
			}
		}
	case KindDeploy:
		// Phase 13: a deploy has no rollback digest, so targets must be
		// empty; the hash it installs is what makes the deploy bookkeeping
		// possible and must be there.
		if len(p.Targets) != 0 {
			return fmt.Errorf("targets must be empty for kind %q (got %d)", KindDeploy, len(p.Targets))
		}
		if strings.TrimSpace(p.DeployHash) == "" {
			return fmt.Errorf("deploy_hash must not be empty for kind %q", KindDeploy)
		}
	default:
		return fmt.Errorf("unknown kind %q (want %q or empty)", p.Kind, KindDeploy)
	}
	return nil
}

// HelperName returns the container name of the helper run: a fixed prefix,
// the sanitized project and the caller's suffix (the agent's Unix time). The
// name is deterministic for a given plan and suffix, which is what makes the
// argument list testable; the suffix is a parameter instead of a clock read
// so the package stays free of hidden time.
func HelperName(p Plan, suffix string) string {
	name := helperNamePrefix + sanitizeName(Project(p.Project))
	if suffix = sanitizeName(suffix); suffix != "" {
		name += "-" + suffix
	}
	return name
}

// sanitizeName reduces a compose project or suffix to the characters docker
// accepts in a container name, so a project name with a slash or a space
// cannot produce an invalid `--name` (and with it a helper that never
// starts).
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Mount is one mount of the agent's own container as docker reports it
// (`docker inspect --format '{{json .Mounts}}'`): the host side (Source) and
// the absolute path inside the container (Destination).
type Mount struct {
	Host     string
	Path     string
	ReadOnly bool
}

// MountArgs renders mounts as the `-v host:path[:ro]` pairs of a docker run
// line. Incomplete entries (no host or no path) are skipped instead of
// producing a mount that docker rejects.
func MountArgs(mounts []Mount) []string {
	args := make([]string, 0, 2*len(mounts))
	for _, m := range mounts {
		if strings.TrimSpace(m.Host) == "" || strings.TrimSpace(m.Path) == "" {
			continue
		}
		spec := m.Host + ":" + m.Path
		if m.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-v", spec)
	}
	return args
}

// HelperArgs builds the docker run argument list (without the "docker"
// binary) that starts the helper:
//
//	run -d --rm --name dockops-selfupdate-<project>-<suffix>
//	    [--user <uid:gid>] [--group-add <gid> ...] --volumes-from <agent container>
//	    [-v <host>:<path>[:ro] ...]
//	    <image> -selfupdate-helper -selfupdate-plan <stateDir>/selfupdate-plan.json
//
// Decisions worth naming:
//   - -d --rm: the helper outlives the agent container that starts it
//     (detached, its own cgroup) and removes itself when it exits. That is
//     the entire point of the phase — a child process would die with the
//     agent during the recreate.
//   - --volumes-from AND explicit mounts: --volumes-from hands over VOLUMES
//     only, never bind mounts. An installation whose state dir, compose
//     checkouts and docker socket are binds — the normal case — therefore
//     got a helper that saw none of them: it could not even read the plan,
//     died before writing a result, and the agent booked "helper left no
//     result" with no diagnosis source. The caller passes the agent's own
//     mounts (docker inspect .Mounts), so the helper sees exactly what the
//     agent sees at the same absolute paths; --volumes-from stays for
//     installs that really use named volumes, where no host path exists.
//   - stateDir is read-write in the agent and stays read-write through the
//     inherited mounts: the plan is read from it, the result and the helper
//     log are written there.
//   - the helper image is the image reference being deployed: the agent has
//     just pulled it, so the helper runs the same version it installs.
//
// selfContainer is the agent's own container (name or ID). Inside a docker
// container os.Hostname() is the short container ID, which docker accepts.
// An empty value is an error rather than a degraded run: without the mounts
// the helper cannot do its job, and starting it anyway would burn an update
// and leave a confusing log behind.
func HelperArgs(p Plan, image, stateDir, suffix, selfContainer string, mounts []Mount, user string, groups []string) ([]string, error) {
	if strings.TrimSpace(selfContainer) == "" {
		return nil, errors.New("helper args: the agent's own container (for --volumes-from) is unknown")
	}
	args := []string{
		"run", "-d", "--rm",
		"--name", HelperName(p, suffix),
	}
	// The helper writes the result and its log into the state directory. Run
	// as the image default (root) it would own both files with 0600, and the
	// agent — a non-root uid inside its own container — could not read its own
	// verdict back: live that showed as "self update: unreadable result file;
	// falling back to the digest comparison" with a deploy that stayed pending
	// forever. user is the agent's own docker user spec (Config.User), so the
	// helper runs with exactly the identity the agent has.
	if u := strings.TrimSpace(user); u != "" {
		args = append(args, "--user", u)
	}
	// Supplementary groups are not inherited by --user: they are docker's
	// group_add entries of the agent (normally the host's docker gid, which
	// owns /var/run/docker.sock). Without them the helper starts, reads the
	// plan and then cannot talk to the daemon:
	//   permission denied while trying to connect to the Docker daemon socket
	for _, g := range groups {
		if g = strings.TrimSpace(g); g != "" {
			args = append(args, "--group-add", g)
		}
	}
	args = append(args, "--volumes-from", selfContainer)
	args = append(args, MountArgs(mounts)...)
	args = append(args,
		image,
		"-selfupdate-helper",
		// The flag name must match cmd/dockops' flag set exactly: the helper
		// parses its arguments with the same flag package as the agent, so a
		// drifted name is "flag provided but not defined" — the process exits
		// with status 2 before it can write a result or a log line. That is
		// what happened live with the abbreviated "-plan": every helper died
		// in under a second and the agent only saw "helper left no result".
		// TestHelperArgsMatchTheCLIFlags guards the name.
		"-selfupdate-plan", PlanPath(stateDir),
	)
	return args, nil
}

// rollbackDocument mirrors the in-process rollback override (see
// internal/reconciler apply.go): one image pin per service, in compose's own
// schema. yaml.Marshal sorts map keys, so the file is deterministic.
type rollbackDocument struct {
	Services map[string]rollbackService `yaml:"services"`
}

type rollbackService struct {
	Image string `yaml:"image"`
}

// RollbackOverride renders the override that pins every target's service to
// repository@previous. The helper writes it into the stack directory and
// passes it after the stack's own compose files, so the later file wins —
// exactly the phase-11 rollback, just executed by the surviving container.
// A target without repository or previous digest is an error: an override
// with a dangling image reference would replace the failure it is meant to
// fix with a compose parse error.
func RollbackOverride(p Plan) ([]byte, error) {
	if len(p.Targets) == 0 {
		return nil, errors.New("rollback override: plan has no targets")
	}
	doc := rollbackDocument{Services: make(map[string]rollbackService, len(p.Targets))}
	for i, t := range p.Targets {
		if strings.TrimSpace(t.Service) == "" {
			return nil, fmt.Errorf("rollback override: targets[%d].service must not be empty", i)
		}
		if strings.TrimSpace(t.Repository) == "" {
			return nil, fmt.Errorf("rollback override: targets[%d].repository must not be empty", i)
		}
		if strings.TrimSpace(t.Previous) == "" {
			return nil, fmt.Errorf("rollback override: targets[%d].previous must not be empty", i)
		}
		doc.Services[t.Service] = rollbackService{Image: t.Repository + "@" + t.Previous}
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("rollback override: render: %w", err)
	}
	return raw, nil
}

// RollbackFileName returns the file name of the rollback override of a
// project, in the same pattern the in-process rollback uses.
func RollbackFileName(project string) string {
	return fmt.Sprintf(RollbackFilePattern, project)
}

// IsNotExist reports whether err (possibly wrapped) means "no such file",
// i.e. the normal case of "no plan / no result yet". Exposed so callers do
// not have to import io/fs for one check.
func IsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
