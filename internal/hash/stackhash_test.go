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
		"ignore_services":  func(s *config.Stack) { s.IgnoreServices = []string{"db"} },
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

// multiFileStack is baseStack deployed from two compose files.
func multiFileStack() config.Stack {
	s := baseStack()
	s.ComposeFile = ""
	s.ComposeFiles = []string{"/srv/stacks/web/compose.yaml", "/srv/stacks/web/compose.override.yaml"}
	return s
}

const overrideCompose = `services:
  app:
    environment:
      LOG_LEVEL: info
`

func TestComputeMultiFile(t *testing.T) {
	stack := multiFileStack()

	base, err := Compute(stack, []byte(baseCompose), []byte(overrideCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}

	// Reformatting the override file alone must not change the hash.
	reformatted := "# comment\nservices:\n  app:\n    environment:\n      LOG_LEVEL: info   # inline\n"
	got, err := Compute(stack, []byte(baseCompose), []byte(reformatted))
	if err != nil {
		t.Fatalf("Compute() (reformatted) error = %v", err)
	}
	if got != base {
		t.Error("hash changed after reformatting the override file")
	}

	// Changing override content changes the hash.
	changed := strings.Replace(overrideCompose, "info", "debug", 1)
	got, err = Compute(stack, []byte(baseCompose), []byte(changed))
	if err != nil {
		t.Fatalf("Compute() (changed override) error = %v", err)
	}
	if got == base {
		t.Error("hash unchanged after changing the override file content")
	}

	// Swapping the file order changes the hash: compose override rules make
	// a different order a different deployment.
	swappedStack := multiFileStack()
	swappedStack.ComposeFiles = []string{swappedStack.ComposeFiles[1], swappedStack.ComposeFiles[0]}
	got, err = Compute(swappedStack, []byte(overrideCompose), []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() (swapped) error = %v", err)
	}
	if got == base {
		t.Error("hash unchanged after swapping the compose file order")
	}
}

func TestComputeContentsFilesMismatch(t *testing.T) {
	stack := multiFileStack()
	if _, err := Compute(stack, []byte(baseCompose)); err == nil {
		t.Error("Compute() with one content for two files succeeded, want mismatch error")
	}
	single := baseStack()
	if _, err := Compute(single); err == nil {
		t.Error("Compute() with no contents for one file succeeded, want mismatch error")
	}
}

func TestComputeIgnoreServicesWithUnsetServices(t *testing.T) {
	// Services unset: the deployed set is "everything the compose files
	// define minus ignore_services" — the hash must react to ignore changes
	// even though the effective list cannot be enumerated here.
	base := baseStack()
	base.Services = nil
	baseHash, err := Compute(base, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	ignored := base
	ignored.IgnoreServices = []string{"db"}
	got, err := Compute(ignored, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() (ignored) error = %v", err)
	}
	if got == baseHash {
		t.Error("hash unchanged after adding ignore_services to a services-less stack")
	}

	// Reordering ignore_services is order-insensitive (a set, like services).
	reordered := base
	reordered.IgnoreServices = []string{"db", "app"}
	first, err := Compute(reordered, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	reordered.IgnoreServices = []string{"app", "db"}
	second, err := Compute(reordered, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	if first != second {
		t.Error("hash changed after reordering ignore_services (should be a set)")
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
// The current vector is the schema-2 form (compose file list, effective
// services, ignore_services).
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
	const want = "05294051bb498074c0e77559aa505d28e3d17afb9a1e6f74848fb5586a08ccd5"
	if got != want {
		t.Errorf("golden vector hash = %s, want %s (schemaVersion changed?)", got, want)
	}
}
