// Package stackhash computes a deterministic hash over a stack definition and
// its normalized compose content. The hash identifies the deployed state of a
// stack: two configurations that deploy the same thing must produce the same
// hash regardless of YAML formatting, key order or comments, and any change
// that affects the deployment must change the hash.
//
// The visible part of the phase-2 briefing specifies the covered fields
// (name, enabled, compose_file, sorted profiles and services, env_files,
// sops_files with path and age_keyfile, vault_paths, hooks) plus a
// "normalized" compose content, but is cut off mid-sentence. The remaining
// decisions follow the project convention for open points and are documented
// at the decision sites below.
package stackhash

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/local/dockops/internal/config"
)

// schemaVersion prefixes the canonical form. Any change to the canonical
// representation must bump it so persisted hashes (state store) are not
// silently compared against a different scheme.
const schemaVersion = 1

// canonicalStack is the fixed, JSON-serializable projection of a
// config.Stack that enters the hash. Field order is irrelevant for the hash
// (encoding/json emits struct fields in declaration order, which is stable),
// but kept aligned with the briefing's field list for readability.
type canonicalStack struct {
	SchemaVersion int                 `json:"schema_version"`
	Name          string              `json:"name"`
	Enabled       bool                `json:"enabled"`
	ComposeFile   string              `json:"compose_file"`
	Profiles      []string            `json:"profiles"`
	Services      []string            `json:"services"`
	EnvFiles      []string            `json:"env_files"`
	SopsFiles     []canonicalSopsFile `json:"sops_files"`
	VaultPaths    []string            `json:"vault_paths"`
	Hooks         canonicalHooks      `json:"hooks"`
	Compose       any                 `json:"compose"`
}

// canonicalSopsFile is the hash-relevant projection of config.SopsFile.
type canonicalSopsFile struct {
	Path       string `json:"path"`
	AgeKeyfile string `json:"age_keyfile"`
}

// canonicalHooks is the hash-relevant projection of config.Hooks.
type canonicalHooks struct {
	PreDeploy  []string `json:"pre_deploy"`
	PostDeploy []string `json:"post_deploy"`
}

// Compute returns the deterministic SHA-256 hash (hex encoded) over the
// canonical representation of stack and the normalized composeContent.
//
// Normalization decisions where the briefing is cut off (all conservative):
//   - composeContent is parsed as YAML and canonicalized (see
//     canonicalYAML): key order, indentation, comments, anchors and quoting
//     styles do not affect the hash; scalar text values (e.g. "1.0" vs
//     "1.00") and their YAML types do.
//   - profiles and services are sorted, per the briefing; the other list
//     fields keep their order because it is semantically relevant
//     (env_files and vault_paths override in sequence, hooks run in
//     sequence).
//   - compose_file enters as the resolved path stored in the stack; hashes
//     are therefore only comparable within one host (the briefing lists the
//     field explicitly).
//   - nil and empty lists are normalized to the same form so a missing key
//     and an explicit empty list hash identically.
//   - empty compose content hashes as a null document instead of failing;
//     whether a compose file is required is the caller's validation.
//
// The returned error signals unparsable or non-canonicalizable compose
// content (syntax errors, duplicate keys after canonicalization, non-scalar
// mapping keys, alias cycles), with stack and file context.
func Compute(stack config.Stack, composeContent []byte) (string, error) {
	compose, err := canonicalYAML(composeContent)
	if err != nil {
		return "", fmt.Errorf("stack %s: normalize compose file %s: %w", stack.Name, stack.ComposeFile, err)
	}

	canon := canonicalStack{
		SchemaVersion: schemaVersion,
		Name:          stack.Name,
		Enabled:       stack.Enabled,
		ComposeFile:   stack.ComposeFile,
		Profiles:      sorted(stack.Profiles),
		Services:      sorted(stack.Services),
		EnvFiles:      nilIfEmpty(stack.EnvFiles),
		SopsFiles:     canonicalSopsFiles(stack.SopsFiles),
		VaultPaths:    nilIfEmpty(stack.VaultPaths),
		Hooks: canonicalHooks{
			PreDeploy:  nilIfEmpty(stack.Hooks.PreDeploy),
			PostDeploy: nilIfEmpty(stack.Hooks.PostDeploy),
		},
		Compose: compose,
	}

	// encoding/json is deterministic: struct fields in declaration order,
	// map keys sorted, and json.Number values verbatim.
	raw, err := json.Marshal(canon)
	if err != nil {
		return "", fmt.Errorf("stack %s: serialize canonical form: %w", stack.Name, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// sorted returns a sorted copy of list; nil and empty both normalize to nil
// so both absence and explicit emptiness hash the same.
func sorted(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, len(list))
	copy(out, list)
	sort.Strings(out)
	return out
}

// nilIfEmpty normalizes empty slices to nil (see sorted).
func nilIfEmpty(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	return list
}

// canonicalSopsFiles projects the sops files in their given order: the
// sequence of decryption steps is semantically relevant, so unlike
// profiles/services they are not sorted.
func canonicalSopsFiles(files []config.SopsFile) []canonicalSopsFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]canonicalSopsFile, 0, len(files))
	for _, f := range files {
		out = append(out, canonicalSopsFile{Path: f.Path, AgeKeyfile: f.AgeKeyfile})
	}
	return out
}

