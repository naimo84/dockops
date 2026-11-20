package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/local/dockops/internal/config"
)

// hangingDocker installs a fake docker that never answers on its own.
func hangingDocker(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A docker call that never returns would stop the single-threaded reconcile
// loop for good; every call has a deadline and says so when it hits it.
func TestDockerCallsHaveDeadlines(t *testing.T) {
	hangingDocker(t)
	e := quietExecutor()
	e.readTimeout = 100 * time.Millisecond
	e.changeTimeout = 100 * time.Millisecond

	start := time.Now()
	_, err := e.PsProject(context.Background(), "web")
	if err == nil || !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Errorf("PsProject() error = %v, want a timeout", err)
	}
	_, err = e.ImageDigest(context.Background(), "nginx:1")
	if err == nil || !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Errorf("ImageDigest() error = %v, want a timeout", err)
	}
	err = e.DownProject(context.Background(), "web")
	if err == nil || !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Errorf("DownProject() error = %v, want a timeout", err)
	}
	stackDir := t.TempDir()
	writeStackFile(t, stackDir, "compose.yaml", "services:\n  app:\n    image: alpine\n")
	err = e.Up(context.Background(), config.Stack{Name: "web", ComposeFile: "compose.yaml"}, stackDir, filepath.Join(stackDir, "x.env"))
	if err == nil || !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Errorf("Up() error = %v, want a timeout", err)
	}
	// Four hung calls, each bounded by its deadline plus the wait delay.
	if elapsed := time.Since(start); elapsed > 4*(100*time.Millisecond+commandWaitDelay) {
		t.Errorf("hung calls took %s", elapsed)
	}
}

// A cancelled caller context is reported as such, not as a timeout.
func TestDockerCallCancelledIsNotATimeout(t *testing.T) {
	hangingDocker(t)
	e := quietExecutor()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := e.PsProject(ctx, "web")
	if err == nil || strings.Contains(err.Error(), "timed out") {
		t.Errorf("PsProject() with a cancelled context error = %v, want a plain failure", err)
	}
}
