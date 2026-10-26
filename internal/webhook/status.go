// GET /status — the monitoring surface: it compares the desired state
// (state.json, written by the reconciler) against the running containers
// (docker ps / compose ps via ComposeLister), so external watchdogs (Gatus,
// curl, a Telegram bot) can tell drift without docker access of their own.
//
// Robustness beats precision here: a broken docker command degrades exactly
// the affected part of the payload (a stack's drift becomes "unknown", the
// unmanaged listing becomes a top-level error field) while the report
// keeps answering — monitoring must not lose every stack because one
// stack's ps failed. The document assembly lives in the exported
// BuildStatus so the HTTP handler and the CLI's -print-status mode share
// one implementation.
package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/state"
)

// Drift classifications of a managed stack (see driftFor).
const (
	// driftOK: at least one container running, none exited.
	driftOK = "ok"
	// driftStopped: stack recorded in state, but no container running.
	driftStopped = "stopped"
	// driftPartial: containers running, but at least one exited.
	driftPartial = "partial"
	// driftUnknown: the container listing failed (error field set).
	driftUnknown = "unknown"
)

// bearerPrefix is the Authorization scheme /status accepts when Deps.Token
// is set.
const bearerPrefix = "Bearer "

// StatusDeps carries the /status collaborators: the status subset of Deps,
// extractable so the CLI's one-shot mode (-print-status) builds the same
// document without a server. State and Compose are required (BuildStatus
// rejects nil); every other field degrades as documented.
type StatusDeps struct {
	// State is the deployment state store — the desired side of the report.
	State *state.Store

	// Compose lists the running containers — the actual side of the report.
	Compose ComposeLister

	// RepoURL and Branch identify the host repository; the report carries
	// them under host_repo.
	RepoURL string
	Branch  string

	// HeadCommit returns the commit currently checked out in the host
	// repository (host_repo.commit). Nil or an error degrades the commit to
	// "" — missing git metadata must not fail the report.
	HeadCommit func() (string, error)

	// SelfProject is the compose project the agent itself runs under; it is
	// never reported as unmanaged (see Deps.SelfProject).
	SelfProject string

	// Logger receives the degradation warnings (failed docker queries); nil
	// falls back to slog.Default().
	Logger *slog.Logger
}

// StatusResponse is the JSON document of GET /status and -print-status.
// Stacks and unmanaged are always JSON arrays (never null) so consumers
// need no null handling; both are sorted by name for deterministic output.
type StatusResponse struct {
	// GeneratedAt is the RFC3339 timestamp of the snapshot.
	GeneratedAt string `json:"generated_at"`
	// HostRepo describes the host repository the agent reconciles from.
	HostRepo HostRepo `json:"host_repo"`
	// Stacks holds one entry per state.json stack, sorted by name.
	Stacks []StackStatus `json:"stacks"`
	// Unmanaged holds running compose projects without a state entry,
	// sorted by project.
	Unmanaged []UnmanagedProject `json:"unmanaged"`
	// Error names a failed unmanaged listing (top level); per-stack
	// failures live on the stack entries instead.
	Error string `json:"error,omitempty"`
}

// HostRepo identifies the host repository: configured URL and branch, plus
// the commit the host checkout currently points at ("" when unknown).
type HostRepo struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

// StackStatus is the desired-vs-actual report of one managed stack.
type StackStatus struct {
	// Name is the stack (and compose project) name.
	Name string `json:"name"`
	// DeployedHash is the stack hash of the last recorded deploy.
	DeployedHash string `json:"deployed_hash"`
	// LastDeployed is the RFC3339 timestamp of the last state change
	// (omitted only for a zero timestamp, which the store never persists).
	LastDeployed string `json:"last_deployed,omitempty"`
	// Containers lists the project's containers sorted by name; an empty
	// array means the project has none.
	Containers []compose.Container `json:"containers"`
	// Drift classifies the stack against the drift matrix.
	Drift string `json:"drift"`
	// Error carries the container listing failure when Drift is "unknown".
	Error string `json:"error,omitempty"`
	// Updates reports the last image update check of this stack (phase 10).
	// Nil when the stack has no records — no check ran yet, or its
	// update_policy is pinned. Built exclusively from the state: /status
	// stays fast, offline-capable and free of registry lookups.
	Updates *StackUpdates `json:"updates,omitempty"`
}

