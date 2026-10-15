package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testRemote = "infra"

// writeFile creates path (and parent directories) with the given content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newRepo creates a host repository with a stub stacks.yaml and a separate
// work directory for remote checkouts, both below a fresh temp dir.
func newRepo(t *testing.T) (rootFile, hostDir, workDir string) {
	t.Helper()
	base := t.TempDir()
	hostDir = filepath.Join(base, "host")
	workDir = filepath.Join(base, "work")
	rootFile = filepath.Join(hostDir, "stacks.yaml")
	writeFile(t, rootFile, "global: {}\n")
	return rootFile, hostDir, workDir
}

// writeRemoteStack writes a stack.yaml plus the given fixture files into
// <workDir>/<testRemote>/<path>/, simulating a remote checkout.
func writeRemoteStack(t *testing.T, workDir, path, stackYAML string, fixtures ...string) {
	t.Helper()
	dir := filepath.Join(workDir, testRemote, filepath.FromSlash(path))
	for _, f := range fixtures {
		writeFile(t, filepath.Join(dir, f), "# fixture\n")
	}
	writeFile(t, filepath.Join(dir, "stack.yaml"), stackYAML)
}

// rootWithImports builds a root stacks.yaml using workDir and the given
// imports block (indented YAML list).
func rootWithImports(workDir, importsYAML string) string {
	return fmt.Sprintf("global:\n  work_dir: %q\nremotes:\n  %s:\n    url: https://example.com/infra.git\n%s", workDir, testRemote, importsYAML)
}

func TestDefaultsMinimalRoot(t *testing.T) {
	t.Setenv("VAULT_ADDR", "")
	rootFile, _, _ := newRepo(t)

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := time.Duration(cfg.Global.PollInterval); got != 5*time.Minute {
		t.Errorf("poll_interval = %v, want 5m", got)
	}
	if cfg.Global.PruneRemovedStacks {
		t.Errorf("prune_removed_stacks = true, want false")
	}
	if cfg.Global.WorkDir != "/var/lib/dockops/repos" {
		t.Errorf("work_dir = %q, want /var/lib/dockops/repos", cfg.Global.WorkDir)
	}
	wantInclude := []string{"**/stack.yaml", "**/*.stack.yaml"}
	if !reflect.DeepEqual(cfg.Global.Include, wantInclude) {
		t.Errorf("include = %v, want %v", cfg.Global.Include, wantInclude)
	}
	wantPatterns := []string{"compose.yaml", "compose.yml", "compose.docker-compose.yaml", "docker-compose.yml"}
	if !reflect.DeepEqual(cfg.Global.ComposeFilePatterns, wantPatterns) {
		t.Errorf("compose_file_patterns = %v, want %v", cfg.Global.ComposeFilePatterns, wantPatterns)
	}
	v := cfg.Global.Vault
	if v.MountPath != "secret" {
		t.Errorf("vault.mount_path = %q, want secret", v.MountPath)
	}
	if v.AuthMethod != "token" {
		t.Errorf("vault.auth_method = %q, want token", v.AuthMethod)
	}
	if v.TokenFromEnv != "VAULT_TOKEN" {
		t.Errorf("vault.token_from_env = %q, want VAULT_TOKEN", v.TokenFromEnv)
	}
	if v.RoleIDFromEnv != "VAULT_ROLE_ID" {
		t.Errorf("vault.role_id_from_env = %q, want VAULT_ROLE_ID", v.RoleIDFromEnv)
	}
	if v.SecretIDFromEnv != "VAULT_SECRET_ID" {
		t.Errorf("vault.secret_id_from_env = %q, want VAULT_SECRET_ID", v.SecretIDFromEnv)
	}
	if v.Address != "" {
		t.Errorf("vault.address = %q, want empty", v.Address)
	}
	if len(cfg.Stacks) != 0 {
		t.Errorf("stacks = %v, want none", cfg.Stacks)
	}
}

func TestVaultAddressEnvFallback(t *testing.T) {
	rootFile, _, _ := newRepo(t)

	t.Run("env fallback", func(t *testing.T) {
		t.Setenv("VAULT_ADDR", "http://127.0.0.1:8200")
		writeFile(t, rootFile, "global:\n  vault:\n    mount_path: kv\n")

		cfg, err := Load(rootFile)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Global.Vault.Address != "http://127.0.0.1:8200" {
			t.Errorf("vault.address = %q, want env value", cfg.Global.Vault.Address)
		}
		if cfg.Global.Vault.MountPath != "kv" {
			t.Errorf("vault.mount_path = %q, want kv (explicit value kept)", cfg.Global.Vault.MountPath)
		}
	})

	t.Run("explicit address wins over env", func(t *testing.T) {
		t.Setenv("VAULT_ADDR", "http://127.0.0.1:8200")
		writeFile(t, rootFile, "global:\n  vault:\n    address: https://vault.example.com:8200\n")

		cfg, err := Load(rootFile)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Global.Vault.Address != "https://vault.example.com:8200" {
			t.Errorf("vault.address = %q, want explicit value", cfg.Global.Vault.Address)
		}
	})
}

