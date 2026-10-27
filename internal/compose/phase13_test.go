package compose

import (
	"strings"
	"testing"
)

// TestUpArgs: the shared command-line recipe the phase-13 deploy helper uses
// (Request + profiles + env file + services), and the one difference that is
// the caller's decision — --no-deps for a targeted recreate, none for a
// deploy.
func TestUpArgs(t *testing.T) {
	req := Request{Project: "dockops", StackDir: "/stacks/dockops", ComposeFiles: []string{"compose.yaml", "extra.yaml"}}

	got, err := UpArgs(req, nil, "", false, nil)
	if err != nil {
		t.Fatalf("UpArgs() error = %v", err)
	}
	if want := []string{"compose", "-p", "dockops", "-f", "/stacks/dockops/compose.yaml", "-f", "/stacks/dockops/extra.yaml", "up", "-d", "--remove-orphans"}; !equalArgs(got, want) {
		t.Errorf("UpArgs() = %v, want %v", got, want)
	}

	got, err = UpArgs(req, []string{"sops", "ml"}, "/stacks/dockops/.env", true, []string{"app", "db"})
	if err != nil {
		t.Fatalf("UpArgs() error = %v", err)
	}
	want := []string{
		"compose", "-p", "dockops",
		"-f", "/stacks/dockops/compose.yaml", "-f", "/stacks/dockops/extra.yaml",
		"--profile", "sops", "--profile", "ml",
		"--env-file", "/stacks/dockops/.env",
		"up", "-d", "--remove-orphans", "--no-deps",
		"app", "db",
	}
	if !equalArgs(got, want) {
		t.Errorf("UpArgs() = %v, want %v", got, want)
	}
	if !strings.HasSuffix(strings.Join(got, " "), "--no-deps app db") {
		t.Errorf("UpArgs() = %v, want the services after the flags", got)
	}

	// An empty project is the one input the recipe refuses.
	if _, err := UpArgs(Request{StackDir: "/s"}, nil, "", false, nil); err == nil {
		t.Error("UpArgs() without a project must fail")
	}
}
