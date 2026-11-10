// Relative bind-mount sources of deployed compose files (issues #18 and #23).
//
// dockops runs compose from inside a container but the daemon resolves the
// paths it receives in its own namespace, the host. A relative bind source
// (`./config.yaml:/app/config.yaml:ro`) is resolved by compose against the
// directory of the compose file — the *container* stack directory
// /var/lib/dockops/repos/<remote>/<stack>, which the host does not know. The
// daemon then creates an empty directory at that path and bind-mounts it onto
// the file target: no deploy error, just a container that dies later
// ("is a directory", "A storage backend must be specified").
//
// The fix is a rendered override: every relative bind source is rewritten to
// an absolute path, and the override is passed as an extra -f after the
// stack's own compose files. Compose matches an entry with the same target and
// the additional file wins, so the absolute source replaces the relative one.
//
// Absolute sources are deliberately not validated here. Operators who declare
// them know the host namespace; refusing them would break every adopted stack
// whose host path differs from the container path (the documented workaround
// this feature makes unnecessary, not invalid). Only sources dockops itself
// can see as broken — relative ones without a resolvable host counterpart —
// fail, and they fail before compose is called.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// BindRoot is one container→host path mapping of the agent's own container,
// taken from `docker inspect .Mounts`.
type BindRoot struct {
	// Container is the mount destination inside the container (e.g.
	// /var/lib/dockops/repos/host).
	Container string
	// Host is the same path in the daemon's namespace (e.g.
	// /home/docker/apps).
	Host string
}

// translate maps a container path to the host path through the deepest
// matching bind root. ok is false when no root contains the path.
//
// Deepest wins because mounts nest: /var/lib/dockops is the state volume
// while /var/lib/dockops/repos/host is a separate bind of the host checkout.
// A shallow match would translate a path into the wrong namespace.
func translate(p string, roots []BindRoot) (string, bool) {
	clean := filepath.Clean(p)
	var best BindRoot
	found := false
	for _, root := range roots {
		c := filepath.Clean(root.Container)
		if c == "" || c == "." || root.Host == "" || !filepath.IsAbs(c) {
			continue
		}
		if clean != c && !strings.HasPrefix(clean, c+string(filepath.Separator)) {
			continue
		}
		if !found || len(c) > len(filepath.Clean(best.Container)) {
			best, found = root, true
		}
	}
	if !found {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(best.Container), clean)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return filepath.Clean(best.Host), true
	}
	return filepath.Join(best.Host, rel), true
}

// RewriteRelativeBinds renders the override that replaces every relative bind
// source of the stack's compose files with its absolute host path:
//
//	source ./config.yaml, stack dir /var/lib/dockops/repos/infrastructure/x
//	  → container-absolute /var/lib/dockops/repos/infrastructure/x/config.yaml
//	  → host-absolute /home/docker/infrastructure/x/config.yaml
//
// roots are the agent's own mount mappings (see BindRoot); an empty list means
// the agent runs on the host (no container namespace to translate) and the
// container-absolute path is already the daemon's.
//
// Returns the override document and the number of rewritten sources. A zero
// count with a nil error means "nothing to do" — the caller then calls compose
// without an extra file. A relative source that resolves outside every bind
// root is an error: dockops cannot name a host counterpart, and letting it
// through would recreate the silent empty-directory mount.
//
// Compose merge semantics are honoured per file in -f order: a source set in a
// later file replaces the earlier one; a later file that sets only `target`
// keeps the earlier source (compose's own rule, mirrored here so the override
// states the effective value).
func RewriteRelativeBinds(files []string, stackDir string, roots []BindRoot) ([]byte, int, error) {
	effective := map[string]bindEntry{}
	for _, file := range files {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(stackDir, path)
		}
		entries, err := readBindEntries(path)
		if err != nil {
			return nil, 0, err
		}
		for target, entry := range entries {
			if prev, ok := effective[target]; ok {
				entry.mergeOver(prev)
			}
			effective[target] = entry
		}
	}

	targets := make([]string, 0, len(effective))
	for target := range effective {
		targets = append(targets, target)
	}
	sort.Strings(targets)

	doc := bindDocument{Services: map[string][]string{}}
	rewritten := 0
	for _, target := range targets {
		entry := effective[target]
		if entry.source == "" || filepath.IsAbs(entry.source) {
			continue
		}
		if strings.HasPrefix(entry.source, "~") {
			return nil, 0, fmt.Errorf("stack %s: bind source %q (target %s) is relative to a home directory; declare it as an absolute host path", stackDir, entry.source, target)
		}
		absolute := filepath.Join(stackDir, entry.source)
		hostPath, ok := absolute, true
		if len(roots) > 0 {
			hostPath, ok = translate(absolute, roots)
		}
		if !ok {
			return nil, 0, fmt.Errorf("stack %s: bind source %q (target %s) resolves to %s, which is outside every mount of the dockops container; mount the stack checkout at an identical path or declare the source as an absolute host path", stackDir, entry.source, target, absolute)
		}
		doc.Services[entry.service] = append(doc.Services[entry.service],
			strings.Join([]string{hostPath, target, entry.options}, ":"))
		rewritten++
	}
	if rewritten == 0 {
		return nil, 0, nil
	}
	for service, volumes := range doc.Services {
		sort.Strings(volumes)
		doc.Services[service] = volumes
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return nil, 0, fmt.Errorf("render bind override: %w", err)
	}
	return raw, rewritten, nil
}

