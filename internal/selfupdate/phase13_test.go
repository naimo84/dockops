package selfupdate

import (
	"os"
	"strings"
	"testing"
	"time"
)

// deployPlan is the smallest deploy plan Validate accepts (phase 13): no
// targets, no services ("all"), a hash and the profiles of the stack.
func deployPlan() Plan {
	return Plan{
		Version:              Version,
		Kind:                 KindDeploy,
		Project:              "dockops",
		StackDir:             "/var/lib/dockops/repos/host/dockops",
		ComposeFiles:         []string{"compose.yaml"},
		EnvFile:              "/var/lib/dockops/repos/host/dockops/.env",
		Profiles:             []string{"sops"},
		GraceSeconds:         10,
		HealthTimeoutSeconds: 90,
		HealthPollSeconds:    3,
		DeployHash:           "0123456789abcdef",
	}
}

func TestPhase13ValidateDeployKind(t *testing.T) {
	tests := []struct {
		name string
		plan func() Plan
		want string // empty = valid
	}{
		{
			name: "deploy with targets",
			plan: func() Plan {
				p := deployPlan()
				p.Targets = validPlan().Targets
				return p
			},
			want: "targets must be empty",
		},
		{
			name: "deploy without hash",
			plan: func() Plan {
				p := deployPlan()
				p.DeployHash = "  "
				return p
			},
			want: "deploy_hash",
		},
		{
			name: "deploy with services",
			plan: func() Plan {
				p := deployPlan()
				p.Services = []string{"dockops"}
				return p
			},
		},
		{
			name: "deploy with empty service entry",
			plan: func() Plan {
				p := deployPlan()
				p.Services = []string{""}
				return p
			},
			want: "services[0]",
		},
		{
			name: "deploy with empty profile",
			plan: func() Plan {
				p := deployPlan()
				p.Profiles = []string{""}
				return p
			},
			want: "profiles[0]",
		},
		{
			name: "deploy without health deadlines",
			plan: func() Plan {
				p := deployPlan()
				p.HealthTimeoutSeconds = 0
				return p
			},
			want: "health_timeout_seconds",
		},
		{
			name: "images without services",
			plan: func() Plan {
				p := validPlan()
				p.Services = nil
				return p
			},
			want: "services",
		},
		{
			name: "images without targets",
			plan: func() Plan {
				p := validPlan()
				p.Targets = nil
				return p
			},
			want: "targets",
		},
		{
			name: "unknown kind",
			plan: func() Plan {
				p := validPlan()
				p.Kind = "deployment"
				return p
			},
			want: "unknown kind",
		},
		{
			name: "explicit images kind",
			plan: func() Plan {
				// The field is two-valued by design: empty means images, and
				// nothing writes an "images" literal (see Plan.Kind).
				p := validPlan()
				p.Kind = "images"
				return p
			},
			want: "unknown kind",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.plan().Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestPhase13ReadPlanWithoutKindIsImages(t *testing.T) {
	dir := t.TempDir()
	// A phase-12 file: the kind field does not exist in it at all.
	raw := `{
  "version": 1,
  "project": "dockops",
  "stack_dir": "/var/lib/dockops/repos/host/dockops",
  "compose_files": ["compose.yaml"],
  "env_file": "/var/lib/dockops/repos/host/dockops/.env",
  "services": ["dockops"],
  "containers": ["dockops-1"],
  "grace_seconds": 10,
  "health_timeout_seconds": 180,
  "health_poll_seconds": 3,
  "targets": [{"image": "ghcr.io/x/dockops:latest", "repository": "ghcr.io/x/dockops",
    "digest": "sha256:22", "previous": "sha256:11", "service": "dockops"}]
}
`
	path := PlanPath(dir)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	got, err := ReadPlan(path)
	if err != nil {
		t.Fatalf("ReadPlan() error = %v", err)
	}
	if got.Kind != "" {
		t.Errorf("Kind = %q, want empty (an absent kind is the image move)", got.Kind)
	}
	if len(got.Targets) != 1 || got.Targets[0].Service != "dockops" {
		t.Errorf("Targets = %+v, want the phase-12 target", got.Targets)
	}
}

func TestPhase13ReadPlanDeploy(t *testing.T) {
	dir := t.TempDir()
	want := deployPlan()
	want.Services = []string{"dockops", "sidecar"}
	if err := WriteAtomic(PlanPath(dir), want); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	got, err := ReadPlan(PlanPath(dir))
	if err != nil {
		t.Fatalf("ReadPlan() error = %v", err)
	}
	if got.Kind != KindDeploy {
		t.Errorf("Kind = %q, want %q", got.Kind, KindDeploy)
	}
	if got.DeployHash != want.DeployHash {
		t.Errorf("DeployHash = %q, want %q", got.DeployHash, want.DeployHash)
	}
	if len(got.Profiles) != 1 || got.Profiles[0] != "sops" {
		t.Errorf("Profiles = %v, want [sops]", got.Profiles)
	}
	if len(got.Services) != 2 || got.Services[1] != "sidecar" {
		t.Errorf("Services = %v, want both services", got.Services)
	}
}

func TestPhase13ResultDeployRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Result{
		Version:    Version,
		Kind:       KindDeploy,
		Status:     StatusFailed,
		DeployHash: "0123456789abcdef",
		Error:      "compose up: exit status 1",
		At:         time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
	}
	if err := WriteAtomic(ResultPath(dir), want); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	got, err := ReadResult(ResultPath(dir))
	if err != nil {
		t.Fatalf("ReadResult() error = %v", err)
	}
	if got.Kind != KindDeploy || got.Status != StatusFailed || got.DeployHash != want.DeployHash {
		t.Errorf("ReadResult() = %+v, want the deploy verdict", got)
	}
	if got.Error != want.Error {
		t.Errorf("Error = %q, want %q", got.Error, want.Error)
	}
	if !got.At.Equal(want.At) {
		t.Errorf("At = %s, want %s", got.At, want.At)
	}
}

func TestPhase13ReadResultRejectsUnknownStatus(t *testing.T) {
	dir := t.TempDir()
	path := ResultPath(dir)
	if err := os.WriteFile(path, []byte(`{"status": "exploded"}`), 0o600); err != nil {
		t.Fatalf("write result: %v", err)
	}
	if _, err := ReadResult(path); err == nil || !strings.Contains(err.Error(), "unknown status") {
		t.Fatalf("ReadResult() error = %v, want the unknown-status error", err)
	}
}

func TestPhase13ReadResultWithoutKind(t *testing.T) {
	// A phase-12 result keeps loading: no kind, no deploy hash, status ok.
	dir := t.TempDir()
	path := ResultPath(dir)
	if err := os.WriteFile(path, []byte(`{"status": "ok", "digest": "sha256:new"}`), 0o600); err != nil {
		t.Fatalf("write result: %v", err)
	}
	got, err := ReadResult(path)
	if err != nil {
		t.Fatalf("ReadResult() error = %v", err)
	}
	if got.Kind != "" || got.DeployHash != "" {
		t.Errorf("result = %+v, want no deploy fields", got)
	}
}
