package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// LoadLenient leaves broken stack definitions out — local and imported
// alike — and reports them; Load stays strict and fails on the first one.
func TestLoadLenientIsolatesBrokenStacks(t *testing.T) {
	rootFile, hostDir, workDir := newRepo(t)
	writeFile(t, rootFile, rootWithImports(workDir, "imports:\n  - remote: "+testRemote+"\n    path: good\n  - remote: "+testRemote+"\n    path: missing\n"))
	writeFile(t, filepath.Join(hostDir, "web", "stack.yaml"), "name: web\n")
	writeFile(t, filepath.Join(hostDir, "web", "compose.yaml"), "services: {}\n")
	writeFile(t, filepath.Join(hostDir, "typo", "stack.yaml"), "name: typo\nenabeld: true\n")
	writeFile(t, filepath.Join(hostDir, "typo", "compose.yaml"), "services: {}\n")
	writeRemoteStack(t, workDir, "good", "name: good\n", "compose.yaml")

	cfg, err := LoadLenient(rootFile)
	if err != nil {
		t.Fatalf("LoadLenient() error = %v", err)
	}
	names := make([]string, 0, len(cfg.Stacks))
	for _, s := range cfg.Stacks {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "web,good" {
		t.Errorf("stacks = %v, want web and good", names)
	}
	if len(cfg.StackErrors) != 2 {
		t.Fatalf("StackErrors = %v, want the typo and the missing import", cfg.StackErrors)
	}
	if got := cfg.StackErrors[0].Error(); !strings.Contains(got, filepath.Join("typo", "stack.yaml")) || !strings.Contains(got, "enabeld") {
		t.Errorf("first stack error = %q, want the file and the unknown field", got)
	}
	if got := cfg.StackErrors[1].Error(); !strings.Contains(got, `path="missing"`) {
		t.Errorf("second stack error = %q, want the import named", got)
	}

	if _, err := Load(rootFile); err == nil || !strings.Contains(err.Error(), "enabeld") {
		t.Errorf("Load() error = %v, want the first broken stack (strict)", err)
	}
}
