package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/local/dockops/internal/selfupdate"
)

// helperPlan is a complete plan for the helper tests; the stack directory
// exists so the rollback override can be written.
func helperPlan(t *testing.T) selfupdate.Plan {
	t.Helper()
	dir := t.TempDir()
	return selfupdate.Plan{
		Version:              selfupdate.Version,
		Project:              "dockops",
		StackDir:             dir,
		ComposeFiles:         []string{filepath.Join(dir, "compose.yaml")},
		EnvFile:              filepath.Join(dir, ".env"),
		Services:             []string{"dockops"},
		Containers:           []string{"dockops-1"},
		GraceSeconds:         0,
		HealthTimeoutSeconds: 1,
		HealthPollSeconds:    1,
		Targets: []selfupdate.Target{{
			Image:      "dockops:latest",
			Repository: "dockops",
			Digest:     "sha256:new",
			Previous:   "sha256:old",
			Service:    "dockops",
		}},
	}
}

// newTestHelper wires a helper with an injected runner, a no-op sleep and a
// log file in the plan's directory — no docker, no wall clock.
func newTestHelper(t *testing.T, plan selfupdate.Plan, run func(context.Context, []string) ([]byte, error)) *helper {
	t.Helper()
	logFile, err := openHelperLog(selfupdate.HelperLogPath(filepath.Dir(plan.ComposeFiles[0])))
	if err != nil {
		t.Fatalf("openHelperLog() error = %v", err)
	}
	t.Cleanup(func() { logFile.Close() })
	return &helper{
		plan:     plan,
		stateDir: filepath.Dir(plan.ComposeFiles[0]),
		log:      logFile,
		run:      run,
		sleep:    func(context.Context, time.Duration) error { return nil },
		now:      func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) },
	}
}

// resultOf reads the result the helper left behind.
func resultOf(t *testing.T, plan selfupdate.Plan) selfupdate.Result {
	t.Helper()
	res, err := selfupdate.ReadResult(selfupdate.ResultPath(filepath.Dir(plan.ComposeFiles[0])))
	if err != nil {
		t.Fatalf("ReadResult() error = %v", err)
	}
	return res
}

func TestParseFlagsSelfUpdateHelper(t *testing.T) {
	// The helper needs no repository: it reads the plan and docker only.
	opts, err := parseFlags([]string{"-selfupdate-helper", "-selfupdate-plan", "/var/lib/dockops/selfupdate-plan.json"}, fakeEnv(nil))
	if err != nil {
		t.Fatalf("parseFlags() error = %v", err)
	}
	if !opts.SelfUpdateHelper || opts.SelfUpdatePlan != "/var/lib/dockops/selfupdate-plan.json" {
		t.Errorf("opts = %+v, want the helper mode with its plan", opts)
	}
	if opts.RepoURL != "" {
		t.Errorf("RepoURL = %q, want empty (no repository in helper mode)", opts.RepoURL)
	}

	if _, err := parseFlags([]string{"-selfupdate-helper"}, fakeEnv(nil)); err == nil ||
		!strings.Contains(err.Error(), "selfupdate-plan") {
		t.Errorf("parseFlags(helper without plan) error = %v, want it to demand -selfupdate-plan", err)
	}

	// The daemon still needs a repository.
	if _, err := parseFlags(nil, fakeEnv(nil)); err == nil {
		t.Error("parseFlags() error = nil, want the missing-repository error")
	}
}

