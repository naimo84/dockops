package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/local/dockops/internal/selfupdate"
)

// deployPlan builds a phase-13 deploy plan in a temp stack directory.
func deployPlan(t *testing.T) selfupdate.Plan {
	t.Helper()
	dir := t.TempDir()
	return selfupdate.Plan{
		Version:      selfupdate.Version,
		Kind:         selfupdate.KindDeploy,
		Project:      "dockops",
		StackDir:     dir,
		ComposeFiles: []string{filepath.Join(dir, "compose.yaml")},
		EnvFile:      filepath.Join(dir, ".env"),
		DeployHash:   "abc123",
		GraceSeconds: 0,
	}
}

// TestHelperDeployRewritesRelativeBinds: the helper's deploy must run compose
// with a generated bind override that names the host path of every relative
// source (issues #18/#23) — the agent's container paths are as unreadable for
// the daemon here as there.
func TestHelperDeployRewritesRelativeBinds(t *testing.T) {
	plan := deployPlan(t)
	if err := os.WriteFile(plan.ComposeFiles[0], []byte(`
services:
  dockops:
    image: dockops:latest
    volumes:
      - ./config.yaml:/app/config.yaml:ro
`), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	var commands []string
	h := newTestHelper(t, plan, func(_ context.Context, args []string) ([]byte, error) {
		commands = append(commands, strings.Join(args, " "))
		return nil, nil
	})
	// The agent mounts the checkout at the identical container path; the host
	// side is the real one.
	h.mounts = []selfupdate.Mount{{Path: plan.StackDir, Host: "/host" + plan.StackDir}}

	if err := h.runPlan(context.Background()); err != nil {
		t.Fatalf("runPlan() error = %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("commands = %v, want exactly the deploy up", commands)
	}
	override := filepath.Join(plan.StackDir, "dockops-binds-dockops.yaml")
	if !strings.Contains(commands[0], "-f "+override) {
		t.Errorf("command = %q, want the bind override", commands[0])
	}
	if res := resultOf(t, plan); res.Status != selfupdate.StatusOK {
		t.Errorf("result.Status = %q, want ok", res.Status)
	}
	if _, err := os.Stat(override); !os.IsNotExist(err) {
		t.Errorf("bind override left behind after the deploy (stat err = %v)", err)
	}
}

// TestHelperDeployRefusesUntranslatableBind: without a mount mapping that
// covers the stack dir the deploy must fail with a result instead of mounting
// an empty directory.
func TestHelperDeployRefusesUntranslatableBind(t *testing.T) {
	plan := deployPlan(t)
	if err := os.WriteFile(plan.ComposeFiles[0], []byte(`
services:
  dockops:
    image: dockops:latest
    volumes:
      - ./config.yaml:/app/config.yaml:ro
`), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	ran := false
	h := newTestHelper(t, plan, func(_ context.Context, args []string) ([]byte, error) {
		ran = true
		return nil, nil
	})
	h.mounts = []selfupdate.Mount{{Path: "/var/lib/dockops", Host: "/home/docker/dockops"}}

	if err := h.runPlan(context.Background()); err != nil {
		t.Fatalf("runPlan() error = %v (a failed deploy is a result, not a process error)", err)
	}
	if ran {
		t.Error("compose ran despite the unresolvable bind source")
	}
	res := resultOf(t, plan)
	if res.Status != selfupdate.StatusFailed || !strings.Contains(res.Error, "outside every mount") {
		t.Errorf("result = %+v, want failed with the bind refusal", res)
	}
}

// TestHelperArgsCarryMountSpecs: the helper receives the same mount list it
// gets as -v arguments, in the container=host spelling.
func TestHelperArgsCarryMountSpecs(t *testing.T) {
	mounts := []selfupdate.Mount{
		{Host: "/home/docker/apps", Path: "/var/lib/dockops/repos/host"},
		{Host: "/home/docker/infrastructure", Path: "/var/lib/dockops/repos/infrastructure"},
	}
	args, err := selfupdate.HelperArgs(helperPlan(t), "dockops:latest", "/state", "1", "agent", mounts, "", nil)
	if err != nil {
		t.Fatalf("HelperArgs() error = %v", err)
	}
	joined := strings.Join(args, " ")
	want := "-selfupdate-mounts /var/lib/dockops/repos/host=/home/docker/apps;/var/lib/dockops/repos/infrastructure=/home/docker/infrastructure"
	if !strings.Contains(joined, want) {
		t.Fatalf("args = %q, want %q", joined, want)
	}
	// The flag must be the one the CLI parses.
	opts, err := parseFlags([]string{"-selfupdate-helper", "-selfupdate-plan", "/state/selfupdate-plan.json", "-selfupdate-mounts", selfupdate.MountSpecs(mounts)}, fakeEnv(nil))
	if err != nil {
		t.Fatalf("parseFlags() error = %v", err)
	}
	if got := selfupdate.ParseMountSpecs(opts.SelfUpdateMounts); len(got) != 2 || got[0].Path != mounts[0].Path || got[0].Host != mounts[0].Host {
		t.Errorf("parseFlags() mounts = %+v, want the agent's two mappings", got)
	}
}

// TestMountSpecsRoundTrip: identical paths (no translation needed) are dropped,
// malformed entries are skipped instead of guessed at.
func TestMountSpecsRoundTrip(t *testing.T) {
	if got := selfupdate.MountSpecs([]selfupdate.Mount{{Host: "/s", Path: "/s"}}); got != "" {
		t.Errorf("MountSpecs(identical) = %q, want empty", got)
	}
	got := selfupdate.ParseMountSpecs("/a=/b;;bogus;/c=/d=e")
	if len(got) != 2 || got[0].Path != "/a" || got[0].Host != "/b" || got[1].Host != "/d=e" {
		t.Errorf("ParseMountSpecs() = %+v, want the two well-formed entries", got)
	}
}
