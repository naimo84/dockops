// Package vault implements a minimal HashiCorp Vault client for the KV v2
// secrets engine, built directly on net/http. Deliberately no Vault Go
// library: the two operations dockops needs (token/AppRole login and a KV v2
// read) are plain HTTP calls, and net/http keeps the binary slim and the
// client testable with httptest.Server.
package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/local/dockops/internal/config"
)

// Smart defaults mirroring the config loader, so a Client built from a
// hand-written config.Vault behaves identically to one whose values went
// through config.Load (New also serves callers that bypass the loader).
const (
	defaultMountPath   = "secret"
	defaultAuthMethod  = "token"
	defaultTokenEnv    = "VAULT_TOKEN"
	defaultRoleIDEnv   = "VAULT_ROLE_ID"
	defaultSecretIDEnv = "VAULT_SECRET_ID"
)

// maxErrorBody bounds how much of an error response body is embedded in an
// error message (the briefing prescribes 200 characters).
const maxErrorBody = 200

// Client talks to a single Vault server: Login authenticates it, ReadKV
// reads KV v2 secrets. It is not safe for concurrent use; the deployment
// loop is single-goroutine (same conservative stance as internal/state).
type Client struct {
	address     string // base address without trailing slash
	mountPath   string // KV v2 mount without slashes
	authMethod  string // "token" or "approle"
	tokenEnv    string // env var holding the token (token auth)
	roleIDEnv   string // env var holding the role ID (approle auth)
	secretIDEnv string // env var holding the secret ID (approle auth)
	token       string // client token from Login; empty before login
	httpClient  *http.Client
	logger      *slog.Logger
}

// New prepares a client for cfg without contacting the server (no login;
// call Login). The address is cfg.Address or, when unset, the VAULT_ADDR
// environment variable; a trailing slash is normalized away. Unset optional
// fields fall back to the same smart defaults the config loader applies. A
// nil logger falls back to slog.Default().
func New(cfg config.Vault, logger *slog.Logger) (*Client, error) {
	address := cfg.Address
	if address == "" {
		address = os.Getenv("VAULT_ADDR")
	}
	if address == "" {
		return nil, fmt.Errorf("vault: no address configured (set global.vault.address or VAULT_ADDR)")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		address:     strings.TrimRight(address, "/"),
		mountPath:   defaultString(cfg.MountPath, defaultMountPath),
		authMethod:  defaultString(cfg.AuthMethod, defaultAuthMethod),
		tokenEnv:    defaultString(cfg.TokenFromEnv, defaultTokenEnv),
		roleIDEnv:   defaultString(cfg.RoleIDFromEnv, defaultRoleIDEnv),
		secretIDEnv: defaultString(cfg.SecretIDFromEnv, defaultSecretIDEnv),
		httpClient:  &http.Client{},
		logger:      logger,
	}, nil
}

// Login authenticates according to the configured auth method and stores the
// resulting client token for subsequent ReadKV calls.
//
//   - "token": the token is read from the token env var (VAULT_TOKEN by
//     default). Token auth needs no login request — but an empty token is an
//     error, since every subsequent read would fail anyway.
//   - "approle": role ID and secret ID are read from their env vars
//     (VAULT_ROLE_ID / VAULT_SECRET_ID by default) and exchanged for a
//     client token via POST /v1/auth/approle/login.
//
// Credentials are read from the environment at call time, not in New, so a
// client can be built before the environment is prepared (and tests can use
// t.Setenv). Tokens are never logged.
func (c *Client) Login(ctx context.Context) error {
	switch c.authMethod {
	case "token":
		token := os.Getenv(c.tokenEnv)
		if token == "" {
			return fmt.Errorf("vault: token auth: %s is not set (token auth needs no login request, but the token itself is required)", c.tokenEnv)
		}
		c.token = token
		c.logger.Info("vault auth ready", "method", "token")
		return nil
	case "approle":
		roleID := os.Getenv(c.roleIDEnv)
		if roleID == "" {
			return fmt.Errorf("vault: approle auth: %s is not set", c.roleIDEnv)
		}
		secretID := os.Getenv(c.secretIDEnv)
		if secretID == "" {
			return fmt.Errorf("vault: approle auth: %s is not set", c.secretIDEnv)
		}
		token, err := c.approleLogin(ctx, roleID, secretID)
		if err != nil {
			return err
		}
		c.token = token
		c.logger.Info("vault login succeeded", "method", "approle")
		return nil
	default:
		return fmt.Errorf("vault: unsupported auth_method %q (must be \"token\" or \"approle\")", c.authMethod)
	}
}

