package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestUpdatePolicyApply: "apply" is a valid policy since phase 11 and keeps
// the stack in the update check (unlike pinned).
func TestUpdatePolicyApply(t *testing.T) {
	cfg, err := loadStack10(t, "name: web\nupdate_policy: apply\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	stack := cfg.Stacks[0]
	if got := stack.UpdatePolicy; got != UpdatePolicyApply {
		t.Errorf("UpdatePolicy = %q, want apply", got)
	}
	if !stack.ChecksImageUpdates() {
		t.Error("ChecksImageUpdates() = false, want true for apply")
	}
	if !stack.AppliesImageUpdates() {
		t.Error("AppliesImageUpdates() = false, want true for apply")
	}
	if stack.EffectiveUpdatePolicy() != UpdatePolicyApply {
		t.Errorf("EffectiveUpdatePolicy() = %q, want apply", stack.EffectiveUpdatePolicy())
	}
}

// TestUpdatePolicyNotifyAndPinnedDoNotApply pins the other two policies'
// meaning for the apply step.
func TestUpdatePolicyNotifyAndPinnedDoNotApply(t *testing.T) {
	for _, policy := range []string{"", "notify", "pinned"} {
		yaml := "name: web\n"
		if policy != "" {
			yaml += "update_policy: " + policy + "\n"
		}
		cfg, err := loadStack10(t, yaml)
		if err != nil {
			t.Fatalf("Load(%q) error = %v", policy, err)
		}
		if cfg.Stacks[0].AppliesImageUpdates() {
			t.Errorf("AppliesImageUpdates() = true for policy %q", policy)
		}
	}
}

// TestHealthTimeoutDefaults: absent means the documented 90s, filled in by
// the loader so every reader sees the effective value.
func TestHealthTimeoutDefaults(t *testing.T) {
	cfg, err := loadStack10(t, "name: web\nupdate_policy: apply\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	stack := cfg.Stacks[0]
	if got := time.Duration(stack.HealthTimeout); got != 90*time.Second {
		t.Errorf("HealthTimeout = %v, want the 90s default", got)
	}
	if got := stack.EffectiveHealthTimeout(); got != DefaultHealthTimeout {
		t.Errorf("EffectiveHealthTimeout() = %v, want %v", got, DefaultHealthTimeout)
	}
}

// TestHealthTimeoutExplicit: a configured window wins over the default.
func TestHealthTimeoutExplicit(t *testing.T) {
	cfg, err := loadStack10(t, "name: web\nupdate_policy: apply\nhealth_timeout: 30s\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Stacks[0].EffectiveHealthTimeout(); got != 30*time.Second {
		t.Errorf("EffectiveHealthTimeout() = %v, want 30s", got)
	}
}

// TestHealthTimeoutZeroUsesDefault: 0 is "use the default", not "no gate".
func TestHealthTimeoutZeroUsesDefault(t *testing.T) {
	cfg, err := loadStack10(t, "name: web\nupdate_policy: apply\nhealth_timeout: 0s\n")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	stack := cfg.Stacks[0]
	if got := time.Duration(stack.HealthTimeout); got != DefaultHealthTimeout {
		t.Errorf("HealthTimeout = %v, want the filled-in default %v", got, DefaultHealthTimeout)
	}
	if got := stack.EffectiveHealthTimeout(); got != DefaultHealthTimeout {
		t.Errorf("EffectiveHealthTimeout() = %v, want %v", got, DefaultHealthTimeout)
	}
}

// TestHealthTimeoutNegativeRejected: a negative window would fail the gate
// without waiting — a configuration mistake, not a feature.
func TestHealthTimeoutNegativeRejected(t *testing.T) {
	_, err := loadStack10(t, "name: web\nupdate_policy: apply\nhealth_timeout: -5s\n")
	if err == nil {
		t.Fatal("Load() error = nil, want a validation error for health_timeout")
	}
	if !strings.Contains(err.Error(), "health_timeout") || !strings.Contains(err.Error(), "negative") {
		t.Errorf("Load() error = %q, want field name and reason", err)
	}
}

// TestHealthTimeoutHandBuiltStack: a hand-built stack (no loader) still gets
// the default from EffectiveHealthTimeout, mirroring EffectiveUpdatePolicy.
func TestHealthTimeoutHandBuiltStack(t *testing.T) {
	var stack Stack
	if got := stack.EffectiveHealthTimeout(); got != DefaultHealthTimeout {
		t.Errorf("EffectiveHealthTimeout() = %v, want %v", got, DefaultHealthTimeout)
	}
}

// TestNotificationsUpdateApplyEvents: update_applied and update_failed are
// valid `on` values; only update_failed is part of the default set.
func TestNotificationsUpdateApplyEvents(t *testing.T) {
	cfg, err := loadWithNotifications(t, `notifications:
  on: [update_applied, update_failed]
  destinations:
    - type: webhook
      url: https://example.invalid/hook
`)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{NotificationUpdateApplied, NotificationUpdateFailed}
	if got := cfg.Notifications.On; !reflect.DeepEqual(got, want) {
		t.Errorf("On = %v, want %v", got, want)
	}

	def, err := loadWithNotifications(t, `notifications:
  destinations:
    - type: webhook
      url: https://example.invalid/hook
`)
	if err != nil {
		t.Fatalf("Load(default) error = %v", err)
	}
	var hasFailed bool
	for _, kind := range def.Notifications.On {
		if kind == NotificationUpdateFailed {
			hasFailed = true
		}
		if kind == NotificationUpdateApplied {
			t.Errorf("default on = %v, want update_applied to stay opt-in", def.Notifications.On)
		}
	}
	if !hasFailed {
		t.Errorf("default on = %v, want update_failed in the failure set", def.Notifications.On)
	}
}

// TestNotificationsUnknownEventRejected keeps the validation strict for a
// misspelled new kind.
func TestNotificationsUnknownEventRejected(t *testing.T) {
	_, err := loadWithNotifications(t, `notifications:
  on: [update_aplied]
  destinations:
    - type: webhook
      url: https://example.invalid/hook
`)
	if err == nil || !strings.Contains(err.Error(), "update_aplied") {
		t.Fatalf("Load() error = %v, want the unknown event named", err)
	}
}
