package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// requestTimeout bounds every single HTTP request (manifest HEAD and
	// token GET). A hanging registry must never stall a reconcile pass.
	requestTimeout = 10 * time.Second
	// maxTokenBody bounds the token response body (they are a few hundred
	// bytes; anything bigger is not a token endpoint).
	maxTokenBody = 1 << 20
	// maxManifestBody bounds how much of a HEAD response is drained; the
	// digest comes from a header, the body is discarded.
	maxManifestBody = 4 << 10
	// manifestAccept asks for every manifest type a registry may answer
	// with: OCI image index, Docker manifest list, OCI manifest and
	// Docker manifest v2.
	manifestAccept = "application/vnd.oci.image.index.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, " +
		"application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.v2+json"
)

// Error classes of a registry lookup. Callers distinguish them with
// errors.Is; every error additionally names the URL (or the reference) it
// belongs to.
var (
	// ErrNotFound reports a manifest the registry does not know (missing
	// tag, wrong repository).
	ErrNotFound = errors.New("registry: manifest not found")
	// ErrUnauthorized reports a registry that refused the request, also
	// after the bearer-token exchange.
	ErrUnauthorized = errors.New("registry: unauthorized")
	// ErrRateLimited reports HTTP 429. It is not a reason to fail a
	// reconcile pass — the caller records it and tries again later.
	ErrRateLimited = errors.New("registry: rate limited")
)

// Credentials are the optional login data of one registry host. They are
// only sent to that host's token endpoint (HTTP Basic), never to the
// registry API itself.
type Credentials struct {
	// Host is the registry host as it appears in an image reference,
	// optionally with a port ("gitea.example.com:3000").
	Host string
	// Username and Password authenticate the token request.
	Username string
	Password string
}

// Resolver resolves the remote digest of an image reference. It is the
// interface the reconciler depends on, so tests can count lookups without a
// registry.
type Resolver interface {
	// RemoteDigest returns the digest the reference currently points at
	// ("sha256:…"). A digest-pinned reference returns its pinned digest
	// without a network call.
	RemoteDigest(ctx context.Context, ref string) (string, error)
}

// token is a cached bearer token of one registry host.
type token struct {
	value     string
	expiresAt time.Time // zero means "no announced expiry"
}

// Client resolves image digests over the registry v2 API. Create it once and
// reuse it: the HTTP client and the per-host bearer tokens are kept across
// calls (a pass resolves many images against the same hosts).
type Client struct {
	logger *slog.Logger
	http   *http.Client
	creds  map[string]Credentials
	now    func() time.Time

	// timeout bounds each request; it is a field so tests can shorten it.
	timeout time.Duration

	mu     sync.Mutex
	tokens map[string]token // key: normalized API host
}

// Compile-time assertion that the implementation satisfies the interface the
// reconciler consumes.
var _ Resolver = (*Client)(nil)

// New builds a Client for the given registry credentials (duplicates: the
// last entry wins). A nil logger falls back to slog.Default(). New performs
// no network access.
func New(creds []Credentials, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	c := &Client{
		logger:  logger,
		http:    &http.Client{Timeout: requestTimeout},
		creds:   make(map[string]Credentials, len(creds)),
		now:     time.Now,
		timeout: requestTimeout,
		tokens:  make(map[string]token),
	}
	for _, cred := range creds {
		host := normalizeHost(cred.Host)
		if host == "" {
			continue
		}
		c.creds[host] = cred
	}
	return c
}

// RemoteDigest implements Resolver: it asks the registry for the manifest
// digest of ref's tag, walking the bearer-token challenge when the registry
// demands authentication (HTTP 401 with WWW-Authenticate: Bearer …).
func (c *Client) RemoteDigest(ctx context.Context, ref string) (string, error) {
	parsed, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	if parsed.DigestPinned {
		// Immutable by construction: the tag (if any) cannot move away
		// from the digest, so no registry round-trip is needed.
		return parsed.Digest, nil
	}
	manifestURL := c.manifestURL(parsed.Registry, parsed.Repository, parsed.Tag)

	digest, challenge, err := c.headManifest(ctx, manifestURL, "")
	if err == nil {
		return digest, nil
	}
	if !errors.Is(err, ErrUnauthorized) || challenge == "" {
		return "", err
	}

	tokenKey := tokenCacheKey(parsed.Registry, parsed.Repository)
	bearer, ok := c.cachedToken(tokenKey)
	if !ok {
		bearer, err = c.fetchToken(ctx, parsed.Registry, challenge, parsed.Repository)
		if err != nil {
			return "", err
		}
	}
	digest, _, err = c.headManifest(ctx, manifestURL, bearer)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			// The cached token was rejected (expired, revoked): drop it so
			// the next attempt starts a fresh challenge.
			c.dropToken(tokenKey)
		}
		return "", err
	}
	return digest, nil
}

