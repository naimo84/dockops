// Package registry resolves the current remote digest of container images
// through the Docker Registry HTTP API v2, so the reconciler can tell whether
// a deployed image is still the one its tag points at.
//
// Only the small subset dockops needs is implemented: parse an image
// reference, ask the registry for the manifest digest of its tag (HEAD, no
// manifest download), and — for private registries — walk the bearer-token
// challenge once per host. Nothing here deploys, pulls or writes; the package
// is stdlib-only (net/http, encoding/json, regexp, strings)
// and every failure is returned as an error, never a panic.
//
// Decisions where the phase-10 briefing is silent (conservative, documented
// at their sites):
//   - a digest-pinned reference (@sha256:…) is immutable by definition, so
//     its remote digest is the pinned one — no network call,
//   - plain HTTP is allowed only for loopback registries (localhost,
//     127.0.0.1, ::1): the schema has no "insecure" flag, and silently
//     downgrading a remote registry to HTTP would leak its token,
//   - tokens are cached in memory per host until their announced expiry (or
//     until a registry rejects them).
package registry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Registry defaults of an image reference (Docker's own normalization
// rules).
const (
	// DefaultRegistry is the registry of a reference without a host part.
	DefaultRegistry = "docker.io"
	// DefaultTag is used when a reference names neither tag nor digest.
	DefaultTag = "latest"
	// dockerHubAPIHost is the host that answers the v2 API for Docker Hub;
	// "docker.io" itself only redirects. The parsed Ref keeps the
	// human-readable DefaultRegistry.
	dockerHubAPIHost = "index.docker.io"
)

var (
	// digestPattern validates an @-pinned digest: a supported algorithm
	// followed by a hex encoded value.
	digestPattern = regexp.MustCompile(`^(sha256|sha512):[a-f0-9]{32,}$`)
	// tagPattern validates an explicit tag (the distribution spec allows
	// [A-Za-z0-9_][A-Za-z0-9._-]{0,127}).
	tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// Ref is a parsed image reference.
type Ref struct {
	// Registry is the registry host, optionally with a port
	// ("ghcr.io", "gitea.example.com:3000"); DefaultRegistry when the
	// reference carries no host.
	Registry string
	// Repository is the path below /v2/, normalized for Docker Hub
	// ("library/nginx" for "nginx", "grafana/grafana" for
	// "grafana/grafana").
	Repository string
	// Tag is the tag to resolve; empty for a digest-pinned reference.
	Tag string
	// DigestPinned reports a reference of the form repo@sha256:…, which
	// needs no registry lookup.
	DigestPinned bool
	// Digest is the pinned digest, empty unless DigestPinned is set.
	Digest string
}

// String renders the reference in canonical form (registry/repository:tag or
// registry/repository@digest).
func (r Ref) String() string {
	if r.DigestPinned {
		return r.Registry + "/" + r.Repository + "@" + r.Digest
	}
	if r.Tag == "" {
		return r.Registry + "/" + r.Repository
	}
	return r.Registry + "/" + r.Repository + ":" + r.Tag
}

// ParseRef parses an image reference into registry, repository and tag,
// applying Docker's defaults: no host part means DefaultRegistry, a
// single-component Docker Hub name gets the "library/" prefix, and a
// missing tag means DefaultTag. A reference pinned with @<digest> yields
// DigestPinned and no tag.
func ParseRef(ref string) (Ref, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Ref{}, errors.New("registry: empty image reference")
	}
	if strings.ContainsAny(ref, " \t\r\n") {
		return Ref{}, fmt.Errorf("registry: image reference %q must not contain whitespace", ref)
	}

	out := Ref{}
	name := ref
	if i := strings.Index(ref, "@"); i >= 0 {
		name = ref[:i]
		digest := ref[i+1:]
		if !digestPattern.MatchString(digest) {
			return Ref{}, fmt.Errorf("registry: image reference %q: invalid digest %q (want sha256:<hex>)", ref, digest)
		}
		out.Digest = digest
		out.DigestPinned = true
	}

	// A leading component is a registry host when it looks like one: it
	// carries a dot (domain), a colon (port) or is "localhost". Everything
	// else is a Docker Hub namespace.
	if first, rest, ok := strings.Cut(name, "/"); ok && isRegistryHost(first) {
		out.Registry = first
		name = rest
	} else {
		out.Registry = DefaultRegistry
	}

	// Tag separator: the last colon after the last slash (a colon before
	// it belongs to a host:port pair, which is already stripped). A tag in
	// front of a digest pin is allowed by docker but irrelevant — the
	// digest wins.
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		tag := name[i+1:]
		if !out.DigestPinned {
			if !tagPattern.MatchString(tag) {
				return Ref{}, fmt.Errorf("registry: image reference %q: invalid tag %q", ref, tag)
			}
			out.Tag = tag
		}
		name = name[:i]
	}

	if name == "" {
		return Ref{}, fmt.Errorf("registry: image reference %q: repository is empty", ref)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" {
			return Ref{}, fmt.Errorf("registry: image reference %q: repository %q contains an empty path segment", ref, name)
		}
	}
	if out.Registry == DefaultRegistry && !strings.Contains(name, "/") {
		name = "library/" + name
	}
	if out.Tag == "" && !out.DigestPinned {
		out.Tag = DefaultTag
	}
	out.Repository = name
	return out, nil
}

// isRegistryHost reports whether the first path component of a reference is
// a registry host rather than a Docker Hub namespace.
func isRegistryHost(component string) bool {
	if component == "localhost" {
		return true
	}
	return strings.ContainsAny(component, ".:")
}

// apiHost is the host that serves the v2 API for a registry: Docker Hub's
// "docker.io" is a user-facing alias only.
func apiHost(registryHost string) string {
	switch registryHost {
	case DefaultRegistry, dockerHubAPIHost, "registry-1.docker.io":
		return dockerHubAPIHost
	}
	return registryHost
}
