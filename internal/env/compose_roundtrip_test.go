package env

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
)

// requireDockerCompose skips the test where no docker compose CLI is
// installed. `docker compose config` only parses — no daemon needed.
func requireDockerCompose(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("docker compose unavailable: %v: %s", err, out)
	}
}

// composeReads returns the values docker compose reads for keys from the env
// file at envFile, through compose's own dotenv parser and interpolation.
func composeReads(t *testing.T, dir, envFile string, keys []string) map[string]string {
	t.Helper()
	var compose strings.Builder
	compose.WriteString("services:\n  probe:\n    image: alpine\n    environment:\n")
	for _, k := range keys {
		compose.WriteString("      " + k + ": ${" + k + "}\n")
	}
	composeFile := filepath.Join(dir, "compose.yaml")
	writeFile(t, composeFile, compose.String())

	out, err := exec.Command("docker", "compose", "-p", "dockops-roundtrip", "-f", composeFile,
		"--env-file", envFile, "config", "--format", "json").Output()
	if err != nil {
		var stderr []byte
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = exitErr.Stderr
		}
		t.Fatalf("docker compose config: %v: %s", err, stderr)
	}
	var doc struct {
		Services map[string]struct {
			Environment map[string]*string `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parse compose config: %v\n%s", err, out)
	}
	got := make(map[string]string, len(keys))
	for k, v := range doc.Services["probe"].Environment {
		if v != nil {
			// config re-escapes a literal "$" as "$$" for its own output.
			got[k] = strings.ReplaceAll(*v, "$$", "$")
		}
	}
	return got
}

// The generated env file must make docker compose read every Vault value
// verbatim — whatever the value contains and wherever a template puts it —
// while env_files text keeps its compose meaning.
func TestGeneratedEnvFileRoundTripsThroughCompose(t *testing.T) {
	requireDockerCompose(t)

	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "config.env"), "QUOTED=\"two words\"\nCOMMENTED=value # note\n")
	writeFile(t, filepath.Join(baseDir, "tpl.env"), strings.Join([]string{
		"T_UNQUOTED=${VAULT:p#tricky}",
		`T_DOUBLE="pre-${VAULT:p#tricky}-post"`,
		`T_SINGLE='it\'s ${VAULT:p#tricky}'`,
		"T_COMMENT=${VAULT:p#dollar} # note",
		"T_PLAIN=${VAULT:p#plain}",
	}, "\n")+"\n")
	resolver := &fakeResolver{vault: map[string]map[string]string{
		"p":  {"tricky": trickySecret, "dollar": "a$b", "plain": "pg-secret"},
		"kv": {"V_TRICKY": trickySecret, "V_DOLLAR": "pa$word", "V_PLAIN": "pg"},
	}}
	stack := config.Stack{
		Name:              "roundtrip",
		Dir:               baseDir,
		EnvFiles:          []string{"config.env"},
		VaultPaths:        []string{"kv"},
		VaultEnvTemplates: []string{"tpl.env"},
	}
	m, err := Merge(context.Background(), stack, resolver, baseDir, SystemVars{})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	path, cleanup, err := WriteShm(m.Dotenv)
	if err != nil {
		t.Fatalf("WriteShm() error = %v", err)
	}
	defer cleanup()

	want := map[string]string{
		"QUOTED":     "two words",
		"COMMENTED":  "value",
		"V_TRICKY":   trickySecret,
		"V_DOLLAR":   "pa$word",
		"V_PLAIN":    "pg",
		"T_UNQUOTED": trickySecret,
		"T_DOUBLE":   "pre-" + trickySecret + "-post",
		"T_SINGLE":   "it's " + trickySecret,
		"T_COMMENT":  "a$b",
		"T_PLAIN":    "pg-secret",
		"STACK_NAME": "roundtrip",
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	got := composeReads(t, baseDir, path, keys)
	for _, k := range keys {
		if got[k] != want[k] {
			t.Errorf("compose reads %s = %q, want %q", k, got[k], want[k])
		}
	}
}
