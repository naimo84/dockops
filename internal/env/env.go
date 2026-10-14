// Package env assembles the environment of a stack from its env_files, its
// sops-encrypted files, its Vault paths and the system variables dockops
// provides, and writes the merged result as a temporary --env-file for
// docker compose.
package env

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/dotenv"
	"github.com/local/dockops/internal/secrets"
)

// shmDir is the primary parent directory for secret env files (/dev/shm, a
// tmpfs, so the plaintext never touches the disk). A package variable so
// tests can simulate a host without /dev/shm.
var shmDir = "/dev/shm"

// SystemVars are the variables dockops itself injects into every stack
// environment. They are passed explicitly instead of being read from the
// process environment (os.Getenv), so callers — and tests — control the
// values.
type SystemVars struct {
	// StackName becomes STACK_NAME; empty falls back to stack.Name.
	StackName string
	// StackDir becomes STACK_DIR; empty falls back to stack.Dir.
	StackDir string
	// GitCommit becomes GIT_COMMIT (the commit the stack definition was
	// deployed from; empty when unknown).
	GitCommit string
}

// MergeStackEnv assembles the environment of stack: all env_files are
// parsed, all sops files decrypted and all Vault paths read via secrets,
// then the system variables are set. baseDir is the directory relative
// env_files and sops paths resolve against (typically stack.Dir, passed
// separately so callers and tests control it).
//
// Merge priority, low to high: env_files < sops_files < vault_paths <
// system variables — and within each group, later list entries win. The
// config loader (internal/config) already concatenates the base stack's
// lists with the import's ("base first, then import"), so the required
// "base before import" priority falls out of the plain list order; nothing
// here re-derives the base/import origin. The system variables are set last
// and therefore cannot be overridden by any secret source — a stack cannot
// lie about its own identity.
func MergeStackEnv(ctx context.Context, stack config.Stack, secrets secrets.Resolver, baseDir string, sys SystemVars) (map[string]string, error) {
	env := make(map[string]string)

	for i, rel := range stack.EnvFiles {
		path := rel
		if !filepath.IsAbs(path) {
			path = filepath.Join(baseDir, path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("stack %s: env_files[%d] %s: %w", stack.Name, i, path, err)
		}
		for k, v := range dotenv.Parse(raw) {
			env[k] = v
		}
	}

	for i, f := range stack.SopsFiles {
		resolved := f
		if f.Path != "" && !filepath.IsAbs(f.Path) {
			resolved.Path = filepath.Join(baseDir, f.Path)
		}
		kv, err := secrets.ResolveSops(ctx, resolved)
		if err != nil {
			return nil, fmt.Errorf("stack %s: sops_files[%d] %s: %w", stack.Name, i, resolved.Path, err)
		}
		for k, v := range kv {
			env[k] = v
		}
	}

	for i, p := range stack.VaultPaths {
		kv, err := secrets.ResolveVault(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("stack %s: vault_paths[%d] %s: %w", stack.Name, i, p, err)
		}
		for k, v := range kv {
			env[k] = v
		}
	}

	name, dir := sys.StackName, sys.StackDir
	if name == "" {
		name = stack.Name
	}
	if dir == "" {
		dir = stack.Dir
	}
	env["STACK_NAME"] = name
	env["STACK_DIR"] = dir
	env["GIT_COMMIT"] = sys.GitCommit
	return env, nil
}

// WriteShm writes env in dotenv format to a fresh file
// dockops-<random>-<contenthash>.env with 0600 permissions and returns its
// path plus a cleanup function.
//
// The parent directory is /dev/shm (tmpfs — secrets never reach the disk);
// when it does not exist, the temp directory (TMPDIR) is used instead and a
// warning is logged.
//
// The briefing names the file dockops-<random>-<stackhash>.env but its
// WriteShm signature carries no stack hash; the conservative resolution
// (per project convention) is a short hash of the file content in that
// place — it is deterministic per stack environment, which is all the name
// has to guarantee, while the random component makes collisions impossible.
//
// cleanup overwrites the file with random bytes of the same length (so the
// plaintext is not simply left behind on the tmpfs) and removes it. It is
// idempotent and safe to call more than once and from any goroutine.
func WriteShm(env map[string]string) (path string, cleanup func(), err error) {
	content := dotenv.Format(env)

	dir := shmDir
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		fallback := os.TempDir()
		slog.Default().Warn("shm directory not available, falling back to temp dir",
			"shm_dir", dir, "fallback", fallback, "error", statErr)
		dir = fallback
	}

	suffix, err := randomHex(4)
	if err != nil {
		return "", nil, fmt.Errorf("env: generate env file name: %w", err)
	}
	path = filepath.Join(dir, fmt.Sprintf("dockops-%s-%s.env", suffix, contentHash(content)))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", nil, fmt.Errorf("env: write %s: %w", path, err)
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(path)
		return "", nil, fmt.Errorf("env: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", nil, fmt.Errorf("env: write %s: %w", path, err)
	}

	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			// Best-effort scrub: overwrite with random bytes of the same
			// length before unlinking. Failures are logged, not returned —
			// cleanup must stay callable in defer chains.
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				buf := make([]byte, info.Size())
				if _, randErr := rand.Read(buf); randErr != nil {
					slog.Default().Warn("cannot gather random bytes to scrub env file", "path", path, "error", randErr)
				} else if writeErr := os.WriteFile(path, buf, 0o600); writeErr != nil {
					slog.Default().Warn("cannot scrub env file before removal", "path", path, "error", writeErr)
				}
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				slog.Default().Warn("cannot remove env file", "path", path, "error", err)
			}
		})
	}
	return path, cleanup, nil
}

// randomHex returns n random bytes, hex encoded (2n characters). The error
// is returned instead of panicking (project rule: no panic in library
// code), even though crypto/rand only fails when the system entropy source
// is broken.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// contentHash returns the first 12 hex characters of the SHA-256 over data.
func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}
