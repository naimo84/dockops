package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultWorkDir is the fallback for global.work_dir. It is exported so the
// reconciler can bootstrap the host checkout below it before the root
// configuration (which declares work_dir) is loadable.
const DefaultWorkDir = "/var/lib/dockops/repos"

// EnvHostCheckout is the environment fallback for global.host_checkout: an
// explicit YAML value wins, the env only fills a missing field (same
// precedence as VAULT_ADDR). Exported because the one-shot status mode
// (cmd/dockops -print-status) follows the same variable without loading a
// configuration.
const EnvHostCheckout = "DOCKOPS_HOST_CHECKOUT"

// Smart defaults applied when the corresponding field is unset in stacks.yaml.
const (
	defaultPollInterval     = 5 * time.Minute
	defaultVaultMountPath   = "secret"
	defaultVaultAuthMethod  = "token"
	defaultVaultTokenEnv    = "VAULT_TOKEN"
	defaultVaultRoleIDEnv   = "VAULT_ROLE_ID"
	defaultVaultSecretIDEnv = "VAULT_SECRET_ID"
	defaultRemoteBranch     = "main"

	defaultNotificationRepeatAfter = time.Hour
	defaultNotificationTimeout     = 10 * time.Second
)

// Notification event kinds, the accepted values of notifications.on and the
// kinds the reconciler reports through the notifier.
const (
	NotificationDeployFailed = "deploy_failed"
	NotificationPruneFailed  = "prune_failed"
	NotificationRepoFailed   = "repo_failed"
	// NotificationUpdateAvailable is the phase-10 image update event. It
	// is valid in notifications.on but deliberately NOT part of the
	// default set: a new image is normal operation, and nobody should be
	// messaged about it without opting in.
	NotificationUpdateAvailable = "update_available"
	// NotificationUpdateApplied is the phase-11 event of a successfully
	// applied image update. Like update_available it is opt-in: a working
	// apply is normal operation, and its record is in GET /status.
	NotificationUpdateApplied = "update_applied"
	// NotificationUpdateFailed reports a failed apply: pull error, failed
	// compose up, failed health gate or — the case a human is needed for —
	// a failed rollback. Failure class, therefore part of the default set
	// (like deploy_failed).
	NotificationUpdateFailed = "update_failed"
)

// defaultNotificationEvents is the `on` default: the failure events only.
// update_available and update_applied stay out until notifications.on
// requests them; update_failed belongs here for the same reason
// deploy_failed does — a stack whose update could not be applied (or not
// even rolled back) is broken, not routine.
var defaultNotificationEvents = []string{
	NotificationDeployFailed,
	NotificationPruneFailed,
	NotificationRepoFailed,
	NotificationUpdateFailed,
}

var (
	defaultInclude             = []string{"**/stack.yaml", "**/*.stack.yaml"}
	defaultComposeFilePatterns = []string{"compose.yaml", "compose.yml", "compose.docker-compose.yaml", "docker-compose.yml"}
)

// Config is the fully resolved dockops configuration: the root file with all
// defaults applied, plus every local and imported stack. Disabled stacks are
// included; callers filter by Stack.Enabled.
type Config struct {
	Global  Global
	Remotes map[string]Remote
	Imports []Import
	Stacks  []Stack
	// Notifications is the optional host-global notification block (nil
	// when the root config has none). The reconciler builds its notifier
	// from it.
	Notifications *Notifications
	// Registries holds the optional registry credentials of the image
	// update check (phase 10), expanded and validated by the loader.
	Registries []Registry
}

