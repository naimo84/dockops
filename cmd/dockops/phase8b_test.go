// Phase-8b regression tests: the -print-status flag and the one-shot
// document writer it drives.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/state"
	"github.com/local/dockops/internal/webhook"
)

// discardLogger swallows the degradation warnings writeStatus logs.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeLister implements webhook.ComposeLister with one canned host
// inventory, so the one-shot path is testable without docker.
type fakeLister struct {
	infos    []compose.ContainerInfo
	infosErr error
}

func (f *fakeLister) PsContainers(_ context.Context) ([]compose.ContainerInfo, error) {
	return f.infos, f.infosErr
}

func TestParseFlagsPrintStatus(t *testing.T) {
	// Default off.
	opts, err := parseFlags([]string{"-repo", "u"}, fakeEnv(nil))
	if err != nil {
		t.Fatalf("parseFlags() error = %v", err)
	}
	if opts.PrintStatus {
		t.Errorf("PrintStatus = true, want false by default")
	}

	// Env fallback.
	opts, err = parseFlags([]string{"-repo", "u"}, fakeEnv(map[string]string{
		envPrintStatus: "true",
	}))
	if err != nil {
		t.Fatalf("parseFlags() error = %v", err)
	}
	if !opts.PrintStatus {
		t.Errorf("PrintStatus = false, want env value true")
	}

	// Flag wins over env.
	opts, err = parseFlags([]string{"-print-status"}, fakeEnv(map[string]string{
		envPrintStatus: "false",
	}))
	if err != nil {
		t.Fatalf("parseFlags() error = %v", err)
	}
	if !opts.PrintStatus {
		t.Errorf("PrintStatus = false, want flag value true")
	}

	// Invalid env value: configuration error naming the variable.
	_, err = parseFlags([]string{"-repo", "u"}, fakeEnv(map[string]string{
		envPrintStatus: "yes",
	}))
	if err == nil || !strings.Contains(err.Error(), envPrintStatus) {
		t.Errorf("parseFlags() with invalid env error = %v, want error naming %s", err, envPrintStatus)
	}
}

func TestParseFlagsPrintStatusWithoutRepo(t *testing.T) {
	// -print-status needs no host repository: the document is built from
	// the state file and docker alone.
	opts, err := parseFlags([]string{"-print-status"}, fakeEnv(nil))
	if err != nil {
		t.Fatalf("parseFlags(-print-status without repo) error = %v", err)
	}
	if !opts.PrintStatus {
		t.Errorf("PrintStatus = false, want true")
	}

	// Without -print-status the repo requirement is intact.
	if _, err := parseFlags([]string{"-once"}, fakeEnv(nil)); err == nil {
		t.Errorf("parseFlags(-once without repo) error = nil, want missing-repo error")
	}
}

