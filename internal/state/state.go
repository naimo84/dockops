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
	"sync"
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
	// StackDir is the directory the stack definition lived in at deploy
	// time. Only used by pruning, to tear a stack down after it disappeared
	// from the configuration. Optional: state files written before the
	// field existed simply have no value here.
	StackDir string `json:"stack_dir,omitempty"`
	// ComposeFile is the resolved compose file path at deploy time, for the
	// same purpose as StackDir. Optional, see there.
	ComposeFile string `json:"compose_file,omitempty"`
	// ComposeFiles holds the resolved compose file paths when the stack was
	// deployed from multiple files (compose_files); ComposeFile stays empty
	// in that case. Optional, see ComposeFile.
	ComposeFiles []string `json:"compose_files,omitempty"`
	// Images holds the last update check per image reference (phase 10).
	// Purely additive and optional: state files written before the field
	// existed load unchanged, and fileVersion stays 1 — nothing about a
	// deployment depends on it (it neither triggers nor blocks one), so a
	// migration or a schema bump would be noise.
	Images map[string]ImageRecord `json:"images,omitempty"`
	// UpdatePolicy is the stack's update_policy at deploy time ("notify",
	// "pinned" or "apply"). Phase 11 stores it because GET /status must
	// report the effective policy exclusively from the state: the phase-10
	// assumption that "a record set implies notify" no longer holds once
	// apply stacks keep records too. Optional, like Images — an older file
	// simply has no value, and callers read it as notify (the default).
	UpdatePolicy string `json:"update_policy,omitempty"`

	// The phase-13 deploy pending bookkeeping. Additive and omitempty like
	// the fields above — fileVersion stays 1: a state file without them is
	// an agent that never handed a deploy over, and the fields only
	// document a hand-over that is already in flight.

	// DeployPendingHash is the stack hash whose deploy was handed to the
	// detached helper (phase 13). While it is set, deployStack leaves the
	// stack untouched and starts no second helper, and the next agent start
	// finalizes the outcome from the helper's result file. It is the stack
	// counterpart of ImageRecord.ApplyPendingDigest: a deploy concerns the
	// stack, an image record concerns one image.
	DeployPendingHash string `json:"deploy_pending_hash,omitempty"`
	// DeployPendingAt is the UTC time of that hand-over, zero when none
	// ever ran (same zero-value serialization as ImageRecord.AppliedAt).
	// It is the age basis of the finalization timeout: without it a helper
	// that left no result would keep its marker forever and the stack would
	// never be deployed again.
	DeployPendingAt time.Time `json:"deploy_pending_at,omitempty"`
}

