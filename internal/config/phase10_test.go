package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// loadRoot10 writes the given root YAML into a fresh host repository and
// loads it (no stacks beyond what the YAML declares).
func loadRoot10(t *testing.T, rootYAML string) (*Config, error) {
	t.Helper()
	rootFile, _, _ := newRepo(t)
	writeFile(t, rootFile, rootYAML)
	return Load(rootFile)
}

// loadStack10 writes a stack directory with the given stack.yaml plus a
// minimal compose file and loads the full configuration.
func loadStack10(t *testing.T, stackYAML string) (*Config, error) {
	t.Helper()
	rootFile, hostDir, _ := newRepo(t)
	dir := filepath.Join(hostDir, "web")
	writeFile(t, filepath.Join(dir, "compose.yaml"), "services:\n  app:\n    image: nginx:latest\n")
	writeFile(t, filepath.Join(dir, "stack.yaml"), stackYAML)
	return Load(rootFile)
}

func TestRegistriesAbsentMeansAnonymous(t *testing.T) {
	cfg, err := loadRoot10(t, "global: {}\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Registries != nil {
		t.Errorf("Registries = %+v, want nil without a registries block", cfg.Registries)
	}
}

func TestRegistriesEnvExpansion(t *testing.T) {
	t.Setenv("GITEA_REGISTRY_TOKEN", "s3cr3t-token")
	root := `global: {}
registries:
  - host: gitea.naimo84.dev:3000
    username: loki
    password: ${GITEA_REGISTRY_TOKEN}
  - host: registry.example.com
    username: ${REGISTRY_USER}
    password: ${REGISTRY_PASSWORD}
`
	t.Setenv("REGISTRY_USER", "robot")
	t.Setenv("REGISTRY_PASSWORD", "hunter2")

	cfg, err := loadRoot10(t, root)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []Registry{
		{Host: "gitea.naimo84.dev:3000", Username: "loki", Password: "s3cr3t-token"},
		{Host: "registry.example.com", Username: "robot", Password: "hunter2"},
	}
	if !reflect.DeepEqual(cfg.Registries, want) {
		t.Errorf("Registries = %+v, want %+v", cfg.Registries, want)
	}
}

func TestRegistriesMissingEnvVar(t *testing.T) {
	t.Setenv("GITEA_REGISTRY_TOKEN", "")
	root := `global: {}
registries:
  - host: gitea.naimo84.dev:3000
    username: loki
    password: ${GITEA_REGISTRY_TOKEN}
`
	_, err := loadRoot10(t, root)
	if err == nil {
		t.Fatal("Load() error = nil, want a missing-variable error")
	}
	for _, want := range []string{"registries[0].password", "GITEA_REGISTRY_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error = %q, want it to mention %q", err, want)
		}
	}
}