// Load parses the root configuration at rootFile (stacks.yaml in the host
// repository root), discovers all local stack definitions via
// global.include, loads and merges every import from the remote checkouts
// under global.work_dir, validates the combined result and returns it.
// Every failure is returned as an error with file, path and field context.
func Load(rootFile string) (*Config, error) {
	root, err := loadRoot(rootFile)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Global:        root.Global,
		Remotes:       root.Remotes,
		Imports:       root.Imports,
		Notifications: root.Notifications,
		Registries:    root.Registries,
	}

	// The host repository root is the directory of the root config file.
	// Relative work_dir values resolve against it (conservative choice; the
	// briefing only shows absolute paths).
	hostRoot := filepath.Dir(rootFile)
	if cfg.Global.WorkDir != "" && !filepath.IsAbs(cfg.Global.WorkDir) {
		cfg.Global.WorkDir = filepath.Join(hostRoot, cfg.Global.WorkDir)
	}

	local, err := discoverStacks(hostRoot, cfg.Global.Include, cfg.Global.ComposeFilePatterns)
	if err != nil {
		return nil, fmt.Errorf("discover local stacks below %s: %w", hostRoot, err)
	}
	cfg.Stacks = append(cfg.Stacks, local...)

	for _, imp := range cfg.Imports {
		rem := cfg.Remotes[imp.Remote] // existence validated by loadRoot
		stack, err := loadImportStack(cfg.Global, rem, imp)
		if err != nil {
			return nil, fmt.Errorf("import remote=%q path=%q: %w", imp.Remote, imp.Path, err)
		}
		cfg.Stacks = append(cfg.Stacks, stack)
	}

	if err := checkDuplicateNames(cfg.Stacks); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadRoot parses, validates and applies defaults to only the root
// stacks.yaml at path, without discovering local stacks and without loading
// imports. The reconciler needs it to read remotes and global settings
// before the remote checkouts exist (imports would fail to load); everyone
// else wants Load.
func LoadRoot(path string) (*Root, error) {
	return loadRoot(path)
}

// loadRoot reads, decodes and validates the root stacks.yaml.
func loadRoot(path string) (*Root, error) {
	var doc rootYAML
	if err := decodeYAMLFile(path, &doc); err != nil {
		return nil, fmt.Errorf("load root config: %w", err)
	}
	root := Root{Global: doc.Global, Remotes: doc.Remotes, Imports: doc.Imports}
	if doc.Notifications != nil {
		n, err := buildNotifications(*doc.Notifications, path)
		if err != nil {
			return nil, err
		}
		root.Notifications = n
	}
	registries, err := buildRegistries(doc.Registries, path)
	if err != nil {
		return nil, err
	}
	root.Registries = registries
	applyRootDefaults(&root)
	if err := validateRoot(&root, path); err != nil {
		return nil, err
	}
	return &root, nil
}

// applyRootDefaults fills unset fields with the smart defaults. Note on
// prune_removed_stacks: the briefing shows the placeholder value "smart";
// it is interpreted as a bool with a conservative default of false (never
// remove resources without explicit opt-in).
func applyRootDefaults(root *Root) {
	g := &root.Global
	if g.PollInterval == 0 {
		g.PollInterval = Duration(defaultPollInterval)
	}
	if g.WorkDir == "" {
		g.WorkDir = DefaultWorkDir
	}
	if len(g.Include) == 0 {
		g.Include = slices.Clone(defaultInclude)
	}
	if len(g.ComposeFilePatterns) == 0 {
		g.ComposeFilePatterns = slices.Clone(defaultComposeFilePatterns)
	}
	if g.Vault.MountPath == "" {
		g.Vault.MountPath = defaultVaultMountPath
	}
	if g.Vault.AuthMethod == "" {
		g.Vault.AuthMethod = defaultVaultAuthMethod
	}
	if g.Vault.TokenFromEnv == "" {
		g.Vault.TokenFromEnv = defaultVaultTokenEnv
	}
	if g.Vault.RoleIDFromEnv == "" {
		g.Vault.RoleIDFromEnv = defaultVaultRoleIDEnv
	}
	if g.Vault.SecretIDFromEnv == "" {
		g.Vault.SecretIDFromEnv = defaultVaultSecretIDEnv
	}
	if g.UpdateCheckInterval == nil {
		// The key is absent: fill the default explicitly so every reader
		// sees the effective value. An explicit 0 stays 0 (= off).
		d := Duration(DefaultUpdateCheckInterval)
		g.UpdateCheckInterval = &d
	}
	// "smartDefaultOrEnv(VAULT_ADDR)": explicit value wins, env as fallback.
	if g.Vault.Address == "" {
		g.Vault.Address = os.Getenv("VAULT_ADDR")
	}
	// Same precedence for the host checkout path (EnvHostCheckout): the YAML
	// value wins, the env fills a missing field. Applied before validation so
	// an invalid env value fails with the same field context as an invalid
	// YAML value.
	if g.HostCheckout == "" {
		g.HostCheckout = os.Getenv(EnvHostCheckout)
	}
	// Map iteration is nondeterministic, but this loop only mutates in place.
	for name, rem := range root.Remotes {
		if rem.Branch == "" {
			rem.Branch = defaultRemoteBranch
			root.Remotes[name] = rem
		}
	}
}