// TestWriteStatus exercises the one-shot writer end to end: a state file, a
// fake docker surface with both failure modes, and the output contract —
// two-space indentation, trailing newline, docker failures inside the
// document instead of a failed call.
func TestWriteStatus(t *testing.T) {
	deployed := time.Date(2026, 10, 2, 8, 30, 0, 0, time.UTC)
	store, err := state.New(filepath.Join(t.TempDir(), "state.json"), discardLogger())
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	if err := store.Set("alpha", state.State{Hash: "h-alpha", Status: state.StatusDeployed, UpdatedAt: deployed}); err != nil {
		t.Fatalf("state.Set(alpha) error = %v", err)
	}
	if err := store.Set("beta", state.State{Hash: "h-beta", Status: state.StatusFailed, UpdatedAt: deployed}); err != nil {
		t.Fatalf("state.Set(beta) error = %v", err)
	}

	fl := &fakeLister{
		infos: []compose.ContainerInfo{
			{Project: "alpha", Name: "alpha-1", Service: "web", State: "running"},
			{Project: "beta", Name: "beta-1", Service: "db", State: "exited"},
			{Project: "legacy", Name: "legacy-1", State: "running"},
		},
	}

	var buf bytes.Buffer
	if err := writeStatus(context.Background(), webhook.StatusDeps{
		State:       store,
		Compose:     fl,
		RepoURL:     "git@example.com:homelab/host.git",
		Branch:      "main",
		HeadCommit:  nil, // no git metadata: degrades to an empty commit
		SelfProject: "dockops",
		Logger:      discardLogger(),
	}, &buf); err != nil {
		t.Fatalf("writeStatus() error = %v, want nil (docker failures live in the document)", err)
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output does not end with a newline: %q", out)
	}
	if !strings.HasPrefix(out, "{\n  \"generated_at\"") {
		t.Errorf("output does not start with two-space indented JSON: %q", out[:min(len(out), 40)])
	}

	// Structure: alpha ok with hash and timestamp, beta stopped, unmanaged
	// from the same inventory.
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode writeStatus output %q: %v", out, err)
	}
	if arr, _ := doc["stacks"].([]any); len(arr) != 2 {
		t.Fatalf("stacks = %v, want 2 entries (alpha, beta)", doc["stacks"])
	}
	stacks, _ := doc["stacks"].([]any)
	alpha, _ := stacks[0].(map[string]any)
	if alpha["name"] != "alpha" || alpha["drift"] != "ok" {
		t.Errorf("stacks[0] = %v, want alpha/ok", alpha)
	}
	if alpha["deployed_hash"] != "h-alpha" || alpha["last_deployed"] != "2026-10-02T08:30:00Z" {
		t.Errorf("alpha = %v, want hash and RFC3339 timestamp from the state entry", alpha)
	}
	beta, _ := stacks[1].(map[string]any)
	if beta["drift"] != "stopped" {
		t.Errorf("beta drift = %v, want stopped (all containers exited)", beta["drift"])
	}
	unmanaged, _ := doc["unmanaged"].([]any)
	if len(unmanaged) != 1 {
		t.Fatalf("unmanaged = %v, want 1 entry (legacy)", doc["unmanaged"])
	}
	legacy, _ := unmanaged[0].(map[string]any)
	if legacy["project"] != "legacy" {
		t.Errorf("unmanaged[0].project = %v, want legacy", legacy["project"])
	}
	hostRepo, _ := doc["host_repo"].(map[string]any)
	if hostRepo["commit"] != "" {
		t.Errorf("host_repo.commit = %v, want empty string without a HeadCommit callback", hostRepo["commit"])
	}
}

// TestWriteStatusListingError: the single docker call fails host-wide, so the
// document — not the exit code — carries the damage: every stack unknown,
// top-level error set, output still parseable.
func TestWriteStatusListingError(t *testing.T) {
	store, err := state.New(filepath.Join(t.TempDir(), "state.json"), discardLogger())
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	if err := store.Set("alpha", state.State{Hash: "h-alpha", Status: state.StatusDeployed}); err != nil {
		t.Fatalf("state.Set(alpha) error = %v", err)
	}

	var buf bytes.Buffer
	if err := writeStatus(context.Background(), webhook.StatusDeps{
		State:   store,
		Compose: &fakeLister{infosErr: errors.New("docker ps blew up")},
		Logger:  discardLogger(),
	}, &buf); err != nil {
		t.Fatalf("writeStatus() error = %v, want nil (docker failures live in the document)", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("decode writeStatus output %q: %v", buf.String(), err)
	}
	if msg, _ := doc["error"].(string); !strings.Contains(msg, "docker ps blew up") {
		t.Errorf("top-level error = %v, want the listing failure", doc["error"])
	}
	stacks, _ := doc["stacks"].([]any)
	if len(stacks) != 1 {
		t.Fatalf("stacks = %v, want 1 entry", doc["stacks"])
	}
	alpha, _ := stacks[0].(map[string]any)
	if alpha["drift"] != "unknown" {
		t.Errorf("alpha drift = %v, want unknown", alpha["drift"])
	}
	if msg, _ := alpha["error"].(string); !strings.Contains(msg, "docker ps blew up") {
		t.Errorf("alpha error = %v, want the listing failure", alpha["error"])
	}
}

// TestWriteStatusRequiresCollaborators: missing configuration fails before
// anything is written.
func TestWriteStatusRequiresCollaborators(t *testing.T) {
	var buf bytes.Buffer
	if err := writeStatus(context.Background(), webhook.StatusDeps{Compose: &fakeLister{}}, &buf); err == nil {
		t.Errorf("writeStatus() without state store error = nil, want error")
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written on error", buf.String())
	}
}
