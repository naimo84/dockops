package config

import (
	"os"
	"strings"
	"testing"
)

// ptr returns a pointer to s: global.env_file.mode is a *string so that an
// absent key (default) and an explicitly empty value (error) differ.
func ptr(s string) *string { return &s }

// loadEnvFileRoot loads a root stacks.yaml with the given global block body
// and returns the merged configuration.
func loadEnvFileRoot(t *testing.T, body string) (*Config, error) {
	t.Helper()
	t.Setenv(EnvFileMode, "")
	t.Setenv(EnvFileGroup, "")
	rootFile, _, _ := newRepo(t)
	writeFile(t, rootFile, body)
	return Load(rootFile)
}

func TestPhase14EnvFileModeValid(t *testing.T) {
	cases := []struct {
		raw  string
		want os.FileMode
	}{
		{"640", 0o640},
		{"0640", 0o640},
		{"0600", 0o600},
		{"600", 0o600},
		{"0440", 0o440},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := loadEnvFileRoot(t, "global:\n  env_file:\n    mode: \""+tc.raw+"\"\n")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Global.EnvFile.Mode == nil {
				t.Fatal("global.env_file.mode is nil after loading")
			}
			if got := *cfg.Global.EnvFile.Mode; got != tc.raw {
				t.Errorf("mode = %q, want %q (the configured literal)", got, tc.raw)
			}
			if got := cfg.Global.EnvFile.FileMode(); got != tc.want {
				t.Errorf("FileMode() = %04o, want %04o", got, tc.want)
			}
		})
	}
}

func TestPhase14EnvFileModeInvalid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		// want names a substring the error must carry besides the field.
		want string
	}{
		{"not octal", "64x", `"64x"`},
		{"empty", "", "must not be empty"},
		{"world readable", "0644", "world-readable"},
		{"world readable other", "0604", "world-readable"},
		{"setuid", "04640", "setuid"},
		{"sticky world readable", "01777", "setuid, setgid and sticky"},
		{"owner cannot read", "0200", "owner must be able to read"},
		{"decimal looking", "0680", `"0680"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadEnvFileRoot(t, "global:\n  env_file:\n    mode: \""+tc.raw+"\"\n")
			if err == nil {
				t.Fatalf("Load() with mode %q succeeded, want a configuration error", tc.raw)
			}
			msg := err.Error()
			if !strings.Contains(msg, "global.env_file.mode") {
				t.Errorf("error %q does not name the field global.env_file.mode", msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not contain %q", msg, tc.want)
			}
		})
	}
}

func TestPhase14EnvFileDefaults(t *testing.T) {
	// No block at all: the historical artefact — 0600, group untouched.
	cfg, err := loadEnvFileRoot(t, "global: {}\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Global.EnvFile.Mode == nil {
		t.Fatal("global.env_file.mode is nil, want the 0600 default")
	}
	if got := *cfg.Global.EnvFile.Mode; got != "0600" {
		t.Errorf("mode = %q, want %q", got, "0600")
	}
	if got := cfg.Global.EnvFile.FileMode(); got != DefaultEnvFileMode {
		t.Errorf("FileMode() = %04o, want %04o", got, DefaultEnvFileMode)
	}
	if cfg.Global.EnvFile.Group != "" {
		t.Errorf("group = %q, want empty (do not touch the group)", cfg.Global.EnvFile.Group)
	}

	// A group without a mode still gets the default mode.
	cfg, err = loadEnvFileRoot(t, "global:\n  env_file:\n    group: docker\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := *cfg.Global.EnvFile.Mode; got != "0600" {
		t.Errorf("mode = %q with only a group configured, want %q", got, "0600")
	}
	if got := cfg.Global.EnvFile.Group; got != "docker" {
		t.Errorf("group = %q, want %q", got, "docker")
	}
}

func TestPhase14EnvFileEnvFallback(t *testing.T) {
	// The variables fill a missing block (no YAML value to win here).
	t.Setenv(EnvFileMode, "640")
	t.Setenv(EnvFileGroup, "docker")
	rootFile, _, _ := newRepo(t)
	writeFile(t, rootFile, "global: {}\n")
	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Global.EnvFile.Mode == nil || *cfg.Global.EnvFile.Mode != "640" {
		t.Errorf("mode = %v, want the env fallback %q", cfg.Global.EnvFile.Mode, "640")
	}
	if got := cfg.Global.EnvFile.FileMode(); got != 0o640 {
		t.Errorf("FileMode() = %04o, want 0640", got)
	}
	if got := cfg.Global.EnvFile.Group; got != "docker" {
		t.Errorf("group = %q, want the env fallback %q", got, "docker")
	}

	// An explicit YAML value wins over both variables.
	writeFile(t, rootFile, "global:\n  env_file:\n    mode: \"0600\"\n    group: staff\n")
	cfg, err = Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Global.EnvFile.Mode == nil || *cfg.Global.EnvFile.Mode != "0600" {
		t.Errorf("mode = %v, want the YAML value %q", cfg.Global.EnvFile.Mode, "0600")
	}
	if got := cfg.Global.EnvFile.Group; got != "staff" {
		t.Errorf("group = %q, want the YAML value %q", got, "staff")
	}
}

func TestPhase14EnvFileEnvFallbackIsValidated(t *testing.T) {
	// The env value goes through the same validation as a YAML value, with
	// the same field context.
	t.Setenv(EnvFileMode, "0644")
	t.Setenv(EnvFileGroup, "")
	rootFile, _, _ := newRepo(t)
	writeFile(t, rootFile, "global: {}\n")
	_, err := Load(rootFile)
	if err == nil {
		t.Fatal("Load() with DOCKOPS_ENV_FILE_MODE=0644 succeeded, want an error")
	}
	if msg := err.Error(); !strings.Contains(msg, "global.env_file.mode") || !strings.Contains(msg, "world-readable") {
		t.Errorf("error %q does not name field and reason", msg)
	}
}

func TestPhase14EnvFileUnknownKey(t *testing.T) {
	// Strict decoding: a typo in the block is a load error, not a silently
	// ignored setting.
	_, err := loadEnvFileRoot(t, "global:\n  env_file:\n    modus: \"0640\"\n")
	if err == nil {
		t.Fatal("Load() with an unknown env_file key succeeded, want an error")
	}
	if msg := err.Error(); !strings.Contains(msg, "modus") {
		t.Errorf("error %q does not name the unknown key", msg)
	}
}

func TestPhase14EnvFileFileModeFallback(t *testing.T) {
	// A hand-built Global never passes the loader: an unset or unparsable
	// mode degrades to the historical 0600 instead of an unreadable file.
	if got := (EnvFile{}).FileMode(); got != 0o600 {
		t.Errorf("EnvFile{}.FileMode() = %04o, want 0600", got)
	}
	if got := (EnvFile{Mode: ptr("nope")}).FileMode(); got != 0o600 {
		t.Errorf("EnvFile{nope}.FileMode() = %04o, want 0600", got)
	}
	if got := (EnvFile{Mode: ptr("640")}).FileMode(); got != 0o640 {
		t.Errorf("EnvFile{640}.FileMode() = %04o, want 0640", got)
	}
}
