package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/local/dockops/internal/selfupdate"
)

// deployHelperPlan is a complete phase-13 deploy plan for the helper tests:
// no targets, a hash, profiles and two services ("all" would be an empty
// list).
func deployHelperPlan(t *testing.T) selfupdate.Plan {
	t.Helper()
	dir := t.TempDir()
	return selfupdate.Plan{
		Version:              selfupdate.Version,
		Kind:                 selfupdate.KindDeploy,
		Project:              "dockops",
		StackDir:             dir,
		ComposeFiles:         []string{filepath.Join(dir, "compose.yaml")},
		EnvFile:              filepath.Join(dir, ".env"),
		Services:             []string{"dockops", "sidecar"},
		GraceSeconds:         0,
		HealthTimeoutSeconds: 90,
		HealthPollSeconds:    3,
		Profiles:             []string{"sops"},
		DeployHash:           "hash-new",
	}
}

func TestPhase13SelfImageEnvWins(t *testing.T) {
	// DOCKOPS_SELF_IMAGE is the documented override: when it is set the
	// startup lookup must not ask docker at all (the value wins, whitespace
	// trimmed).
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := selfImage(fakeEnv(map[string]string{envSelfImage: "  ghcr.io/x/dockops:1.2.3  "}), logger); got != "ghcr.io/x/dockops:1.2.3" {
		t.Errorf("selfImage() = %q, want the trimmed env value", got)
	}
}

func TestPhase13DeployUpArgs(t *testing.T) {
	p := deployHelperPlan(t)
	args, err := deployUpArgs(p)
	if err != nil {
		t.Fatalf("deployUpArgs() error = %v", err)
	}
	got := strings.Join(args, " ")
	want := "compose -p dockops -f " + p.ComposeFiles[0] + " --profile sops --env-file " + p.EnvFile +
		" up -d --remove-orphans dockops sidecar"
	if got != want {
		t.Errorf("deployUpArgs() = %q, want %q", got, want)
	}
	// A deploy is a normal up: dependencies resolve, nothing is pinned to
	// the named services.
	if strings.Contains(got, "--no-deps") {
		t.Errorf("deployUpArgs() = %q, want no --no-deps (a deploy is not a targeted recreate)", got)
	}

	// Without services the command line ends at the flags: compose reads
	// that as "every service", exactly like the in-process Up.
	p.Services = nil
	p.Profiles = nil
	args, err = deployUpArgs(p)
	if err != nil {
		t.Fatalf("deployUpArgs() error = %v", err)
	}
	got = strings.Join(args, " ")
	if !strings.HasSuffix(got, "up -d --remove-orphans") {
		t.Errorf("deployUpArgs() without services = %q, want the flags last", got)
	}
	if strings.Contains(got, "--profile") {
		t.Errorf("deployUpArgs() = %q, want no --profile without profiles", got)
	}
}

func TestPhase13HelperDeployPlan(t *testing.T) {
	plan := deployHelperPlan(t)
	var commands [][]string
	h := newTestHelper(t, plan, func(_ context.Context, args []string) ([]byte, error) {
		commands = append(commands, args)
		if contains(args, "inspect") {
			t.Error("deploy plan must not run the health gate (no inspect)")
		}
		return []byte(" Container dockops  Started\n"), nil
	})

	if err := h.runPlan(context.Background()); err != nil {
		t.Fatalf("runPlan() error = %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("commands = %v, want exactly one compose up", commands)
	}
	joined := strings.Join(commands[0], " ")
	for _, want := range []string{
		"compose -p dockops",
		"-f " + plan.ComposeFiles[0],
		"--profile sops",
		"--env-file " + plan.EnvFile,
		"up -d --remove-orphans dockops sidecar",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("command = %q, want it to contain %q", joined, want)
		}
	}
	if strings.Contains(joined, "--no-deps") {
		t.Errorf("command = %q, want no --no-deps", joined)
	}

	res := resultOf(t, plan)
	if res.Status != selfupdate.StatusOK {
		t.Fatalf("result = %+v, want ok", res)
	}
	if res.Kind != selfupdate.KindDeploy {
		t.Errorf("result.Kind = %q, want %q", res.Kind, selfupdate.KindDeploy)
	}
	if res.DeployHash != plan.DeployHash {
		t.Errorf("result.DeployHash = %q, want %q", res.DeployHash, plan.DeployHash)
	}
	if res.Error != "" {
		t.Errorf("result.Error = %q, want empty", res.Error)
	}
	if !res.At.Equal(time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("At = %s, want the injected clock", res.At)
	}
	// No rollback artefact: a deploy has nothing to roll back to.
	rollback := filepath.Join(plan.StackDir, selfupdate.RollbackFileName(plan.Project))
	if _, err := os.Stat(rollback); !os.IsNotExist(err) {
		t.Errorf("rollback override exists (stat err = %v), want none in deploy mode", err)
	}
}

func TestPhase13HelperDeployFailure(t *testing.T) {
	plan := deployHelperPlan(t)
	ups := 0
	h := newTestHelper(t, plan, func(_ context.Context, args []string) ([]byte, error) {
		if contains(args, "up") {
			ups++
			return nil, errors.New("service dockops failed to start")
		}
		return nil, nil
	})

	if err := h.runPlan(context.Background()); err != nil {
		t.Fatalf("runPlan() error = %v (a failed deploy is a result, not a process error)", err)
	}
	if ups != 1 {
		t.Errorf("compose up calls = %d, want exactly one (no rollback attempt)", ups)
	}
	res := resultOf(t, plan)
	if res.Status != selfupdate.StatusFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if !strings.Contains(res.Error, "failed to start") {
		t.Errorf("result.Error = %q, want the compose failure", res.Error)
	}
	if res.Kind != selfupdate.KindDeploy || res.DeployHash != plan.DeployHash {
		t.Errorf("result = %+v, want kind and hash carried", res)
	}
	rollback := filepath.Join(plan.StackDir, selfupdate.RollbackFileName(plan.Project))
	if _, err := os.Stat(rollback); !os.IsNotExist(err) {
		t.Errorf("rollback override exists (stat err = %v), want none", err)
	}
	// The helper log is the only witness of a failed recreate.
	raw, err := os.ReadFile(selfupdate.HelperLogPath(h.stateDir))
	if err != nil {
		t.Fatalf("read helper log: %v", err)
	}
	for _, want := range []string{"kind=deploy", "up -d --remove-orphans", "deploy failed", "status=failed"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("helper log = %q, want it to contain %q", raw, want)
		}
	}
}

func TestPhase13HelperImagePlanIgnoresDeployFlags(t *testing.T) {
	// The phase-12 path stays untouched: an image plan never runs the deploy
	// command line (no profiles flag, --no-deps present).
	p := helperPlan(t)
	p.Profiles = []string{"sops"}
	args, err := deployUpArgs(p)
	if err != nil {
		t.Fatalf("deployUpArgs() error = %v", err)
	}
	if strings.Contains(strings.Join(args, " "), "--no-deps") {
		t.Errorf("args = %v, want no --no-deps in the deploy command line", args)
	}
	if got := strings.Join(composeUpArgs(p, nil), " "); !strings.Contains(got, "--no-deps") {
		t.Errorf("composeUpArgs() = %q, want the phase-12 flag set (--no-deps)", got)
	}
}