// validateRoot checks required fields and cross-references in the root config.
func validateRoot(root *Root, file string) error {
	switch root.Global.Vault.AuthMethod {
	case "token", "approle":
	default:
		return fmt.Errorf("%s: global.vault.auth_method: invalid value %q (must be \"token\" or \"approle\")", file, root.Global.Vault.AuthMethod)
	}
	if err := validateCheckoutPath(root.Global.HostCheckout); err != nil {
		return fmt.Errorf("%s: global.host_checkout: %w", file, err)
	}
	if root.Global.EffectiveUpdateCheckInterval() < 0 {
		// 0 is the documented "off" switch; a negative interval is a
		// configuration mistake (and would mean "always due" here).
		return fmt.Errorf("%s: global.update_check_interval: must not be negative (0 disables the update check)", file)
	}
	for _, name := range sortedRemoteNames(root.Remotes) {
		if root.Remotes[name].URL == "" {
			return fmt.Errorf("%s: remotes[%s]: url is required", file, name)
		}
		if err := validateCheckoutPath(root.Remotes[name].Checkout); err != nil {
			return fmt.Errorf("%s: remotes[%s].checkout: %w", file, name, err)
		}
	}
	for i, imp := range root.Imports {
		if imp.Remote == "" {
			return fmt.Errorf("%s: imports[%d]: remote is required", file, i)
		}
		if _, ok := root.Remotes[imp.Remote]; !ok {
			return fmt.Errorf("%s: imports[%d]: unknown remote %q (not defined in remotes)", file, i, imp.Remote)
		}
		if imp.Path == "" {
			return fmt.Errorf("%s: imports[%d]: path is required", file, i)
		}
		if err := validateImportPath(imp.Path); err != nil {
			return fmt.Errorf("%s: imports[%d]: %w", file, i, err)
		}
	}
	return nil
}

// sortedRemoteNames returns the remote names in deterministic order so that
// validation errors are reproducible.
func sortedRemoteNames(remotes map[string]Remote) []string {
	names := make([]string, 0, len(remotes))
	for name := range remotes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateCheckoutPath checks a configured checkout path
// (global.host_checkout, remotes.<name>.checkout): non-empty values must be
// absolute and limited to [A-Za-z0-9._/-]. The charset keeps whitespace,
// quotes and shell metacharacters out of paths that end up in git command
// lines and log output. Unlike import paths there is deliberately no prefix
// jail — the path is host configuration naming a clone anywhere on the host,
// not client input confined to a checkout.
func validateCheckoutPath(p string) error {
	if p == "" {
		return nil
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("path %q must be absolute", p)
	}
	for i := 0; i < len(p); i++ {
		if !isCheckoutPathByte(p[i]) {
			return fmt.Errorf("path %q must only contain the characters A-Z a-z 0-9 . _ - / (no whitespace, quotes or shell metacharacters)", p)
		}
	}
	return nil
}

// isCheckoutPathByte reports whether c is one of the checkout path
// characters [A-Za-z0-9._/-]. Byte-wise checking is safe: every allowed
// character is ASCII, and every byte of a multi-byte UTF-8 sequence is >= 0x80
// and therefore rejected.
func isCheckoutPathByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.' || c == '_' || c == '-' || c == '/':
		return true
	}
	return false
}