func TestRemoteBranchDefault(t *testing.T) {
	rootFile, _, _ := newRepo(t)
	writeFile(t, rootFile, `remotes:
  origin:
    url: https://example.com/origin.git
  dev:
    url: https://example.com/dev.git
    branch: develop
`)

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Remotes["origin"].Branch != "main" {
		t.Errorf("origin.branch = %q, want main (default)", cfg.Remotes["origin"].Branch)
	}
	if cfg.Remotes["dev"].Branch != "develop" {
		t.Errorf("dev.branch = %q, want develop (explicit)", cfg.Remotes["dev"].Branch)
	}
}

func TestRemoteURLRequired(t *testing.T) {
	rootFile, _, _ := newRepo(t)
	writeFile(t, rootFile, "remotes:\n  broken:\n    branch: develop\n")

	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() error = nil, want url-required error")
	}
	for _, want := range []string{rootFile, "remotes[broken]", "url is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestInvalidAuthMethod(t *testing.T) {
	rootFile, _, _ := newRepo(t)
	writeFile(t, rootFile, "global:\n  vault:\n    auth_method: ldap\n")

	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() error = nil, want invalid auth_method error")
	}
	for _, want := range []string{rootFile, "auth_method", `"ldap"`, `"token" or "approle"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestInvalidPollInterval(t *testing.T) {
	rootFile, _, _ := newRepo(t)
	writeFile(t, rootFile, "global:\n  poll_interval: fast\n")

	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() error = nil, want invalid duration error")
	}
	for _, want := range []string{rootFile, "invalid duration", "fast"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestLocalStackDiscoveryRecursive(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))

	writeFile(t, filepath.Join(hostDir, "app-a", "stack.yaml"), "name: media\n")
	writeFile(t, filepath.Join(hostDir, "app-a", "compose.yaml"), "services: {}\n")
	writeFile(t, filepath.Join(hostDir, "group", "nested", "app-b", "stack.yaml"), "{}\n")
	writeFile(t, filepath.Join(hostDir, "group", "nested", "app-b", "compose.yml"), "services: {}\n")
	writeFile(t, filepath.Join(hostDir, "tools", "monitoring.stack.yaml"), "name: monitoring\n")
	writeFile(t, filepath.Join(hostDir, "tools", "compose.yaml"), "services: {}\n")
	// No stack definition here: must be ignored.
	writeFile(t, filepath.Join(hostDir, "no-stack", "compose.yaml"), "services: {}\n")
	// .git directories are skipped during discovery.
	writeFile(t, filepath.Join(hostDir, ".git", "evil", "stack.yaml"), "name: evil\n")

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Stacks) != 3 {
		t.Fatalf("len(stacks) = %d, want 3: %+v", len(cfg.Stacks), cfg.Stacks)
	}

	media, appB, monitoring := cfg.Stacks[0], cfg.Stacks[1], cfg.Stacks[2]
	if media.Name != "media" {
		t.Errorf("stacks[0].Name = %q, want media (explicit)", media.Name)
	}
	if !media.Enabled {
		t.Errorf("stacks[0].Enabled = false, want true (default)")
	}
	if want := filepath.Join(hostDir, "app-a", "compose.yaml"); media.ComposeFile != want {
		t.Errorf("media.ComposeFile = %q, want %q", media.ComposeFile, want)
	}
	if appB.Name != "app-b" {
		t.Errorf("stacks[1].Name = %q, want app-b (folder fallback)", appB.Name)
	}
	if want := filepath.Join(hostDir, "group", "nested", "app-b", "compose.yml"); appB.ComposeFile != want {
		t.Errorf("app-b.ComposeFile = %q, want %q", appB.ComposeFile, want)
	}
	if monitoring.Name != "monitoring" {
		t.Errorf("stacks[2].Name = %q, want monitoring", monitoring.Name)
	}
	for i, s := range cfg.Stacks {
		if s.Source != "local" {
			t.Errorf("stacks[%d].Source = %q, want local", i, s.Source)
		}
	}
}

func TestCustomIncludeGlob(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n  include: [\"apps/*/stack.yaml\"]\n", workDir))

	writeFile(t, filepath.Join(hostDir, "apps", "web", "stack.yaml"), "{}\n")
	writeFile(t, filepath.Join(hostDir, "apps", "web", "compose.yaml"), "services: {}\n")
	// One level too deep for apps/*/stack.yaml.
	writeFile(t, filepath.Join(hostDir, "apps", "deep", "nested", "stack.yaml"), "{}\n")
	writeFile(t, filepath.Join(hostDir, "apps", "deep", "nested", "compose.yaml"), "services: {}\n")
	// Root level does not match either.
	writeFile(t, filepath.Join(hostDir, "stack.yaml"), "{}\n")
	writeFile(t, filepath.Join(hostDir, "compose.yaml"), "services: {}\n")

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Stacks) != 1 {
		t.Fatalf("len(stacks) = %d, want 1: %+v", len(cfg.Stacks), cfg.Stacks)
	}
	if cfg.Stacks[0].Name != "web" {
		t.Errorf("stacks[0].Name = %q, want web", cfg.Stacks[0].Name)
	}
}

func TestComposeAutoDetectionOrder(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))

	// compose.yaml pattern absent: next match in default order is compose.yml.
	writeFile(t, filepath.Join(hostDir, "s1", "stack.yaml"), "name: s1\n")
	writeFile(t, filepath.Join(hostDir, "s1", "compose.yml"), "services: {}\n")
	writeFile(t, filepath.Join(hostDir, "s1", "docker-compose.yml"), "services: {}\n")
	// Only the last pattern matches here.
	writeFile(t, filepath.Join(hostDir, "s2", "stack.yaml"), "name: s2\n")
	writeFile(t, filepath.Join(hostDir, "s2", "docker-compose.yml"), "services: {}\n")

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want := filepath.Join(hostDir, "s1", "compose.yml"); cfg.Stacks[0].ComposeFile != want {
		t.Errorf("s1.ComposeFile = %q, want %q", cfg.Stacks[0].ComposeFile, want)
	}
	if want := filepath.Join(hostDir, "s2", "docker-compose.yml"); cfg.Stacks[1].ComposeFile != want {
		t.Errorf("s2.ComposeFile = %q, want %q", cfg.Stacks[1].ComposeFile, want)
	}
}

func TestComposeExplicit(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)

	t.Run("existing file is used", func(t *testing.T) {
		writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
		writeFile(t, filepath.Join(hostDir, "app", "stack.yaml"), "name: app\ncompose_file: custom.yaml\n")
		writeFile(t, filepath.Join(hostDir, "app", "custom.yaml"), "services: {}\n")

		cfg, err := Load(rootFile)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if want := filepath.Join(hostDir, "app", "custom.yaml"); cfg.Stacks[0].ComposeFile != want {
			t.Errorf("ComposeFile = %q, want %q", cfg.Stacks[0].ComposeFile, want)
		}
	})

	t.Run("missing file is a validation error", func(t *testing.T) {
		writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
		stackFile := filepath.Join(hostDir, "app", "stack.yaml")
		writeFile(t, stackFile, "name: app\ncompose_file: missing.yaml\n")
		writeFile(t, filepath.Join(hostDir, "app", "compose.yaml"), "services: {}\n")

		_, err := Load(rootFile)
		if err == nil {
			t.Fatal("Load() error = nil, want compose_file-missing error")
		}
		for _, want := range []string{stackFile, `compose_file "missing.yaml" not found`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})
}

func TestComposeAutoDetectMissing(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
	stackFile := filepath.Join(hostDir, "app", "stack.yaml")
	writeFile(t, stackFile, "name: app\n")

	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() error = nil, want no-compose-file error")
	}
	for _, want := range []string{stackFile, "no compose file found", "compose.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestDisabledStackToleratesMissingCompose(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
	writeFile(t, filepath.Join(hostDir, "app", "stack.yaml"), "name: app\nenabled: false\n")

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Stacks) != 1 {
		t.Fatalf("len(stacks) = %d, want 1", len(cfg.Stacks))
	}
	if cfg.Stacks[0].Enabled {
		t.Errorf("Enabled = true, want false")
	}
	if cfg.Stacks[0].ComposeFile != "" {
		t.Errorf("ComposeFile = %q, want empty", cfg.Stacks[0].ComposeFile)
	}
}

func TestEmptyStackYAMLDefaultsEnabled(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
	writeFile(t, filepath.Join(hostDir, "app", "stack.yaml"), "{}\n")
	writeFile(t, filepath.Join(hostDir, "app", "compose.yaml"), "services: {}\n")

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Stacks[0].Enabled {
		t.Errorf("Enabled = false, want true (default)")
	}
}

func TestLocalStackVaultEnvTemplates(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
	writeFile(t, filepath.Join(hostDir, "app", "stack.yaml"),
		"name: app\nvault_env_templates: [vault.env, more.env]\n")
	writeFile(t, filepath.Join(hostDir, "app", "compose.yaml"), "services: {}\n")

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v (vault_env_templates must be a known field)", err)
	}
	want := []string{"vault.env", "more.env"}
	if got := cfg.Stacks[0].VaultEnvTemplates; !reflect.DeepEqual(got, want) {
		t.Errorf("VaultEnvTemplates = %v, want %v (relative, resolved at merge time)", got, want)
	}
}

func TestImportMerge(t *testing.T) {
	rootFile, _, workDir := newRepo(t)
	writeRemoteStack(t, workDir, "stacks/immich", `name: immich
compose_file: compose.yaml
profiles: [base]
services: [server, worker]
env_files: [base.env]
vault_paths: [secret/base]
vault_env_templates: [tpl-base.env]
sops_files:
  - path: base.sops
    age_keyfile: /keys/base.txt
hooks:
  pre_deploy: [echo base-pre]
  post_deploy: [echo base-post]
`, "compose.yaml")

	writeFile(t, rootFile, rootWithImports(workDir, `imports:
  - remote: infra
    path: stacks/immich
    profiles: [ml, base]
    services: [server]
    env_files: [base.env, extra.env]
    sops_files: [extra.sops]
    vault_paths: [secret/base, secret/extra]
    vault_env_templates: [tpl-base.env, tpl-extra.env]
    hooks:
      pre_deploy: [echo base-pre, echo imp-pre]
`))

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Stacks) != 1 {
		t.Fatalf("len(stacks) = %d, want 1: %+v", len(cfg.Stacks), cfg.Stacks)
	}
	s := cfg.Stacks[0]

	// Name and metadata.
	if s.Name != "immich" {
		t.Errorf("Name = %q, want immich", s.Name)
	}
	if !s.Enabled {
		t.Errorf("Enabled = false, want true (base default)")
	}
	if want := "import infra/stacks/immich"; s.Source != want {
		t.Errorf("Source = %q, want %q", s.Source, want)
	}
	if want := filepath.Join(workDir, "infra", "stacks", "immich", "compose.yaml"); s.ComposeFile != want {
		t.Errorf("ComposeFile = %q, want %q", s.ComposeFile, want)
	}

	// Concat lists: base first, deduplicated.
	if want := []string{"base", "ml"}; !reflect.DeepEqual(s.Profiles, want) {
		t.Errorf("Profiles = %v, want %v", s.Profiles, want)
	}
	if want := []string{"base.env", "extra.env"}; !reflect.DeepEqual(s.EnvFiles, want) {
		t.Errorf("EnvFiles = %v, want %v", s.EnvFiles, want)
	}
	if want := []string{"secret/base", "secret/extra"}; !reflect.DeepEqual(s.VaultPaths, want) {
		t.Errorf("VaultPaths = %v, want %v", s.VaultPaths, want)
	}
	if want := []string{"tpl-base.env", "tpl-extra.env"}; !reflect.DeepEqual(s.VaultEnvTemplates, want) {
		t.Errorf("VaultEnvTemplates = %v, want %v (concat like vault_paths, deduplicated)", s.VaultEnvTemplates, want)
	}
	wantSops := []SopsFile{{Path: "base.sops", AgeKeyfile: "/keys/base.txt"}, {Path: "extra.sops"}}
	if !reflect.DeepEqual(s.SopsFiles, wantSops) {
		t.Errorf("SopsFiles = %v, want %v", s.SopsFiles, wantSops)
	}
	if want := []string{"echo base-pre", "echo imp-pre"}; !reflect.DeepEqual(s.Hooks.PreDeploy, want) {
		t.Errorf("Hooks.PreDeploy = %v, want %v", s.Hooks.PreDeploy, want)
	}
	if want := []string{"echo base-post"}; !reflect.DeepEqual(s.Hooks.PostDeploy, want) {
		t.Errorf("Hooks.PostDeploy = %v, want %v", s.Hooks.PostDeploy, want)
	}

	// services: import list replaces the base list (subset filter).
	if want := []string{"server"}; !reflect.DeepEqual(s.Services, want) {
		t.Errorf("Services = %v, want %v (replace semantics)", s.Services, want)
	}
}

func TestImportServicesEmptyKeepsAll(t *testing.T) {
	baseYAML := "services: [server, worker]\n"

	cases := []struct {
		name       string
		importYAML string
		want       []string
	}{
		{name: "unset import services keeps base", importYAML: "", want: []string{"server", "worker"}},
		{name: "empty import services keeps base", importYAML: "    services: []\n", want: []string{"server", "worker"}},
		{name: "set import services replaces base", importYAML: "    services: [worker]\n", want: []string{"worker"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rootFile, _, workDir := newRepo(t)
			writeRemoteStack(t, workDir, "stacks/app", baseYAML, "compose.yaml")
			writeFile(t, rootFile, rootWithImports(workDir, fmt.Sprintf("imports:\n  - remote: infra\n    path: stacks/app\n%s", tc.importYAML)))

			cfg, err := Load(rootFile)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if !reflect.DeepEqual(cfg.Stacks[0].Services, tc.want) {
				t.Errorf("Services = %v, want %v", cfg.Stacks[0].Services, tc.want)
			}
		})
	}
}

func TestImportNameOverride(t *testing.T) {
	cases := []struct {
		name       string
		baseYAML   string
		importYAML string
		want       string
	}{
		{
			name:       "import name wins over base name",
			baseYAML:   "name: base-name\n",
			importYAML: "    name: imp-name\n",
			want:       "imp-name",
		},
		{
			name:       "base name wins when import has none",
			baseYAML:   "name: base-name\n",
			importYAML: "",
			want:       "base-name",
		},
		{
			name:       "import name fills missing base name",
			baseYAML:   "{}\n",
			importYAML: "    name: imp-name\n",
			want:       "imp-name",
		},
		{
			name:       "folder name fallback when both unset",
			baseYAML:   "{}\n",
			importYAML: "",
			want:       "immich",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rootFile, _, workDir := newRepo(t)
			writeRemoteStack(t, workDir, "stacks/immich", tc.baseYAML, "compose.yaml")
			writeFile(t, rootFile, rootWithImports(workDir, fmt.Sprintf("imports:\n  - remote: infra\n    path: stacks/immich\n%s", tc.importYAML)))

			cfg, err := Load(rootFile)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Stacks[0].Name != tc.want {
				t.Errorf("Name = %q, want %q", cfg.Stacks[0].Name, tc.want)
			}
		})
	}
}

func TestImportUnknownRemote(t *testing.T) {
	rootFile, _, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\nimports:\n  - remote: ghost\n    path: stacks/app\n", workDir))

	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() error = nil, want unknown-remote error")
	}
	for _, want := range []string{rootFile, "imports[0]", `unknown remote "ghost"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestImportMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name     string
		rootYAML string
		wantErr  string
	}{
		{
			name:     "missing remote",
			rootYAML: "imports:\n  - path: stacks/app\n",
			wantErr:  "imports[0]: remote is required",
		},
		{
			name:     "missing path",
			rootYAML: "imports:\n  - remote: infra\n",
			wantErr:  "imports[0]: path is required",
		},
		{
			name:     "absolute path",
			rootYAML: "imports:\n  - remote: infra\n    path: /etc\n",
			wantErr:  "must be relative",
		},
		{
			name:     "escaping path",
			rootYAML: "imports:\n  - remote: infra\n    path: ../outside\n",
			wantErr:  "must not escape",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rootFile, _, workDir := newRepo(t)
			writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\nremotes:\n  infra:\n    url: https://example.com/infra.git\n%s", workDir, tc.rootYAML))

			_, err := Load(rootFile)
			if err == nil {
				t.Fatal("Load() error = nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), rootFile) {
				t.Errorf("error %q does not contain the root file path %q", err, rootFile)
			}
		})
	}
}

