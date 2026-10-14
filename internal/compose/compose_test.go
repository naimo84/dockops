package compose

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
)

// fakeDocker writes a shell script named "docker" into a fresh directory
// and prepends it to PATH. The fake dumps its arguments one per line into
// $DOCKER_ARGS_FILE, echoes stdin-free noise to stdout/stderr and exits with
// $DOCKER_EXIT (default 0).
func fakeDocker(t *testing.T) (argsFile string) {
	t.Helper()
	binDir := t.TempDir()
	argsFile = filepath.Join(t.TempDir(), "args.log")
	script := `#!/bin/sh
for arg in "$@"; do
	echo "$arg" >> "$DOCKER_ARGS_FILE"
done
echo "fake docker stdout"
echo "fake docker stderr" >&2
exit ${DOCKER_EXIT:-0}
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("DOCKER_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

// recordedArgs returns the arguments the fake received for one invocation.
func recordedArgs(t *testing.T, argsFile string) []string {
	t.Helper()
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		if os.IsNotExist(err) {
			t.Fatal("fake docker was not invoked")
		}
		t.Fatalf("read args file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func quietExecutor() *Executor {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), "")
}

func TestUpArguments(t *testing.T) {
	argsFile := fakeDocker(t)
	e := quietExecutor()
	stackDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "stack.env")

	stack := config.Stack{
		Name:        "web",
		ComposeFile: "compose.yaml",
		Profiles:    []string{"debug", "extra"},
		Services:    []string{"api", "worker"},
	}
	if err := e.Up(context.Background(), stack, stackDir, envFile); err != nil {
		t.Fatalf("Up() error = %v", err)
	}

	want := []string{
		"compose",
		"-p", "web",
		"-f", filepath.Join(stackDir, "compose.yaml"),
		"--profile", "debug",
		"--profile", "extra",
		"--env-file", envFile,
		"up", "-d", "--remove-orphans",
		"api", "worker", // positional services, no "--" separator
	}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("Up() args = %v, want %v", got, want)
	}
}

func TestUpMinimalArguments(t *testing.T) {
	argsFile := fakeDocker(t)
	e := quietExecutor()
	stackDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "stack.env")

	stack := config.Stack{Name: "solo", ComposeFile: "compose.yml"}
	if err := e.Up(context.Background(), stack, stackDir, envFile); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	want := []string{
		"compose",
		"-p", "solo",
		"-f", filepath.Join(stackDir, "compose.yml"),
		"--env-file", envFile,
		"up", "-d", "--remove-orphans",
	}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("Up() args = %v, want %v", got, want)
	}
}

func TestUpAbsoluteComposeFilePassesThrough(t *testing.T) {
	argsFile := fakeDocker(t)
	e := quietExecutor()
	stackDir := t.TempDir()
	composeFile := filepath.Join(t.TempDir(), "elsewhere", "compose.yaml")
	if err := os.MkdirAll(filepath.Dir(composeFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	stack := config.Stack{Name: "web", ComposeFile: composeFile}
	if err := e.Up(context.Background(), stack, stackDir, filepath.Join(t.TempDir(), "e.env")); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	got := recordedArgs(t, argsFile)
	if got[4] != composeFile {
		t.Errorf("-f = %q, want absolute path %q passed through", got[4], composeFile)
	}
}

func TestDownArguments(t *testing.T) {
	argsFile := fakeDocker(t)
	e := quietExecutor()
	stackDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "stack.env")

	stack := config.Stack{
		Name:        "web",
		ComposeFile: "compose.yaml",
		// Profiles and services must NOT leak into down.
		Profiles: []string{"debug"},
		Services: []string{"api"},
	}
	if err := e.Down(context.Background(), stack, stackDir, envFile); err != nil {
		t.Fatalf("Down() error = %v", err)
	}
	want := []string{
		"compose",
		"-p", "web",
		"-f", filepath.Join(stackDir, "compose.yaml"),
		"--env-file", envFile,
		"down", "--remove-orphans",
	}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("Down() args = %v, want %v", got, want)
	}
}

func TestFailingCommand(t *testing.T) {
	fakeDocker(t)
	t.Setenv("DOCKER_EXIT", "1")
	e := quietExecutor()

	err := e.Up(context.Background(), config.Stack{Name: "web"}, t.TempDir(), "stack.env")
	if err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("Up() error = %v, want exit status 1", err)
	}
	if err != nil && !strings.Contains(err.Error(), "fake docker stdout") {
		t.Errorf("Up() error = %v, want output snippet", err)
	}

	err = e.Down(context.Background(), config.Stack{Name: "web"}, t.TempDir(), "stack.env")
	if err == nil {
		t.Error("Down() succeeded despite failing docker, want error")
	}
}

func TestMissingBinary(t *testing.T) {
	e := New(nil, filepath.Join(t.TempDir(), "no-such-docker"))
	err := e.Up(context.Background(), config.Stack{Name: "web"}, t.TempDir(), "stack.env")
	if err == nil || !strings.Contains(err.Error(), "no-such-docker") {
		t.Errorf("Up() with missing binary error = %v, want binary mention", err)
	}
}

func TestNewDefaults(t *testing.T) {
	if got := New(nil, "").binary; got != "docker" {
		t.Errorf("New(\"\").binary = %q, want docker", got)
	}
	if got := New(nil, "podman").binary; got != "podman" {
		t.Errorf("New(\"podman\").binary = %q, want podman", got)
	}
}