// manifestURL builds the v2 manifest URL of one tag.
func (c *Client) manifestURL(registryHost, repository, tag string) string {
	return schemeFor(registryHost) + "://" + apiHost(registryHost) +
		"/v2/" + repository + "/manifests/" + url.PathEscape(tag)
}

// headManifest asks for the manifest digest of url. On success it returns the
// Docker-Content-Digest header; on HTTP 401 it returns the parsed challenge
// (empty when the header is missing or not a bearer challenge) together with
// an ErrUnauthorized error.
func (c *Client) headManifest(ctx context.Context, manifestURL, bearer string) (string, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, manifestURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("registry: build request for %s: %w", manifestURL, err)
	}
	req.Header.Set("Accept", manifestAccept)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("registry: %s: %w", manifestURL, err)
	}
	defer resp.Body.Close()
	// HEAD responses have no body, but a misbehaving registry may send one;
	// draining a bounded amount keeps the connection reusable.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxManifestBody))

	switch {
	case resp.StatusCode == http.StatusOK:
		digest := resp.Header.Get("Docker-Content-Digest")
		if digest == "" {
			// Without the header the answer is unusable: computing the
			// digest would mean downloading and re-hashing the manifest.
			return "", "", fmt.Errorf("registry: %s: response has no Docker-Content-Digest header", manifestURL)
		}
		return digest, "", nil
	case resp.StatusCode == http.StatusUnauthorized:
		challenge := resp.Header.Get("WWW-Authenticate")
		return "", challenge, fmt.Errorf("%w: %s: authentication required", ErrUnauthorized, manifestURL)
	case resp.StatusCode == http.StatusForbidden:
		return "", "", fmt.Errorf("%w: %s: access denied", ErrUnauthorized, manifestURL)
	case resp.StatusCode == http.StatusNotFound:
		return "", "", fmt.Errorf("%w: %s", ErrNotFound, manifestURL)
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", "", fmt.Errorf("%w: %s", ErrRateLimited, manifestURL)
	default:
		return "", "", fmt.Errorf("registry: %s: unexpected status %s", manifestURL, resp.Status)
	}
}

// tokenResponse is the JSON body of a token endpoint. Registries disagree on
// the field name: Docker Hub and friends use "token", others "access_token".
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// fetchToken performs the bearer-token exchange of one challenge. Basic
// credentials for the registry host are attached when configured — Docker
// Hub and ghcr.io accept anonymous tokens, private registries do not.
func (c *Client) fetchToken(ctx context.Context, registryHost, challenge, repository string) (string, error) {
	parsed, ok := parseChallenge(challenge)
	if !ok || parsed.Realm == "" {
		return "", fmt.Errorf("%w: %s: unrecognized bearer challenge %q", ErrUnauthorized, registryHost, challenge)
	}
	scope := parsed.Scope
	if scope == "" {
		scope = "repository:" + repository + ":pull"
	}
	tokenURL, err := url.Parse(parsed.Realm)
	if err != nil {
		return "", fmt.Errorf("%w: %s: invalid token realm %q: %w", ErrUnauthorized, registryHost, parsed.Realm, err)
	}
	query := tokenURL.Query()
	if parsed.Service != "" {
		query.Set("service", parsed.Service)
	}
	if scope != "" {
		query.Set("scope", scope)
	}
	tokenURL.RawQuery = query.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("registry: build token request for %s: %w", tokenURL.Redacted(), err)
	}
	req.Header.Set("Accept", "application/json")
	if cred, ok := c.credentials(registryHost); ok {
		req.SetBasicAuth(cred.Username, cred.Password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The query carries the scope, never a secret: the URL is safe to
		// report, credentials travel in the header.
		return "", fmt.Errorf("registry: token %s: %w", tokenURL.Redacted(), err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody))

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("%w: token %s: %s", ErrUnauthorized, tokenURL.Redacted(), resp.Status)
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", fmt.Errorf("%w: token %s", ErrRateLimited, tokenURL.Redacted())
	default:
		return "", fmt.Errorf("registry: token %s: unexpected status %s", tokenURL.Redacted(), resp.Status)
	}

	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("registry: token %s: parse response: %w", tokenURL.Redacted(), err)
	}
	value := tok.Token
	if value == "" {
		value = tok.AccessToken
	}
	if value == "" {
		return "", fmt.Errorf("%w: token %s: response carries no token", ErrUnauthorized, tokenURL.Redacted())
	}
	c.mu.Lock()
	c.tokens[tokenCacheKey(registryHost, repository)] = token{value: value, expiresAt: expiryFor(c.now(), tok.ExpiresIn)}
	c.mu.Unlock()
	return value, nil
}

