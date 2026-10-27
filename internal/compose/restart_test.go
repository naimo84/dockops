package compose

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
)

// TestRestartArguments pins the command line of a restart: project name, one
// -f per compose file in configured order, --env-file, then the restart
// subcommand — and nothing else. In particular no "up" and no "pull": a
// restart must not deploy.
func TestRestartArguments(t *testing.T) {
	argsFile := fakeDocker(t)
	e := quietExecutor()
	stackDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "stack.env")

	stack := config.Stack{
		Name:         "web",
		ComposeFiles: []string{"compose.yaml", "override.yaml"},
	}
	if err := e.Restart(context.Background(), stack, stackDir, envFile); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}

	want := []string{
		"compose",
		"-p", "web",
		"-f", filepath.Join(stackDir, "compose.yaml"),
		"-f", filepath.Join(stackDir, "override.yaml"),
		"--env-file", envFile,
		"restart",
	}
	got := recordedArgs(t, argsFile)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Restart() args = %v, want %v", got, want)
	}
	if last := got[len(got)-1]; last != "restart" {
		t.Errorf("last arg = %q, want the restart subcommand", last)
	}
	for _, arg := range got {
		if arg == "up" || arg == "pull" {
			t.Errorf("Restart() args contain %q: a restart must not deploy (%v)", arg, got)
		}
	}
}

// TestRestartWithoutEnvFile: outside a deployment (or for a stack that never
// deployed) there is no env file; the flag is omitted instead of passing an
// empty path docker compose would reject.
func TestRestartWithoutEnvFile(t *testing.T) {
	argsFile := fakeDocker(t)
	e := quietExecutor()
	stackDir := t.TempDir()

	stack := config.Stack{Name: "solo", ComposeFile: "compose.yml"}
	if err := e.Restart(context.Background(), stack, stackDir, ""); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	want := []string{
		"compose",
		"-p", "solo",
		"-f", filepath.Join(stackDir, "compose.yml"),
		"restart",
	}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("Restart() args = %v, want %v", got, want)
	}
}

func TestRestartEmptyProject(t *testing.T) {
	argsFile := fakeDocker(t)
	e := quietExecutor()

	err := e.Restart(context.Background(), config.Stack{ComposeFile: "compose.yaml"}, t.TempDir(), "")
	if err == nil {
		t.Fatal("Restart() with an empty project name error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "project name") {
		t.Errorf("Restart() error = %v, want a project-name hint", err)
	}
	if _, statErr := os.Stat(argsFile); !os.IsNotExist(statErr) {
		t.Errorf("docker was invoked for an empty project name")
	}
}

func TestRestartFailure(t *testing.T) {
	fakeDocker(t)
	t.Setenv("DOCKER_EXIT", "1")
	e := quietExecutor()

	err := e.Restart(context.Background(), config.Stack{Name: "web", ComposeFile: "compose.yaml"}, t.TempDir(), "")
	if err == nil {
		t.Fatal("Restart() with a failing docker error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("Restart() error = %v, want the command line", err)
	}
}
