package stackhash

import (
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
)

// baseStack is the reference stack most tests mutate.
func baseStack() config.Stack {
	return config.Stack{
		Name:        "web",
		Enabled:     true,
		ComposeFile: "/srv/stacks/web/compose.yaml",
		Profiles:    []string{"full", "minimal"},
		Services:    []string{"app", "db"},
		EnvFiles:    []string{"web.env", "secrets.env"},
		SopsFiles: []config.SopsFile{
			{Path: "db-password.enc", AgeKeyfile: "/etc/dockops/age.key"},
			{Path: "api-token.enc"},
		},
		VaultPaths: []string{"secret/web/tls", "secret/web/api"},
		Hooks: config.Hooks{
			PreDeploy:  []string{"echo pre1", "echo pre2"},
			PostDeploy: []string{"echo post"},
		},
	}
}

// baseCompose and reformattedCompose describe the same compose file in
// different spellings: reordered keys, different indentation and quoting,
// comments, and an anchor instead of the repeated block.
const baseCompose = `services:
  app:
    image: registry.example/app:1.4.2
    ports:
      - "8080:80"
    environment:
      LOG_LEVEL: debug
    replicas: 2
  db:
    image: postgres:16
    volumes:
      - db-data:/var/lib/postgresql/data
logging:
  driver: json-file
  options:
    max-size: 10m
volumes:
  db-data: {}
`

const reformattedCompose = `
# leading comment
volumes:
  db-data: {}     # inline comment
logging:
  options:
    max-size: 10m
  driver: json-file
services:
  db:
    volumes:
      - db-data:/var/lib/postgresql/data
    image: postgres:16
  app:
    environment:
      LOG_LEVEL: debug
    replicas: 2
    image: registry.example/app:1.4.2
    ports:
      - 8080:80
`

// anchoredCompose replaces the repeated logging block with an anchor.
const anchoredCompose = `services:
  app:
    image: registry.example/app:1.4.2
    ports:
      - "8080:80"
    environment:
      LOG_LEVEL: debug
    replicas: 2
  db:
    image: postgres:16
    volumes:
      - db-data:/var/lib/postgresql/data
logging: &logging
  driver: json-file
  options:
    max-size: 10m
volumes:
  db-data: {}
`