func TestSelfUpdateEnabled(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", true},
		{"on", true},
		{"true", true},
		{"off", false},
		{"OFF", false},
		{" off ", false},
	}
	for _, tc := range tests {
		env := map[string]string{envSelfUpdate: tc.value}
		if got := selfUpdateEnabled(fakeEnv(env)); got != tc.want {
			t.Errorf("selfUpdateEnabled(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
	if !selfUpdateEnabled(fakeEnv(nil)) {
		t.Error("selfUpdateEnabled(unset) = false, want the default on")
	}
}

func TestComposeUpArgs(t *testing.T) {
	p := helperPlan(t)
	args := composeUpArgs(p, nil)
	got := strings.Join(args, " ")
	want := "compose -p dockops -f " + p.ComposeFiles[0] + " --env-file " + p.EnvFile + " up -d --no-deps dockops"
	if got != want {
		t.Errorf("composeUpArgs() = %q, want %q", got, want)
	}

	override := filepath.Join(p.StackDir, selfupdate.RollbackFileName(p.Project))
	args = composeUpArgs(p, []string{override})
	got = strings.Join(args, " ")
	if !strings.Contains(got, "-f "+p.ComposeFiles[0]+" -f "+override) {
		t.Errorf("composeUpArgs() = %q, want the override after the stack file", got)
	}
}

func TestComposeUpArgsResolvesRelativeFiles(t *testing.T) {
	// The helper's working directory is not the stack directory, so relative
	// plan paths must be joined (the loaded configuration is normally
	// absolute, but the helper must not depend on that).
	p := helperPlan(t)
	p.ComposeFiles = []string{"compose.yaml", "override.yaml"}
	args := composeUpArgs(p, nil)
	got := strings.Join(args, " ")
	for _, want := range []string{
		"-f " + filepath.Join(p.StackDir, "compose.yaml"),
		"-f " + filepath.Join(p.StackDir, "override.yaml"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("composeUpArgs() = %q, want it to contain %q", got, want)
		}
	}
}

func TestHelperPlanSuccess(t *testing.T) {
	plan := helperPlan(t)
	var commands [][]string
	h := newTestHelper(t, plan, func(_ context.Context, args []string) ([]byte, error) {
		commands = append(commands, args)
		switch {
		case contains(args, "inspect"):
			return []byte("running|healthy"), nil
		}
		return nil, nil
	})

	if err := h.runPlan(context.Background()); err != nil {
		t.Fatalf("runPlan() error = %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("commands = %v, want one up and one inspect", commands)
	}
	if !contains(commands[0], "up") || contains(commands[0], "inspect") {
		t.Errorf("first command = %v, want the compose up", commands[0])
	}
	res := resultOf(t, plan)
	if res.Status != selfupdate.StatusOK || res.Digest != "sha256:new" || res.Error != "" {
		t.Errorf("result = %+v, want ok with the target digest", res)
	}
	if !res.At.Equal(time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("At = %s, want the injected clock", res.At)
	}
	logPath := selfupdate.HelperLogPath(h.stateDir)
	logInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat helper log: %v", err)
	}
	if got := logInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("helper log mode = %o, want 600", got)
	}
	logRaw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read helper log: %v", err)
	}
	for _, want := range []string{"helper started", "health gate passed", "helper finished: status=ok"} {
		if !strings.Contains(string(logRaw), want) {
			t.Errorf("helper log = %q, want it to contain %q", logRaw, want)
		}
	}
}

func TestHelperRunsGraceBeforeCompose(t *testing.T) {
	plan := helperPlan(t)
	plan.GraceSeconds = 3
	var slept time.Duration
	h := newTestHelper(t, plan, func(_ context.Context, args []string) ([]byte, error) {
		if slept == 0 {
			t.Error("compose ran before the grace period elapsed")
		}
		return []byte("running|"), nil
	})
	h.sleep = func(_ context.Context, d time.Duration) error {
		slept = d
		return nil
	}
	if err := h.runPlan(context.Background()); err != nil {
		t.Fatalf("runPlan() error = %v", err)
	}
	if slept != 3*time.Second {
		t.Errorf("grace = %s, want 3s", slept)
	}
}

func TestHelperRollback(t *testing.T) {
	plan := helperPlan(t)
	rollbackFile := filepath.Join(plan.StackDir, selfupdate.RollbackFileName(plan.Project))
	ups := 0
	h := newTestHelper(t, plan, func(_ context.Context, args []string) ([]byte, error) {
		switch {
		case contains(args, "inspect"):
			return []byte("running|healthy"), nil
		case contains(args, "up"):
			ups++
			if !contains(args, rollbackFile) {
				return nil, errors.New("container failed to start")
			}
			return nil, nil
		}
		return nil, nil
	})

	if err := h.runPlan(context.Background()); err != nil {
		t.Fatalf("runPlan() error = %v", err)
	}
	if ups != 2 {
		t.Errorf("compose up calls = %d, want the forward and the rollback attempt", ups)
	}
	res := resultOf(t, plan)
	if res.Status != selfupdate.StatusRolledBack {
		t.Fatalf("result = %+v, want rolled_back", res)
	}
	if !strings.Contains(res.Error, "container failed to start") || !strings.Contains(res.Error, "compose up") {
		t.Errorf("result.Error = %q, want the cause", res.Error)
	}
	// The override is a temporary artefact of the attempt.
	if _, err := os.Stat(rollbackFile); !os.IsNotExist(err) {
		t.Errorf("rollback override still exists (stat err = %v), want it removed", err)
	}
}

