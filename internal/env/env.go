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
	"errors"
	"fmt"
	"io/fs"
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

// stackEnvFile is the name of the generated dotenv file in the stack
// directory (see WriteStackEnv).
const stackEnvFile = ".env"

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
// parsed, all sops files decrypted, all Vault paths read and all vault
// templates rendered via secrets, then the system variables are set. baseDir
// is the directory relative env_files, sops and vault template paths resolve
// against (typically stack.Dir, passed separately so callers and tests
// control it).
//
// Merge priority, low to high: env_files < sops_files < vault_paths and
// vault_env_templates < system variables — and within each group, later
// list entries win. The vault templates sit on the same level as
// vault_paths (the briefing groups them there for compatibility with
// existing vault.env files) and are applied after them, so on a key
// collision the template wins — a deterministic, documented choice among
// the open points. The config loader (internal/config) already concatenates
// the base stack's lists with the import's ("base first, then import"), so
// the required "base before import" priority falls out of the plain list
// order; nothing here re-derives the base/import origin. The system
// variables are set last and therefore cannot be overridden by any secret
// source — a stack cannot lie about its own identity.
func MergeStackEnv(ctx context.Context, stack config.Stack, secrets secrets.Resolver, baseDir string, sys SystemVars) (map[string]string, error) {
	env := make(map[string]string)

	// The stack directory's own .env is dockops output: WriteStackEnv
	// regenerates it from exactly these inputs before every compose call.
	// Reading it back (a stale file from a previous deployment or one
	// committed to the repository) would freeze old resolved values into
	// the merge, so any env_files entry resolving to it is skipped.
	generatedEnv := filepath.Clean(filepath.Join(baseDir, stackEnvFile))
	for i, rel := range stack.EnvFiles {
		path := rel
		if !filepath.IsAbs(path) {
			path = filepath.Join(baseDir, path)
		}
		if filepath.Clean(path) == generatedEnv {
			continue
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

	// Vault templates render on the same priority level as vault_paths
	// (applied after them; see the merge priority note above). The same
	// generated-<stackDir>/.env skip as for env_files applies: a template
	// pointing at dockops' own output would freeze stale resolved values.
	for i, rel := range stack.VaultEnvTemplates {
		path := rel
		if !filepath.IsAbs(path) {
			path = filepath.Join(baseDir, path)
		}
		if filepath.Clean(path) == generatedEnv {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("stack %s: vault_env_templates[%d] %s: %w", stack.Name, i, path, err)
		}
		kv, err := RenderVaultTemplate(ctx, string(raw), resolverVaultReader{resolver: secrets})
		if err != nil {
			return nil, fmt.Errorf("stack %s: vault_env_templates[%d] %s: %w", stack.Name, i, path, err)
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

// WriteStackEnv writes env in dotenv format to <stackDir>/.env — atomically
// (a temporary file in the same directory, renamed over the target) and
// with 0600 permissions, since the merged environment carries secrets. The
// content is the deterministic dotenv format (sorted keys, literal
// KEY=VALUE lines, no quoting) that the compose dotenv parser reads
// verbatim.
//
// The file backs everything compose resolves relative to the stack
// directory: `env_file: ./.env` references and ${...} interpolation in the
// compose file. It carries the same merged environment as the shm --env-file
// (env_files, sops, vault, system variables).
//
// It returns the file path and a rollback function for deployment failures:
// rollback restores the file that occupied the path before — overwriting it
// with the saved previous content — or removes it when none existed. On a
// successful deployment rollback must NOT be called: the file stays so
// later `docker compose logs/exec` invocations resolve the same
// environment. Note that a checkout-updating pass (git reset/clean) may
// remove or revert it; the next deployment of the stack regenerates it.
//
// The generated file is deliberately NOT part of the stack hash
// (internal/hash): it is a deployment artifact derived from sources the
// hash already covers by reference. A changed secret value therefore does
// not trigger a redeployment — a conscious decision matching the Komodo
// reality the briefing describes (secret rotation alone deploys nothing
// until the stack definition changes).
func WriteStackEnv(stackDir string, env map[string]string) (path string, rollback func(), err error) {
	content := dotenv.Format(env)
	path = filepath.Join(stackDir, stackEnvFile)

	// Snapshot the previous file (content and permissions) for rollback.
	var prev []byte
	var prevMode os.FileMode
	hadPrev := true
	prev, err = os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, fmt.Errorf("env: read previous %s: %w", path, err)
		}
		hadPrev = false
	} else if info, statErr := os.Stat(path); statErr == nil {
		prevMode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(stackDir, "."+stackEnvFile+".dockops-*")
	if err != nil {
		return "", nil, fmt.Errorf("env: create temp file in %s: %w", stackDir, err)
	}
	tmpName := tmp.Name()
	// os.CreateTemp creates 0600 files; the permission survives the rename.
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", nil, fmt.Errorf("env: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", nil, fmt.Errorf("env: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return "", nil, fmt.Errorf("env: rename %s to %s: %w", tmpName, path, err)
	}

	var once sync.Once
	rollback = func() {
		once.Do(func() {
			if !hadPrev {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					slog.Default().Warn("cannot remove generated stack env file", "path", path, "error", err)
				}
				return
			}
			// Restore the previous file with its original permissions;
			// failures are logged because rollback must stay callable in
			// defer chains. os.WriteFile only applies perm to newly created
			// files, so the mode is restored with an explicit Chmod.
			if prevMode == 0 {
				prevMode = 0o600
			}
			if err := os.WriteFile(path, prev, prevMode); err != nil {
				slog.Default().Warn("cannot restore previous stack env file", "path", path, "error", err)
			} else if err := os.Chmod(path, prevMode); err != nil {
				slog.Default().Warn("cannot restore stack env file permissions", "path", path, "error", err)
			}
		})
	}
	return path, rollback, nil
}

// contentHash returns the first 12 hex characters of the SHA-256 over data.
func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}