// StackUpdates is the persisted image update situation of one stack.
type StackUpdates struct {
	// Policy is the stack's update policy at deploy time: "notify",
	// "pinned" or "apply" (phase 11). It comes from the state, which
	// records it with every deployment — the phase-10 shortcut "records
	// imply notify" stopped holding once apply stacks kept records too. An
	// entry written before the state carried the policy reads as notify,
	// the default.
	Policy string `json:"policy"`
	// CheckedAt is the newest check timestamp (RFC3339, empty when
	// unknown).
	CheckedAt string `json:"checked_at"`
	// Pending counts the images whose tag moved (update_available).
	Pending int `json:"pending"`
	// Services lists one entry per checked image, sorted by service.
	Services []ServiceUpdate `json:"services"`
}

// ServiceUpdate is the update state of one image of a stack.
type ServiceUpdate struct {
	// Service is the compose service name.
	Service string `json:"service"`
	// Image is the image reference (the registry's identity of the image).
	Image string `json:"image"`
	// CurrentDigest is the locally recorded digest, empty for a locally
	// built image.
	CurrentDigest string `json:"current_digest"`
	// RemoteDigest is the digest the registry reported.
	RemoteDigest string `json:"remote_digest"`
	// UpdateAvailable reports that the tag points at a different digest.
	UpdateAvailable bool `json:"update_available"`
	// Error carries a failed check (registry error, locally built image).
	Error string `json:"error"`

	// The phase-11 apply fields. Additive and omitted while empty, so a
	// notify stack's document is byte-identical to the phase-10 one.

	// PreviousDigest is the digest the image ran before the last apply
	// attempt (the rollback target), empty when dockops never applied it.
	PreviousDigest string `json:"previous_digest,omitempty"`
	// AppliedAt is the RFC3339 time of the last successful apply.
	AppliedAt string `json:"applied_at,omitempty"`
	// RolledBackAt is the RFC3339 time of the last rollback.
	RolledBackAt string `json:"rolled_back_at,omitempty"`
	// ApplyError is the failure text of the last apply attempt (pull,
	// compose up, health gate or rollback).
	ApplyError string `json:"apply_error,omitempty"`
	// Blocked reports that this exact remote digest already failed its
	// apply, so update_policy: apply currently behaves like notify for the
	// image. Always present (like update_available), because "is this
	// stack waiting for a human?" is the question the field answers.
	Blocked bool `json:"blocked"`

	// Pending reports that a phase-12 self update of this image is in
	// flight: the agent wrote the plan, started the detached helper and will
	// finalize the outcome on its next start. Omitted while false, so a
	// notify or apply stack's document stays unchanged.
	//
	// It sits on the service entry, not on the updates block, because
	// StackUpdates.pending already counts the images whose tag moved
	// (phase 10). The marker is per image anyway — ApplyPendingDigest is
	// recorded per image record.
	Pending bool `json:"pending,omitempty"`
}

// UnmanagedProject is a running compose project without a state entry.
type UnmanagedProject struct {
	// Project is the compose project name (com.docker.compose.project).
	Project string `json:"project"`
	// Containers lists the project's running container names.
	Containers []string `json:"containers"`
}