func TestRegistriesValidation(t *testing.T) {
	cases := []struct {
		name string
		root string
		want string
	}{
		{
			name: "missing host",
			root: "global: {}\nregistries:\n  - username: a\n    password: b\n",
			want: "registries[0].host",
		},
		{
			name: "scheme in host",
			root: "global: {}\nregistries:\n  - host: https://ghcr.io\n",
			want: "without scheme",
		},
		{
			name: "path in host",
			root: "global: {}\nregistries:\n  - host: ghcr.io/x\n",
			want: "without path",
		},
		{
			name: "password without username",
			root: "global: {}\nregistries:\n  - host: ghcr.io\n    password: b\n",
			want: "configured together",
		},
		{
			name: "duplicate host",
			root: "global: {}\nregistries:\n  - host: ghcr.io\n  - host: GHCR.io\n",
			want: "duplicate host",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadRoot10(t, tc.root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestUpdateCheckIntervalDefault(t *testing.T) {
	cfg, err := loadRoot10(t, "global: {}\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Global.UpdateCheckInterval == nil {
		t.Fatal("UpdateCheckInterval = nil, want the filled-in default")
	}
	if got := cfg.Global.EffectiveUpdateCheckInterval(); got != time.Hour {
		t.Errorf("EffectiveUpdateCheckInterval() = %v, want 1h", got)
	}
}

func TestUpdateCheckIntervalZeroDisables(t *testing.T) {
	cfg, err := loadRoot10(t, "global:\n  update_check_interval: 0\n")
	if err != nil {
		t.Fatalf("Load() error = %v, want 0 to be valid (emergency brake)", err)
	}
	if got := cfg.Global.EffectiveUpdateCheckInterval(); got != 0 {
		t.Errorf("EffectiveUpdateCheckInterval() = %v, want 0 (disabled)", got)
	}
}

func TestUpdateCheckIntervalExplicit(t *testing.T) {
	cfg, err := loadRoot10(t, "global:\n  update_check_interval: 2h30m\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Global.EffectiveUpdateCheckInterval(); got != 150*time.Minute {
		t.Errorf("EffectiveUpdateCheckInterval() = %v, want 2h30m", got)
	}
}

func TestUpdateCheckIntervalInvalid(t *testing.T) {
	if _, err := loadRoot10(t, "global:\n  update_check_interval: often\n"); err == nil {
		t.Error("Load() with an unparsable interval error = nil, want error")
	}
	if _, err := loadRoot10(t, "global:\n  update_check_interval: -5m\n"); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("Load() with a negative interval error = %v, want a negative-interval error", err)
	}
}

func TestUpdatePolicyDefault(t *testing.T) {
	cfg, err := loadStack10(t, "name: web\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Stacks[0].UpdatePolicy; got != UpdatePolicyNotify {
		t.Errorf("UpdatePolicy = %q, want %q", got, UpdatePolicyNotify)
	}
	if !cfg.Stacks[0].ChecksImageUpdates() {
		t.Error("ChecksImageUpdates() = false, want true for the default policy")
	}
}

func TestUpdatePolicyPinned(t *testing.T) {
	cfg, err := loadStack10(t, "name: web\nupdate_policy: pinned\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Stacks[0].UpdatePolicy; got != UpdatePolicyPinned {
		t.Errorf("UpdatePolicy = %q, want pinned", got)
	}
	if cfg.Stacks[0].ChecksImageUpdates() {
		t.Error("ChecksImageUpdates() = true, want false for pinned")
	}
}

func TestUpdatePolicyInvalid(t *testing.T) {
	// "apply" was the invalid example until phase 11 introduced it; an
	// unknown value must still fail with field and value in the message.
	_, err := loadStack10(t, "name: web\nupdate_policy: auto\n")
	if err == nil {
		t.Fatal("Load() error = nil, want a validation error for update_policy")
	}
	if !strings.Contains(err.Error(), "update_policy") || !strings.Contains(err.Error(), "auto") {
		t.Errorf("Load() error = %q, want field and value in the message", err)
	}
}

func TestNotificationsUpdateAvailableEvent(t *testing.T) {
	// update_available is opt-in: valid in `on`, absent from the default.
	srvRoot := `global: {}
notifications:
  on: [update_available]
  destinations:
    - type: webhook
      url: https://example.invalid/hook
`
	cfg, err := loadRoot10(t, srvRoot)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Notifications.On; !reflect.DeepEqual(got, []string{NotificationUpdateAvailable}) {
		t.Errorf("On = %v, want [update_available]", got)
	}

	def, err := loadRoot10(t, `global: {}
notifications:
  destinations:
    - type: webhook
      url: https://example.invalid/hook
`)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, kind := range def.Notifications.On {
		if kind == NotificationUpdateAvailable {
			t.Errorf("default on = %v, want update_available to stay opt-in", def.Notifications.On)
		}
	}
	wantDefault := []string{NotificationDeployFailed, NotificationPruneFailed, NotificationRepoFailed, NotificationUpdateFailed}
	if !reflect.DeepEqual(def.Notifications.On, wantDefault) {
		t.Errorf("default on = %v, want %v", def.Notifications.On, wantDefault)
	}
}
