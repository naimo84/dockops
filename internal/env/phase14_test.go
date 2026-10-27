package env

import (
	"bytes"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/local/dockops/internal/config"
)

// captureLog redirects the default logger into a buffer for the duration of
// the test: the phase-14 warnings are the diagnosis of an unusable group, so
// they are part of the contract. slog's default logger is global; the tests
// of this package run sequentially, so the swap cannot overlap.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// statGID returns the owning group of path.
func statGID(t *testing.T, path string) uint32 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: no syscall.Stat_t (platform without unix gids?)", path)
	}
	return st.Gid
}

// assertNoTempLeftovers fails when the stack dir holds anything but .env —
// the permissions are applied before the rename, so no intermediate file may
// remain.
func assertNoTempLeftovers(t *testing.T, stackDir string) {
	t.Helper()
	entries, err := os.ReadDir(stackDir)
	if err != nil {
		t.Fatalf("read dir %s: %v", stackDir, err)
	}
	if len(entries) != 1 || entries[0].Name() != ".env" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("stack dir contains %v, want exactly [.env]", names)
	}
}

func TestPhase14WriteStackEnvMode(t *testing.T) {
	stackDir := t.TempDir()
	env := map[string]string{"SECRET": "hunter2", "A": "1"}

	path, rollback, err := WriteStackEnv(stackDir, env, Options{Mode: 0o640})
	if err != nil {
		t.Fatalf("WriteStackEnv() error = %v", err)
	}
	_ = rollback

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("permissions = %04o, want 0640", perm)
	}
	// The mode must not change the content: same deterministic dotenv format.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if want := "A=1\nSECRET=hunter2\n"; string(raw) != want {
		t.Errorf("content = %q, want %q", raw, want)
	}
	assertNoTempLeftovers(t, stackDir)
}

func TestPhase14WriteStackEnvDefaultStays0600(t *testing.T) {
	stackDir := t.TempDir()
	path, rollback, err := WriteStackEnv(stackDir, map[string]string{"A": "1"}, Options{})
	if err != nil {
		t.Fatalf("WriteStackEnv() error = %v", err)
	}
	_ = rollback

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %04o, want 0600 (zero Options)", perm)
	}
	if gid := statGID(t, path); gid != uint32(os.Getegid()) {
		t.Errorf("gid = %d, want the agent's egid %d", gid, os.Getegid())
	}
}

func TestPhase14WriteStackEnvNumericGroup(t *testing.T) {
	// The agent's own egid needs no membership beyond itself, so the chgrp
	// cannot fail with EPERM here.
	stackDir := t.TempDir()
	path, _, err := WriteStackEnv(stackDir, map[string]string{"A": "1"},
		Options{Mode: 0o640, Group: strconv.Itoa(os.Getegid())})
	if err != nil {
		t.Fatalf("WriteStackEnv() error = %v", err)
	}
	if gid := statGID(t, path); gid != uint32(os.Getegid()) {
		t.Errorf("gid = %d, want %d", gid, os.Getegid())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("permissions = %04o, want 0640", perm)
	}
}

func TestPhase14WriteStackEnvGroupByName(t *testing.T) {
	// The name of the agent's own egid: resolved through os/user (pure Go,
	// /etc/group). Hosts (or containers) without that entry skip the case —
	// the resolution path itself is covered by the unknown-group test.
	g, err := user.LookupGroupId(strconv.Itoa(os.Getegid()))
	if err != nil {
		t.Skipf("own egid %d has no /etc/group entry: %v", os.Getegid(), err)
	}
	stackDir := t.TempDir()
	path, _, err := WriteStackEnv(stackDir, map[string]string{"A": "1"},
		Options{Group: g.Name})
	if err != nil {
		t.Fatalf("WriteStackEnv() error = %v", err)
	}
	if gid := statGID(t, path); gid != uint32(os.Getegid()) {
		t.Errorf("gid = %d, want the egid %d of group %q", gid, os.Getegid(), g.Name)
	}
}

