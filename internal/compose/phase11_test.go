package compose

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
)

// TestPullArguments pins the pull command line of the phase-11 apply step:
// project, stack files, the pull subcommand and the services positionally.
func TestPullArguments(t *testing.T) {
	argsFile := fakeDocker(t)
	stackDir := t.TempDir()
	req := RequestFor(config.Stack{Name: "web", ComposeFile: "compose.yaml"}, stackDir)

	if err := quietExecutor().Pull(context.Background(), req, []string{"app", "worker"}); err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	want := []string{
		"compose", "-p", "web",
		"-f", stackDir + "/compose.yaml",
		"pull", "app", "worker",
	}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("Pull() args = %v, want %v", got, want)
	}
}

// TestPullWithoutServicesPullsTheProject: an empty service list means "the
// whole project" — no positional arguments at all.
func TestPullWithoutServicesPullsTheProject(t *testing.T) {
	argsFile := fakeDocker(t)
	stackDir := t.TempDir()
	req := RequestFor(config.Stack{Name: "web", ComposeFile: "compose.yaml"}, stackDir)

	if err := quietExecutor().Pull(context.Background(), req, nil); err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	want := []string{"compose", "-p", "web", "-f", stackDir + "/compose.yaml", "pull"}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("Pull() args = %v, want %v (empty list = whole project)", got, want)
	}
}

// TestUpOverrideArgumentOrder is the core of the rollback path: the override
// files must come after the stack files (later file wins), followed by the
// up flags and the positional services.
func TestUpOverrideArgumentOrder(t *testing.T) {
	argsFile := fakeDocker(t)
	stackDir := t.TempDir()
	req := RequestFor(config.Stack{
		Name:         "web",
		ComposeFiles: []string{"base.yaml", "override.yaml"},
	}, stackDir)
	rollback := stackDir + "/dockops-rollback-web.yaml"

	if err := quietExecutor().UpOverride(context.Background(), req, []string{rollback}, []string{"app"}); err != nil {
		t.Fatalf("UpOverride() error = %v", err)
	}
	want := []string{
		"compose", "-p", "web",
		"-f", stackDir + "/base.yaml",
		"-f", stackDir + "/override.yaml",
		"-f", rollback,
		"up", "-d", "--remove-orphans", "--no-deps",
		"app",
	}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("UpOverride() args = %v, want %v", got, want)
	}
}

// TestUpOverrideWithoutExtraFiles is the forward direction of the apply step:
// same up path as Up, without override files, with --no-deps and positionals.
func TestUpOverrideWithoutExtraFiles(t *testing.T) {
	argsFile := fakeDocker(t)
	stackDir := t.TempDir()
	req := RequestFor(config.Stack{Name: "web", ComposeFile: "compose.yaml"}, stackDir)

	if err := quietExecutor().UpOverride(context.Background(), req, nil, []string{"app"}); err != nil {
		t.Fatalf("UpOverride() error = %v", err)
	}
	want := []string{
		"compose", "-p", "web",
		"-f", stackDir + "/compose.yaml",
		"up", "-d", "--remove-orphans", "--no-deps",
		"app",
	}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("UpOverride() args = %v, want %v", got, want)
	}
}

// TestUpOverrideRelativeExtraFile: a relative override file resolves against
// the stack directory, like the stack's own compose files.
func TestUpOverrideRelativeExtraFile(t *testing.T) {
	argsFile := fakeDocker(t)
	stackDir := t.TempDir()
	req := RequestFor(config.Stack{Name: "web", ComposeFile: "compose.yaml"}, stackDir)

	if err := quietExecutor().UpOverride(context.Background(), req, []string{"rollback.yaml"}, []string{"app"}); err != nil {
		t.Fatalf("UpOverride() error = %v", err)
	}
	got := recordedArgs(t, argsFile)
	if len(got) < 7 || got[6] != stackDir+"/rollback.yaml" {
		t.Errorf("UpOverride() args = %v, want the override resolved to %s", got, stackDir+"/rollback.yaml")
	}
}

// TestUpOverrideRejectsEmptyProject guards the invariant every compose call
// shares: a command without -p would act on a directory-derived project.
func TestUpOverrideRejectsEmptyProject(t *testing.T) {
	fakeDocker(t)
	err := quietExecutor().UpOverride(context.Background(), Request{}, nil, []string{"app"})
	if err == nil || !strings.Contains(err.Error(), "project name") {
		t.Fatalf("UpOverride() error = %v, want a project name error", err)
	}
	if err := quietExecutor().Pull(context.Background(), Request{}, nil); err == nil {
		t.Fatal("Pull() error = nil, want a project name error")
	}
}

// TestPsProjectReportsService checks the phase-11 field of the ps parsing:
// the service name is carried through, and a missing key stays empty instead
// of being guessed.
func TestPsProjectReportsService(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, `[
		{"Service":"app","Name":"web-app-1","State":"running","Health":"starting"},
		{"Name":"legacy-1","State":"running","Health":"healthy"}
	]`)

	containers, err := quietExecutor().PsProject(context.Background(), "web")
	if err != nil {
		t.Fatalf("PsProject() error = %v", err)
	}
	want := []Container{
		{Service: "app", Name: "web-app-1", State: "running", Health: "starting"},
		{Name: "legacy-1", State: "running", Health: "healthy"},
	}
	if !reflect.DeepEqual(containers, want) {
		t.Errorf("PsProject() = %+v, want %+v", containers, want)
	}
	if containers[1].Service != "" {
		t.Errorf("Service = %q for a row without the key, want empty (no guessing)", containers[1].Service)
	}
}
