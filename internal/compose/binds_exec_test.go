package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
)

// bindExecStack builds a stack with one relative bind source in a temp dir.
func bindExecStack(t *testing.T) (config.Stack, string) {
	t.Helper()
	stackDir := t.TempDir()
	writeStackFile(t, stackDir, "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - ./x.txt:/x.txt:ro
`)
	return config.Stack{Name: "s", ComposeFile: "compose.yaml"}, stackDir
}

// TestUpPassesBindOverrideAndRemovesIt: Up appends the generated override as an
// extra -f after the stack's own file, and the file does not outlive the call.
func TestUpPassesBindOverrideAndRemovesIt(t *testing.T) {
	argsFile := fakeDocker(t)
	setDockerStdout(t, `[{"Type":"bind","Source":"`+t.TempDir()+`","Destination":"/anything"}]`)
	stack, stackDir := bindExecStack(t)

	// The fake inspect output is what OwnMounts reads; it must contain the
	// stack dir as a container path, and the host side is what it maps to.
	roots := []BindRoot{{Container: stackDir, Host: "/host" + stackDir}}
	setDockerStdout(t, `[{"Type":"bind","Source":"/host`+stackDir+`","Destination":"`+stackDir+`"}]`)

	ex := quietExecutor().WithSelfContainer("agent")
	if err := ex.Up(context.Background(), stack, stackDir, ""); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	override := filepath.Join(stackDir, "dockops-binds-s.yaml")
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if !containsPair(got, "-f", override) {
		t.Errorf("args = %v, want the bind override %s", got, override)
	}
	if _, err := os.Stat(override); !os.IsNotExist(err) {
		t.Errorf("bind override still exists after Up (stat err = %v, roots = %v)", err, roots)
	}
}

// TestUpRefusesUntranslatableRelativeSource: a relative source whose container
// path no mount covers must fail before compose runs.
func TestUpRefusesUntranslatableRelativeSource(t *testing.T) {
	argsFile := fakeDocker(t)
	setDockerStdout(t, `[{"Type":"bind","Source":"/home/docker/dockops","Destination":"/var/lib/dockops"}]`)
	stack, stackDir := bindExecStack(t)

	ex := quietExecutor().WithSelfContainer("agent")
	err := ex.Up(context.Background(), stack, stackDir, "")
	if err == nil || !strings.Contains(err.Error(), "outside every mount") {
		t.Fatalf("Up() error = %v, want the bind refusal", err)
	}
	if raw, readErr := os.ReadFile(argsFile); readErr == nil {
		// The fake logs every command; the only allowed one is the inspect
		// of the agent's own mounts — no compose invocation may appear.
		if strings.Contains(string(raw), "compose") {
			t.Errorf("compose ran despite the bind error: %v", strings.Split(strings.TrimSpace(string(raw)), "\n"))
		}
	}
}

// TestOwnMountsKeepsBindsOnly: named volumes and tmpfs have no host path a bind
// source could point at and must be dropped.
func TestOwnMountsKeepsBindsOnly(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, `[
		{"Type":"bind","Source":"/home/docker/apps","Destination":"/var/lib/dockops/repos/host"},
		{"Type":"volume","Source":"/var/lib/docker/volumes/x/_data","Destination":"/data"},
		{"Type":"tmpfs","Source":"tmpfs","Destination":"/run"}
	]`)
	roots, err := quietExecutor().OwnMounts(context.Background(), "agent")
	if err != nil {
		t.Fatalf("OwnMounts() error = %v", err)
	}
	if len(roots) != 1 || roots[0] != (BindRoot{Container: "/var/lib/dockops/repos/host", Host: "/home/docker/apps"}) {
		t.Errorf("OwnMounts() = %+v, want the single bind", roots)
	}
	if _, err := quietExecutor().OwnMounts(context.Background(), ""); err == nil {
		t.Error("OwnMounts(\"\") must fail")
	}
}

// TestUpArgsWithOverridesKeepsOrder: the helper's deploy line puts the
// overrides after the stack's own files, before the flags.
func TestUpArgsWithOverridesKeepsOrder(t *testing.T) {
	req := Request{Project: "p", StackDir: "/s", ComposeFiles: []string{"compose.yaml"}}
	got, err := UpArgsWithOverrides(req, []string{"/s/binds.yaml"}, []string{"ml"}, "/s/.env", false, []string{"a"})
	if err != nil {
		t.Fatalf("UpArgsWithOverrides() error = %v", err)
	}
	want := []string{"compose", "-p", "p", "-f", "/s/compose.yaml", "-f", "/s/binds.yaml", "--profile", "ml", "--env-file", "/s/.env", "up", "-d", "--remove-orphans", "a"}
	if !equalArgs(got, want) {
		t.Errorf("UpArgsWithOverrides() = %v, want %v", got, want)
	}
}

// containsPair reports whether lines contains a and b consecutively.
func containsPair(lines []string, a, b string) bool {
	for i := 0; i+1 < len(lines); i++ {
		if lines[i] == a && lines[i+1] == b {
			return true
		}
	}
	return false
}