// bindDocument is the override shape: short-syntax volumes per service, so a
// rewritten entry replaces exactly its own target (compose merges volumes by
// target, and the later file wins).
type bindDocument struct {
	Services map[string][]string `yaml:"services"`
}

// bindEntry is one volume line of a service, in its short syntax split into
// the parts the rewrite needs.
type bindEntry struct {
	service string
	source  string
	target  string
	// options is the remainder of the short syntax (ro, z, propagation, …),
	// empty when the line has none.
	options string
}

// mergeOver applies compose's merge rule for one volume target: a later file
// wins field by field, an unset field keeps the earlier value.
func (e bindEntry) mergeOver(prev bindEntry) bindEntry {
	if e.source == "" {
		e.source = prev.source
	}
	if e.target == "" {
		e.target = prev.target
	}
	if e.options == "" {
		e.options = prev.options
	}
	if e.service == "" {
		e.service = prev.service
	}
	return e
}

// readBindEntries returns the bind entries of one compose file keyed by
// target. Named volumes (no path separator before the target) hold no host
// path and are skipped — they are portable by construction and the whole
// point of them.
func readBindEntries(path string) (map[string]bindEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// Non-strict on purpose: only `services.*.volumes` is of interest, the
	// rest of the document is compose's to parse (same decision as
	// composeServiceNames).
	var doc struct {
		Services map[string]struct {
			Volumes []any `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	entries := map[string]bindEntry{}
	for name, svc := range doc.Services {
		for i, v := range svc.Volumes {
			entry, ok, err := parseVolume(name, v, path, i)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			entries[entry.target] = entry
		}
	}
	return entries, nil
}

// parseVolume normalizes one volumes element (short string syntax or long
// mapping syntax). ok is false for named volumes and anonymous volumes, which
// carry no host-relative source.
func parseVolume(service string, v any, file string, index int) (bindEntry, bool, error) {
	switch value := v.(type) {
	case string:
		parts := strings.Split(value, ":")
		// One part is an anonymous volume ("/data"); two or more is a bind
		// or a named volume. Windows drive letters are not a concern: dockops
		// targets Linux hosts.
		if len(parts) < 2 {
			return bindEntry{}, false, nil
		}
		source, target := parts[0], parts[1]
		if !looksLikePath(source) {
			return bindEntry{}, false, nil
		}
		return bindEntry{
			service: service,
			source:  source,
			target:  target,
			options: strings.Join(parts[2:], ":"),
		}, true, nil
	case map[string]any:
		typ, _ := value["type"].(string)
		source, _ := value["source"].(string)
		target, _ := value["target"].(string)
		if typ != "bind" || source == "" {
			return bindEntry{}, false, nil
		}
		options := []string{}
		if ro, ok := value["read_only"].(bool); ok && ro {
			options = append(options, "ro")
		}
		if bc, ok := value["bind"].(map[string]any); ok {
			if prop, ok := bc["propagation"].(string); ok && prop != "" {
				options = append(options, prop)
			}
		}
		if target == "" {
			return bindEntry{}, false, fmt.Errorf("%s: service %s: volumes[%d]: bind mount without target", file, service, index)
		}
		return bindEntry{
			service: service,
			source:  source,
			target:  target,
			options: strings.Join(options, ":"),
		}, true, nil
	default:
		return bindEntry{}, false, fmt.Errorf("%s: service %s: volumes[%d]: unsupported volume entry %T", file, service, index, v)
	}
}

// looksLikePath reports whether a volume source is a host path (relative or
// absolute) instead of a named volume. Named volumes are bare names
// ("pgdata", "app_cache") without a separator, ".", ".." or "~".
func looksLikePath(source string) bool {
	if source == "." || source == ".." || strings.HasPrefix(source, "~") {
		return true
	}
	if filepath.IsAbs(source) {
		return true
	}
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../")
}

// OwnMounts returns the mount mappings of the container dockops itself runs
// in:
//
//	<binary> inspect <container> --format '{{json .Mounts}}'
//
// name is the agent's own container name (see the DOCKOPS_SELF_* lookup in
// cmd/dockops) or its ID; an empty name is an error — guessing "the container
// I am in" from /proc would give an ID the daemon may not know under that
// spelling. The result feeds RewriteRelativeBinds: it is the only source of
// truth for how a container path maps into the daemon's namespace.
//
// Mounts that are not binds (named volumes, tmpfs) are dropped: a named volume
// has no host path a bind source could point at.
func (e *Executor) OwnMounts(ctx context.Context, name string) ([]BindRoot, error) {
	if name == "" {
		return nil, fmt.Errorf("docker inspect: container name must not be empty")
	}
	out, err := e.output(ctx, []string{"inspect", name, "--format", "{{json .Mounts}}"})
	if err != nil {
		return nil, err
	}
	var mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &mounts); err != nil {
		return nil, fmt.Errorf("docker inspect %s: parse Mounts: %w", name, err)
	}
	roots := make([]BindRoot, 0, len(mounts))
	for _, m := range mounts {
		if m.Type != "bind" || m.Source == "" || m.Destination == "" {
			continue
		}
		roots = append(roots, BindRoot{Container: m.Destination, Host: m.Source})
	}
	return roots, nil
}
