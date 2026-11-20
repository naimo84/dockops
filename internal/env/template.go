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
	values, _, err := renderTemplate(ctx, templateContent, vault)
	return values, err
}

// templatePart is one piece of a template line: literal template text, or a
// value read from Vault in place of a reference.
type templatePart struct {
	text   string
	secret bool
}

// renderTemplate renders templateContent line by line and returns two maps
// with the same keys. values holds each line's value with the Vault values
// inserted as they are — what RenderVaultTemplate returns and what the hooks
// see. dotenvText holds the text the generated env files carry for docker
// compose.
//
// The template's own text was written for compose: quotes, ${...} references
// and inline comments mean there what they mean in any compose .env, so a
// line without Vault references reaches the env files unchanged. Inserted
// Vault values, however, must arrive verbatim — a password "pa$word" must
// not be interpolated to "pa". They are escaped according to where they
// stand (see dotenvValue).
func renderTemplate(ctx context.Context, templateContent string, vault secrets.VaultReader) (values, dotenvText map[string]string, err error) {
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

	values = make(map[string]string)
	dotenvText = make(map[string]string)
	for i, line := range strings.Split(templateContent, "\n") {
		// References are resolved on every line, comments included: a
		// commented-out reference to a missing key failing loudly is the
		// conservative direction (see RenderVaultTemplate).
		parts, err := resolveLine(line, read)
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		key, value, ok := splitTemplateLine(parts)
		if !ok {
			continue
		}
		values[key] = joinParts(value)
		dotenvText[key] = dotenvValue(value)
	}
	return values, dotenvText, nil
}

// resolveLine splits line into literal text and the resolved values of its
// Vault references, in order.
func resolveLine(line string, read func(path string) (map[string]string, error)) ([]templatePart, error) {
	matches := vaultRefPattern.FindAllStringIndex(line, -1)
	if len(matches) == 0 {
		return []templatePart{{text: line}}, nil
	}
	parts := make([]templatePart, 0, 2*len(matches)+1)
	last := 0
	for _, m := range matches {
		ref := line[m[0]:m[1]]
		path, key, err := parseVaultRef(ref)
		if err != nil {
			return nil, err
		}
		kv, err := read(path)
		if err != nil {
			return nil, fmt.Errorf("reference %s: %w", ref, err)
		}
		value, ok := kv[key]
		if !ok {
			return nil, fmt.Errorf("reference %s: key %q not found at vault path %q", ref, key, path)
		}
		if m[0] > last {
			parts = append(parts, templatePart{text: line[last:m[0]]})
		}
		parts = append(parts, templatePart{text: value, secret: true})
		last = m[1]
	}
	if last < len(line) {
		parts = append(parts, templatePart{text: line[last:]})
	}
	return parts, nil
}

// splitTemplateLine applies the dotenv line rules (see internal/dotenv) to a
// resolved line: blank lines, comments and lines without "=" carry no key;
// the key ends at the first "=" of the template text — never at one inside a
// Vault value — and surrounding whitespace of key and value is dropped.
// Whitespace that belongs to a Vault value is kept.
func splitTemplateLine(parts []templatePart) (key string, value []templatePart, ok bool) {
	if len(parts) == 0 {
		return "", nil, false
	}
	if first := parts[0]; !first.secret {
		lead := strings.TrimSpace(first.text)
		if strings.HasPrefix(lead, "#") || (lead == "" && len(parts) == 1) {
			return "", nil, false
		}
	}
	var keyText strings.Builder
	for i, p := range parts {
		eq := -1
		if !p.secret {
			eq = strings.IndexByte(p.text, '=')
		}
		if eq < 0 {
			keyText.WriteString(p.text)
			continue
		}
		keyText.WriteString(p.text[:eq])
		key = strings.TrimSpace(keyText.String())
		if key == "" {
			return "", nil, false
		}
		value = append([]templatePart{{text: p.text[eq+1:]}}, parts[i+1:]...)
		return key, trimParts(value), true
	}
	return "", nil, false
}

// trimParts drops the whitespace around a value — of its literal ends only —
// and every literal part that ends up empty.
func trimParts(parts []templatePart) []templatePart {
	if len(parts) > 0 && !parts[0].secret {
		parts[0].text = strings.TrimLeft(parts[0].text, " \t\r")
	}
	if n := len(parts); n > 0 && !parts[n-1].secret {
		parts[n-1].text = strings.TrimRight(parts[n-1].text, " \t\r")
	}
	out := parts[:0]
	for _, p := range parts {
		if p.secret || p.text != "" {
			out = append(out, p)
		}
	}
	return out
}

