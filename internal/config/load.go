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

// Smart defaults applied when the corresponding field is unset in stacks.yaml.
const (
	defaultPollInterval     = 5 * time.Minute
	defaultVaultMountPath   = "secret"
	defaultVaultAuthMethod  = "token"
	defaultVaultTokenEnv    = "VAULT_TOKEN"
	defaultVaultRoleIDEnv   = "VAULT_ROLE_ID"
	defaultVaultSecretIDEnv = "VAULT_SECRET_ID"
	defaultRemoteBranch     = "main"
)

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
	cfg := &Config{Global: root.Global, Remotes: root.Remotes, Imports: root.Imports}

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
		stack, err := loadImportStack(cfg.Global, imp)
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
	var root Root
	if err := decodeYAMLFile(path, &root); err != nil {
		return nil, fmt.Errorf("load root config: %w", err)
	}
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
	// "smartDefaultOrEnv(VAULT_ADDR)": explicit value wins, env as fallback.
	if g.Vault.Address == "" {
		g.Vault.Address = os.Getenv("VAULT_ADDR")
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
	for _, name := range sortedRemoteNames(root.Remotes) {
		if root.Remotes[name].URL == "" {
			return fmt.Errorf("%s: remotes[%s]: url is required", file, name)
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
// checkout at <workDir>/<remote>/<path> and merges the import overrides on
// top of it.
func loadImportStack(g Global, imp Import) (Stack, error) {
	dir := filepath.Join(g.WorkDir, imp.Remote, imp.Path)
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
	if err := validateStack(&base); err != nil {
		return Stack{}, fmt.Errorf("%s: %w", file, err)
	}

	merged := applyImport(base, imp)
	// Name fallback only after the merge, so the import name-override rule
	// (see applyImport) is applied to the explicit names.
	if merged.Name == "" {
		merged.Name = filepath.Base(dir)
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

// validateStack checks required fields within a parsed stack definition.
func validateStack(s *Stack) error {
	for i, sf := range s.SopsFiles {
		if sf.Path == "" {
			return fmt.Errorf("sops_files[%d]: path is required", i)
		}
	}
	return nil
}

// resolveComposeFile sets stack.ComposeFile either from the explicit
// compose_file (which must exist) or by auto-detecting the first existing
// candidate from composeFilePatterns in the stack directory.
//
// Decisions where the briefing is silent (kept conservative):
//   - an explicit compose_file must exist regardless of `enabled`,
//   - if auto-detection finds nothing, only enabled stacks fail — disabled
//     stacks stay without a compose file since they are never deployed,
//   - relative compose_file paths resolve against the stack directory.
func resolveComposeFile(stack *Stack, composeFilePatterns []string) error {
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
