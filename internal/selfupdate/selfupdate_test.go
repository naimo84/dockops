package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validPlan is the smallest plan Validate accepts; single-field tests copy
// it and break exactly one rule.
func validPlan() Plan {
	return Plan{
		Version:              Version,
		Project:              "dockops",
		StackDir:             "/var/lib/dockops/repos/host/dockops",
		ComposeFiles:         []string{"compose.yaml"},
		EnvFile:              "/var/lib/dockops/repos/host/dockops/.env",
		Services:             []string{"dockops"},
		Containers:           []string{"dockops-1"},
		GraceSeconds:         10,
		HealthTimeoutSeconds: 180,
		HealthPollSeconds:    3,
		Targets: []Target{{
			Image:      "ghcr.io/x/dockops:latest",
			Repository: "ghcr.io/x/dockops",
			Digest:     "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			Previous:   "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			Service:    "dockops",
		}},
	}
}

func TestPlanRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := PlanPath(dir)
	want := validPlan()

	if err := WriteAtomic(path, want); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat plan: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("plan mode = %o, want 600", got)
	}
	got, err := ReadPlan(path)
	if err != nil {
		t.Fatalf("ReadPlan() error = %v", err)
	}
	if got.Project != want.Project || got.StackDir != want.StackDir || got.EnvFile != want.EnvFile {
		t.Errorf("ReadPlan() = %+v, want %+v", got, want)
	}
	if len(got.ComposeFiles) != 1 || got.ComposeFiles[0] != "compose.yaml" {
		t.Errorf("ComposeFiles = %v, want [compose.yaml]", got.ComposeFiles)
	}
	if len(got.Services) != 1 || got.Services[0] != "dockops" {
		t.Errorf("Services = %v, want [dockops]", got.Services)
	}
	if len(got.Targets) != 1 || got.Targets[0] != want.Targets[0] {
		t.Errorf("Targets = %+v, want %+v", got.Targets, want.Targets)
	}
	if got.GraceSeconds != 10 || got.HealthTimeoutSeconds != 180 || got.HealthPollSeconds != 3 {
		t.Errorf("timings = %d/%d/%d, want 10/180/3", got.GraceSeconds, got.HealthTimeoutSeconds, got.HealthPollSeconds)
	}
}

func TestReadPlanMissingFile(t *testing.T) {
	_, err := ReadPlan(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("ReadPlan() error = nil, want an error for a missing plan")
	}
	if !IsNotExist(err) {
		t.Errorf("ReadPlan() error = %v, want one wrapping fs.ErrNotExist", err)
	}
}

func TestReadPlanRejectsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	p := validPlan()
	p.Version = 2
	if err := WriteAtomic(PlanPath(dir), p); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	if _, err := ReadPlan(PlanPath(dir)); err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("ReadPlan() error = %v, want an unsupported-version error", err)
	}
}

func TestReadPlanValidates(t *testing.T) {
	dir := t.TempDir()
	p := validPlan()
	p.Project = ""
	if err := WriteAtomic(PlanPath(dir), p); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	if _, err := ReadPlan(PlanPath(dir)); err == nil || !strings.Contains(err.Error(), "project") {
		t.Fatalf("ReadPlan() error = %v, want it to reject the empty project", err)
	}
}

func TestPlanValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Plan)
		want   string
	}{
		{"version", func(p *Plan) { p.Version = 0 }, "version"},
		{"project", func(p *Plan) { p.Project = "" }, "project"},
		{"stack dir", func(p *Plan) { p.StackDir = "  " }, "stack_dir"},
		{"no compose files", func(p *Plan) { p.ComposeFiles = nil }, "compose_files"},
		{"empty compose file", func(p *Plan) { p.ComposeFiles = []string{""} }, "compose_files[0]"},
		{"no services", func(p *Plan) { p.Services = nil }, "services"},
		{"empty service", func(p *Plan) { p.Services = []string{""} }, "services[0]"},
		{"negative grace", func(p *Plan) { p.GraceSeconds = -1 }, "grace_seconds"},
		{"zero health timeout", func(p *Plan) { p.HealthTimeoutSeconds = 0 }, "health_timeout_seconds"},
		{"zero health poll", func(p *Plan) { p.HealthPollSeconds = 0 }, "health_poll_seconds"},
		{"no targets", func(p *Plan) { p.Targets = nil }, "targets"},
		{"target service", func(p *Plan) { p.Targets[0].Service = "" }, "targets[0].service"},
		{"target repository", func(p *Plan) { p.Targets[0].Repository = "" }, "targets[0].repository"},
		{"target digest", func(p *Plan) { p.Targets[0].Digest = "" }, "targets[0].digest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlan()
			tc.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
	if err := validPlan().Validate(); err != nil {
		t.Errorf("Validate() of the valid plan = %v, want nil", err)
	}
	// A zero grace period is legal: the wait only coordinates, it does not
	// guarantee anything.
	p := validPlan()
	p.GraceSeconds = 0
	if err := p.Validate(); err != nil {
		t.Errorf("Validate() with grace_seconds 0 = %v, want nil", err)
	}
}

func TestIsSelf(t *testing.T) {
	tests := []struct {
		stack, project string
		want           bool
	}{
		{"dockops", "dockops", true},
		{"dockops", "", true}, // empty self project = the default
		{"dockops", "other", false},
		{"web", "", false},
		{"", "", false},
		{"", "dockops", false},
	}
	for _, tc := range tests {
		if got := IsSelf(tc.stack, tc.project); got != tc.want {
			t.Errorf("IsSelf(%q, %q) = %v, want %v", tc.stack, tc.project, got, tc.want)
		}
	}
	if got := Project(""); got != DefaultProject {
		t.Errorf("Project(\"\") = %q, want %q", got, DefaultProject)
	}
	if got := Project("custom"); got != "custom" {
		t.Errorf("Project(\"custom\") = %q, want custom", got)
	}
}

func TestHelperArgs(t *testing.T) {
	p := validPlan()
	args, err := HelperArgs(p, "ghcr.io/x/dockops:latest", "/var/lib/dockops", "1700000000", "3f1c2a9b4d5e")
	if err != nil {
		t.Fatalf("HelperArgs() error = %v", err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{
		"run -d --rm",
		"--name dockops-selfupdate-dockops-1700000000",
		"--volumes-from 3f1c2a9b4d5e",
		"ghcr.io/x/dockops:latest -selfupdate-helper -selfupdate-plan /var/lib/dockops/selfupdate-plan.json",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("HelperArgs() = %q, want it to contain %q", got, want)
		}
	}
	// Mounts come from the caller (Deps.SelfMounts) as explicit -v pairs of
	// the agent's HOST paths — never as a -v of the container path, which
	// would mount empty host directories. This case passes none, so the list
	// stays at --volumes-from. The mounts arrive through
	// --volumes-from instead.
	for _, unwanted := range []string{"-v ", p.StackDir, "/var/run/docker.sock"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("HelperArgs() = %q, must not contain %q", got, unwanted)
		}
	}
	if args[len(args)-1] != PlanPath("/var/lib/dockops") {
		t.Errorf("last arg = %q, want the plan path", args[len(args)-1])
	}
	if args[0] != "run" {
		t.Errorf("args[0] = %q, want run (the docker binary is the executor's, not the list's)", args[0])
	}
}

func TestHelperArgsRejectsUnknownSelfContainer(t *testing.T) {
	for _, self := range []string{"", "   "} {
		if _, err := HelperArgs(validPlan(), "img:1", "/var/lib/dockops", "1", self); err == nil {
			t.Errorf("HelperArgs() with self container %q must fail: without --volumes-from the helper has no mounts", self)
		}
	}
}

func TestHelperNameSanitizes(t *testing.T) {
	p := validPlan()
	p.Project = "my/project name"
	if got, want := HelperName(p, "42"), "dockops-selfupdate-my-project-name-42"; got != want {
		t.Errorf("HelperName() = %q, want %q", got, want)
	}
	if got, want := HelperName(validPlan(), ""), "dockops-selfupdate-dockops"; got != want {
		t.Errorf("HelperName() without suffix = %q, want %q", got, want)
	}
	empty := validPlan()
	empty.Project = ""
	if got, want := HelperName(empty, "1"), "dockops-selfupdate-dockops-1"; got != want {
		t.Errorf("HelperName() of an empty project = %q, want the default project (%q)", got, want)
	}
}

func TestRollbackOverride(t *testing.T) {
	p := validPlan()
	p.Targets = append(p.Targets, Target{
		Repository: "ghcr.io/x/sidecar",
		Previous:   "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		Digest:     "sha256:4444444444444444444444444444444444444444444444444444444444444444",
		Service:    "sidecar",
	})
	raw, err := RollbackOverride(p)
	if err != nil {
		t.Fatalf("RollbackOverride() error = %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		"services:",
		// yaml.v3 indents nested mappings by four spaces per level.
		"dockops:\n        image: ghcr.io/x/dockops@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"sidecar:\n        image: ghcr.io/x/sidecar@sha256:3333333333333333333333333333333333333333333333333333333333333333",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RollbackOverride() = %q, want it to contain %q", got, want)
		}
	}
}

func TestRollbackOverrideErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Plan)
		want   string
	}{
		{"no targets", func(p *Plan) { p.Targets = nil }, "no targets"},
		{"no service", func(p *Plan) { p.Targets[0].Service = "" }, "service"},
		{"no repository", func(p *Plan) { p.Targets[0].Repository = "" }, "repository"},
		{"no previous", func(p *Plan) { p.Targets[0].Previous = "" }, "previous"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlan()
			tc.mutate(&p)
			if _, err := RollbackOverride(p); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RollbackOverride() error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestResultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := ResultPath(dir)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	want := Result{Status: StatusRolledBack, Digest: "sha256:abc", Error: "health gate timed out", At: at}
	if err := WriteAtomic(path, want); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	got, err := ReadResult(path)
	if err != nil {
		t.Fatalf("ReadResult() error = %v", err)
	}
	if got.Status != want.Status || got.Digest != want.Digest || got.Error != want.Error {
		t.Errorf("ReadResult() = %+v, want %+v", got, want)
	}
	if !got.At.Equal(at) {
		t.Errorf("At = %s, want %s", got.At, at)
	}
}

func TestReadResult(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(dir, "result.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write result: %v", err)
		}
		return path
	}

	for _, status := range []string{StatusOK, StatusRolledBack, StatusRollbackFailed} {
		path := write(t, `{"status":"`+status+`","at":"2026-10-26T12:00:00Z"}`)
		got, err := ReadResult(path)
		if err != nil {
			t.Fatalf("ReadResult(%s) error = %v", status, err)
		}
		if got.Status != status {
			t.Errorf("Status = %q, want %q", got.Status, status)
		}
	}

	// The version is optional (the design's example omits it), but a
	// different one is rejected instead of guessed at.
	if _, err := ReadResult(write(t, `{"version":1,"status":"ok"}`)); err != nil {
		t.Errorf("ReadResult(version 1) error = %v, want nil", err)
	}
	if _, err := ReadResult(write(t, `{"version":2,"status":"ok"}`)); err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Errorf("ReadResult(version 2) error = %v, want an unsupported-version error", err)
	}
	if _, err := ReadResult(write(t, `{"status":"exploded"}`)); err == nil || !strings.Contains(err.Error(), "unknown status") {
		t.Errorf("ReadResult(unknown status) error = %v, want an unknown-status error", err)
	}
	if _, err := ReadResult(write(t, `   `)); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("ReadResult(empty) error = %v, want an empty-file error", err)
	}
	if _, err := ReadResult(filepath.Join(dir, "absent.json")); !IsNotExist(err) {
		t.Errorf("ReadResult(missing) error = %v, want one wrapping fs.ErrNotExist", err)
	}
}

func TestWriteAtomicCreatesParentOnlyWhenPresent(t *testing.T) {
	// WriteAtomic writes next to the target and therefore needs the
	// directory to exist; a missing plan directory is an error, not a
	// silent MkdirAll (the state dir is the caller's responsibility).
	err := WriteAtomic(filepath.Join(t.TempDir(), "missing", "plan.json"), validPlan())
	if err == nil {
		t.Fatal("WriteAtomic() error = nil, want an error for a missing directory")
	}
}