func TestHelperRollbackFailed(t *testing.T) {
	plan := helperPlan(t)
	h := newTestHelper(t, plan, func(_ context.Context, args []string) ([]byte, error) {
		if contains(args, "inspect") {
			return []byte("running|healthy"), nil
		}
		if contains(args, "up") {
			return nil, errors.New("compose up boom")
		}
		return nil, nil
	})

	if err := h.runPlan(context.Background()); err != nil {
		t.Fatalf("runPlan() error = %v", err)
	}
	res := resultOf(t, plan)
	if res.Status != selfupdate.StatusRollbackFailed {
		t.Fatalf("result = %+v, want rollback_failed", res)
	}
	if !strings.Contains(res.Error, selfupdate.RollbackFailedTag) {
		t.Errorf("result.Error = %q, want the %q marker", res.Error, selfupdate.RollbackFailedTag)
	}
	if res.Digest != "sha256:new" {
		t.Errorf("result.Digest = %q, want the target digest (the agent blocks it)", res.Digest)
	}
}

func TestHelperHealthGateUnhealthyIsHard(t *testing.T) {
	plan := helperPlan(t)
	h := newTestHelper(t, plan, func(_ context.Context, args []string) ([]byte, error) {
		if contains(args, "inspect") {
			return []byte("running|unhealthy"), nil
		}
		return nil, nil
	})
	err := h.healthGate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Fatalf("healthGate() error = %v, want a hard unhealthy failure", err)
	}
}

func TestHelperHealthGateWaitsOnCreated(t *testing.T) {
	plan := helperPlan(t)
	calls := 0
	h := newTestHelper(t, plan, func(_ context.Context, args []string) ([]byte, error) {
		if contains(args, "inspect") {
			calls++
			if calls == 1 {
				return []byte("created|"), nil
			}
			return []byte("running|healthy"), nil
		}
		return nil, nil
	})
	if err := h.healthGate(context.Background()); err != nil {
		t.Fatalf("healthGate() error = %v, want nil after the container started", err)
	}
	if calls < 2 {
		t.Errorf("inspect calls = %d, want at least two (created is polled)", calls)
	}
}

func TestHelperGateTargetsFallBackToServices(t *testing.T) {
	plan := helperPlan(t)
	h := newTestHelper(t, plan, nil)
	if got := h.gateTargets(); len(got) != 1 || got[0] != "dockops-1" {
		t.Errorf("gateTargets() = %v, want the plan's container name", got)
	}
	// Without a container list the gate works on service names; inspect
	// then resolves them through compose ps (see the next test).
	plan.Containers = nil
	h = newTestHelper(t, plan, nil)
	if got := h.gateTargets(); len(got) != 1 || got[0] != "dockops" {
		t.Errorf("gateTargets() without containers = %v, want the service names", got)
	}
}

func TestHelperInspectFallsBackToServiceContainers(t *testing.T) {
	plan := helperPlan(t)
	h := newTestHelper(t, plan, func(_ context.Context, args []string) ([]byte, error) {
		switch {
		case contains(args, "inspect") && len(args) > 0 && args[len(args)-1] == "dockops":
			// A service name is not a container: docker refuses it.
			return nil, errors.New("no such object: dockops")
		case contains(args, "inspect"):
			return []byte("running|healthy"), nil
		case contains(args, "ps"):
			return []byte("container-id-1\n"), nil
		}
		return nil, nil
	})
	state, health, err := h.inspect(context.Background(), "dockops")
	if err != nil {
		t.Fatalf("inspect() error = %v", err)
	}
	if state != "running" || health != "healthy" {
		t.Errorf("inspect() = %q/%q, want running/healthy", state, health)
	}
}

// contains reports whether args carries the given element (substring for
// paths: the rollback override is absolute).
func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want || (strings.Contains(want, "/") && strings.Contains(a, want)) {
			return true
		}
	}
	return false
}
