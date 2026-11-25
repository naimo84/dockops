package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
)

// mapLookup is a Lookup over a fixed map.
func mapLookup(m map[string]string) Lookup {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func TestInterpolate(t *testing.T) {
	env := mapLookup(map[string]string{"SET": "value", "EMPTY": "", "DIR": "/abs"})
	tests := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"$SET/x", "value/x"},
		{"${SET}", "value"},
		{"${UNSET}", ""},
		{"${UNSET:-/def}", "/def"},
		{"${EMPTY:-/def}", "/def"},
		{"${EMPTY-/def}", ""},
		{"${UNSET-/def}", "/def"},
		{"${SET:-/def}", "value"},
		{"${SET:+alt}", "alt"},
		{"${EMPTY:+alt}", ""},
		{"${EMPTY+alt}", "alt"},
		{"${UNSET:-${DIR}/nested}", "/abs/nested"},
		{"$$SET", "$SET"},
		{"a$", "a$"},
		{"${UNSET:-/def}:/target:ro", "/def:/target:ro"},
	}
	for _, tc := range tests {
		got, err := interpolate(tc.in, env)
		if err != nil {
			t.Errorf("interpolate(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("interpolate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"${UNSET:?needed}", "${EMPTY:?needed}", "${UNSET?needed}", "${OPEN", "${1BAD}", "${SET!x}"} {
		if _, err := interpolate(bad, env); err == nil {
			t.Errorf("interpolate(%q) error = nil, want an error", bad)
		}
	}
	if _, err := interpolate("${EMPTY?needed}", env); err != nil {
		t.Errorf("${EMPTY?needed}: set-but-empty must pass, got %v", err)
	}
}

// TestRewriteInterpolatedAbsoluteSource reproduces the traefik stack: a
// short entry whose source is ${VAR:-/absolute/default}. Splitting at ":"
// before interpolating cut it into "${TRAEFIK_CROWDSEC_FILE" and refused the
// deploy; compose expands first, and so must dockops — the result is an
// absolute path, which is left alone.
func TestRewriteInterpolatedAbsoluteSource(t *testing.T) {
	stackDir := "/var/lib/dockops/repos/infrastructure/traefik"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  traefik:
    image: traefik
    volumes:
      - ${TRAEFIK_CROWDSEC_FILE:-/home/docker/infrastructure/traefik/dynamic-crowdsec/off.yaml}:/etc/traefik/dynamic/crowdsec.yaml:ro
`)
	roots := []BindRoot{{Container: "/var/lib/dockops/repos/infrastructure", Host: "/home/docker/infrastructure"}}
	for name, env := range map[string]Lookup{
		"unset uses the default": mapLookup(nil),
		"set from the stack env": mapLookup(map[string]string{"TRAEFIK_CROWDSEC_FILE": "/home/docker/infrastructure/traefik/dynamic-crowdsec/on.yaml"}),
	} {
		raw, count, err := RewriteRelativeBindsEnv([]string{file}, stackDir, roots, env)
		if err != nil {
			t.Errorf("%s: error = %v", name, err)
			continue
		}
		if count != 0 || raw != nil {
			t.Errorf("%s: rewrote %d sources (%q), want none for an absolute source", name, count, raw)
		}
	}
}

// TestRewriteInterpolatedRelativeSource: a variable that yields a relative
// path is rewritten like a literal one, with the value the deploy uses.
func TestRewriteInterpolatedRelativeSource(t *testing.T) {
	stackDir := "/var/lib/dockops/repos/host/x"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - ${CONF_DIR:-./config}:/etc/cfg:ro
      - type: bind
        source: ${DATA_DIR}
        target: /data
`)
	roots := []BindRoot{{Container: "/var/lib/dockops/repos/host", Host: "/home/docker/apps"}}
	raw, count, err := RewriteRelativeBindsEnv([]string{file}, stackDir, roots, mapLookup(map[string]string{"DATA_DIR": "./data"}))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if count != 2 {
		t.Fatalf("rewrote %d sources, want 2 (%q)", count, raw)
	}
	for _, want := range []string{"/home/docker/apps/x/config:/etc/cfg:ro", "/home/docker/apps/x/data:/data"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("override = %q, want %q", raw, want)
		}
	}
}

