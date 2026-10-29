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
// at the decision sites below; phase 6a extended the field list to the
// compose file list (compose_files), the effective service filter and
// ignore_services, and phase 6b added vault_env_templates.
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
//
// Version 2 replaced the single compose_file with the compose file list
// (compose_files), switched Services to the effective service filter result
// (services minus ignore_services) and added the sorted ignore_services
// list — all changes that affect what a deployment runs. Version 3 added
// vault_env_templates (paths, like env_files: template contents are
// deployment artifacts, a changed secret value must not redeploy). A state
// file written against an older version therefore mismatches once and every
// stack redeploys a single time, which is the safe direction.
//
// Version 4 adds the content digest of every vault_env_template (see
// ComputeWithTemplates). Version 3 hashed only the template path, so editing
// a template — a new host in a list, a different KV key, an added
// placeholder — left the hash untouched and the agent never re-rendered the
// stack environment; the change sat in the checkout until some unrelated
// compose edit moved the hash. The digests cover the template *text*, i.e.
// the deployment recipe, not the resolved values: rotating a secret in Vault
// still does not redeploy anything.
const schemaVersion = 4

// canonicalStack is the fixed, JSON-serializable projection of a
// config.Stack that enters the hash. Field order is irrelevant for the hash
// (encoding/json emits struct fields in declaration order, which is stable),
// but kept aligned with the briefing's field list for readability.
type canonicalStack struct {
	SchemaVersion     int                 `json:"schema_version"`
	Name              string              `json:"name"`
	Enabled           bool                `json:"enabled"`
	ComposeFiles      []string            `json:"compose_files"`
	Profiles          []string            `json:"profiles"`
	Services          []string            `json:"services"`
	IgnoreServices    []string            `json:"ignore_services"`
	EnvFiles          []string            `json:"env_files"`
	SopsFiles         []canonicalSopsFile `json:"sops_files"`
	VaultPaths        []string            `json:"vault_paths"`
	VaultEnvTemplates []string            `json:"vault_env_templates"`
	// VaultEnvTemplateContents carries one SHA-256 hex digest per template,
	// in VaultEnvTemplates order. Nil when the caller could not supply the
	// contents (see Compute).
	VaultEnvTemplateContents []string       `json:"vault_env_template_contents"`
	Hooks                    canonicalHooks `json:"hooks"`
	Compose                  []any          `json:"compose"`
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
// canonical representation of stack and the normalized compose contents —
// one content per compose file of the stack, in -f order. A stack with the
// multi-file compose_files configuration hashes over all file contents;
// a single-file stack passes exactly one content.
//
// Normalization decisions where the briefing is cut off (all conservative):
//   - every compose content is parsed as YAML and canonicalized (see
//     canonicalYAML): key order, indentation, comments, anchors and quoting
//     styles do not affect the hash; scalar text values (e.g. "1.0" vs
//     "1.00") and their YAML types do. The list order of the files is kept:
//     compose override semantics ("later wins") make a different order a
//     different deployment.
//   - profiles and services are sorted, per the briefing; ignore_services
//     is sorted for the same set-like reason. The other list fields keep
//     their order because it is semantically relevant (env_files,
//     vault_paths and vault_env_templates override in sequence, hooks run
//     in sequence).
//   - services enter the hash as the effective filter result
//     (stack.EffectiveServices, i.e. services minus ignore_services). With
//     no configured services the deployed set is "everything the compose
//     files define minus ignore_services" — fully determined by the hashed
//     ignore list and compose contents, so the effective deployment is
//     always covered exactly once, never twice.
//   - the compose file paths enter as the effective list stored in the
//     stack; hashes are therefore only comparable within one host (the
//     briefing lists the field explicitly).
//   - nil and empty lists are normalized to the same form so a missing key
//     and an explicit empty list hash identically.
//   - empty compose content hashes as a null document instead of failing;
//     whether a compose file is required is the caller's validation.
//
// The returned error signals unparsable or non-canonicalizable compose
// content (syntax errors, duplicate keys after canonicalization, non-scalar
// mapping keys, alias cycles) or a mismatch between the number of contents
// and the stack's compose files, with stack and file context.
func Compute(stack config.Stack, composeContents ...[]byte) (string, error) {
	return ComputeWithTemplates(stack, nil, composeContents...)
}

// ComputeWithTemplates is Compute plus the raw contents of the stack's
// vault_env_templates, one per entry of stack.VaultEnvTemplates in that
// order. The deploy path passes them, so a template edit redeploys the stack
// and re-renders its environment; Compute (no contents) hashes the template
// paths only and is kept for callers that have no access to the files.
//
// Only the template text enters the hash, not the values resolved from
// Vault: rotating a secret must not recreate containers (see schemaVersion).
// A nil or empty templateContents hashes no digests, so a stack without
// templates hashes identically either way.
func ComputeWithTemplates(stack config.Stack, templateContents [][]byte, composeContents ...[]byte) (string, error) {
	if len(templateContents) != 0 && len(templateContents) != len(stack.VaultEnvTemplates) {
		return "", fmt.Errorf("stack %s: %d template contents for %d vault_env_templates",
			stack.Name, len(templateContents), len(stack.VaultEnvTemplates))
	}
	digests := make([]string, len(templateContents))
	for i, content := range templateContents {
		sum := sha256.Sum256(content)
		digests[i] = hex.EncodeToString(sum[:])
	}
	files := stack.EffectiveComposeFiles()
	if len(files) != len(composeContents) {
		return "", fmt.Errorf("stack %s: %d compose contents for %d compose files", stack.Name, len(composeContents), len(files))
	}
	compose := make([]any, len(composeContents))
	for i, content := range composeContents {
		doc, err := canonicalYAML(content)
		if err != nil {
			return "", fmt.Errorf("stack %s: normalize compose file %s: %w", stack.Name, files[i], err)
		}
		compose[i] = doc
	}

	canon := canonicalStack{
		SchemaVersion:            schemaVersion,
		Name:                     stack.Name,
		Enabled:                  stack.Enabled,
		ComposeFiles:             nilIfEmpty(files),
		Profiles:                 sorted(stack.Profiles),
		Services:                 sorted(stack.EffectiveServices()),
		IgnoreServices:           sorted(stack.IgnoreServices),
		EnvFiles:                 nilIfEmpty(stack.EnvFiles),
		SopsFiles:                canonicalSopsFiles(stack.SopsFiles),
		VaultPaths:               nilIfEmpty(stack.VaultPaths),
		VaultEnvTemplates:        nilIfEmpty(stack.VaultEnvTemplates),
		VaultEnvTemplateContents: nilIfEmpty(digests),
		Hooks: canonicalHooks{
			PreDeploy:  nilIfEmpty(stack.Hooks.PreDeploy),
			PostDeploy: nilIfEmpty(stack.Hooks.PostDeploy),
		},
		Compose: nilIfEmptyAny(compose),
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

// nilIfEmptyAny is nilIfEmpty for the canonical compose document list (all
// contents empty canonicalize to nil, an absent list stays nil).
func nilIfEmptyAny(list []any) []any {
	if len(list) == 0 {
		return nil
	}
	return list
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