func TestPhase14WriteStackEnvUnknownGroupWarns(t *testing.T) {
	logs := captureLog(t)
	stackDir := t.TempDir()
	const group = "dockops-no-such-group"

	path, _, err := WriteStackEnv(stackDir, map[string]string{"A": "1"},
		Options{Mode: 0o640, Group: group})
	if err != nil {
		t.Fatalf("WriteStackEnv() with an unknown group error = %v, want nil (warning only)", err)
	}
	// The file exists, carries the mode and keeps the agent's group: the
	// missing chgrp is a warning, never a missing artefact.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("permissions = %04o, want 0640", perm)
	}
	if gid := statGID(t, path); gid != uint32(os.Getegid()) {
		t.Errorf("gid = %d, want the unchanged egid %d", gid, os.Getegid())
	}
	assertNoTempLeftovers(t, stackDir)

	msg := logs.String()
	if !strings.Contains(msg, "cannot resolve stack env group") {
		t.Errorf("log %q does not warn about the unresolvable group", msg)
	}
	if !strings.Contains(msg, group) {
		t.Errorf("log %q does not name the configured group %q", msg, group)
	}
	if !strings.Contains(msg, path) {
		t.Errorf("log %q does not name the stack env path %q", msg, path)
	}
}

func TestPhase14WriteStackEnvChgrpRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chgrp to gid 0 cannot be refused")
	}
	if os.Getegid() == 0 {
		t.Skip("egid 0: the refused-group case cannot be constructed")
	}
	logs := captureLog(t)
	stackDir := t.TempDir()

	// gid 0 as a non-root agent: the chown is EPERM (the agent is no member
	// of root's group), which must cost a warning, not the deployment.
	path, _, err := WriteStackEnv(stackDir, map[string]string{"A": "1"},
		Options{Mode: 0o640, Group: "0"})
	if err != nil {
		t.Fatalf("WriteStackEnv() with a refused chgrp error = %v, want nil (warning only)", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("permissions = %04o, want 0640 (the mode must survive the failed chgrp)", perm)
	}
	if gid := statGID(t, path); gid == 0 {
		t.Errorf("gid = 0, want the unchanged egid %d", os.Getegid())
	}

	msg := logs.String()
	if !strings.Contains(msg, "cannot change stack env group") {
		t.Errorf("log %q does not warn about the refused chgrp", msg)
	}
	for _, want := range []string{"gid=0", strconv.Itoa(os.Getegid())} {
		if !strings.Contains(msg, want) {
			t.Errorf("log %q does not carry %q (both gids belong in the diagnosis)", msg, want)
		}
	}
}

func TestPhase14RollbackRestoresPreviousMode(t *testing.T) {
	// A failed deployment rolls the artefact back to the previous file —
	// including its narrower mode, not the one this attempt wrote.
	stackDir := t.TempDir()
	path := filepath.Join(stackDir, ".env")
	if err := os.WriteFile(path, []byte("OLD=1\n"), 0o600); err != nil {
		t.Fatalf("write previous .env: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod previous .env: %v", err)
	}

	gotPath, rollback, err := WriteStackEnv(stackDir, map[string]string{"NEW": "2"}, Options{Mode: 0o640})
	if err != nil {
		t.Fatalf("WriteStackEnv() error = %v", err)
	}
	if gotPath != path {
		t.Fatalf("path = %q, want %q", gotPath, path)
	}
	if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode before rollback = %v (%v), want 0640", info.Mode().Perm(), statErr)
	}

	rollback()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored %s: %v", path, err)
	}
	if want := "OLD=1\n"; string(raw) != want {
		t.Errorf("restored content = %q, want %q", raw, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("restored mode = %04o, want the previous 0600", perm)
	}
}

func TestPhase14OptionsFromEnvFile(t *testing.T) {
	mode := "640"
	opts := OptionsFromEnvFile(config.EnvFile{Mode: &mode, Group: "docker"})
	if opts.Mode != 0o640 {
		t.Errorf("Mode = %04o, want 0640", opts.Mode)
	}
	if opts.Group != "docker" {
		t.Errorf("Group = %q, want %q", opts.Group, "docker")
	}

	// An absent mode means the historical default.
	opts = OptionsFromEnvFile(config.EnvFile{})
	if opts.Mode != 0o600 {
		t.Errorf("Mode = %04o, want 0600 for an unset mode", opts.Mode)
	}
	if opts.Group != "" {
		t.Errorf("Group = %q, want empty", opts.Group)
	}
	if got := (Options{}).mode(); got != 0o600 {
		t.Errorf("zero Options mode() = %04o, want 0600", got)
	}
}