// joinParts concatenates the parts of a value.
func joinParts(parts []templatePart) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.text)
	}
	return b.String()
}

// dotenvValue renders a template value as dotenv text for compose: the
// template's literal text keeps its compose meaning, every Vault value is
// escaped so compose reads it verbatim. Where the value stands decides how:
//
//   - inside "...": the Vault value is escaped for double quotes in place;
//   - inside '...': single quotes cannot carry every string (a backslash in
//     front of the closing quote, for one), so the value is rewritten as a
//     double-quoted one — the literal text becomes literal double-quoted
//     text, the Vault values are escaped;
//   - unquoted: compose would cut a Vault value at " #", trim it and
//     interpolate its "$", so the value is rewritten as a double-quoted one.
//     The inline comment of the template is dropped first, exactly where
//     compose drops it; the literal text keeps "$" interpolation (unquoted
//     and double-quoted values interpolate alike) and only gets its
//     backslashes and double quotes escaped.
//
// Text behind a closing quote is passed through; compose ignores it. A quote
// that is never closed is no valid compose value — the value is then
// treated as unquoted. A value whose Vault values compose reads verbatim in
// any position (see inertSecret) is passed through as well, so ordinary
// secrets leave the generated files exactly as they always looked.
func dotenvValue(parts []templatePart) string {
	inert := true
	for _, p := range parts {
		if p.secret && !inertSecret(p.text) {
			inert = false
		}
	}
	if inert {
		return joinParts(parts)
	}
	if first := parts[0]; !first.secret && first.text != "" {
		switch first.text[0] {
		case '"':
			if out, ok := quotedValue(parts, '"'); ok {
				return out
			}
		case '\'':
			if out, ok := quotedValue(parts, '\''); ok {
				return out
			}
		}
	}
	return unquotedValue(parts)
}

// inertSecret reports whether compose reads a Vault value verbatim wherever
// it stands in a template value: no interpolation, comment, quote or escape
// character, no line break and no whitespace at its ends (which an unquoted
// value would lose to trimming).
func inertSecret(value string) bool {
	if strings.ContainsAny(value, "$#\"'`\\\n\r") {
		return false
	}
	return value == strings.TrimSpace(value)
}

// quotedValue renders a value that opens with quote; ok is false when the
// quote is never closed.
func quotedValue(parts []templatePart, quote byte) (string, bool) {
	var b strings.Builder
	b.WriteByte('"')
	for i, p := range parts {
		if p.secret {
			b.WriteString(dotenv.EscapeQuoted(p.text))
			continue
		}
		text := p.text
		if i == 0 {
			text = text[1:] // the opening quote
		}
		end := closingQuote(text, quote)
		if end < 0 {
			b.WriteString(quotedLiteral(text, quote))
			continue
		}
		b.WriteString(quotedLiteral(text[:end], quote))
		b.WriteByte('"')
		// Behind the closing quote compose reads nothing; the rest (Vault
		// values included) is carried along unchanged.
		b.WriteString(text[end+1:])
		for _, rest := range parts[i+1:] {
			b.WriteString(rest.text)
		}
		return b.String(), true
	}
	return "", false
}

// quotedLiteral converts literal text from inside quote into the inside of a
// double-quoted value: double-quoted text already is, single-quoted text is
// literal (only \' is an escape) and therefore escaped in full.
func quotedLiteral(text string, quote byte) string {
	if quote == '"' {
		return text
	}
	return dotenv.EscapeQuoted(strings.ReplaceAll(text, `\'`, `'`))
}

// closingQuote returns the index of the first unescaped quote in text, or -1.
// Inside double quotes a backslash escapes any character; inside single
// quotes only the quote itself.
func closingQuote(text string, quote byte) int {
	for i := 0; i < len(text); i++ {
		switch {
		case text[i] == '\\' && i+1 < len(text) && (quote == '"' || text[i+1] == '\''):
			i++
		case text[i] == quote:
			return i
		}
	}
	return -1
}

// unquotedValue renders an unquoted value as a double-quoted one; see
// dotenvValue.
func unquotedValue(parts []templatePart) string {
	cut := make([]templatePart, 0, len(parts))
	for _, p := range parts {
		if !p.secret {
			if j := strings.Index(p.text, " #"); j >= 0 {
				cut = append(cut, templatePart{text: p.text[:j]})
				break
			}
		}
		cut = append(cut, p)
	}
	cut = trimParts(cut)
	var b strings.Builder
	b.WriteByte('"')
	for _, p := range cut {
		if p.secret {
			b.WriteString(dotenv.EscapeQuoted(p.text))
			continue
		}
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p.text))
	}
	b.WriteByte('"')
	return b.String()
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