// ImageRecord is the persisted outcome of the last update check of one image
// reference of a stack (phase 10). It records what was compared when; it
// never drives a deployment.
type ImageRecord struct {
	// Service is the compose service the image belongs to. The map is
	// keyed by image reference (an image may serve several services, and
	// the reference is what the registry is asked about); the service name
	// is kept so GET /status can name it without a compose call.
	Service string `json:"service,omitempty"`
	// CurrentDigest is the locally recorded repository digest
	// (docker image inspect RepoDigests); empty for a locally built image.
	CurrentDigest string `json:"current_digest,omitempty"`
	// RemoteDigest is the digest the registry reported for the reference.
	RemoteDigest string `json:"remote_digest,omitempty"`
	// UpdateAvailable reports that remote and local digest differ — a new
	// image is published under the tag the stack uses.
	UpdateAvailable bool `json:"update_available,omitempty"`
	// CheckedAt is the UTC time of the check (also the due-date basis for
	// update_check_interval).
	CheckedAt time.Time `json:"checked_at"`
	// Error carries a failed check: registry errors (rate limit, auth,
	// missing manifest) and "locally built image" (no RepoDigest). An
	// error never means "update available".
	Error string `json:"error,omitempty"`

	// The phase-11 apply bookkeeping. All fields are additive and
	// omitempty: a notify or pinned stack never sets them, older state
	// files load unchanged and fileVersion stays 1 (nothing here drives a
	// deployment — it only documents what the apply step did).

	// PreviousDigest is the digest the image ran before the last apply
	// attempt. It is the rollback target: the generated override file pins
	// exactly this digest.
	PreviousDigest string `json:"previous_digest,omitempty"`
	// AppliedAt is the UTC time of the last successful apply, zero when
	// the image was never applied by dockops. Note that encoding/json's
	// omitempty does not omit a zero time.Time: the key stays with the zero
	// timestamp, and readers treat that as "never happened" (the briefing
	// prescribes this field shape, so the wart is documented, not worked
	// around with a pointer type).
	AppliedAt time.Time `json:"applied_at,omitempty"`
	// RolledBackAt is the UTC time of the last rollback, zero when no
	// rollback ran (same zero-value serialization as AppliedAt).
	RolledBackAt time.Time `json:"rolled_back_at,omitempty"`
	// ApplyBlockedDigest is the remote digest whose apply failed its health
	// gate (or its pull/up). While it equals RemoteDigest, update_policy:
	// apply treats the image like notify — no retry. A digest comparison
	// replaces a time-based backoff on purpose: it is honest (the same
	// broken image stays broken) and it self-clears as soon as the tag
	// moves to a different digest.
	ApplyBlockedDigest string `json:"apply_blocked_digest,omitempty"`
	// ApplyError is the error text of the last failed apply attempt (pull,
	// compose up, health gate or rollback). Cleared by a successful apply
	// and by a check that no longer finds an update.
	ApplyError string `json:"apply_error,omitempty"`

	// The phase-12 self update bookkeeping. Additive and omitempty like the
	// fields above — fileVersion stays 1: a state file without them is an
	// agent that never ran a self update, and the fields only document a
	// hand-over that is already in flight.

	// ApplyPendingDigest is the target digest of a running self update:
	// the agent wrote the plan, started the detached helper and cannot
	// record the outcome itself (its own container is what gets recreated).
	// While it is set, the apply path refuses to start a second helper,
	// GET /status reports the record as pending, and the next agent start
	// finalizes the outcome from the helper's result file. It is the one
	// honest interim state the dying process can leave behind.
	ApplyPendingDigest string `json:"apply_pending_digest,omitempty"`
	// PendingAt is the UTC time the self update was handed to the helper,
	// zero when none ever ran (same zero-value serialization as AppliedAt).
	// It answers "how long has this been pending?" without guessing from
	// CheckedAt.
	PendingAt time.Time `json:"pending_at,omitempty"`
}

// fileModel is the JSON document persisted by Save.
type fileModel struct {
	Version int              `json:"version"`
	Stacks  map[string]State `json:"stacks"`
}

// Store holds the per-stack deployment states of one state file. It is safe
// for concurrent use: the poll loop mutates it from its own goroutine while
// the webhook server's request goroutines read it (GET /status, phase 8).
// Before that second reader existed the store deliberately went without
// locking; the RWMutex is its documented successor.
type Store struct {
	path   string
	logger *slog.Logger
	mu     sync.RWMutex
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

// Path returns the file path the store persists to. Callers that need the
// state directory derive it from this instead of duplicating how the path
// was configured — the phase-12 self update keeps its plan, result and
// helper log next to the state file (the one directory the agent and the
// helper share).
func (s *Store) Path() string {
	return s.path
}

// Get returns the persisted state of the named stack; ok is false when the
// store holds no entry for it.
func (s *Store) Get(name string) (State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stacks[name] = st
	return nil
}

// SetImages records the update-check results of one stack, replacing its
// previous image records. The stack entry itself must exist: an image record
// belongs to a deployment (it compares that deployment's digests), so there
// is nothing to attach it to otherwise — the caller logs and skips. Hash,
// status, timestamps and compose traces stay untouched; call Save to persist.
func (s *Store) SetImages(stack string, recs map[string]ImageRecord) error {
	if stack == "" {
		return fmt.Errorf("state: stack name must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stacks[stack]
	if !ok {
		return fmt.Errorf("state: stack %s: no state entry to attach image records to", stack)
	}
	if recs == nil {
		st.Images = nil
	} else {
		copied := make(map[string]ImageRecord, len(recs))
		for ref, rec := range recs {
			rec.CheckedAt = rec.CheckedAt.UTC()
			copied[ref] = rec
		}
		st.Images = copied
	}
	s.stacks[stack] = st
	return nil
}

// Delete removes the named stack's entry and reports whether one existed.
// Call Save to persist the removal.
func (s *Store) Delete(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.stacks[name]; !ok {
		return false
	}
	delete(s.stacks, name)
	return true
}

// Names returns all stack names with an entry, sorted for determinism.
func (s *Store) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.stacks[name]
	return !ok || st.Hash != hash || st.Status != StatusDeployed
}

// Save atomically persists the store: it writes a temporary file in the
// state file's directory, fsyncs it and renames it over the target, so
// readers never observe a partially written state. Temporary files are
// created with 0o600 and keep that permission after the rename (the state
// is not secret, but restrictive defaults are the conservative choice).
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