// BuildStatus assembles the complete status document — the shared source of
// GET /status and the CLI's -print-status mode, which is why it lives here
// and not inside the handler. Docker failures degrade into the document per
// the package contract (a stack's drift becomes "unknown" with an error
// field, the unmanaged listing becomes the top-level error) and never fail
// the call; the only error is missing configuration (nil State or Compose).
func BuildStatus(ctx context.Context, deps StatusDeps) (StatusResponse, error) {
	if deps.State == nil || deps.Compose == nil {
		return StatusResponse{}, errors.New("status requires a state store and a compose runner")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	resp := StatusResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		HostRepo: HostRepo{
			URL:    deps.RepoURL,
			Branch: deps.Branch,
			Commit: resolveHeadCommit(logger, deps.HeadCommit),
		},
		Stacks:    make([]StackStatus, 0), // JSON [], not null
		Unmanaged: make([]UnmanagedProject, 0),
	}

	// Managed stacks: State.Names() is sorted, so the report is, too.
	managed := make(map[string]struct{})
	for _, name := range deps.State.Names() {
		managed[name] = struct{}{}
		st, _ := deps.State.Get(name)
		resp.Stacks = append(resp.Stacks, buildStackStatus(ctx, logger, deps.Compose, name, st))
	}

	// Unmanaged: running compose projects without a state entry; the
	// agent's own project never counts (it is managed by the operator's
	// compose invocation, not by itself). A failed listing degrades to the
	// top-level error field — the stacks part stays usable.
	refs, err := deps.Compose.PsAllProjects(ctx)
	if err != nil {
		logger.Warn("status: listing running compose projects failed", "error", err)
		resp.Error = fmt.Sprintf("list running projects: %v", err)
	} else {
		for _, ref := range refs {
			if _, ok := managed[ref.Project]; ok {
				continue
			}
			if deps.SelfProject != "" && ref.Project == deps.SelfProject {
				continue
			}
			resp.Unmanaged = append(resp.Unmanaged, UnmanagedProject{
				Project:    ref.Project,
				Containers: ref.Containers,
			})
		}
	}
	return resp, nil
}

// handleStatus serves GET /status: optional Bearer authentication, then the
// snapshot from the shared BuildStatus. It answers 200 whenever the
// endpoint is configured, even when individual docker queries fail (see the
// package comment); only the missing configuration itself is an error (503,
// so miswiring surfaces loudly instead of yielding an empty-but-green
// report).
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !h.statusAuthorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	resp, err := BuildStatus(r.Context(), h.status)
	if err != nil {
		http.Error(w, "status endpoint not configured (state store and compose runner required)", http.StatusServiceUnavailable)
		return
	}
	h.logger.Debug("status served", "stacks", len(resp.Stacks), "unmanaged", len(resp.Unmanaged))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Error("status: write response", "error", err)
	}
}

// buildStackStatus builds the report of one managed stack: recorded hash
// and timestamp from the state entry, live containers from compose ps,
// drift from the matrix below. A failed ps degrades to drift "unknown" with
// the error message on the entry — the report keeps going.
func buildStackStatus(ctx context.Context, logger *slog.Logger, lister ComposeLister, name string, st state.State) StackStatus {
	entry := StackStatus{
		Name:         name,
		DeployedHash: st.Hash,
		Containers:   make([]compose.Container, 0),
		// The update block comes from the state alone and is therefore
		// independent of the container listing: a broken ps must not hide
		// the recorded image situation, and a broken image record must not
		// change the drift statement.
		Updates: buildStackUpdates(st),
	}
	if !st.UpdatedAt.IsZero() {
		entry.LastDeployed = st.UpdatedAt.UTC().Format(time.RFC3339)
	}
	containers, err := lister.PsProject(ctx, name)
	if err != nil {
		logger.Warn("status: listing containers failed", "stack", name, "error", err)
		entry.Drift = driftUnknown
		entry.Error = err.Error()
		return entry
	}
	entry.Containers = sortContainers(containers)
	entry.Drift = driftFor(containers)
	return entry
}

