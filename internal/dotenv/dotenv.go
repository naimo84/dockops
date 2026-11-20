// Package dotenv parses and formats the minimal dotenv dialect shared by all
// dockops environment inputs and outputs: the output of
// `sops -d --output-type dotenv` (internal/sops), the env_files of a stack
// and the generated --env-file handed to docker compose (internal/env).
//
// Reading (Parse) is deliberately minimal:
//   - one KEY=VALUE pair per line, split at the first "=",
//   - blank lines and lines starting with "#" are ignored,
//   - lines without "=" are ignored (dotenv inputs are data files, not user
//     configuration, so garbage lines must not abort a deployment),
//   - whitespace around keys and values is not significant,
//   - the value text is kept as written: quotes, ${...} references and
//     inline comments stay in it.
//
// Writing (Format) emits each value text verbatim, and docker compose then
// reads the file with its own dotenv parser, which is NOT verbatim for
// unquoted values: "$" starts an interpolation, " #" an inline comment,
// surrounding whitespace is trimmed and a leading quote makes the value a
// quoted one. That is exactly right for text a user wrote for compose (an
// env_files line), and wrong for a value dockops obtained elsewhere (a Vault
// secret "pa$word" would reach the container as "pa"). Such values go
// through Literal first, which produces the text compose reads back as the
// original string.
package dotenv

import (
	"sort"
	"strings"
)

// Parse extracts all KEY=VALUE pairs from data; see the package comment for
// the dialect. It never fails — input that does not match is ignored.
func Parse(data []byte) map[string]string {
	env := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		// TrimSpace also strips a trailing "\r" from CRLF input.
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		env[key] = strings.TrimSpace(value)
	}
	return env
}

// Format serializes env deterministically: keys sorted, one KEY=VALUE per
// line, trailing newline. The same map therefore always formats to the same
// bytes — a prerequisite for stable file names and byte comparisons.
//
// Values are written as they are, so they must already be dotenv text in
// compose's sense: either text a user wrote for compose, or the result of
// Literal for any other string.
func Format(env map[string]string) []byte {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(env[k])
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// Literal returns the dotenv text docker compose reads back as exactly value.
// A value compose reads verbatim anyway is returned unchanged, so ordinary
// values keep their familiar KEY=value form; every other value is
// double-quoted with the escapes compose's parser undoes (see EscapeQuoted).
func Literal(value string) string {
	if !needsQuoting(value) {
		return value
	}
	return `"` + EscapeQuoted(value) + `"`
}

// EscapeQuoted escapes value for the inside of a double-quoted compose dotenv
// value: backslash, double quote, newline and carriage return become
// backslash escapes, and "$" is doubled so interpolation leaves it alone.
// Every other byte — including "#", single quotes and tabs — is literal
// between double quotes.
func EscapeQuoted(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	// Byte-wise is safe: every escaped character is ASCII, and the bytes of
	// a multi-byte UTF-8 sequence are all >= 0x80.
	for i := 0; i < len(value); i++ {
		switch c := value[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '$':
			b.WriteString(`$$`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// needsQuoting reports whether compose would read the unquoted value
// differently: an interpolation or comment character, any quote or backslash,
// a line break, or whitespace at either end (unquoted values are trimmed).
// The empty value needs no quotes ("KEY=" is the empty string).
func needsQuoting(value string) bool {
	if value == "" {
		return false
	}
	if strings.ContainsAny(value, "$#\"'`\\\n\r") {
		return true
	}
	first, last := value[0], value[len(value)-1]
	return first == ' ' || first == '\t' || last == ' ' || last == '\t'
}
