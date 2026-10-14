// Package dotenv parses and formats the minimal dotenv dialect shared by all
// dockops environment inputs and outputs: the output of
// `sops -d --output-type dotenv` (internal/sops), the env_files of a stack
// and the generated --env-file handed to docker compose (internal/env).
//
// Dialect — deliberately minimal and matched to what `docker compose
// --env-file` itself accepts (the briefing leaves the exact semantics open;
// mirroring the consumer avoids surprising differences):
//   - one KEY=VALUE pair per line, split at the first "=",
//   - blank lines and lines starting with "#" are ignored,
//   - lines without "=" are ignored (dotenv inputs are data files, not user
//     configuration, so garbage lines must not abort a deployment),
//   - whitespace around keys and values is not significant,
//   - no quoting, escaping or shell expansion: values are taken verbatim,
//     which also means values cannot contain line breaks.
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
