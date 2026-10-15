package env

// Vault template rendering lives in this file: dotenv files whose values
// carry ${VAULT:<path>#<key>} references (the syntax of the vault-env files
// already present in the fleet), resolved against the same KV v2 mount as
// vault_paths. Kept separate so env.go stays the merge/write layer.

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/local/dockops/internal/dotenv"
	"github.com/local/dockops/internal/secrets"
)

// vaultRefPattern matches one template reference, ${VAULT:<path>#<key>} as
// well as the alternative ${VAULT:<path>:<key>} spelling. The body between
// the braces may be anything except a closing brace; the separator split and
// the emptiness checks happen in parseVaultRef, which produces the precise
// error messages.
var vaultRefPattern = regexp.MustCompile(`\$\{VAULT:[^}]+\}`)

// RenderVaultTemplate renders templateContent line by line: every
// ${VAULT:<path>#<key>} occurrence is replaced with the value of key in the
// KV v2 secret at path (read via vault), everything else stays literal. The
// rendered text is parsed with the shared dotenv dialect and returned as a
// KEY=VALUE map — the KEY names therefore live in the template itself (line
// format `MY_VAR=${VAULT:hosts/host-01/traefik#db_password}`).
//
// Reference syntax (both separators accepted, "#" wins when both appear):
//
//	${VAULT:<path>#<key>}   preferred
//	${VAULT:<path>:<key>}   alternative — split at the FIRST colon after the
//	                        path, so keys (but not paths) may contain colons
//
// path is the KV path below the configured mount, exactly like a vault_paths
// entry; key is the field name in the secret's data map. References on lines
// the dotenv parser later ignores (comments, lines without "=") are resolved
// too — the briefing says every occurrence is replaced, and a commented-out
// reference to a missing key failing loudly is the conservative direction.
//
// Each distinct path is read at most once per template call (a small
// per-call cache; KV reads are cheap, the cache merely spares duplicate
// reads of the same path within one file). Client-side caching beyond one
// template is deliberately left to the Vault client — see internal/vault.
//
// Errors name the line number and the full reference (missing path or key,
// read failure); the caller adds the template file context, since this
// function only sees content. Resolved values never enter error messages.
func RenderVaultTemplate(ctx context.Context, templateContent string, vault secrets.VaultReader) (map[string]string, error) {
	cache := make(map[string]map[string]string)
	read := func(path string) (map[string]string, error) {
		if kv, ok := cache[path]; ok {
			return kv, nil
		}
		kv, err := vault.ReadKV(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		cache[path] = kv
		return kv, nil
	}

	lines := strings.Split(templateContent, "\n")
	for i, line := range lines {
		rendered, err := renderLine(line, read)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		lines[i] = rendered
	}
	return dotenv.Parse([]byte(strings.Join(lines, "\n"))), nil
}

// renderLine replaces every vault reference in line with its resolved value,
// preserving all literal text around the references.
func renderLine(line string, read func(path string) (map[string]string, error)) (string, error) {
	matches := vaultRefPattern.FindAllStringIndex(line, -1)
	if len(matches) == 0 {
		return line, nil
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		ref := line[m[0]:m[1]]
		path, key, err := parseVaultRef(ref)
		if err != nil {
			return "", err
		}
		kv, err := read(path)
		if err != nil {
			return "", fmt.Errorf("reference %s: %w", ref, err)
		}
		value, ok := kv[key]
		if !ok {
			return "", fmt.Errorf("reference %s: key %q not found at vault path %q", ref, key, path)
		}
		b.WriteString(line[last:m[0]])
		b.WriteString(value)
		last = m[1]
	}
	b.WriteString(line[last:])
	return b.String(), nil
}

// parseVaultRef splits a full reference token like ${VAULT:secret/x#pw} into
// its vault path and key. "#" separates first; without a "#", the FIRST ":"
// after the path separates instead (see RenderVaultTemplate for the
// rationale). Empty path or key is a configuration error, not a lookup.
func parseVaultRef(ref string) (path, key string, err error) {
	body := strings.TrimSuffix(strings.TrimPrefix(ref, "${VAULT:"), "}")
	if sep := strings.Index(body, "#"); sep >= 0 {
		path, key = body[:sep], body[sep+1:]
	} else if sep := strings.Index(body, ":"); sep >= 0 {
		path, key = body[:sep], body[sep+1:]
	} else {
		return "", "", fmt.Errorf("reference %s: no separator (want ${VAULT:<path>#<key>} or ${VAULT:<path>:<key>})", ref)
	}
	if path == "" || key == "" {
		return "", "", fmt.Errorf("reference %s: empty path or key", ref)
	}
	return path, key, nil
}

// resolverVaultReader adapts a secrets.Resolver to the secrets.VaultReader
// consumed by RenderVaultTemplate. The merge (MergeStackEnv) already holds a
// Resolver; the adapter exposes its vault half without the implementations
// having to implement a second interface.
type resolverVaultReader struct {
	resolver secrets.Resolver
}

// ReadKV implements secrets.VaultReader via the wrapped resolver.
func (a resolverVaultReader) ReadKV(ctx context.Context, path string) (map[string]string, error) {
	return a.resolver.ResolveVault(ctx, path)
}

// Compile-time assertion that the adapter satisfies the reader interface
// RenderVaultTemplate consumes.
var _ secrets.VaultReader = resolverVaultReader{}