// validateImportPath rejects absolute paths and paths escaping the remote
// checkout. The briefing does not specify this; rejecting is the
// conservative choice and yields a clear error instead of surprising reads
// outside the checkout.
func validateImportPath(p string) error {
	if filepath.IsAbs(p) {
		return fmt.Errorf("path %q must be relative to the remote checkout", p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q must not escape the remote checkout", p)
	}
	return nil
}

// decodeYAMLFile strictly decodes the YAML file at path into out. Unknown
// fields are rejected so typos surface as load errors instead of being
// silently ignored.
func decodeYAMLFile(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s: file is empty", path)
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// discoverStacks finds all stack definition files below root that match any
// of the include globs and loads each one.
func discoverStacks(root string, include, composeFilePatterns []string) ([]Stack, error) {
	files, err := globFiles(root, include)
	if err != nil {
		return nil, err
	}
	stacks := make([]Stack, 0, len(files))
	for _, file := range files {
		stack, err := loadStackFile(file, composeFilePatterns)
		if err != nil {
			return nil, err
		}
		stacks = append(stacks, stack)
	}
	return stacks, nil
}

// globFiles returns the regular files below root whose path relative to root
// matches any of the include globs. Globs follow path.Match syntax with the
// addition that a "**" segment matches zero or more whole directories.
// Results are deduplicated and sorted. Directories named .git are skipped;
// they never contain stack definitions (conservative choice, the briefing
// is silent here).
func globFiles(root string, patterns []string) ([]string, error) {
	seen := make(map[string]bool)
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		for _, pattern := range patterns {
			if matchGlob(pattern, rel) {
				if !seen[rel] {
					seen[rel] = true
					files = append(files, path)
				}
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Strings(files)
	return files, nil
}

// matchGlob reports whether the slash-separated relative path rel matches
// pattern, where a "**" segment matches zero or more path segments and all
// other segments follow path.Match syntax.
func matchGlob(pattern, rel string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(rel, "/"))
}

// matchSegments implements matchGlob on already-split paths.
func matchSegments(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(path); i++ {
			if matchSegments(pattern[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	ok, err := filepath.Match(pattern[0], path[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pattern[1:], path[1:])
}

// loadStackFile parses a single stack definition file, applies the folder
// name fallback and resolves the compose file.
func loadStackFile(file string, composeFilePatterns []string) (Stack, error) {
	// `enabled` defaults to true: the zero value is false, so the default is
	// pre-set here and only overwritten when the YAML sets the key.
	stack := Stack{Enabled: true}
	if err := decodeYAMLFile(file, &stack); err != nil {
		return Stack{}, fmt.Errorf("load stack definition: %w", err)
	}
	stack.File = file
	stack.Dir = filepath.Dir(file)
	stack.Source = "local"
	applyStackDefaults(&stack)
	if stack.Name == "" {
		stack.Name = filepath.Base(stack.Dir)
	}
	if err := validateStack(&stack); err != nil {
		return Stack{}, fmt.Errorf("%s: %w", file, err)
	}
	if err := resolveComposeFile(&stack, composeFilePatterns); err != nil {
		return Stack{}, fmt.Errorf("%s: %w", file, err)
	}
	return stack, nil
}

// loadImportStack loads the base stack referenced by imp from the remote
// checkout — at rem.Checkout when the remote configures one, otherwise at
// <workDir>/<remote>/<path> — and merges the import overrides on top of it.
// The resolution must match where the reconciler clones the remote
// (git.Manager.EnsureRepoAt with the same configured path), otherwise the
// import would be looked for in a checkout dockops never touched.
func loadImportStack(g Global, rem Remote, imp Import) (Stack, error) {
	checkoutRoot := rem.Checkout
	if checkoutRoot == "" {
		checkoutRoot = filepath.Join(g.WorkDir, imp.Remote)
	}
	dir := filepath.Join(checkoutRoot, imp.Path)
	file, err := findImportStackFile(dir)
	if err != nil {
		return Stack{}, err
	}

	// Same enabled-default handling as for local stacks.
	base := Stack{Enabled: true}
	if err := decodeYAMLFile(file, &base); err != nil {
		return Stack{}, fmt.Errorf("load stack definition: %w", err)
	}
	base.File = file
	base.Dir = dir
	base.Source = fmt.Sprintf("import %s/%s", imp.Remote, imp.Path)
	applyStackDefaults(&base)
	if err := validateStack(&base); err != nil {
		return Stack{}, fmt.Errorf("%s: %w", file, err)
	}

	merged := applyImport(base, imp)
	// Name fallback only after the merge, so the import name-override rule
	// (see applyImport) is applied to the explicit names.
	if merged.Name == "" {
		merged.Name = filepath.Base(dir)
	}
	// Re-validate after the merge: services/ignore_services can only
	// become contradictory through the combination of base and import
	// (e.g. import ignore_services hitting base services).
	if err := validateStack(&merged); err != nil {
		return Stack{}, fmt.Errorf("%s: %w", file, err)
	}
	if err := resolveComposeFile(&merged, g.ComposeFilePatterns); err != nil {
		return Stack{}, fmt.Errorf("%s: %w", file, err)
	}
	return merged, nil
}

// findImportStackFile locates the stack definition inside an import
// directory: stack.yaml takes precedence, otherwise exactly one
// *.stack.yaml file must exist. None or several are both configuration
// errors.
func findImportStackFile(dir string) (string, error) {
	primary := filepath.Join(dir, "stack.yaml")
	if regularFileExists(primary) {
		return primary, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.stack.yaml"))
	if err != nil {
		return "", fmt.Errorf("glob stack definitions in %s: %w", dir, err)
	}
	files := make([]string, 0, len(matches))
	for _, m := range matches {
		if regularFileExists(m) {
			files = append(files, m)
		}
	}
	switch len(files) {
	case 1:
		return files[0], nil
	case 0:
		return "", fmt.Errorf("no stack definition found in %s (need stack.yaml or *.stack.yaml)", dir)
	default:
		sort.Strings(files)
		return "", fmt.Errorf("multiple stack definitions found in %s: %v (keep exactly one)", dir, files)
	}
}

// applyStackDefaults fills the stack-level defaults whose zero value is not
// the documented default: an absent update_policy means notify, an absent
// health_timeout means DefaultHealthTimeout. A negative health_timeout stays
// untouched so validateStack reports it (0 is the documented "use the
// default", not "off").
func applyStackDefaults(s *Stack) {
	if s.UpdatePolicy == "" {
		s.UpdatePolicy = UpdatePolicyNotify
	}
	if s.HealthTimeout == 0 {
		s.HealthTimeout = Duration(DefaultHealthTimeout)
	}
}

// validateStack checks required fields and cross-field semantics within a
// parsed stack definition.
func validateStack(s *Stack) error {
	// An empty value stays valid so hand-built stacks (tests, the prune
	// ghost) need not set a policy; EffectiveUpdatePolicy reads it as
	// notify.
	switch s.UpdatePolicy {
	case "", UpdatePolicyNotify, UpdatePolicyPinned, UpdatePolicyApply:
	default:
		return fmt.Errorf("update_policy: invalid value %q (must be notify, pinned or apply)", s.UpdatePolicy)
	}
	// 0 means "use the default" (the loader already filled it), a negative
	// window would make the health gate fail after the first poll without
	// ever waiting — a configuration mistake, not a feature.
	if s.HealthTimeout < 0 {
		return fmt.Errorf("health_timeout: must not be negative (0 uses the default of %s)", DefaultHealthTimeout)
	}
	for i, sf := range s.SopsFiles {
		if sf.Path == "" {
			return fmt.Errorf("sops_files[%d]: path is required", i)
		}
	}
	if overlap := overlapSorted(s.Services, s.IgnoreServices); len(overlap) > 0 {
		return fmt.Errorf("services and ignore_services overlap in %v (a service cannot be both deployed and ignored)", overlap)
	}
	return nil
}

// overlapSorted returns the entries present in both lists, sorted for
// reproducible validation errors.
func overlapSorted(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, v := range b {
		set[v] = struct{}{}
	}
	var out []string
	for _, v := range a {
		if _, ok := set[v]; ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// resolveComposeFile sets the stack's compose file paths: the multi-file
// compose_files list (each entry must exist) when given, otherwise
// stack.ComposeFile either from the explicit compose_file (which must
// exist) or auto-detected as the first existing candidate from
// composeFilePatterns in the stack directory.
//
// Decisions where the briefing is silent (kept conservative):
//   - an explicit compose_file must exist regardless of `enabled`; the same
//     holds for every compose_files entry,
//   - if auto-detection finds nothing, only enabled stacks fail — disabled
//     stacks stay without a compose file since they are never deployed,
//   - relative paths resolve against the stack directory,
//   - compose_files and compose_file are mutually exclusive, and an
//     explicitly empty compose_files list is rejected instead of silently
//     falling back to auto-detection (a clear error beats a surprise).
func resolveComposeFile(stack *Stack, composeFilePatterns []string) error {
	if stack.ComposeFile != "" && len(stack.ComposeFiles) > 0 {
		return fmt.Errorf("compose_file and compose_files are mutually exclusive (set only one)")
	}
	if stack.ComposeFiles != nil && len(stack.ComposeFiles) == 0 {
		return fmt.Errorf("compose_files must list at least one file (unset the key to use compose_file or auto-detection)")
	}
	if len(stack.ComposeFiles) > 0 {
		resolved := make([]string, 0, len(stack.ComposeFiles))
		for i, f := range stack.ComposeFiles {
			p := f
			if !filepath.IsAbs(p) {
				p = filepath.Join(stack.Dir, p)
			}
			if !regularFileExists(p) {
				return fmt.Errorf("compose_files[%d] %q not found (expected at %s)", i, f, p)
			}
			resolved = append(resolved, p)
		}
		stack.ComposeFiles = resolved
		return nil
	}
	if stack.ComposeFile != "" {
		p := stack.ComposeFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(stack.Dir, p)
		}
		if !regularFileExists(p) {
			return fmt.Errorf("compose_file %q not found (expected at %s)", stack.ComposeFile, p)
		}
		stack.ComposeFile = p
		return nil
	}
	for _, pattern := range composeFilePatterns {
		candidate := filepath.Join(stack.Dir, pattern)
		if regularFileExists(candidate) {
			stack.ComposeFile = candidate
			return nil
		}
	}
	if stack.Enabled {
		return fmt.Errorf("no compose file found in %s: tried %v", stack.Dir, composeFilePatterns)
	}
	return nil
}

// regularFileExists reports whether path exists and is a regular file
// (symlinks to regular files included).
func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// checkDuplicateNames rejects stacks whose resolved names collide. Stack
// names are the identity across local and imported stacks, so a collision
// is a validation error naming the colliding name and both sources.
func checkDuplicateNames(stacks []Stack) error {
	sources := make(map[string]string, len(stacks))
	for _, s := range stacks {
		source := fmt.Sprintf("%s (%s)", s.Source, s.File)
		if first, ok := sources[s.Name]; ok {
			return fmt.Errorf("duplicate stack name %q: %s and %s both define it", s.Name, first, source)
		}
		sources[s.Name] = source
	}
	return nil
}

// rootYAML is the decode shape of the root stacks.yaml. It mirrors Root
// field by field, but decodes the notifications block through a pointer and
// with a pointer to `enabled`: the loader must tell a missing block (feature
// off) from an empty one, and an unset `enabled` (default true) from an
// explicit `false`. Decoding straight into Root would lose both distinctions
// because their zero values are the "absent" values.
type rootYAML struct {
	Global        Global             `yaml:"global"`
	Notifications *notificationsYAML `yaml:"notifications"`
	Registries    []Registry         `yaml:"registries"`
	Remotes       map[string]Remote  `yaml:"remotes"`
	Imports       []Import           `yaml:"imports"`
}

// notificationsYAML is the decode shape of the notifications block; see
// rootYAML for why `enabled` is a pointer here.
type notificationsYAML struct {
	Enabled      *bool                     `yaml:"enabled"`
	On           []string                  `yaml:"on"`
	RepeatAfter  Duration                  `yaml:"repeat_after"`
	Timeout      Duration                  `yaml:"timeout"`
	Destinations []NotificationDestination `yaml:"destinations"`
}

// buildNotifications turns the decoded notifications block into its
// validated form: defaults, ${VAR} expansion of every string field against
// the process environment, then validation. file supplies the error context,
// so every failure names the file and the exact field.
//
// Decisions where the briefing is silent (conservative):
//   - an explicitly empty `on: []` falls back to all three events: a block
//     that notifies about nothing is almost certainly a mistake, and
//     "notify about nothing" is spelled `enabled: false`,
//   - a zero repeat_after/timeout means "unset" and takes the default,
//     negative values are rejected,
//   - enabled: false still loads, expands and validates every destination:
//     a broken channel must surface when it is configured, not when the
//     maintenance window ends,
//   - ${VAR} expansion covers every string field of a destination (type,
//     url, token, chat_id, webhook_url, priority, each tag and each header
//     value); header names stay literal because they are map keys.
func buildNotifications(doc notificationsYAML, file string) (*Notifications, error) {
	n := &Notifications{
		Enabled:      true,
		On:           slices.Clone(defaultNotificationEvents),
		RepeatAfter:  Duration(defaultNotificationRepeatAfter),
		Timeout:      Duration(defaultNotificationTimeout),
		Destinations: doc.Destinations,
	}
	if doc.Enabled != nil {
		n.Enabled = *doc.Enabled
	}
	if len(doc.On) > 0 {
		n.On = doc.On
	}
	if doc.RepeatAfter != 0 {
		n.RepeatAfter = doc.RepeatAfter
	}
	if doc.Timeout != 0 {
		n.Timeout = doc.Timeout
	}
	for i := range n.Destinations {
		if err := expandDestination(&n.Destinations[i], file, i); err != nil {
			return nil, err
		}
	}
	if err := validateNotifications(n, file); err != nil {
		return nil, err
	}
	return n, nil
}

// notificationField formats the field path used in error messages, e.g.
// notifications.destinations[0].token.
func notificationField(index int, name string) string {
	if index < 0 {
		return "notifications." + name
	}
	return fmt.Sprintf("notifications.destinations[%d].%s", index, name)
}

// expandDestination expands ${VAR} references in every string field of one
// destination. The expansion happens at load time on purpose: a missing
// secret must fail the pass loudly instead of silently not alarming (the
// fleet provides these values through the container's .env, see README).
func expandDestination(d *NotificationDestination, file string, index int) error {
	scalars := []struct {
		name  string
		value *string
	}{
		{"type", &d.Type},
		{"url", &d.URL},
		{"priority", &d.Priority},
		{"token", &d.Token},
		{"chat_id", &d.ChatID},
		{"webhook_url", &d.WebhookURL},
	}
	for _, f := range scalars {
		expanded, err := expandNotificationValue(notificationField(index, f.name), *f.value)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		*f.value = expanded
	}
	for i := range d.Tags {
		expanded, err := expandNotificationValue(notificationField(index, fmt.Sprintf("tags[%d]", i)), d.Tags[i])
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		d.Tags[i] = expanded
	}
	// Sorted keys keep the error message deterministic when several header
	// values are broken.
	for _, name := range sortedHeaderNames(d.Headers) {
		expanded, err := expandNotificationValue(notificationField(index, "headers."+name), d.Headers[name])
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		d.Headers[name] = expanded
	}
	return nil
}

// expandNotificationValue applies os.Expand semantics with one deliberate
// difference: a reference to an unset or empty variable is an error naming
// the variable, instead of the silent empty string os.Expand produces.
func expandNotificationValue(field, value string) (string, error) {
	var missing string
	expanded := os.Expand(value, func(name string) string {
		v := os.Getenv(name)
		if v == "" && missing == "" {
			missing = name
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("%s: environment variable %s is not set", field, missing)
	}
	return expanded, nil
}

// buildRegistries turns the decoded registries list into its validated form:
// ${VAR} expansion of every string field (same mechanics and error texts as
// the notification destinations), then validation. A missing list is not an
// error — anonymous registry access is the normal case.
func buildRegistries(doc []Registry, file string) ([]Registry, error) {
	if len(doc) == 0 {
		return nil, nil
	}
	out := make([]Registry, len(doc))
	copy(out, doc)
	for i := range out {
		if err := expandRegistry(&out[i], file, i); err != nil {
			return nil, err
		}
	}
	if err := validateRegistries(out, file); err != nil {
		return nil, err
	}
	return out, nil
}

// registryField formats the field path used in error messages, e.g.
// registries[0].password.
func registryField(index int, name string) string {
	return fmt.Sprintf("registries[%d].%s", index, name)
}

// expandRegistry expands ${VAR} references in every string field of one
// registry entry (host, username, password). Failure mode as for the
// notification destinations: a missing secret fails the load naming the field
// and the variable, instead of silently authenticating with an empty
// password.
func expandRegistry(r *Registry, file string, index int) error {
	fields := []struct {
		name  string
		value *string
	}{
		{"host", &r.Host},
		{"username", &r.Username},
		{"password", &r.Password},
	}
	for _, f := range fields {
		expanded, err := expandNotificationValue(registryField(index, f.name), *f.value)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		*f.value = expanded
	}
	return nil
}

// validateRegistries checks the registry entries: a host without scheme or
// path, credentials as a pair, and no duplicate hosts (a silent last-wins
// would hide a broken entry).
func validateRegistries(regs []Registry, file string) error {
	seen := make(map[string]int, len(regs))
	for i, r := range regs {
		host := strings.TrimSpace(r.Host)
		switch {
		case host == "":
			return fmt.Errorf("%s: %s: is required (the registry host as it appears in an image reference)", file, registryField(i, "host"))
		case strings.Contains(host, "://"):
			return fmt.Errorf("%s: %s: must be a bare host without scheme (e.g. gitea.example.com:3000)", file, registryField(i, "host"))
		case strings.ContainsAny(host, "/ \t"):
			return fmt.Errorf("%s: %s: must be a bare host without path (e.g. gitea.example.com:3000)", file, registryField(i, "host"))
		}
		if (r.Username == "") != (r.Password == "") {
			return fmt.Errorf("%s: %s: username and password must be configured together", file, registryField(i, "host"))
		}
		key := strings.ToLower(host)
		if first, ok := seen[key]; ok {
			return fmt.Errorf("%s: %s: duplicate host %q (already configured at registries[%d])", file, registryField(i, "host"), host, first)
		}
		seen[key] = i
	}
	return nil
}

// sortedHeaderNames returns the header names in deterministic order.
func sortedHeaderNames(headers map[string]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateNotifications checks the event filter, the timings and every
// destination's required fields. Unknown types and events are errors: a typo
// must not turn into a channel that never fires.
func validateNotifications(n *Notifications, file string) error {
	for i, kind := range n.On {
		switch kind {
		case NotificationDeployFailed, NotificationPruneFailed, NotificationRepoFailed,
			NotificationUpdateAvailable, NotificationUpdateApplied, NotificationUpdateFailed:
		default:
			return fmt.Errorf("%s: notifications.on[%d]: unknown event %q (must be deploy_failed, prune_failed, repo_failed, update_available, update_applied or update_failed)", file, i, kind)
		}
	}
	if n.RepeatAfter < 0 {
		return fmt.Errorf("%s: notifications.repeat_after: must not be negative", file)
	}
	if n.Timeout <= 0 {
		return fmt.Errorf("%s: notifications.timeout: must be positive (e.g. 10s)", file)
	}
	if len(n.Destinations) == 0 {
		return fmt.Errorf("%s: notifications.destinations: at least one destination is required (remove the notifications block to disable notifications)", file)
	}
	for i, d := range n.Destinations {
		if err := validateDestination(d, file, i); err != nil {
			return err
		}
	}
	return nil
}

// validateDestination checks the type and the fields it requires.
func validateDestination(d NotificationDestination, file string, index int) error {
	require := func(name, value string) error {
		if value != "" {
			return nil
		}
		if d.Type == "" {
			return fmt.Errorf("%s: notifications.destinations[%d].type: is required (webhook, ntfy, telegram, discord or slack)", file, index)
		}
		return fmt.Errorf("%s: %s: is required for type %s", file, notificationField(index, name), d.Type)
	}
	switch d.Type {
	case "webhook", "ntfy":
		return require("url", d.URL)
	case "telegram":
		if err := require("token", d.Token); err != nil {
			return err
		}
		return require("chat_id", d.ChatID)
	case "discord", "slack":
		return require("webhook_url", d.WebhookURL)
	case "":
		return fmt.Errorf("%s: notifications.destinations[%d].type: is required (webhook, ntfy, telegram, discord or slack)", file, index)
	default:
		return fmt.Errorf("%s: notifications.destinations[%d].type: unknown type %q (must be webhook, ntfy, telegram, discord or slack)", file, index, d.Type)
	}
}
