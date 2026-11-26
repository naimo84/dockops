package env

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/local/dockops/internal/config"
)

// TARGET_HOSTNAME is injected for opted-in stacks at the lowest priority.
func TestMergeInjectsTargetHostname(t *testing.T) {
	baseDir := t.TempDir()
	stack := config.Stack{Name: "traefik", Dir: baseDir, InjectHostname: true, TargetHostname: "hvaler"}
	got, err := MergeStackEnv(context.Background(), stack, &fakeResolver{}, baseDir, SystemVars{})
	if err != nil {
		t.Fatalf("MergeStackEnv() error = %v", err)
	}
	if got[TargetHostnameVar] != "hvaler" {
		t.Errorf("TARGET_HOSTNAME = %q, want hvaler", got[TargetHostnameVar])
	}

	// An explicit env_files value wins.
	writeFile(t, filepath.Join(baseDir, "host.env"), "TARGET_HOSTNAME=override\n")
	stack.EnvFiles = []string{"host.env"}
	got, err = MergeStackEnv(context.Background(), stack, &fakeResolver{}, baseDir, SystemVars{})
	if err != nil {
		t.Fatalf("MergeStackEnv() error = %v", err)
	}
	if got[TargetHostnameVar] != "override" {
		t.Errorf("TARGET_HOSTNAME = %q, want the env_files value", got[TargetHostnameVar])
	}
}

// Without opt-in nothing is injected.
func TestMergeNoTargetHostnameWithoutOptIn(t *testing.T) {
	baseDir := t.TempDir()
	stack := config.Stack{Name: "web", Dir: baseDir, TargetHostname: "hvaler"}
	got, err := MergeStackEnv(context.Background(), stack, &fakeResolver{}, baseDir, SystemVars{})
	if err != nil {
		t.Fatalf("MergeStackEnv() error = %v", err)
	}
	if _, ok := got[TargetHostnameVar]; ok {
		t.Errorf("TARGET_HOSTNAME set without inject_hostname: %q", got[TargetHostnameVar])
	}
}
