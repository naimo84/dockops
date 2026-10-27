package main

import (
	"testing"

	"github.com/local/dockops/internal/selfupdate"
)

// TestHelperArgsMatchTheCLIFlags is the guard for a drift that cost two live
// deploy attempts: internal/selfupdate built "-plan" while the CLI only knows
// "-selfupdate-plan". Inside the helper the same flag set parses the same
// arguments, so the container exited with status 2 before it could write a
// result or a log line — the agent could only report "helper left no result",
// with nothing to diagnose from.
//
// The helper's own arguments are the ones after the image, so this test feeds
// exactly those to the real parseFlags: any future rename on one side fails
// here instead of on a host.
func TestHelperArgsMatchTheCLIFlags(t *testing.T) {
	p := deployHelperPlan(t)
	const image = "ghcr.io/x/dockops:test"
	const stateDir = "/var/lib/dockops"

	args, err := selfupdate.HelperArgs(p, image, stateDir, "1700000000", "abc123", nil, "")
	if err != nil {
		t.Fatalf("HelperArgs() error = %v", err)
	}
	if err != nil {
		t.Fatalf("HelperArgs() error = %v", err)
	}

	start := -1
	for i, a := range args {
		if a == image {
			start = i + 1
			break
		}
	}
	if start <= 0 || start >= len(args) {
		t.Fatalf("helper args = %v, want the image followed by the helper flags", args)
	}

	opts, err := parseFlags(args[start:], fakeEnv(nil))
	if err != nil {
		t.Fatalf("parseFlags(%v) error = %v — the helper would exit before writing a result", args[start:], err)
	}
	if !opts.SelfUpdateHelper {
		t.Errorf("SelfUpdateHelper = false, want true for %v", args[start:])
	}
	if want := selfupdate.PlanPath(stateDir); opts.SelfUpdatePlan != want {
		t.Errorf("SelfUpdatePlan = %q, want %q", opts.SelfUpdatePlan, want)
	}
}
