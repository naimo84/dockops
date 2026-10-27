package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/local/dockops/internal/selfupdate"
)

// TestPhase13ParseMountList covers the DOCKOPS_SELF_MOUNTS escape hatch: the
// override exists for installations where docker's own answer is unusable.
func TestPhase13ParseMountList(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []selfupdate.Mount
	}{
		{name: "empty", in: "", want: nil},
		{
			name: "plain and read-only",
			in:   " /home/docker/dockops:/var/lib/dockops , /var/run/docker.sock:/var/run/docker.sock:ro ",
			want: []selfupdate.Mount{
				{Host: "/home/docker/dockops", Path: "/var/lib/dockops"},
				{Host: "/var/run/docker.sock", Path: "/var/run/docker.sock", ReadOnly: true},
			},
		},
		{
			name: "unparsable entries are dropped",
			in:   "no-colon-here, :/only-path, /only-host:,",
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMountList(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseMountList(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseMountList(%q)[%d] = %+v, want %+v", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestPhase13SelfMountsEnvWins pins the override: when DOCKOPS_SELF_MOUNTS is
// set no docker lookup happens (the value wins, whitespace trimmed).
func TestPhase13SelfMountsEnvWins(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	got := selfMounts(fakeEnv(map[string]string{
		envSelfMounts: "  /home/docker/dockops:/var/lib/dockops  ",
	}), logger)
	if len(got) != 1 || got[0].Host != "/home/docker/dockops" || got[0].Path != "/var/lib/dockops" {
		t.Fatalf("selfMounts() = %+v, want the single env mount", got)
	}
	if got[0].ReadOnly {
		t.Error("selfMounts()[0].ReadOnly = true, want a read-write mount")
	}
}

// TestPhase13SelfMountsSurvivesMissingDocker documents the degraded path: no
// docker answer is a warning, not a crash, and the reconciler then starts the
// helper with volumes only. Hostname is a test binary here, so the inspect
// fails — the same shape as a host without the docker CLI.
func TestPhase13SelfMountsSurvivesMissingDocker(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := selfMounts(fakeEnv(nil), logger); len(got) != 0 {
		t.Fatalf("selfMounts() = %+v, want none without a docker answer", got)
	}
}

// TestPhase13MountArgsOrdering is the cmd-side sanity check that the mounts
// reach docker as run flags before the image (docker would otherwise treat
// them as the container command).
func TestPhase13MountArgsOrdering(t *testing.T) {
	args := selfupdate.MountArgs([]selfupdate.Mount{{Host: "/a", Path: "/b"}})
	if strings.Join(args, " ") != "-v /a:/b" {
		t.Fatalf("MountArgs() = %v, want [-v /a:/b]", args)
	}
}

// TestPhase13SelfUserEnvWins pins the --user override: the helper writes result
// and log into the state dir, so it must run with the agent's own uid — as root
// those files are unreadable for the agent and its deploy never finalizes.
func TestPhase13SelfUserEnvWins(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := selfUser(fakeEnv(map[string]string{envSelfUser: "  1000:4001 "}), logger); got != "1000:4001" {
		t.Fatalf("selfUser() = %q, want the trimmed env value", got)
	}
}