func TestImportStackFileMissing(t *testing.T) {
	rootFile, _, workDir := newRepo(t)
	writeFile(t, rootFile, rootWithImports(workDir, "imports:\n  - remote: infra\n    path: stacks/nowhere\n"))

	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() error = nil, want missing-stack-definition error")
	}
	for _, want := range []string{"no stack definition found", "stacks/nowhere", "import remote="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestImportMultipleStackFiles(t *testing.T) {
	rootFile, _, workDir := newRepo(t)
	writeFile(t, filepath.Join(workDir, "infra", "stacks", "app", "a.stack.yaml"), "name: a\n")
	writeFile(t, filepath.Join(workDir, "infra", "stacks", "app", "b.stack.yaml"), "name: b\n")
	writeFile(t, rootFile, rootWithImports(workDir, "imports:\n  - remote: infra\n    path: stacks/app\n"))

	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() error = nil, want multiple-definitions error")
	}
	if want := "multiple stack definitions"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}

func TestImportStackYAMLPrecedence(t *testing.T) {
	rootFile, _, workDir := newRepo(t)
	writeFile(t, filepath.Join(workDir, "infra", "stacks", "app", "stack.yaml"), "name: primary\n")
	writeFile(t, filepath.Join(workDir, "infra", "stacks", "app", "alt.stack.yaml"), "name: alt\n")
	writeFile(t, filepath.Join(workDir, "infra", "stacks", "app", "compose.yaml"), "services: {}\n")
	writeFile(t, rootFile, rootWithImports(workDir, "imports:\n  - remote: infra\n    path: stacks/app\n"))

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Stacks[0].Name != "primary" {
		t.Errorf("Name = %q, want primary (stack.yaml takes precedence)", cfg.Stacks[0].Name)
	}
}

