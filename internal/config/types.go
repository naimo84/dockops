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
	// Notifications is the optional, host-global notification block. Nil
	// (the key is absent) means the feature is off; a present block is
	// validated even when Enabled is false.
	Notifications *Notifications `yaml:"notifications"`
	// Registries holds optional credentials for private registries, used by
	// the image update check (phase 10). An empty list means everything is
	// resolved anonymously (Docker Hub, ghcr.io and friends allow that).
	Registries []Registry `yaml:"registries"`
}

// Registry is one registry login of the image update check. Credentials are
// only sent to that host's token endpoint; anonymous access needs no entry.
// String fields may reference environment variables as ${VAR} (same secret
// path as the notification destinations: the loader expands them from the
// process environment, e.g. from vault-env).
type Registry struct {
	// Host is the registry host as it appears in an image reference,
	// optionally with a port ("gitea.example.com:3000", no scheme).
	Host string `yaml:"host"`
	// Username and Password authenticate the token request.
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Notifications configures where the agent reports failed reconciliation
// passes. It is host-global (root stacks.yaml, not per stack) and therefore
// deliberately not part of the stack hash. All defaults are applied by the
// loader: enabled=true, all three events, repeat_after=1h, timeout=10s.
type Notifications struct {
	// Enabled false keeps the block loaded and validated but sends
	// nothing (maintenance window).
	Enabled bool `yaml:"enabled"`
	// On lists the events to deliver: deploy_failed, prune_failed and/or
	// repo_failed.
	On []string `yaml:"on"`
	// RepeatAfter is the dedupe window per (kind, stack): one alarm per
	// failure and window.
	RepeatAfter Duration `yaml:"repeat_after"`
	// Timeout bounds every single delivery attempt.
	Timeout Duration `yaml:"timeout"`
	// Destinations are the delivery channels, tried one after the other.
	Destinations []NotificationDestination `yaml:"destinations"`
}

// NotificationDestination is one delivery channel. Only the fields that
// belong to its Type are used; the required ones are checked by the loader
// (webhook/ntfy → url, telegram → token + chat_id, discord/slack →
// webhook_url). String fields may reference environment variables as
// ${VAR}; the loader expands them from the process environment.
type NotificationDestination struct {
	Type string `yaml:"type"`
	// URL is the endpoint of a webhook or ntfy destination.
	URL string `yaml:"url"`
	// Headers are extra HTTP headers for a webhook destination.
	Headers map[string]string `yaml:"headers"`
	// Priority and Tags become ntfy's Priority/Tags headers.
	Priority string   `yaml:"priority"`
	Tags     []string `yaml:"tags"`
	// Token and ChatID address a telegram bot destination.
	Token  string `yaml:"token"`
	ChatID string `yaml:"chat_id"`
	// WebhookURL is the endpoint of a discord or slack destination.
	WebhookURL string `yaml:"webhook_url"`
}

// Global holds the global section of stacks.yaml. All fields have smart
// defaults applied by the loader when unset (see applyRootDefaults).
type Global struct {
	PollInterval       Duration `yaml:"poll_interval"`
	PruneRemovedStacks bool     `yaml:"prune_removed_stacks"`
	WorkDir            string   `yaml:"work_dir"`
	// HostCheckout is the absolute path of the host repository checkout.
	// Empty (the default) checks the host repo out at <WorkDir>/host as
	// before; a set value makes dockops work in place on the clone at that
	// path (adopt-or-clone) — fleet hosts where the real clone already
	// exists (e.g. /home/docker/apps) need no bind-mount gymnastics. The
	// env variable EnvHostCheckout fills the gap when the YAML leaves the
	// field unset (the YAML value wins, like VAULT_ADDR).
	HostCheckout        string   `yaml:"host_checkout"`
	Include             []string `yaml:"include"`
	ComposeFilePatterns []string `yaml:"compose_file_patterns"`
	Vault               Vault    `yaml:"vault"`
	// UpdateCheckInterval configures how often the image update check asks
	// the registries. Nil (the key is absent) takes
	// DefaultUpdateCheckInterval; an explicit 0 disables the check
	// entirely — the global emergency brake for rate-limited registries.
	// A pointer is needed because 0 is a meaningful setting, not "unset".
	UpdateCheckInterval *Duration `yaml:"update_check_interval"`
}

// DefaultUpdateCheckInterval is the interval of the image update check when
// update_check_interval is not configured.
const DefaultUpdateCheckInterval = time.Hour

// EffectiveUpdateCheckInterval returns the configured interval, or the
// default when the key is absent. Zero (or negative) means "disabled".
func (g Global) EffectiveUpdateCheckInterval() time.Duration {
	if g.UpdateCheckInterval == nil {
		return DefaultUpdateCheckInterval
	}
	return time.Duration(*g.UpdateCheckInterval)
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
	// UpdatePolicy decides what the image update check does with a stack
	// whose image tag moved: "notify" (default) only reports it, "pinned"
	// skips the stack completely (no registry lookups, no state records),
	// and "apply" pulls the new image, recreates exactly the affected
	// services and health-gates them, rolling back to the previous digest
	// when the gate fails (phase 11).
	// Deliberately not part of the stack hash: it changes no deployment, so
	// editing it must not redeploy anything.
	UpdatePolicy string `yaml:"update_policy"`
	// HealthTimeout bounds the health gate of update_policy: apply — how
	// long the reconciler polls the affected services for "running and
	// healthy" after recreating them. Absent or 0 means
	// DefaultHealthTimeout; a negative value is a load error (there is no
	// documented way to switch the gate off, and a gate that is skipped is
	// exactly the blind apply this phase exists to prevent). Like
	// UpdatePolicy it stays out of the stack hash: it tunes the apply step,
	// not the deployment the apply step targets.
	HealthTimeout Duration `yaml:"health_timeout"`

	// Load metadata, not part of the YAML schema. Dir is the directory
	// containing the definition, File the YAML file it was loaded from, and
	// Source is "local" or "import <remote>/<path>".
	Dir    string `yaml:"-"`
	File   string `yaml:"-"`
	Source string `yaml:"-"`
}

// Stack update policies (stack.yaml update_policy).
const (
	// UpdatePolicyNotify reports a new image digest through the notifier
	// and GET /status and deploys nothing. It is the default.
	UpdatePolicyNotify = "notify"
	// UpdatePolicyPinned skips the stack in the image update check: its
	// images are expected to be maintained deliberately (or the registry
	// must not be polled for it).
	UpdatePolicyPinned = "pinned"
	// UpdatePolicyApply applies a detected update: pull, recreate the
	// affected services only, health-gate them, roll back on failure
	// (phase 11). It runs at most once per update_check_interval and never
	// in a pass whose deployment of the stack failed.
	UpdatePolicyApply = "apply"
)

// DefaultHealthTimeout is the health gate window of update_policy: apply
// when health_timeout is not configured. 90s leaves room for a slow image
// start without turning every bad image into a two-minute outage of the
// stack.
const DefaultHealthTimeout = 90 * time.Second

// EffectiveUpdatePolicy returns the stack's update policy, defaulting to
// UpdatePolicyNotify for a hand-built stack that carries no value.
func (s Stack) EffectiveUpdatePolicy() string {
	if s.UpdatePolicy == "" {
		return UpdatePolicyNotify
	}
	return s.UpdatePolicy
}

// ChecksImageUpdates reports whether the image update check considers this
// stack: everything except UpdatePolicyPinned.
func (s Stack) ChecksImageUpdates() bool {
	return s.EffectiveUpdatePolicy() != UpdatePolicyPinned
}

// AppliesImageUpdates reports whether a detected update is applied by the
// reconciler (update_policy: apply) instead of only reported.
func (s Stack) AppliesImageUpdates() bool {
	return s.EffectiveUpdatePolicy() == UpdatePolicyApply
}

// EffectiveHealthTimeout returns the configured health gate window, or
// DefaultHealthTimeout when the key is absent or 0. Hand-built stacks (tests,
// the prune ghost) never pass through the loader's default, so the default is
// repeated here — the same pattern as EffectiveUpdatePolicy.
func (s Stack) EffectiveHealthTimeout() time.Duration {
	if s.HealthTimeout <= 0 {
		return DefaultHealthTimeout
	}
	return time.Duration(s.HealthTimeout)
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