func TestComputeStableAcrossComposeFormatting(t *testing.T) {
	stack := baseStack()
	want, err := Compute(stack, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	for name, content := range map[string]string{
		"reordered keys, comments, quoting": reformattedCompose,
		"anchor instead of repeat":          anchoredCompose,
	} {
		got, err := Compute(stack, []byte(content))
		if err != nil {
			t.Fatalf("Compute() (%s) error = %v", name, err)
		}
		if got != want {
			t.Errorf("Compute() (%s) = %s, want %s", name, got, want)
		}
	}
}

func TestComputeSensitiveToConfigChanges(t *testing.T) {
	base := baseStack()
	baseHash, err := Compute(base, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}

	mutations := map[string]func(s *config.Stack){
		"name":             func(s *config.Stack) { s.Name = "web2" },
		"enabled":          func(s *config.Stack) { s.Enabled = false },
		"compose_file":     func(s *config.Stack) { s.ComposeFile = "/srv/other/compose.yaml" },
		"profiles":         func(s *config.Stack) { s.Profiles = []string{"minimal"} },
		"services":         func(s *config.Stack) { s.Services = []string{"app"} },
		"env_files":        func(s *config.Stack) { s.EnvFiles = []string{"web.env"} },
		"sops path":        func(s *config.Stack) { s.SopsFiles[0].Path = "other.enc" },
		"sops age_keyfile": func(s *config.Stack) { s.SopsFiles[0].AgeKeyfile = "" },
		"vault_paths":      func(s *config.Stack) { s.VaultPaths = []string{"secret/web/tls"} },
		"pre_deploy hook":  func(s *config.Stack) { s.Hooks.PreDeploy = []string{"echo pre1"} },
		"post_deploy hook": func(s *config.Stack) { s.Hooks.PostDeploy = nil },
	}
	for name, mutate := range mutations {
		s := base
		mutate(&s)
		got, err := Compute(s, []byte(baseCompose))
		if err != nil {
			t.Fatalf("Compute() (%s) error = %v", name, err)
		}
		if got == baseHash {
			t.Errorf("hash unchanged after changing %s", name)
		}
	}

	changedCompose := strings.Replace(baseCompose, "replicas: 2", "replicas: 3", 1)
	got, err := Compute(base, []byte(changedCompose))
	if err != nil {
		t.Fatalf("Compute() (compose change) error = %v", err)
	}
	if got == baseHash {
		t.Error("hash unchanged after changing compose content")
	}
}

func TestComputeOrderSemantics(t *testing.T) {
	base := baseStack()
	baseHash, err := Compute(base, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}

	// profiles and services are sets: reordering must not change the hash.
	for name, mutate := range map[string]func(s *config.Stack){
		"profiles reordered": func(s *config.Stack) { s.Profiles = []string{"minimal", "full"} },
		"services reordered": func(s *config.Stack) { s.Services = []string{"db", "app"} },
	} {
		s := base
		mutate(&s)
		got, err := Compute(s, []byte(baseCompose))
		if err != nil {
			t.Fatalf("Compute() (%s) error = %v", name, err)
		}
		if got != baseHash {
			t.Errorf("hash changed after reordering %s (should be order-insensitive)", name)
		}
	}

	// env_files, vault_paths and hooks are sequences: reordering must
	// change the hash because their order is semantically relevant.
	for name, mutate := range map[string]func(s *config.Stack){
		"env_files reordered":   func(s *config.Stack) { s.EnvFiles = []string{"secrets.env", "web.env"} },
		"vault_paths reordered": func(s *config.Stack) { s.VaultPaths = []string{"secret/web/api", "secret/web/tls"} },
		"pre_deploy reordered":  func(s *config.Stack) { s.Hooks.PreDeploy = []string{"echo pre2", "echo pre1"} },
	} {
		s := base
		mutate(&s)
		got, err := Compute(s, []byte(baseCompose))
		if err != nil {
			t.Fatalf("Compute() (%s) error = %v", name, err)
		}
		if got == baseHash {
			t.Errorf("hash unchanged after reordering %s (order is semantically relevant)", name)
		}
	}
}

func TestComputeNilEqualsEmptyLists(t *testing.T) {
	withEmpty := baseStack()
	withEmpty.EnvFiles = []string{}
	withEmpty.VaultPaths = []string{}
	withEmpty.Hooks.PostDeploy = []string{}

	withNil := baseStack()
	withNil.EnvFiles = nil
	withNil.VaultPaths = nil
	withNil.Hooks.PostDeploy = nil

	emptyHash, err := Compute(withEmpty, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() (empty lists) error = %v", err)
	}
	nilHash, err := Compute(withNil, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() (nil lists) error = %v", err)
	}
	if emptyHash != nilHash {
		t.Errorf("empty-list hash %s != nil-list hash %s (should be equal)", emptyHash, nilHash)
	}
}

func TestComputeEmptyCompose(t *testing.T) {
	emptyHash, err := Compute(baseStack(), []byte(""))
	if err != nil {
		t.Fatalf("Compute() with empty compose error = %v", err)
	}
	wsHash, err := Compute(baseStack(), []byte(" \n\n  \n"))
	if err != nil {
		t.Fatalf("Compute() with whitespace compose error = %v", err)
	}
	if emptyHash != wsHash {
		t.Errorf("empty compose hash %s != whitespace compose hash %s", emptyHash, wsHash)
	}
}

func TestComputeComposeErrors(t *testing.T) {
	stack := baseStack()
	stack.ComposeFile = "/srv/stacks/web/compose.yaml"
	cases := map[string]struct {
		content string
		wantErr string
	}{
		"syntax error":   {"services:\n  - [unclosed", "normalize compose file"},
		"duplicate keys": {"services: {}\nservices: {}", `duplicate mapping key "services"`},
		"non-scalar key": {"? [a, b]\n: v", "non-scalar mapping key"},
		"cyclic alias":   {"a: &x\n  b: *x\n", "cyclic alias"},
	}
	for name, tc := range cases {
		_, err := Compute(stack, []byte(tc.content))
		if err == nil {
			t.Errorf("Compute() (%s) succeeded, want error", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "/srv/stacks/web/compose.yaml") {
			t.Errorf("Compute() (%s) error = %v, want containing %q and the compose file path", name, err, tc.wantErr)
		}
	}
}

// TestComputeGoldenVector pins the exact hash of a fixed input. It must only
// change together with a schemaVersion bump; otherwise the canonical form
// changed unintentionally and persisted hashes would silently lose meaning.
func TestComputeGoldenVector(t *testing.T) {
	stack := config.Stack{
		Name:        "web",
		Enabled:     true,
		ComposeFile: "/srv/stacks/web/compose.yaml",
		Profiles:    []string{"full", "minimal"},
		Services:    []string{"app", "db"},
		EnvFiles:    []string{"web.env", "secrets.env"},
		SopsFiles: []config.SopsFile{
			{Path: "db-password.enc", AgeKeyfile: "/etc/dockops/age.key"},
			{Path: "api-token.enc"},
		},
		VaultPaths: []string{"secret/web/tls", "secret/web/api"},
		Hooks: config.Hooks{
			PreDeploy:  []string{"echo pre1", "echo pre2"},
			PostDeploy: []string{"echo post"},
		},
	}
	got, err := Compute(stack, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	const want = "ec270d9f7bbb4f5ab1e216ce92ccc374ce22f97496eb9620faa27fc07575a10c"
	if got != want {
		t.Errorf("golden vector hash = %s, want %s (schemaVersion changed?)", got, want)
	}
}