func TestImportDottedStackYAMLLoaded(t *testing.T) {
	rootFile, _, workDir := newRepo(t)
	dir := filepath.Join(workDir, "infra", "stacks", "app")
	writeFile(t, filepath.Join(dir, "app.stack.yaml"), "name: dotted\n")
	writeFile(t, filepath.Join(dir, "compose.yaml"), "services: {}\n")
	writeFile(t, rootFile, rootWithImports(workDir, "imports:\n  - remote: infra\n    path: stacks/app\n"))

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Stacks[0].Name != "dotted" {
		t.Errorf("Name = %q, want dotted (*.stack.yaml fallback)", cfg.Stacks[0].Name)
	}
}

func TestDuplicateNamesLocal(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
	fileA := filepath.Join(hostDir, "a", "app", "stack.yaml")
	fileB := filepath.Join(hostDir, "b", "app", "stack.yaml")
	for _, f := range []string{fileA, fileB} {
		writeFile(t, f, "{}\n")
		writeFile(t, filepath.Join(filepath.Dir(f), "compose.yaml"), "services: {}\n")
	}

	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() error = nil, want duplicate-name error")
	}
	for _, want := range []string{`duplicate stack name "app"`, fileA, fileB} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestDuplicateNamesLocalAndImport(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, filepath.Join(hostDir, "shared", "stack.yaml"), "{}\n")
	writeFile(t, filepath.Join(hostDir, "shared", "compose.yaml"), "services: {}\n")
	writeRemoteStack(t, workDir, "stacks/shared", "{}\n", "compose.yaml")
	writeFile(t, rootFile, rootWithImports(workDir, "imports:\n  - remote: infra\n    path: stacks/shared\n"))

	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() error = nil, want duplicate-name error")
	}
	for _, want := range []string{`duplicate stack name "shared"`, "local", "import infra/stacks/shared"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestYAMLSyntaxErrors(t *testing.T) {
	t.Run("root file", func(t *testing.T) {
		rootFile, _, _ := newRepo(t)
		writeFile(t, rootFile, "global: [unclosed\n")

		_, err := Load(rootFile)
		if err == nil {
			t.Fatal("Load() error = nil, want syntax error")
		}
		if !strings.Contains(err.Error(), rootFile) {
			t.Errorf("error %q does not contain the root file path %q", err, rootFile)
		}
	})

	t.Run("stack file", func(t *testing.T) {
		rootFile, hostDir, workDir := newRepo(t)
		writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
		stackFile := filepath.Join(hostDir, "app", "stack.yaml")
		writeFile(t, stackFile, "name: [unclosed\n")

		_, err := Load(rootFile)
		if err == nil {
			t.Fatal("Load() error = nil, want syntax error")
		}
		if !strings.Contains(err.Error(), stackFile) {
			t.Errorf("error %q does not contain the stack file path %q", err, stackFile)
		}
	})
}

func TestEmptyRootFile(t *testing.T) {
	rootFile, _, _ := newRepo(t)
	writeFile(t, rootFile, "")

	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() error = nil, want empty-file error")
	}
	if !strings.Contains(err.Error(), "file is empty") {
		t.Errorf("error %q does not contain %q", err, "file is empty")
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	t.Run("root file", func(t *testing.T) {
		rootFile, _, _ := newRepo(t)
		writeFile(t, rootFile, "globl: {}\n")

		_, err := Load(rootFile)
		if err == nil {
			t.Fatal("Load() error = nil, want unknown-field error")
		}
		if !strings.Contains(err.Error(), "field globl not found") {
			t.Errorf("error %q does not contain unknown-field message", err)
		}
	})

	t.Run("stack file", func(t *testing.T) {
		rootFile, hostDir, workDir := newRepo(t)
		writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
		writeFile(t, filepath.Join(hostDir, "app", "stack.yaml"), "compose_fiel: compose.yaml\n")

		_, err := Load(rootFile)
		if err == nil {
			t.Fatal("Load() error = nil, want unknown-field error")
		}
		if !strings.Contains(err.Error(), "field compose_fiel not found") {
			t.Errorf("error %q does not contain unknown-field message", err)
		}
	})
}

func TestSopsFileRequiresPath(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
	stackFile := filepath.Join(hostDir, "app", "stack.yaml")
	writeFile(t, stackFile, "sops_files:\n  - age_keyfile: /keys/k.txt\n")
	writeFile(t, filepath.Join(hostDir, "app", "compose.yaml"), "services: {}\n")

	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() error = nil, want sops path-required error")
	}
	for _, want := range []string{stackFile, "sops_files[0]: path is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestRelativeWorkDirResolvesAgainstHostRoot(t *testing.T) {
	rootFile, hostDir, _ := newRepo(t)
	writeFile(t, rootFile, "global:\n  work_dir: ../work\n")
	writeRemoteStack(t, filepath.Join(filepath.Dir(hostDir), "work"), "stacks/app", "name: app\n", "compose.yaml")
	writeFile(t, rootFile, rootWithImports("../work", "imports:\n  - remote: infra\n    path: stacks/app\n"))

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	wantDir := filepath.Join(hostDir, "..", "work")
	if cfg.Global.WorkDir != wantDir {
		t.Errorf("work_dir = %q, want %q", cfg.Global.WorkDir, wantDir)
	}
	if cfg.Stacks[0].Name != "app" {
		t.Errorf("Name = %q, want app (import resolved via relative work_dir)", cfg.Stacks[0].Name)
	}
}

func TestIgnoreServicesLocalStack(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
	// Disjoint lists (the overlap is a validation error, see below); the
	// loader must keep both as configured.
	writeFile(t, filepath.Join(hostDir, "app", "stack.yaml"), "name: app\nservices: [api, worker]\nignore_services: [debug]\n")
	writeFile(t, filepath.Join(hostDir, "app", "compose.yaml"), "services: {}\n")

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want := []string{"api", "worker"}; !reflect.DeepEqual(cfg.Stacks[0].Services, want) {
		t.Errorf("Services = %v, want %v (loaded as configured)", cfg.Stacks[0].Services, want)
	}
	if want := []string{"debug"}; !reflect.DeepEqual(cfg.Stacks[0].IgnoreServices, want) {
		t.Errorf("IgnoreServices = %v, want %v", cfg.Stacks[0].IgnoreServices, want)
	}
	// Disjoint ignore lists leave the configured services untouched.
	if want := []string{"api", "worker"}; !reflect.DeepEqual(cfg.Stacks[0].EffectiveServices(), want) {
		t.Errorf("EffectiveServices() = %v, want %v", cfg.Stacks[0].EffectiveServices(), want)
	}
}

func TestExcludeServices(t *testing.T) {
	cases := []struct {
		name     string
		services []string
		ignored  []string
		want     []string
	}{
		{name: "nothing ignored", services: []string{"a", "b"}, ignored: nil, want: []string{"a", "b"}},
		{name: "subtraction preserves order", services: []string{"a", "b", "c"}, ignored: []string{"b"}, want: []string{"a", "c"}},
		{name: "ignored entry not present", services: []string{"a", "b"}, ignored: []string{"x"}, want: []string{"a", "b"}},
		{name: "everything ignored", services: []string{"a"}, ignored: []string{"a"}, want: []string{}},
		{name: "empty services", services: nil, ignored: []string{"a"}, want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExcludeServices(tc.services, tc.ignored); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExcludeServices(%v, %v) = %v, want %v", tc.services, tc.ignored, got, tc.want)
			}
		})
	}
}

