// Package config loads and validates the declarative dockops configuration:
// the root stacks.yaml, all local stack definitions discovered via the
// include globs, and the import stacks merged from remote checkouts.
package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML scalar strings such as "5m" or "90s"
// are parsed with time.ParseDuration.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a string like \"5m\": %w", node.Line, err)
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q: %w", node.Line, s, err)
	}
	*d = Duration(dur)
	return nil
}

// Root is the parsed representation of the root stacks.yaml file in the host
// repository root.
type Root struct {
	Global  Global            `yaml:"global"`
	Remotes map[string]Remote `yaml:"remotes"`
	Imports []Import          `yaml:"imports"`
}

// Global holds the global section of stacks.yaml. All fields have smart
// defaults applied by the loader when unset (see applyRootDefaults).
type Global struct {
	PollInterval        Duration `yaml:"poll_interval"`
	PruneRemovedStacks  bool     `yaml:"prune_removed_stacks"`
	WorkDir             string   `yaml:"work_dir"`
	// HostCheckout is the absolute path of the host repository checkout.
	// Empty (the default) checks the host repo out at <WorkDir>/host as
	// before; a set value makes dockops work in place on the clone at that
	// path (adopt-or-clone) — fleet hosts where the real clone already
	// exists (e.g. /home/docker/apps) need no bind-mount gymnastics. The
	// env variable EnvHostCheckout fills the gap when the YAML leaves the
	// field unset (the YAML value wins, like VAULT_ADDR).
	HostCheckout string `yaml:"host_checkout"`
	Include             []string `yaml:"include"`
	ComposeFilePatterns []string `yaml:"compose_file_patterns"`
	Vault               Vault    `yaml:"vault"`
}

// Vault configures access to a HashiCorp Vault. The address is "smart
// default or env": an explicit value wins, otherwise VAULT_ADDR is used.
// Credential env var names are configurable with sensible defaults.
type Vault struct {
	Address         string `yaml:"address"`
	MountPath       string `yaml:"mount_path"`
	AuthMethod      string `yaml:"auth_method"`
	TokenFromEnv    string `yaml:"token_from_env"`
	RoleIDFromEnv   string `yaml:"role_id_from_env"`
	SecretIDFromEnv string `yaml:"secret_id_from_env"`
}

// Remote describes a git remote. Its checkout is expected at
// <global.work_dir>/<remote name>/ unless Checkout overrides the location.
type Remote struct {
	URL    string `yaml:"url"`
	Branch string `yaml:"branch"`
	// Checkout is the absolute path of this remote's checkout. Empty uses
	// <global.work_dir>/<remote name>; a set value makes dockops work in
	// place on the clone at that path (adopt-or-clone, the recorded origin
	// must match url — see git.Manager.EnsureRepoAt).
	Checkout string `yaml:"checkout"`
}

// Import references a stack definition inside a remote checkout plus
// overrides that are merged on top of the loaded base stack.
type Import struct {
	Remote            string   `yaml:"remote"`
	Path              string   `yaml:"path"`
	Name              string   `yaml:"name"`
	Profiles          []string `yaml:"profiles"`
	Services          []string `yaml:"services"`
	IgnoreServices    []string `yaml:"ignore_services"`
	EnvFiles          []string `yaml:"env_files"`
	SopsFiles         []string `yaml:"sops_files"`
	VaultPaths        []string `yaml:"vault_paths"`
	VaultEnvTemplates []string `yaml:"vault_env_templates"`
	Hooks             Hooks    `yaml:"hooks"`
}

// Hooks holds shell commands that run before and after a stack deployment.
type Hooks struct {
	PreDeploy  []string `yaml:"pre_deploy"`
	PostDeploy []string `yaml:"post_deploy"`
}

// SopsFile references an sops-encrypted file. AgeKeyfile optionally points
// to an age key file; resolving it is up to the caller.
type SopsFile struct {
	Path       string `yaml:"path"`
	AgeKeyfile string `yaml:"age_keyfile"`
}

// Stack is a single stack definition loaded from stack.yaml or *.stack.yaml.
// EnvFiles and SopsFiles paths are relative to Dir; ComposeFile and
// ComposeFiles are resolved to concrete paths by the loader.
type Stack struct {
	Name        string `yaml:"name"`
	Enabled     bool   `yaml:"enabled"`
	ComposeFile string `yaml:"compose_file"`
	// ComposeFiles lists multiple compose files that docker compose merges
	// in list order (later files win). Mutually exclusive with ComposeFile.
	ComposeFiles   []string   `yaml:"compose_files"`
	Profiles       []string   `yaml:"profiles"`
	Services       []string   `yaml:"services"`
	IgnoreServices []string   `yaml:"ignore_services"`
	EnvFiles       []string   `yaml:"env_files"`
	SopsFiles      []SopsFile `yaml:"sops_files"`
	VaultPaths     []string   `yaml:"vault_paths"`
	// VaultEnvTemplates lists dotenv template files (relative to Dir) whose
	// ${VAULT:<path>#<key>} references are resolved against Vault and merged
	// into the stack environment at the same priority as vault_paths —
	// compatibility with existing vault.env files (see env.RenderVaultTemplate).
	VaultEnvTemplates []string `yaml:"vault_env_templates"`
	Hooks             Hooks    `yaml:"hooks"`

	// Load metadata, not part of the YAML schema. Dir is the directory
	// containing the definition, File the YAML file it was loaded from, and
	// Source is "local" or "import <remote>/<path>".
	Dir    string `yaml:"-"`
	File   string `yaml:"-"`
	Source string `yaml:"-"`
}

// EffectiveComposeFiles returns every compose file of the stack in docker
// compose -f order: the multi-file compose_files list when set (the loader
// resolves it to absolute paths), otherwise the single ComposeFile (which
// hand-constructed stacks may still carry as a relative path — callers like
// the compose executor resolve entries against the stack directory).
func (s Stack) EffectiveComposeFiles() []string {
	if len(s.ComposeFiles) > 0 {
		return s.ComposeFiles
	}
	if s.ComposeFile != "" {
		return []string{s.ComposeFile}
	}
	return nil
}

// EffectiveServices returns the services docker compose up receives as
// positional arguments when the stack configures services: Services minus
// IgnoreServices, in configured order. Nil means "no restriction": either
// neither list is set (deploy everything) or only ignore_services is set —
// the latter case cannot be answered from the stack definition alone, so
// the compose executor subtracts ignore_services from the service names of
// the compose files instead. An empty (non-nil) result means every
// configured service is ignored, which the loader rejects as contradictory.
func (s Stack) EffectiveServices() []string {
	return ExcludeServices(s.Services, s.IgnoreServices)
}

// ExcludeServices returns services without the entries of ignored, first
// occurrence per name, order otherwise preserved. The result may be empty
// (everything ignored) — callers decide whether that is an error. It is the
// shared set operation behind the service filters (see EffectiveServices
// and the compose executor's ignore handling).
func ExcludeServices(services, ignored []string) []string {
	if len(ignored) == 0 {
		return services
	}
	drop := make(map[string]struct{}, len(ignored))
	for _, name := range ignored {
		drop[name] = struct{}{}
	}
	out := make([]string, 0, len(services))
	for _, name := range services {
		if _, ok := drop[name]; ok {
			continue
		}
		out = append(out, name)
	}
	return out
}