// expiryFor turns an expires_in value into an expiry timestamp, keeping a
// safety margin so a token is not used in its last seconds.
func expiryFor(now time.Time, expiresIn int) time.Time {
	if expiresIn <= 0 {
		return time.Time{}
	}
	margin := time.Duration(expiresIn) * time.Second / 10
	if margin > 30*time.Second {
		margin = 30 * time.Second
	}
	return now.Add(time.Duration(expiresIn)*time.Second - margin)
}

// challenge is a parsed bearer challenge.
type challenge struct {
	Realm   string
	Service string
	Scope   string
}

// parseChallenge parses `Bearer realm="…",service="…",scope="…"`. The scheme
// is matched case-insensitively; unquoted values are accepted as well
// (some registries omit the quotes).
func parseChallenge(header string) (challenge, bool) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return challenge{}, false
	}
	var ch challenge
	for _, part := range splitChallengeParams(rest) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch key {
		case "realm":
			ch.Realm = value
		case "service":
			ch.Service = value
		case "scope":
			ch.Scope = value
		}
	}
	return ch, true
}

// splitChallengeParams splits the parameter list on commas that are not
// inside a quoted value (a quoted scope may contain commas).
func splitChallengeParams(s string) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			current.WriteRune(r)
		case r == ',' && !inQuotes:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// credentials returns the configured credentials of a registry host,
// including the Docker Hub aliases.
func (c *Client) credentials(registryHost string) (Credentials, bool) {
	for _, host := range hostAliases(registryHost) {
		if cred, ok := c.creds[host]; ok {
			return cred, true
		}
	}
	return Credentials{}, false
}

// tokenCacheKey identifies one bearer token. Registry tokens are scoped per
// repository, so the host alone is not enough: reusing a token issued for
// repository A on repository B answers 401 and every image after the first
// would look unauthorized.
func tokenCacheKey(registryHost, repository string) string {
	return normalizeHost(apiHost(registryHost)) + "|" + repository
}

// cachedToken returns the cached, not yet expired bearer token of a
// registry's API host. Expired entries are dropped on the way out.
func (c *Client) cachedToken(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tok, ok := c.tokens[key]
	if !ok {
		return "", false
	}
	if !tok.expiresAt.IsZero() && !c.now().Before(tok.expiresAt) {
		delete(c.tokens, key)
		return "", false
	}
	return tok.value, true
}

// dropToken forgets the cached token of key.
func (c *Client) dropToken(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, key)
}

// hostAliases returns the spellings under which credentials may be
// configured for registryHost.
func hostAliases(registryHost string) []string {
	host := normalizeHost(registryHost)
	if isDockerHubHost(host) {
		return []string{dockerHubAPIHost, DefaultRegistry, "index.docker.io"}
	}
	return []string{host}
}

// normalizeHost lowercases a host and strips scheme, path and trailing slash
// so configuration and references match.
func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.Trim(host, "/")
	return host
}

// schemeFor picks the URL scheme of a registry: plain HTTP only for loopback
// hosts (tests and a local registry), HTTPS everywhere else.
func schemeFor(registryHost string) string {
	host := normalizeHost(registryHost)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return "http"
	}
	return "https"
}
