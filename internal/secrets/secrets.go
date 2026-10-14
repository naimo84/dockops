// Package secrets abstracts the two secret sources of a stack — sops-
// encrypted files (internal/sops) and HashiCorp Vault KV v2 paths
// (internal/vault) — behind one interface.
//
// The interface exists so the environment merge (internal/env) depends only
// on the concept of "resolving secrets", not on the concrete CLI and HTTP
// implementations. That keeps the merge testable with a fake and leaves the
// wiring of the concrete resolvers to the deployment loop.
package secrets

import (
	"context"

	"github.com/local/dockops/internal/config"
)

// Resolver resolves the secret references of a stack into key/value pairs
// that are merged into the stack environment. Implementations only need to
// be safe for sequential use; the deployment loop resolves secrets one stack
// at a time.
type Resolver interface {
	// ResolveSops decrypts a single sops-encrypted file and returns its
	// key/value pairs. File.Path is already resolved to an absolute path by
	// the caller (internal/env); AgeKeyfile travels along unmodified.
	ResolveSops(ctx context.Context, file config.SopsFile) (map[string]string, error)

	// ResolveVault reads the key/value pairs stored at a single Vault KV v2
	// path (for example "stacks/web") below the configured mount.
	ResolveVault(ctx context.Context, path string) (map[string]string, error)
}
