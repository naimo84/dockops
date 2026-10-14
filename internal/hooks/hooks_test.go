package hooks

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureLogger returns an Engine logging into buf (level Info) so output
// relaying can be asserted.
func captureLogger(buf *bytes.Buffer) *Engine {
	return New(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
}

func quietEngine() *Engine {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRunEmptyCommands(t *testing.T) {
	e := quietEngine()
	if err := e.Run(context.Background(), nil, t.TempDir(), nil, time.Minute); err != nil {
		t.Errorf("Run(nil) error = %v, want nil", err)
	}
}

func TestRunSequentialSuccess(t *testing.T) {
	e := quietEngine()
	dir := t.TempDir()
	commands := []string{
		"echo one > order.txt",
		"echo two >> order.txt",
	}
	if err := e.Run(context.Background(), commands, dir, nil, time.Minute); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "order.txt"))
	if err != nil {
		t.Fatalf("read order.txt: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != "one\ntwo" {
		t.Errorf("order.txt = %q, want both commands executed in order", got)
	}
}

func TestRunStopsAfterFailure(t *testing.T) {
	e := quietEngine()
	dir := t.TempDir()
	commands := []string{
		"echo one > order.txt",
		"exit 3",
		"echo three >> order.txt", // must not run
	}
	err := e.Run(context.Background(), commands, dir, nil, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "failed: exit code 3") {
		t.Fatalf("Run() error = %v, want failed: exit code 3", err)
	}
	if !strings.Contains(err.Error(), "exit 3") {
		t.Errorf("error = %v, want the failing command in the error", err)
	}
	raw, readErr := os.ReadFile(filepath.Join(dir, "order.txt"))
	if readErr != nil {
		t.Fatalf("read order.txt: %v", readErr)
	}
	if got := strings.TrimSpace(string(raw)); got != "one" {
		t.Errorf("order.txt = %q, hooks ran after the failure", got)
	}
}

func TestRunTimeout(t *testing.T) {
	e := quietEngine()
	start := time.Now()
	err := e.Run(context.Background(), []string{"sleep 30"}, t.TempDir(), nil, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Fatalf("Run() error = %v, want timed out after 100ms", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %s to trigger, command was not killed", elapsed)
	}
}

func TestRunDefaultTimeoutUsedForZero(t *testing.T) {
	// A zero timeout falls back to the default; the quick command must
	// succeed rather than being killed instantly.
	e := quietEngine()
	if err := e.Run(context.Background(), []string{"true"}, t.TempDir(), nil, 0); err != nil {
		t.Errorf("Run() with zero timeout error = %v, want default timeout", err)
	}
}

func TestRunEnvPassedThrough(t *testing.T) {
	e := quietEngine()
	dir := t.TempDir()
	env := []string{"DOCKOPS_HOOK_MARKER=hello hooks"}
	command := "env > env.txt"
	if err := e.Run(context.Background(), []string{command}, dir, env, time.Minute); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatalf("read env.txt: %v", err)
	}
	if !strings.Contains(string(raw), "DOCKOPS_HOOK_MARKER=hello hooks") {
		t.Errorf("env.txt lacks DOCKOPS_HOOK_MARKER:\n%s", raw)
	}
}

func TestRunWorkingDir(t *testing.T) {
	e := quietEngine()
	dir := t.TempDir()
	if err := e.Run(context.Background(), []string{"pwd > cwd.txt"}, dir, nil, time.Minute); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "cwd.txt"))
	if err != nil {
		t.Fatalf("read cwd.txt: %v", err)
	}
	got := strings.TrimSpace(string(raw))
	// pwd may report a symlink-resolved path; compare both spellings.
	if got != dir {
		resolved, resErr := filepath.EvalSymlinks(dir)
		if resErr != nil || got != resolved {
			t.Errorf("pwd = %q, want %q", got, dir)
		}
	}
}

func TestRunOutputRelayedToLog(t *testing.T) {
	var buf bytes.Buffer
	e := captureLogger(&buf)
	dir := t.TempDir()
	command := "echo out-line; echo err-line >&2; printf 'partial-no-newline'"
	if err := e.Run(context.Background(), []string{command}, dir, nil, time.Minute); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	logged := buf.String()
	for _, want := range []string{"out-line", "err-line", "partial-no-newline", "hook started", "hook finished"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
	if !strings.Contains(logged, command) {
		t.Errorf("log lacks the hook attribute:\n%s", logged)
	}
}

func TestRunParentContextCancel(t *testing.T) {
	e := quietEngine()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := e.Run(ctx, []string{"true"}, t.TempDir(), nil, time.Minute)
	if err == nil {
		t.Error("Run() with cancelled context succeeded, want error")
	}
}