// canonicalYAML parses content as YAML and returns its canonical,
// JSON-marshalable form (nested map[string]any / []any / scalars). Anchors
// and aliases are resolved, so formatting, comments and node sharing do not
// affect the result. Empty content canonicalizes to nil.
func canonicalYAML(content []byte) (any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, err
	}
	// An empty document decodes to a zero node; whitespace-only content is
	// the same case. There is no separate stream handling: compose files are
	// single-document by definition.
	if doc.Kind == 0 {
		return nil, nil
	}
	return canonicalNode(&doc, nil)
}

// canonicalNode converts a YAML node subtree into its canonical form.
// inProgress holds the alias targets currently being expanded and turns
// cyclic alias graphs into an error instead of a hang.
func canonicalNode(n *yaml.Node, inProgress map[*yaml.Node]bool) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, fmt.Errorf("line %d: expected exactly one document content node, got %d", n.Line, len(n.Content))
		}
		return canonicalNode(n.Content[0], inProgress)

	case yaml.AliasNode:
		target := n.Alias
		if target == nil {
			return nil, fmt.Errorf("line %d: alias without target", n.Line)
		}
		if inProgress[target] {
			return nil, fmt.Errorf("line %d: cyclic alias %q", n.Line, n.Value)
		}
		if inProgress == nil {
			inProgress = make(map[*yaml.Node]bool, 1)
		}
		inProgress[target] = true
		v, err := canonicalNode(target, inProgress)
		delete(inProgress, target)
		return v, err

	case yaml.ScalarNode:
		return canonicalScalar(n), nil

	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, item := range n.Content {
			v, err := canonicalNode(item, inProgress)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil

	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: non-scalar mapping keys are not supported", key.Line)
			}
			if _, dup := out[key.Value]; dup {
				return nil, fmt.Errorf("line %d: duplicate mapping key %q", key.Line, key.Value)
			}
			v, err := canonicalNode(n.Content[i+1], inProgress)
			if err != nil {
				return nil, err
			}
			out[key.Value] = v
		}
		return out, nil

	default:
		return nil, fmt.Errorf("line %d: unsupported YAML node kind %d", n.Line, n.Kind)
	}
}

// canonicalScalar maps a scalar YAML node to a JSON-marshalable value that
// preserves its type: booleans as bool, integers and floats as json.Number
// (verbatim text, so "0x10" and "16" stay distinct — conservative, no
// semantic number folding), null as nil, and everything else (strings,
// timestamps, binary) as its text value. Two scalars that differ only in
// resolved type (e.g. !!str "5" vs !!int 5) hash differently, which is
// intentional.
func canonicalScalar(n *yaml.Node) any {
	switch n.Tag {
	case "!!null":
		return nil
	case "!!bool":
		// yaml.v3 only tags true/false spellings as bool.
		return n.Value == "true" || n.Value == "True" || n.Value == "TRUE"
	case "!!int", "!!float":
		return json.Number(n.Value)
	default:
		return n.Value
	}
}
