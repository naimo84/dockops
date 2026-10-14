// Package hooks runs the pre- and post-deploy shell commands of a stack
// sequentially via sh -c, with per-command timeouts and output relayed to
// the structured logger.
package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// defaultTimeout is the fallback when Run is called with a non-positive
// timeout. The value mirrors the global default (5m) from the briefing;
// callers pass their configured value through Run's timeout parameter.
const defaultTimeout = 5 * time.Minute

// Engine executes hook commands.
type Engine struct {
	logger *slog.Logger
}

// New creates an Engine; a nil logger falls back to slog.Default().
func New(logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{logger: logger}
}

// Run executes commands sequentially, each as `sh -c <command>` in dir with
// env (KEY=VALUE entries) appended to the process environment. timeout is
// per command; a non-positive value falls back to defaultTimeout.
//
// stdout and stderr of every command are relayed to the logger at Info
// level, line by line, with the command as an attribute. A command failing
// with a non-zero exit code ("hook failed: exit code N") or exceeding its
// timeout ("hook timed out after ...") aborts the sequence — later commands
// do not run — and is returned as the error.
func (e *Engine) Run(ctx context.Context, commands []string, dir string, env []string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	for _, command := range commands {
		if err := e.runOne(ctx, command, dir, env, timeout); err != nil {
			return err
		}
	}
	return nil
}

// runOne executes a single hook command with its own timeout.
func (e *Engine) runOne(ctx context.Context, command, dir string, env []string, timeout time.Duration) error {
	// The per-command context is derived from the caller's, so cancelling
	// the caller aborts the sequence as well.
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	// Each hook runs in its own process group, and a timeout kills the
	// whole group: `sh -c` forks, so killing only sh would leave
	// grandchildren running while they keep the output pipes open — Wait
	// would then block until they exit (observed: a 100ms timeout took the
	// full runtime of a `sleep 30` grandchild). Platform note: setpgid
	// keeps this package Unix-only, which matches the deployment target.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid signals the entire process group; ESRCH means it is
		// already gone.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if err == syscall.ESRCH {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	stdout := &lineWriter{logger: e.logger, hook: command}
	stderr := &lineWriter{logger: e.logger, hook: command}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	e.logger.Info("hook started", "hook", command)
	err := cmd.Run()
	stdout.Close()
	stderr.Close()

	if cmdCtx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("hook %q timed out after %s", command, timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("hook %q failed: exit code %d", command, exitErr.ExitCode())
		}
		return fmt.Errorf("hook %q: %w", command, err)
	}
	e.logger.Info("hook finished", "hook", command)
	return nil
}

// lineWriter logs every complete line written to it at Info level with the
// originating hook as an attribute. It buffers a trailing partial line until
// the next write or Close, so output without a final newline is not lost.
type lineWriter struct {
	logger  *slog.Logger
	hook    string
	partial []byte
}

// Write implements io.Writer.
func (w *lineWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.partial[:i]), "\r")
		w.partial = w.partial[i+1:]
		if line != "" {
			w.logger.Info(line, "hook", w.hook)
		}
	}
	return len(p), nil
}

// Close flushes a buffered partial line.
func (w *lineWriter) Close() {
	if len(w.partial) == 0 {
		return
	}
	if line := strings.TrimSpace(string(w.partial)); line != "" {
		w.logger.Info(line, "hook", w.hook)
	}
	w.partial = nil
}
