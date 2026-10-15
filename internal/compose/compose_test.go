package compose

import (
	"context"
	"fmt"
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

func TestUpMultiFileArguments(t *testing.T) {
	argsFile := fakeDocker(t)
	e := quietExecutor()
	stackDir := t.TempDir()
	abs := filepath.Join(t.TempDir(), "abs", "override.yaml")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, f := range []string{abs, filepath.Join(stackDir, "base.yaml"), filepath.Join(stackDir, "extra.yaml")} {
		if err := os.WriteFile(f, []byte("services: {}\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	stack := config.Stack{
		Name:         "web",
		ComposeFiles: []string{"base.yaml", abs, "extra.yaml"}, // relative, absolute, relative
	}
	envFile := filepath.Join(t.TempDir(), "stack.env")
	if err := e.Up(context.Background(), stack, stackDir, envFile); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	want := []string{
		"compose",
		"-p", "web",
		"-f", filepath.Join(stackDir, "base.yaml"),
		"-f", abs,
		"-f", filepath.Join(stackDir, "extra.yaml"),
		"--env-file", envFile,
		"up", "-d", "--remove-orphans",
	}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("Up() args = %v, want %v (list order, one -f per file)", got, want)
	}
}

func TestDownMultiFileArguments(t *testing.T) {
	argsFile := fakeDocker(t)
	e := quietExecutor()
	stackDir := t.TempDir()
	for _, f := range []string{"compose.yaml", "override.yaml"} {
		if err := os.WriteFile(filepath.Join(stackDir, f), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	stack := config.Stack{Name: "web", ComposeFiles: []string{"compose.yaml", "override.yaml"}}
	envFile := filepath.Join(t.TempDir(), "e.env")
	if err := e.Down(context.Background(), stack, stackDir, envFile); err != nil {
		t.Fatalf("Down() error = %v", err)
	}
	want := []string{
		"compose",
		"-p", "web",
		"-f", filepath.Join(stackDir, "compose.yaml"),
		"-f", filepath.Join(stackDir, "override.yaml"),
		"--env-file", envFile,
		"down", "--remove-orphans",
	}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("Down() args = %v, want %v", got, want)
	}
}

// writeComposeFiles writes one compose file per content into dir and returns
// the file names.
func writeComposeFiles(t *testing.T, dir string, contents ...string) []string {
	t.Helper()
	names := make([]string, 0, len(contents))
	for i, content := range contents {
		name := fmt.Sprintf("compose-%d.yaml", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		names = append(names, name)
	}
	return names
}

func TestUpServicesMinusIgnore(t *testing.T) {
	argsFile := fakeDocker(t)
	e := quietExecutor()
	stackDir := t.TempDir()
	// Hand-constructed stack (the loader would reject the overlap): the
	// executor still subtracts defensively.
	stack := config.Stack{
		Name:           "web",
		ComposeFile:    "compose.yaml",
		Services:       []string{"api", "worker", "db"},
		IgnoreServices: []string{"worker"},
	}
	if err := e.Up(context.Background(), stack, stackDir, filepath.Join(t.TempDir(), "e.env")); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	got := recordedArgs(t, argsFile)
	wantTail := []string{"api", "db"}
	if tail := got[len(got)-2:]; !reflect.DeepEqual(tail, wantTail) {
		t.Errorf("Up() service args = %v, want %v (services minus ignore_services)", tail, wantTail)
	}
}

func TestUpIgnoreOnlyDerivesFromComposeFiles(t *testing.T) {
	argsFile := fakeDocker(t)
	e := quietExecutor()
	stackDir := t.TempDir()
	// No services configured: "all" is the union of top-level service names
	// across every compose file (here: one exclusive to the second file).
	files := writeComposeFiles(t, stackDir,
		"services:\n  api:\n    image: nginx\n  worker:\n    image: worker\n",
		"services:\n  db:\n    image: postgres\n",
	)
	stack := config.Stack{Name: "web", ComposeFiles: files, IgnoreServices: []string{"worker"}}
	if err := e.Up(context.Background(), stack, stackDir, filepath.Join(t.TempDir(), "e.env")); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	got := recordedArgs(t, argsFile)
	wantTail := []string{"api", "db"} // sorted, worker missing
	if tail := got[len(got)-2:]; !reflect.DeepEqual(tail, wantTail) {
		t.Errorf("Up() service args = %v, want %v (all compose services minus ignore, sorted)", tail, wantTail)
	}
}

func TestUpIgnoreErrors(t *testing.T) {
	e := quietExecutor()

	t.Run("ignore covers every compose service", func(t *testing.T) {
		stackDir := t.TempDir()
		files := writeComposeFiles(t, stackDir, "services:\n  api:\n    image: nginx\n")
		stack := config.Stack{Name: "web", ComposeFiles: files, IgnoreServices: []string{"api"}}
		err := e.Up(context.Background(), stack, stackDir, "e.env")
		if err == nil || !strings.Contains(err.Error(), "ignore_services covers every service") {
			t.Errorf("Up() error = %v, want covers-every-service error (no positional args would deploy everything)", err)
		}
	})

	t.Run("every configured service ignored", func(t *testing.T) {
		stack := config.Stack{
			Name:           "web",
			ComposeFile:    "compose.yaml",
			Services:       []string{"api"},
			IgnoreServices: []string{"api"},
		}
		err := e.Up(context.Background(), stack, t.TempDir(), "e.env")
		if err == nil || !strings.Contains(err.Error(), "every configured service is in ignore_services") {
			t.Errorf("Up() error = %v, want all-ignored error", err)
		}
	})

	t.Run("unparsable compose file", func(t *testing.T) {
		stackDir := t.TempDir()
		broken := filepath.Join(stackDir, "broken.yaml")
		if err := os.WriteFile(broken, []byte("services:\n  - [unclosed\n"), 0o644); err != nil {
			t.Fatalf("write broken compose: %v", err)
		}
		stack := config.Stack{Name: "web", ComposeFiles: []string{"broken.yaml"}, IgnoreServices: []string{"api"}}
		err := e.Up(context.Background(), stack, stackDir, "e.env")
		if err == nil || !strings.Contains(err.Error(), broken) {
			t.Errorf("Up() error = %v, want error naming the unparsable file", err)
		}
	})

	t.Run("missing compose file", func(t *testing.T) {
		stack := config.Stack{Name: "web", ComposeFiles: []string{"missing.yaml"}, IgnoreServices: []string{"api"}}
		err := e.Up(context.Background(), stack, t.TempDir(), "e.env")
		if err == nil || !strings.Contains(err.Error(), "missing.yaml") {
			t.Errorf("Up() error = %v, want error naming the missing file", err)
		}
	})
}