// buildStackUpdates renders the recorded image update check of one stack.
// nil means "no records" — the block is omitted from the response (never an
// empty object pretending a check happened). The state is the only source:
// no docker, no registry, no configuration.
func buildStackUpdates(st state.State) *StackUpdates {
	if len(st.Images) == 0 {
		return nil
	}
	policy := st.UpdatePolicy
	if policy == "" {
		// State written before the field existed (or by a hand-built
		// entry): notify is the default policy.
		policy = config.UpdatePolicyNotify
	}
	updates := &StackUpdates{
		Policy:   policy,
		Services: make([]ServiceUpdate, 0, len(st.Images)), // JSON [], not null
	}
	var newest time.Time
	for ref, rec := range st.Images {
		if rec.CheckedAt.After(newest) {
			newest = rec.CheckedAt
		}
		if rec.UpdateAvailable {
			updates.Pending++
		}
		updates.Services = append(updates.Services, ServiceUpdate{
			Service:         rec.Service,
			Image:           ref,
			CurrentDigest:   rec.CurrentDigest,
			RemoteDigest:    rec.RemoteDigest,
			UpdateAvailable: rec.UpdateAvailable,
			Error:           rec.Error,
			PreviousDigest:  rec.PreviousDigest,
			AppliedAt:       formatTime(rec.AppliedAt),
			RolledBackAt:    formatTime(rec.RolledBackAt),
			ApplyError:      rec.ApplyError,
			Blocked:         rec.ApplyBlockedDigest != "" && rec.ApplyBlockedDigest == rec.RemoteDigest,
			Pending:         rec.ApplyPendingDigest != "",
		})
	}
	if !newest.IsZero() {
		updates.CheckedAt = newest.UTC().Format(time.RFC3339)
	}
	// Deterministic order: service first (the operator-facing name), the
	// image reference as the tiebreaker for a service using several images.
	sort.Slice(updates.Services, func(i, j int) bool {
		if updates.Services[i].Service != updates.Services[j].Service {
			return updates.Services[i].Service < updates.Services[j].Service
		}
		return updates.Services[i].Image < updates.Services[j].Image
	})
	return updates
}

// formatTime renders a persisted timestamp as RFC3339, or "" for the zero
// time (a field that never happened stays out of the document).
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// driftFor classifies a stack's containers per the drift matrix:
//
//	ok      ≥1 container running, none exited
//	stopped 0 containers running (all exited or none at all)
//	partial ≥1 running but ≥1 exited
//	unknown listing failed (set by the caller, never returned here)
//
// The state store records no expected-service list yet, so the comparison
// the briefing describes ("mind. 1 Service des Stacks fehlt") runs in its
// documented fallback form: running > 0 and no exited container suffice for
// ok, an exited container next to running ones means partial. States other
// than running/exited (created, paused, restarting) count as neither —
// conservative, they are transient by nature.
func driftFor(containers []compose.Container) string {
	running, exited := 0, 0
	for _, c := range containers {
		switch c.State {
		case "running":
			running++
		case "exited":
			exited++
		}
	}
	switch {
	case running == 0:
		return driftStopped
	case exited > 0:
		return driftPartial
	default:
		return driftOK
	}
}

// sortContainers returns the containers sorted by name; the input slice is
// copied so ps output order never leaks into the response and callers may
// share their fixtures.
func sortContainers(containers []compose.Container) []compose.Container {
	sorted := make([]compose.Container, len(containers))
	copy(sorted, containers)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return sorted
}

// resolveHeadCommit resolves host_repo.commit; every failure degrades to ""
// (the report never fails over missing git metadata).
func resolveHeadCommit(logger *slog.Logger, headCommit func() (string, error)) string {
	if headCommit == nil {
		return ""
	}
	commit, err := headCommit()
	if err != nil {
		logger.Warn("status: resolving host commit failed", "error", err)
		return ""
	}
	return commit
}

// statusAuthorized guards GET /status when a status token is set: the
// request must carry "Authorization: Bearer <token>", compared in constant
// time. Without a token the endpoint is open, like /healthz.
func (h *Handler) statusAuthorized(r *http.Request) bool {
	if h.statusToken == "" {
		return true
	}
	header := r.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(header, bearerPrefix)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(rest), []byte(h.statusToken)) == 1
}
