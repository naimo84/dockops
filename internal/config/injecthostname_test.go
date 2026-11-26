package config

import (
	"strings"
	"testing"
)

// loadHostnameImport loads a root with the given global body lines and one
// import of a remote stack whose stack.yaml is stackYAML.
func loadHostnameImport(t *testing.T, globalExtra, stackYAML, importExtra string) *Config {
	t.Helper()
	t.Setenv(EnvHostName, "")
	rootFile, _, workDir := newRepo(t)
	writeRemoteStack(t, workDir, "traefik", stackYAML, "compose.yaml")
	root := "global:\n  work_dir: \"" + workDir + "\"\n" + globalExtra +
		"remotes:\n  " + testRemote + ":\n    url: https://example.com/infra.git\n" +
		"imports:\n  - remote: " + testRemote + "\n    path: traefik\n" + importExtra
	writeFile(t, rootFile, root)
	cfg, err := LoadLenient(rootFile)
	if err != nil {
		t.Fatalf("LoadLenient() error = %v", err)
	}
	return cfg
}

// The base stack opts in, the host names itself: the merged stack carries
// the resolved value.
func TestInjectHostnameResolved(t *testing.T) {
	cfg := loadHostnameImport(t, "  host_name: hvaler\n", "inject_hostname: true\n", "")
	if len(cfg.StackErrors) != 0 {
		t.Fatalf("StackErrors = %v", cfg.StackErrors)
	}
	s := cfg.Stacks[0]
	if !s.InjectHostname || s.TargetHostname != "hvaler" {
		t.Errorf("InjectHostname=%v TargetHostname=%q, want true/hvaler", s.InjectHostname, s.TargetHostname)
	}
}

// The import alone can switch it on.
func TestInjectHostnameFromImport(t *testing.T) {
	cfg := loadHostnameImport(t, "  host_name: omv\n", "name: traefik\n", "    inject_hostname: true\n")
	if len(cfg.StackErrors) != 0 {
		t.Fatalf("StackErrors = %v", cfg.StackErrors)
	}
	if got := cfg.Stacks[0].TargetHostname; got != "omv" {
		t.Errorf("TargetHostname = %q, want omv", got)
	}
}

// Without opt-in the host name stays out of the stack.
func TestInjectHostnameOffLeavesEmpty(t *testing.T) {
	cfg := loadHostnameImport(t, "  host_name: hvaler\n", "name: traefik\n", "")
	if got := cfg.Stacks[0].TargetHostname; got != "" {
		t.Errorf("TargetHostname = %q, want empty without inject_hostname", got)
	}
}

// Opt-in without a host name is a stack error, never an empty variable.
func TestInjectHostnameWithoutHostNameIsStackError(t *testing.T) {
	cfg := loadHostnameImport(t, "", "inject_hostname: true\n", "")
	if len(cfg.Stacks) != 0 || len(cfg.StackErrors) != 1 {
		t.Fatalf("Stacks=%d StackErrors=%v, want the stack isolated as an error", len(cfg.Stacks), cfg.StackErrors)
	}
	if !strings.Contains(cfg.StackErrors[0].Error(), "global.host_name") {
		t.Errorf("error %q does not name global.host_name", cfg.StackErrors[0])
	}
}

// DOCKOPS_HOST_NAME fills a missing YAML value; YAML wins.
func TestHostNameEnvFallback(t *testing.T) {
	rootFile, _, _ := newRepo(t)
	t.Setenv(EnvHostName, "from-env")
	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Global.HostName != "from-env" {
		t.Errorf("HostName = %q, want from-env", cfg.Global.HostName)
	}
	writeFile(t, rootFile, "global:\n  host_name: from-yaml\n")
	cfg, err = Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Global.HostName != "from-yaml" {
		t.Errorf("HostName = %q, want from-yaml", cfg.Global.HostName)
	}
}

func TestHostNameValidation(t *testing.T) {
	t.Setenv(EnvHostName, "")
	for _, bad := range []string{"Hvaler", "hv aler", "-x", "x.", "a_b"} {
		rootFile, _, _ := newRepo(t)
		writeFile(t, rootFile, "global:\n  host_name: \""+bad+"\"\n")
		if _, err := Load(rootFile); err == nil || !strings.Contains(err.Error(), "global.host_name") {
			t.Errorf("host_name %q: err = %v, want a global.host_name error", bad, err)
		}
	}
	for _, good := range []string{"hvaler", "netcup2", "a.b-c"} {
		rootFile, _, _ := newRepo(t)
		writeFile(t, rootFile, "global:\n  host_name: "+good+"\n")
		if _, err := Load(rootFile); err != nil {
			t.Errorf("host_name %q: Load() error = %v", good, err)
		}
	}
}