// approleLogin exchanges roleID/secretID for a client token.
func (c *Client) approleLogin(ctx context.Context, roleID, secretID string) (string, error) {
	payload, err := json.Marshal(map[string]string{"role_id": roleID, "secret_id": secretID})
	if err != nil {
		return "", fmt.Errorf("vault: approle login: encode payload: %w", err)
	}
	status, body, err := c.send(ctx, http.MethodPost, c.address+"/v1/auth/approle/login", payload, false)
	if err != nil {
		return "", fmt.Errorf("vault: approle login: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("vault: approle login: unexpected status %d: %s", status, snippet(body))
	}
	var resp struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("vault: approle login: decode response: %w", err)
	}
	if resp.Auth.ClientToken == "" {
		return "", fmt.Errorf("vault: approle login: response contains no auth.client_token: %s", snippet(body))
	}
	return resp.Auth.ClientToken, nil
}

// ReadKV reads the KV v2 secret at path below the configured mount
// (GET <address>/v1/<mount>/data/<path>) and returns its key/value pairs.
// Only string values are expected in dotenv-style secrets; non-string
// values (numbers, booleans, null) are flattened with fmt.Sprintf("%v") so
// callers always receive strings. HTTP 404 becomes a "not found" error;
// every other non-OK status becomes an error with the status code and a
// bounded body snippet.
func (c *Client) ReadKV(ctx context.Context, path string) (map[string]string, error) {
	if path == "" {
		return nil, fmt.Errorf("vault: read: path must not be empty")
	}
	if c.token == "" {
		return nil, fmt.Errorf("vault: read %s: not logged in (call Login first)", path)
	}
	target := fmt.Sprintf("%s/v1/%s/data/%s", c.address, escapeSegments(c.mountPath), escapeSegments(path))
	status, body, err := c.send(ctx, http.MethodGet, target, nil, true)
	if err != nil {
		return nil, fmt.Errorf("vault: read %s: %w", path, err)
	}
	switch {
	case status == http.StatusOK:
	case status == http.StatusNotFound:
		return nil, fmt.Errorf("vault: read %s: not found", path)
	default:
		return nil, fmt.Errorf("vault: read %s: unexpected status %d: %s", path, status, snippet(body))
	}

	// KV v2 wraps the secret data twice: {"data": {"data": {...}, "metadata": {...}}}.
	var resp struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("vault: read %s: decode response: %w", path, err)
	}
	out := make(map[string]string, len(resp.Data.Data))
	for k, v := range resp.Data.Data {
		if s, ok := v.(string); ok {
			out[k] = s
		} else {
			out[k] = fmt.Sprintf("%v", v)
		}
	}
	return out, nil
}

// send performs one HTTP request against the Vault server, attaches the
// client token when withToken is set and returns the status code and the
// full body.
func (c *Client) send(ctx context.Context, method, target string, payload []byte, withToken bool) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withToken {
		req.Header.Set("X-Vault-Token", c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response body: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// defaultString returns value unless it is empty, then fallback.
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// escapeSegments percent-escapes a mount or secret path per segment, so
// segments containing spaces or "?" stay one path component while the
// separating slashes survive.
func escapeSegments(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// snippet renders a response body for an error message, bounded to
// maxErrorBody bytes; an empty body is rendered explicitly so a silent
// failure is distinguishable from a missing one.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(empty body)"
	}
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "..."
	}
	return s
}
