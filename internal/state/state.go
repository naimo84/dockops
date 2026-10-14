// Package state persists the last deployed state of every stack in a single
// JSON file so the poll loop can detect relevant changes between runs by
// comparing the deterministic stack hash (internal/hash/stackhash).
//
// The phase-2 briefing announces this store by name only ("State-Store");
// its specification was lost. The implementation therefore follows the
// project convention for open points — stdlib only, conservative behavior,
// no premature features — with each decision documented at its site:
//   - single JSON file at a caller-chosen path (JSON: stdlib encoding,
//     machine-written and machine-read; not user-facing config, so no YAML),
//   - one entry per stack name, holding the stack hash at the last deploy,
//     a status and a timestamp,
//   - atomic persistence (temp file + rename) so a crash never leaves a
//     truncated state file behind.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// fileVersion is the on-disk schema version. A state file with a different
// version is rejected instead of guessed at.
const fileVersion = 1

// Status values of a persisted stack entry. A stack is only considered
// successfully deployed with StatusDeployed.
const (
	// StatusDeployed marks a stack whose last deployment succeeded.
	StatusDeployed = "deployed"
	// StatusFailed marks a stack whose last deployment attempt failed; the
	// next poll retries it even with an unchanged hash.
	StatusFailed = "failed"
	// StatusPruned marks a stack whose resources were removed after it
	// disappeared from the configuration (prune_removed_stacks).
	StatusPruned = "pruned"
)

// State is the persisted deployment state of a single stack.
type State struct {
	// Hash is the stack hash (see package stackhash) at the time of the
	// recorded deployment.
	Hash string `json:"hash"`
	// Status is one of the Status constants.
	Status string `json:"status"`
	// UpdatedAt is the UTC time of the last state change.
	UpdatedAt time.Time `json:"updated_at"`
}

// fileModel is the JSON document persisted by Save.
type fileModel struct {
	Version int              `json:"version"`
	Stacks  map[string]State `json:"stacks"`
}

// Store holds the per-stack deployment states of one state file. It is not
// safe for concurrent use; the poll loop is single-goroutine (conservative:
// no locking until there is an actual second goroutine).
type Store struct {
	path   string
	logger *slog.Logger
	stacks map[string]State
}

// New loads (or initializes) the store at path, creating parent directories
// if needed. A missing file yields an empty store; a corrupt file or one
// with an unknown schema version is an error naming the path. A nil logger
// falls back to slog.Default().
func New(path string, logger *slog.Logger) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("state: path must not be empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("state: create directory %s: %w", dir, err)
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Store{path: path, logger: logger, stacks: make(map[string]State)}

	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil // fresh store, nothing persisted yet
	}
	if err != nil {
		return nil, fmt.Errorf("state: read %s: %w", path, err)
	}
	var f fileModel
	// Unknown JSON fields are ignored (unlike the YAML config): the file is
	// machine-written, and forward compatibility beats strictness here.
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("state: parse %s: %w", path, err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("state: %s: unsupported version %d (want %d)", path, f.Version, fileVersion)
	}
	if f.Stacks != nil {
		s.stacks = f.Stacks
	}
	s.logger.Info("state loaded", "path", path, "stacks", len(s.stacks))
	return s, nil
}

// Get returns the persisted state of the named stack; ok is false when the
// store holds no entry for it.
func (s *Store) Get(name string) (State, bool) {
	st, ok := s.stacks[name]
	return st, ok
}

// Set replaces the in-memory state of the named stack. A zero UpdatedAt is
// stamped with the current UTC time (caller-supplied timestamps make tests
// deterministic); call Save to persist. Unknown statuses are rejected so
// typos cannot silently disable change detection.
func (s *Store) Set(name string, st State) error {
	if name == "" {
		return fmt.Errorf("state: stack name must not be empty")
	}
	switch st.Status {
	case StatusDeployed, StatusFailed, StatusPruned:
	default:
		return fmt.Errorf("state: stack %s: invalid status %q (must be deployed, failed or pruned)", name, st.Status)
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now().UTC()
	} else {
		st.UpdatedAt = st.UpdatedAt.UTC()
	}
	s.stacks[name] = st
	return nil
}

// Delete removes the named stack's entry and reports whether one existed.
// Call Save to persist the removal.
func (s *Store) Delete(name string) bool {
	if _, ok := s.stacks[name]; !ok {
		return false
	}
	delete(s.stacks, name)
	return true
}

// Names returns all stack names with an entry, sorted for determinism.
func (s *Store) Names() []string {
	names := make([]string, 0, len(s.stacks))
	for name := range s.stacks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Changed reports whether the named stack needs a (re)deployment given its
// current hash: no entry yet, a different hash, or a last attempt that did
// not end in StatusDeployed (failed deploys are retried on the next poll
// even with an unchanged hash).
func (s *Store) Changed(name string, hash string) bool {
	st, ok := s.stacks[name]
	return !ok || st.Hash != hash || st.Status != StatusDeployed
}

// Save atomically persists the store: it writes a temporary file in the
// state file's directory, fsyncs it and renames it over the target, so
// readers never observe a partially written state. Temporary files are
// created with 0o600 and keep that permission after the rename (the state
// is not secret, but restrictive defaults are the conservative choice).
func (s *Store) Save() error {
	f := fileModel{Version: fileVersion, Stacks: s.stacks}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("state: serialize: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".dockops-state-*")
	if err != nil {
		return fmt.Errorf("state: create temp file next to %s: %w", s.path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("state: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("state: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("state: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("state: rename %s to %s: %w", tmpName, s.path, err)
	}
	s.logger.Info("state saved", "path", s.path, "stacks", len(s.stacks))
	return nil
}
