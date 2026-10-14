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
// <global.work_dir>/<remote name>/.
type Remote struct {
	URL    string `yaml:"url"`
	Branch string `yaml:"branch"`
}

// Import references a stack definition inside a remote checkout plus
// overrides that are merged on top of the loaded base stack.
type Import struct {
	Remote     string   `yaml:"remote"`
	Path       string   `yaml:"path"`
	Name       string   `yaml:"name"`
	Profiles   []string `yaml:"profiles"`
	Services   []string `yaml:"services"`
	EnvFiles   []string `yaml:"env_files"`
	SopsFiles  []string `yaml:"sops_files"`
	VaultPaths []string `yaml:"vault_paths"`
	Hooks      Hooks    `yaml:"hooks"`
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
// EnvFiles and SopsFiles paths are relative to Dir; ComposeFile is resolved
// to a concrete path by the loader.
type Stack struct {
	Name        string     `yaml:"name"`
	Enabled     bool       `yaml:"enabled"`
	ComposeFile string     `yaml:"compose_file"`
	Profiles    []string   `yaml:"profiles"`
	Services    []string   `yaml:"services"`
	EnvFiles    []string   `yaml:"env_files"`
	SopsFiles   []SopsFile `yaml:"sops_files"`
	VaultPaths  []string   `yaml:"vault_paths"`
	Hooks       Hooks      `yaml:"hooks"`

	// Load metadata, not part of the YAML schema. Dir is the directory
	// containing the definition, File the YAML file it was loaded from, and
	// Source is "local" or "import <remote>/<path>".
	Dir    string `yaml:"-"`
	File   string `yaml:"-"`
	Source string `yaml:"-"`
}