func TestServicesIgnoreOverlapRejected(t *testing.T) {
	t.Run("local stack", func(t *testing.T) {
		rootFile, hostDir, workDir := newRepo(t)
		writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
		stackFile := filepath.Join(hostDir, "app", "stack.yaml")
		writeFile(t, stackFile, "name: app\nservices: [api, worker]\nignore_services: [worker]\n")
		writeFile(t, filepath.Join(hostDir, "app", "compose.yaml"), "services: {}\n")

		_, err := Load(rootFile)
		if err == nil {
			t.Fatal("Load() error = nil, want overlap error")
		}
		for _, want := range []string{stackFile, "services and ignore_services overlap", "worker"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})

	t.Run("overlap only arises from the import merge", func(t *testing.T) {
		rootFile, _, workDir := newRepo(t)
		writeRemoteStack(t, workDir, "stacks/app", "name: app\nservices: [api, worker]\n", "compose.yaml")
		writeFile(t, rootFile, rootWithImports(workDir, "imports:\n  - remote: infra\n    path: stacks/app\n    ignore_services: [worker]\n"))

		_, err := Load(rootFile)
		if err == nil {
			t.Fatal("Load() error = nil, want overlap error from the merged stack")
		}
		if !strings.Contains(err.Error(), "services and ignore_services overlap") {
			t.Errorf("error %q does not contain the overlap message", err)
		}
	})
}

func TestImportIgnoreServicesConcat(t *testing.T) {
	rootFile, _, workDir := newRepo(t)
	writeRemoteStack(t, workDir, "stacks/app", "name: app\nservices: [api]\nignore_services: [debug, base-only]\n", "compose.yaml")
	writeFile(t, rootFile, rootWithImports(workDir, `imports:
  - remote: infra
    path: stacks/app
    ignore_services: [debug, imp-only]
`))

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	s := cfg.Stacks[0]
	// Concat + dedup, base first — like profiles and env_files.
	if want := []string{"debug", "base-only", "imp-only"}; !reflect.DeepEqual(s.IgnoreServices, want) {
		t.Errorf("IgnoreServices = %v, want %v (concat base + import, deduplicated)", s.IgnoreServices, want)
	}
	// Base services survive; only import services replace them.
	if want := []string{"api"}; !reflect.DeepEqual(s.Services, want) {
		t.Errorf("Services = %v, want %v (ignore does not touch services)", s.Services, want)
	}
}

func TestComposeFilesExplicit(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
	appDir := filepath.Join(hostDir, "app")
	writeFile(t, filepath.Join(appDir, "stack.yaml"), "name: app\ncompose_files: [compose.yaml, override.yaml]\n")
	writeFile(t, filepath.Join(appDir, "compose.yaml"), "services: {}\n")
	writeFile(t, filepath.Join(appDir, "override.yaml"), "services: {}\n")

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	s := cfg.Stacks[0]
	want := []string{filepath.Join(appDir, "compose.yaml"), filepath.Join(appDir, "override.yaml")}
	if !reflect.DeepEqual(s.ComposeFiles, want) {
		t.Errorf("ComposeFiles = %v, want %v (resolved, list order)", s.ComposeFiles, want)
	}
	if s.ComposeFile != "" {
		t.Errorf("ComposeFile = %q, want empty in the multi-file case", s.ComposeFile)
	}
	if got := s.EffectiveComposeFiles(); !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveComposeFiles() = %v, want %v", got, want)
	}
}

