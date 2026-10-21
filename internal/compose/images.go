// Service→image resolution and local image digests — the read-only docker
// side of the phase-10 image update check. Both helpers only inspect;
// neither pulls, retags nor recreates anything.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/local/dockops/internal/config"
)

// Request names one stack for a read-only compose query: the project name
// (the stack name, as for up/down), the stack directory relative compose
// file paths resolve against, and the compose files in -f order.
type Request struct {
	// Project is the compose project name (stack.Name).
	Project string
	// StackDir is the directory relative compose file paths resolve
	// against.
	StackDir string
	// ComposeFiles are the stack's compose files in -f order (relative
	// entries resolve against StackDir).
	ComposeFiles []string
}

// RequestFor builds the read-only Request of a stack, mirroring the file
// handling of Up and Down.
func RequestFor(stack config.Stack, stackDir string) Request {
	return Request{
		Project:      stack.Name,
		StackDir:     stackDir,
		ComposeFiles: stack.EffectiveComposeFiles(),
	}
}

// fileArgs returns the -f arguments of the request in file order.
func (r Request) fileArgs() []string {
	return resolveComposeFileArgs(r.ComposeFiles, r.StackDir)
}

// Images returns the service→image map of the stack as docker compose
// resolves it:
//
//	<binary> compose -p <project> [-f <file> ...] config --format json
//
// The resolved configuration is authoritative for interpolation, overrides
// and merges — parsing the YAML itself would answer a different question.
// Services without an image (build-only services) are omitted, because there
// is nothing to compare against a registry; an empty map is a valid result.
func (e *Executor) Images(ctx context.Context, req Request) (map[string]string, error) {
	if req.Project == "" {
		return nil, fmt.Errorf("docker compose config: project name must not be empty")
	}
	args := []string{"compose", "-p", req.Project}
	args = append(args, req.fileArgs()...)
	args = append(args, "config", "--format", "json")

	out, err := e.query(ctx, args)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("docker compose config: parse json: %w%s", err, outputSuffix(out))
	}
	images := make(map[string]string, len(doc.Services))
	for name, svc := range doc.Services {
		if svc.Image == "" {
			continue
		}
		images[name] = svc.Image
	}
	return images, nil
}

// ImageDigest returns the repository digest docker recorded for ref:
//
//	<binary> image inspect --format '{{json .RepoDigests}}' <ref>
//
// The matching entry (same repository before the @) wins; the entry of another
// repository in the list is ignored. An empty result with a nil error means
// the image carries no RepoDigest at all — it was built locally and never
// pulled, so it has no registry identity to compare against.
func (e *Executor) ImageDigest(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("docker image inspect: image reference must not be empty")
	}
	out, err := e.query(ctx, []string{"image", "inspect", "--format", "{{json .RepoDigests}}", ref})
	if err != nil {
		return "", err
	}
	var repoDigests []string
	if err := json.Unmarshal(bytes.TrimSpace(out), &repoDigests); err != nil {
		return "", fmt.Errorf("docker image inspect %s: parse RepoDigests: %w", ref, err)
	}
	return matchRepoDigest(ref, repoDigests), nil
}

// matchRepoDigest returns the digest of the RepoDigests entry whose
// repository matches ref's repository, or "" when there is none.
func matchRepoDigest(ref string, repoDigests []string) string {
	want := normalizeRepository(repositoryOf(ref))
	for _, entry := range repoDigests {
		name, digest, ok := strings.Cut(entry, "@")
		if !ok || digest == "" {
			continue
		}
		if normalizeRepository(name) == want {
			return digest
		}
	}
	return ""
}

// repositoryOf strips the digest and tag from an image reference, leaving
// the repository ("ghcr.io/x/app:1.0" → "ghcr.io/x/app").
func repositoryOf(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	return name
}

// normalizeRepository lowercases a repository and drops Docker Hub's
// prefixes, so "nginx", "library/nginx" and "docker.io/library/nginx" — the
// spellings docker inspect and the compose file use for the same image —
// compare equal.
func normalizeRepository(repo string) string {
	r := strings.ToLower(repo)
	for _, prefix := range []string{"index.docker.io/", "registry-1.docker.io/", "docker.io/"} {
		r = strings.TrimPrefix(r, prefix)
	}
	return strings.TrimPrefix(r, "library/")
}

// query runs a read-only docker command and returns its stdout. Unlike run it
// keeps stdout and stderr apart — callers parse stdout as JSON — and embeds
// both, bounded, into the error message when the command fails.
func (e *Executor) query(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, e.binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	command := strings.Join(args, " ")
	if err != nil {
		out := append(stderr.Bytes(), stdout.Bytes()...)
		e.logger.Error("docker command failed", "command", command, "error", err.Error(), "output", snippet(out))
		return nil, fmt.Errorf("docker %s: %w%s", command, err, outputSuffix(out))
	}
	e.logger.Debug("docker command finished", "command", command)
	return stdout.Bytes(), nil
}
