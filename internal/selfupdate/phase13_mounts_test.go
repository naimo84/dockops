package selfupdate

import (
	"strings"
	"testing"
)

// TestPhase13MountArgs covers the rendering of the agent's mounts: every bind
// becomes a -v pair, a missing host or path is dropped instead of producing a
// mount docker would reject, and a read-only mount keeps its :ro suffix.
func TestPhase13MountArgs(t *testing.T) {
	tests := []struct {
		name   string
		mounts []Mount
		want   []string
	}{
		{
			name:   "none",
			mounts: nil,
			want:   nil,
		},
		{
			name: "read-write and read-only",
			mounts: []Mount{
				{Host: "/home/docker/dockops", Path: "/var/lib/dockops"},
				{Host: "/etc/group", Path: "/etc/group", ReadOnly: true},
			},
			want: []string{"-v", "/home/docker/dockops:/var/lib/dockops", "-v", "/etc/group:/etc/group:ro"},
		},
		{
			name: "incomplete entries are skipped",
			mounts: []Mount{
				{Host: "", Path: "/var/lib/dockops"},
				{Host: "/home/docker/apps", Path: ""},
				{Host: "  ", Path: "  "},
				{Host: "/var/run/docker.sock", Path: "/var/run/docker.sock"},
			},
			want: []string{"-v", "/var/run/docker.sock:/var/run/docker.sock"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MountArgs(tc.mounts)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("MountArgs() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPhase13HelperArgsCarryMounts pins the place of the -v pairs: after the
// --volumes-from hand-over and before the image, so docker reads them as run
// flags and not as the container command.
func TestPhase13HelperArgsCarryMounts(t *testing.T) {
	p := deployPlan()
	image := "ghcr.io/x/dockops:test"
	args, err := HelperArgs(p, image, "/var/lib/dockops", "1700000000", "abc123",
		Mount{Host: "/home/docker/dockops", Path: "/var/lib/dockops"},
		Mount{Host: "/var/run/docker.sock", Path: "/var/run/docker.sock", ReadOnly: true},
	)
	if err != nil {
		t.Fatalf("HelperArgs() error = %v", err)
	}
	joined := strings.Join(args, " ")
	want := "run -d --rm --name " + HelperName(p, "1700000000") +
		" --volumes-from abc123" +
		" -v /home/docker/dockops:/var/lib/dockops" +
		" -v /var/run/docker.sock:/var/run/docker.sock:ro" +
		" " + image + " -selfupdate-helper -selfupdate-plan " + PlanPath("/var/lib/dockops")
	if joined != want {
		t.Errorf("HelperArgs() = %q, want %q", joined, want)
	}
}

// TestPhase13HelperArgsStillNeedTheSelfContainer guards the fail-loud rule:
// without the agent's own container there is nothing to inherit and no
// shipment to guess at.
func TestPhase13HelperArgsStillNeedTheSelfContainer(t *testing.T) {
	if _, err := HelperArgs(deployPlan(), "img", "/var/lib/dockops", "1", "  ", Mount{Host: "/a", Path: "/b"}); err == nil {
		t.Fatal("HelperArgs() error = nil, want an error for a missing self container")
	}
}
