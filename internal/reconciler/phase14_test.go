package reconciler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/env"
)

// p14DiscardLogger keeps the reconciler's per-stack logging out of the test
// output.
func p14DiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// p14StackEnvStat returns mode and gid of the generated <stackDir>/.env.
func p14StackEnvStat(t *testing.T, stackDir string) (os.FileMode, uint32) {
	t.Helper()
	path := filepath.Join(stackDir, ".env")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat generated stack env %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: no syscall.Stat_t", path)
	}
	return info.Mode().Perm(), st.Gid
}

// TestPhase14DeployAppliesGlobalEnvFile pins the whole wiring: a deploy loads
// global.env_file and the artefact carries exactly that mode and group.
func TestPhase14DeployAppliesGlobalEnvFile(t *testing.T) {
	gid := strconv.Itoa(os.Getegid())
	f := newFixture(t, `global:
  work_dir: WORKDIR
  env_file:
    mode: "0640"
    group: "`+gid+`"
`)
	f.addLocalStack("web", "name: web\n")

	if err := f.reconciler().Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// The deployment artefact of the stack dir carries the configured
	// permissions — the mode the ops user needs, the group he is a member of.
	mode, got := p14StackEnvStat(t, filepath.Join(f.workDir, "host", "web"))
	if mode != 0o640 {
		t.Errorf("mode = %04o, want 0640 from global.env_file", mode)
	}
	if got != uint32(os.Getegid()) {
		t.Errorf("gid = %d, want the configured %s", got, gid)
	}
}

// TestPhase14DeployWithoutBlockStays0600 is the regression side of the same
// wiring: no env_file block, unchanged artefact.
func TestPhase14DeployWithoutBlockStays0600(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")

	if err := f.reconciler().Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	mode, gid := p14StackEnvStat(t, filepath.Join(f.workDir, "host", "web"))
	if mode != 0o600 {
		t.Errorf("mode = %04o, want 0600 without an env_file block", mode)
	}
	if gid != uint32(os.Getegid()) {
		t.Errorf("gid = %d, want the agent's egid %d", gid, os.Getegid())
	}
}

// TestPhase14CLIStyleOptionsFromConfig mirrors cmd/dockops: the CLI keeps a
// pointer and fills it from every loaded configuration, so the options reach
// the deployment of the same pass (the callback runs before the deploy loop).
func TestPhase14CLIStyleOptionsFromConfig(t *testing.T) {
	f := newFixture(t, `global:
  work_dir: WORKDIR
  env_file:
    mode: "0640"
`)
	f.addLocalStack("web", "name: web\n")

	opts := &env.Options{}
	r := New(Deps{
		RepoURL:        "https://example.invalid/host.git",
		Branch:         "main",
		WorkDir:        f.workDir,
		State:          f.state,
		Logger:         p14DiscardLogger(),
		Git:            f.git,
		Compose:        f.compose,
		Hooks:          f.hooks,
		Sops:           f.sops.decrypt,
		NewVault:       f.vault.make,
		EnvFileOptions: opts,
		OnConfigLoaded: func(cfg *config.Config) {
			*opts = env.OptionsFromEnvFile(cfg.Global.EnvFile)
		},
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if opts.Mode != 0o640 {
		t.Errorf("callback filled mode %04o, want 0640", opts.Mode)
	}
	if mode, _ := p14StackEnvStat(t, filepath.Join(f.workDir, "host", "web")); mode != 0o640 {
		t.Errorf("mode = %04o, want the callback's 0640", mode)
	}
}

// TestPhase14ExplicitOptionsWinOverConfig: a caller that pins the options
// (tests, embedders) is not overruled by the configuration.
func TestPhase14ExplicitOptionsWinOverConfig(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")

	r := New(Deps{
		RepoURL:        "https://example.invalid/host.git",
		Branch:         "main",
		WorkDir:        f.workDir,
		State:          f.state,
		Logger:         p14DiscardLogger(),
		Git:            f.git,
		Compose:        f.compose,
		Hooks:          f.hooks,
		Sops:           f.sops.decrypt,
		NewVault:       f.vault.make,
		EnvFileOptions: &env.Options{Mode: 0o640},
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if mode, _ := p14StackEnvStat(t, filepath.Join(f.workDir, "host", "web")); mode != 0o640 {
		t.Errorf("mode = %04o, want the pinned 0640", mode)
	}
}