func TestComposeFilesValidation(t *testing.T) {
	cases := []struct {
		name      string
		stackYAML string
		wantErr   string
	}{
		{
			name:      "compose_file and compose_files together",
			stackYAML: "name: app\ncompose_file: compose.yaml\ncompose_files: [compose.yaml, override.yaml]\n",
			wantErr:   "compose_file and compose_files are mutually exclusive",
		},
		{
			name:      "missing file",
			stackYAML: "name: app\ncompose_files: [compose.yaml, missing.yaml]\n",
			wantErr:   `compose_files[1] "missing.yaml" not found`,
		},
		{
			name:      "explicitly empty list",
			stackYAML: "name: app\ncompose_files: []\n",
			wantErr:   "compose_files must list at least one file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rootFile, hostDir, workDir := newRepo(t)
			writeFile(t, rootFile, fmt.Sprintf("global:\n  work_dir: %q\n", workDir))
			appDir := filepath.Join(hostDir, "app")
			writeFile(t, filepath.Join(appDir, "stack.yaml"), tc.stackYAML)
			writeFile(t, filepath.Join(appDir, "compose.yaml"), "services: {}\n")
			writeFile(t, filepath.Join(appDir, "override.yaml"), "services: {}\n")

			_, err := Load(rootFile)
			if err == nil {
				t.Fatal("Load() error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestImportComposeFiles(t *testing.T) {
	rootFile, _, workDir := newRepo(t)
	writeRemoteStack(t, workDir, "stacks/app", "name: app\ncompose_files: [base.yaml, prod.yaml]\n", "base.yaml", "prod.yaml")
	writeFile(t, rootFile, rootWithImports(workDir, "imports:\n  - remote: infra\n    path: stacks/app\n"))

	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	dir := filepath.Join(workDir, testRemote, "stacks", "app")
	want := []string{filepath.Join(dir, "base.yaml"), filepath.Join(dir, "prod.yaml")}
	if got := cfg.Stacks[0].EffectiveComposeFiles(); !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveComposeFiles() = %v, want %v (import stacks resolve compose_files against the remote checkout)", got, want)
	}
}
