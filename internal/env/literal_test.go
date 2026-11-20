package env

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/local/dockops/internal/config"
)

// trickySecret is a value no unquoted compose dotenv value can carry:
// interpolation, an inline comment, both quote kinds, a backslash, a line
// break and whitespace at its ends.
const trickySecret = " pa$word #1 \"dq\" 'sq' back\\slash\nline2 "

func TestRenderTemplateDotenvText(t *testing.T) {
	reader := &fakeVaultReader{kv: map[string]map[string]string{
		"p": {"plain": "pg-secret", "tricky": trickySecret, "dollar": "a$b"},
	}}
	cases := []struct {
		name      string
		line      string
		wantValue string // the hooks' view
		wantText  string // the env files' dotenv text
	}{
		{"inert secret unquoted", "K=${VAULT:p#plain}", "pg-secret", "pg-secret"},
		{"inert secret keeps comment", "K=${VAULT:p#plain} # note", "pg-secret # note", "pg-secret # note"},
		{"inert secret in single quotes", "K='${VAULT:p#plain}'", "'pg-secret'", "'pg-secret'"},
		{"no reference", "K='$literal'", "'$literal'", "'$literal'"},
		{"dollar unquoted", "K=${VAULT:p#dollar}", "a$b", `"a$$b"`},
		{"unquoted keeps template interpolation", "K=${BASE}/${VAULT:p#dollar}", "${BASE}/a$b", `"${BASE}/a$$b"`},
		{"unquoted drops the inline comment", "K=x${VAULT:p#dollar} # note", "xa$b # note", `"xa$$b"`},
		{"unquoted escapes literal backslash", `K=c:\${VAULT:p#dollar}`, `c:\a$b`, `"c:\\a$$b"`},
		{"double quoted", `K="pre-${VAULT:p#dollar}-post"`, `"pre-a$b-post"`, `"pre-a$$b-post"`},
		{"double quoted keeps trailing", `K="${VAULT:p#dollar}" # note`, `"a$b" # note`, `"a$$b" # note`},
		{"single quoted becomes double", `K='it\'s $x ${VAULT:p#dollar}'`, `'it\'s $x a$b'`, `"it's $$x a$$b"`},
		{"unterminated quote is unquoted", `K="open ${VAULT:p#dollar}`, `"open a$b`, `"\"open a$$b"`},
		{"tricky", "K=${VAULT:p#tricky}", trickySecret, `" pa$$word #1 \"dq\" 'sq' back\\slash\nline2 "`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values, text, err := renderTemplate(context.Background(), tc.line+"\n", reader)
			if err != nil {
				t.Fatalf("renderTemplate(%q) error = %v", tc.line, err)
			}
			if got := values["K"]; got != tc.wantValue {
				t.Errorf("value = %q, want %q", got, tc.wantValue)
			}
			if got := text["K"]; got != tc.wantText {
				t.Errorf("dotenv text = %s, want %s", got, tc.wantText)
			}
		})
	}
}

// A Vault value never splits its line: a line break or "=" inside it stays
// part of the value instead of starting another variable.
func TestRenderTemplateSecretCannotInjectVariables(t *testing.T) {
	reader := &fakeVaultReader{kv: map[string]map[string]string{
		"p": {"evil": "x\nINJECTED=1", "eq": "a=b"},
	}}
	values, text, err := renderTemplate(context.Background(), "K=${VAULT:p#evil}\nE=${VAULT:p#eq}\n", reader)
	if err != nil {
		t.Fatalf("renderTemplate() error = %v", err)
	}
	want := map[string]string{"K": "x\nINJECTED=1", "E": "a=b"}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("values = %q, want %q", values, want)
	}
	if got := text["K"]; got != `"x\nINJECTED=1"` {
		t.Errorf("dotenv text of K = %s, want the escaped line break", got)
	}
}

// Merge keeps the text a user wrote for compose (env_files, sops) and
// escapes what dockops inserts (Vault values, system variables).
func TestMergeDotenvForms(t *testing.T) {
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "config.env"), "QUOTED=\"two words\"\nREF=${OTHER}\n")
	writeFile(t, filepath.Join(baseDir, "tpl.env"), "TPL=${VAULT:hosts/x#pw}\n")
	resolver := &fakeResolver{
		sops:  map[string]map[string]string{filepath.Join(baseDir, "s.env"): {"SOPS": "pa$$word"}},
		vault: map[string]map[string]string{"hosts/x": {"VAULT_PW": "pa$word", "pw": "t$p"}},
	}
	stack := config.Stack{
		Name:              "web",
		Dir:               baseDir,
		EnvFiles:          []string{"config.env"},
		SopsFiles:         []config.SopsFile{{Path: "s.env"}},
		VaultPaths:        []string{"hosts/x"},
		VaultEnvTemplates: []string{"tpl.env"},
	}
	m, err := Merge(context.Background(), stack, resolver, baseDir, SystemVars{StackDir: "/srv/with space"})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	for k, want := range map[string]string{
		"QUOTED":     `"two words"`,     // env_files: compose strips the quotes, as always
		"REF":        "${OTHER}",        // env_files: compose interpolates, as always
		"SOPS":       "pa$$word",        // sops dotenv: compose text the user wrote
		"VAULT_PW":   `"pa$$word"`,      // vault_paths: escaped, compose reads pa$word
		"TPL":        `"t$$p"`,          // template: the inserted secret is escaped
		"STACK_NAME": "web",             // system variables: plain when nothing needs quoting
		"STACK_DIR":  "/srv/with space", // inner space is verbatim for compose
	} {
		if got := m.Dotenv[k]; got != want {
			t.Errorf("Dotenv[%s] = %s, want %s", k, got, want)
		}
	}
	// The hooks see the plain values.
	for k, want := range map[string]string{"VAULT_PW": "pa$word", "TPL": "t$p", "QUOTED": `"two words"`} {
		if got := m.Values[k]; got != want {
			t.Errorf("Values[%s] = %q, want %q", k, got, want)
		}
	}
	if !reflect.DeepEqual(keySet(m.Values), keySet(m.Dotenv)) {
		t.Errorf("Values and Dotenv carry different keys: %v vs %v", keySet(m.Values), keySet(m.Dotenv))
	}
}

func keySet(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
