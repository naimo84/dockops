package reconciler

import (
	"context"
	"strings"
	"testing"

	"github.com/local/dockops/internal/selfupdate"
)

// TestPhase13HelperSeesTheAgentMounts is the regression test for the failure
// that only showed live: --volumes-from inherits VOLUMES, never bind mounts.
// The devbox installation mounts its state dir, the compose checkouts and the
// docker socket as binds, so the helper started with none of them: it could
// not read the plan, exited before writing a result and the agent booked
// "self deploy: helper left no result" without a diagnose source.
//
// The reconciler therefore passes Deps.SelfMounts on to HelperArgs, which
// re-declares every bind as -v.
func TestPhase13HelperSeesTheAgentMounts(t *testing.T) {
	f, rec, helper := selfDeployFixture(t)
	r := f.phase13Reconciler(rec, helper.run, phase13SelfImage)
	r.selfMounts = []selfupdate.Mount{
		{Host: "/etc/group", Path: "/etc/group", ReadOnly: true},
		{Host: "/home/docker/dockops", Path: "/var/lib/dockops"},
		{Host: "/home/docker/apps", Path: "/var/lib/dockops/repos/host"},
		{Host: "/var/run/docker.sock", Path: "/var/run/docker.sock"},
	}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := helper.count(); got != 1 {
		t.Fatalf("helper runs = %d, want exactly one", got)
	}
	args := helper.lastArgs()
	joined := strings.Join(args, " ")

	want := []string{
		"-volumes-from " + testSelfContainer, // kept: installs with named volumes need it
		"-v /etc/group:/etc/group:ro",
		"-v /home/docker/dockops:/var/lib/dockops",
		"-v /home/docker/apps:/var/lib/dockops/repos/host",
		"-v /var/run/docker.sock:/var/run/docker.sock",
	}
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("helper args = %q, want it to contain %q", joined, w)
		}
	}
	// The mounts are flags of `docker run`: they must come before the image,
	// otherwise docker treats them as the container command.
	imageAt := strings.Index(joined, phase13SelfImage)
	if imageAt < 0 {
		t.Fatalf("helper args = %q, want the helper image %q", joined, phase13SelfImage)
	}
	for _, flag := range []string{"-v /etc/group:", "-v /home/docker/dockops:", "-v /var/run/docker.sock:"} {
		at := strings.Index(joined, flag)
		if at < 0 || at > imageAt {
			t.Errorf("helper args = %q, want %q before the image (position %d, image at %d)", joined, flag, at, imageAt)
		}
	}
}

// TestPhase13HelperWithoutMountsKeepsVolumesFrom pins the compatibility shape:
// with no known mounts the argument list is the pre-phase-12 one. That is
// correct for a named-volume installation (--volumes-from carries it) and it
// keeps the phase-12 tests honest, which never pass mounts.
func TestPhase13HelperWithoutMountsKeepsVolumesFrom(t *testing.T) {
	f, rec, helper := selfDeployFixture(t)
	r := f.phase13Reconciler(rec, helper.run, phase13SelfImage)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	joined := strings.Join(helper.lastArgs(), " ")
	if strings.Contains(joined, "-v ") {
		t.Errorf("helper args = %q, want no -v without known mounts", joined)
	}
	if !strings.Contains(joined, "--volumes-from "+testSelfContainer) {
		t.Errorf("helper args = %q, want --volumes-from", joined)
	}
}