func TestRewriteInterpolationErrors(t *testing.T) {
	for name, volume := range map[string]string{
		"required variable missing": "${CONF:?set CONF}:/etc/cfg",
		"empty source":              "${CONF}:/etc/cfg",
	} {
		file := writeStackFile(t, t.TempDir(), "compose.yaml", "services:\n  a:\n    image: alpine\n    volumes:\n      - "+volume+"\n")
		_, _, err := RewriteRelativeBindsEnv([]string{file}, "/s", nil, mapLookup(nil))
		if err == nil || !strings.Contains(err.Error(), "volumes[0]") {
			t.Errorf("%s: error = %v, want an error naming the volume", name, err)
		}
	}
}

func TestNonPortableBindsSeesInterpolatedSource(t *testing.T) {
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - ${LOGS:-/var/log/app}:/logs
`)
	roots := []BindRoot{{Container: "/var/lib/dockops/repos/host", Host: "/home/docker/apps"}}
	got, err := NonPortableBindsEnv([]string{file}, "/var/lib/dockops/repos/host/x", roots, mapLookup(nil))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "/var/log/app") {
		t.Errorf("findings = %v, want the expanded default reported", got)
	}
}

// TestEnvLookup: values come from the env file (compose dotenv quoting
// undone), the process environment wins, and a missing file only means
// "process environment only".
func TestEnvLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stack.env")
	content := "PLAIN=/a/b\nQUOTED=\"/with space/$$x\"\nSINGLE='/s'\nCOMMENT=/c #note\nSHADOWED=file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHADOWED", "process")
	env, err := EnvLookup(path)
	if err != nil {
		t.Fatalf("EnvLookup() error = %v", err)
	}
	for name, want := range map[string]string{"PLAIN": "/a/b", "QUOTED": "/with space/$x", "SINGLE": "/s", "COMMENT": "/c", "SHADOWED": "process"} {
		if got, ok := env(name); !ok || got != want {
			t.Errorf("%s = %q (set %v), want %q", name, got, ok, want)
		}
	}
	if _, ok := env("DOCKOPS_TEST_NOT_SET_ANYWHERE"); ok {
		t.Error("unknown variable reported as set")
	}
	if _, err := EnvLookup(filepath.Join(t.TempDir(), "missing.env")); err != nil {
		t.Errorf("missing env file: error = %v, want none", err)
	}
}

// TestUpInterpolatesBindSourcesFromEnvFile: the executor judges the sources
// with the deploy's own --env-file, so the traefik form reaches compose.
func TestUpInterpolatesBindSourcesFromEnvFile(t *testing.T) {
	argsFile := fakeDocker(t)
	stackDir := t.TempDir()
	writeStackFile(t, stackDir, "compose.yaml", `
services:
  s:
    image: alpine
    volumes:
      - ${CROWDSEC_FILE:-./off.yaml}:/etc/crowdsec.yaml:ro
`)
	envFile := filepath.Join(t.TempDir(), "stack.env")
	if err := os.WriteFile(envFile, []byte("CROWDSEC_FILE=/srv/on.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setDockerStdout(t, `[{"Type":"bind","Source":"/host`+stackDir+`","Destination":"`+stackDir+`"}]`)
	stack := config.Stack{Name: "s", ComposeFile: "compose.yaml"}

	ex := quietExecutor().WithSelfContainer("agent")
	if err := ex.Up(context.Background(), stack, stackDir, envFile, UpOptions{}); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if strings.Contains(string(raw), "dockops-binds-") {
		t.Errorf("an override was passed for an absolute source: %s", raw)
	}
	if !strings.Contains(string(raw), "up") {
		t.Errorf("compose up did not run: %s", raw)
	}
}
