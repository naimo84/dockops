package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// loadWithNotifications writes a minimal root config plus the given
// notifications block and loads it through the full loader.
func loadWithNotifications(t *testing.T, block string) (*Config, error) {
	t.Helper()
	rootFile, _, _ := newRepo(t)
	writeFile(t, rootFile, "global: {}\n"+block)
	return Load(rootFile)
}

func TestNotificationsBlockAbsentDisablesFeature(t *testing.T) {
	rootFile, _, _ := newRepo(t)
	cfg, err := Load(rootFile)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil (missing notifications block is not an error)", err)
	}
	if cfg.Notifications != nil {
		t.Fatalf("cfg.Notifications = %+v, want nil (feature off)", cfg.Notifications)
	}
}

func TestNotificationsDefaults(t *testing.T) {
	cfg, err := loadWithNotifications(t, `notifications:
  destinations:
    - type: webhook
      url: https://example.invalid/hook
`)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	n := cfg.Notifications
	if n == nil {
		t.Fatal("cfg.Notifications = nil, want block")
	}
	if !n.Enabled {
		t.Error("Enabled = false, want true by default")
	}
	if want := []string{NotificationDeployFailed, NotificationPruneFailed, NotificationRepoFailed}; !reflect.DeepEqual(n.On, want) {
		t.Errorf("On = %v, want %v", n.On, want)
	}
	if got := time.Duration(n.RepeatAfter); got != time.Hour {
		t.Errorf("RepeatAfter = %v, want 1h", got)
	}
	if got := time.Duration(n.Timeout); got != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s", got)
	}
	if len(n.Destinations) != 1 || n.Destinations[0].Type != "webhook" {
		t.Errorf("Destinations = %+v, want one webhook", n.Destinations)
	}
}

func TestNotificationsExplicitValues(t *testing.T) {
	cfg, err := loadWithNotifications(t, `notifications:
  enabled: false
  on: [deploy_failed, repo_failed]
  repeat_after: 5m
  timeout: 2s
  destinations:
    - type: ntfy
      url: https://ntfy.example.invalid/dockops
      priority: high
      tags: [warning, docker]
`)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	n := cfg.Notifications
	if n.Enabled {
		t.Error("Enabled = true, want false (explicit)")
	}
	if want := []string{NotificationDeployFailed, NotificationRepoFailed}; !reflect.DeepEqual(n.On, want) {
		t.Errorf("On = %v, want %v", n.On, want)
	}
	if got := time.Duration(n.RepeatAfter); got != 5*time.Minute {
		t.Errorf("RepeatAfter = %v, want 5m", got)
	}
	if got := time.Duration(n.Timeout); got != 2*time.Second {
		t.Errorf("Timeout = %v, want 2s", got)
	}
	d := n.Destinations[0]
	if d.Priority != "high" || !reflect.DeepEqual(d.Tags, []string{"warning", "docker"}) {
		t.Errorf("ntfy destination = %+v, want priority high and both tags", d)
	}
}

func TestNotificationsEnvExpansion(t *testing.T) {
	t.Setenv("PHASE9_BOT_TOKEN", "123:abc")
	t.Setenv("PHASE9_CHAT_ID", "987654321")
	t.Setenv("PHASE9_HOOK_URL", "https://example.invalid/hook")
	cfg, err := loadWithNotifications(t, `notifications:
  destinations:
    - type: webhook
      url: ${PHASE9_HOOK_URL}
      headers:
        Authorization: Bearer ${PHASE9_BOT_TOKEN}
        X-Literal: no-reference
    - type: telegram
      token: ${PHASE9_BOT_TOKEN}
      chat_id: ${PHASE9_CHAT_ID}
      tags: ["${PHASE9_CHAT_ID}"]
`)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	webhook := cfg.Notifications.Destinations[0]
	if webhook.URL != "https://example.invalid/hook" {
		t.Errorf("webhook url = %q, want the expanded value", webhook.URL)
	}
	if got := webhook.Headers["Authorization"]; got != "Bearer 123:abc" {
		t.Errorf("webhook header Authorization = %q, want expanded token", got)
	}
	if got := webhook.Headers["X-Literal"]; got != "no-reference" {
		t.Errorf("webhook header X-Literal = %q, want unchanged", got)
	}
	telegram := cfg.Notifications.Destinations[1]
	if telegram.Token != "123:abc" || telegram.ChatID != "987654321" {
		t.Errorf("telegram destination = %+v, want expanded token and chat id", telegram)
	}
	if !reflect.DeepEqual(telegram.Tags, []string{"987654321"}) {
		t.Errorf("telegram tags = %v, want expanded entry", telegram.Tags)
	}
}

func TestNotificationsMissingEnvVar(t *testing.T) {
	t.Setenv("PHASE9_UNSET_TOKEN", "")
	t.Setenv("PHASE9_UNSET_URL", "")
	tests := []struct {
		name  string
		block string
		field string
		env   string
	}{
		{
			name: "telegram token",
			block: `notifications:
  destinations:
    - type: telegram
      token: ${PHASE9_UNSET_TOKEN}
      chat_id: "1"
`,
			field: "notifications.destinations[0].token",
			env:   "PHASE9_UNSET_TOKEN",
		},
		{
			name: "webhook url",
			block: `notifications:
  destinations:
    - type: webhook
      url: ${PHASE9_UNSET_URL}
`,
			field: "notifications.destinations[0].url",
			env:   "PHASE9_UNSET_URL",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWithNotifications(t, tc.block)
			if err == nil {
				t.Fatalf("Load() error = nil, want missing variable error")
			}
			for _, want := range []string{tc.field, tc.env, "is not set"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestNotificationsUnknownType(t *testing.T) {
	_, err := loadWithNotifications(t, `notifications:
  destinations:
    - type: mattermost
      url: https://example.invalid/hook
`)
	if err == nil {
		t.Fatal("Load() error = nil, want unknown type error")
	}
	for _, want := range []string{"notifications.destinations[0].type", "mattermost"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestNotificationsMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name  string
		block string
		want  string
	}{
		{
			name: "webhook url",
			block: `notifications:
  destinations:
    - type: webhook
`,
			want: "notifications.destinations[0].url",
		},
		{
			name: "ntfy url",
			block: `notifications:
  destinations:
    - type: ntfy
`,
			want: "notifications.destinations[0].url",
		},
		{
			name: "telegram chat id",
			block: `notifications:
  destinations:
    - type: telegram
      token: abc
`,
			want: "notifications.destinations[0].chat_id",
		},
		{
			name: "discord webhook url",
			block: `notifications:
  destinations:
    - type: discord
`,
			want: "notifications.destinations[0].webhook_url",
		},
		{
			name: "slack webhook url",
			block: `notifications:
  destinations:
    - type: slack
`,
			want: "notifications.destinations[0].webhook_url",
		},
		{
			name: "type",
			block: `notifications:
  destinations:
    - url: https://example.invalid/hook
`,
			want: "notifications.destinations[0].type",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWithNotifications(t, tc.block)
			if err == nil {
				t.Fatalf("Load() error = nil, want required field error")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "required") {
				t.Errorf("error %q does not name %q as required", err, tc.want)
			}
		})
	}
}

func TestNotificationsRequiresDestinations(t *testing.T) {
	for name, block := range map[string]string{
		"empty list": "notifications:\n  destinations: []\n",
		"block only": "notifications: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadWithNotifications(t, block)
			if err == nil {
				t.Fatal("Load() error = nil, want empty destinations error")
			}
			if !strings.Contains(err.Error(), "notifications.destinations") {
				t.Errorf("error %q does not name notifications.destinations", err)
			}
		})
	}
}

func TestNotificationsUnknownEvent(t *testing.T) {
	_, err := loadWithNotifications(t, `notifications:
  on: [deploy_failed, stack_broken]
  destinations:
    - type: webhook
      url: https://example.invalid/hook
`)
	if err == nil {
		t.Fatal("Load() error = nil, want unknown event error")
	}
	for _, want := range []string{"notifications.on[1]", "stack_broken"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestNotificationsDisabledStillValidated(t *testing.T) {
	_, err := loadWithNotifications(t, `notifications:
  enabled: false
  destinations:
    - type: telegram
      token: abc
`)
	if err == nil {
		t.Fatal("Load() error = nil, want validation error even while disabled")
	}
	if !strings.Contains(err.Error(), "chat_id") {
		t.Errorf("error %q does not name chat_id", err)
	}
}

func TestNotificationsNegativeRepeatAfter(t *testing.T) {
	_, err := loadWithNotifications(t, `notifications:
  repeat_after: -5m
  destinations:
    - type: webhook
      url: https://example.invalid/hook
`)
	if err == nil {
		t.Fatal("Load() error = nil, want negative repeat_after error")
	}
	if !strings.Contains(err.Error(), "notifications.repeat_after") {
		t.Errorf("error %q does not name notifications.repeat_after", err)
	}
}

func TestNotificationsUnknownFieldRejected(t *testing.T) {
	// Strict decoding must reach into the block as well: a typo must not
	// silently leave a channel unconfigured.
	_, err := loadWithNotifications(t, `notifications:
  enabld: true
  destinations:
    - type: webhook
      url: https://example.invalid/hook
`)
	if err == nil {
		t.Fatal("Load() error = nil, want unknown field error")
	}
	if !strings.Contains(err.Error(), "enabld") {
		t.Errorf("error %q does not name the unknown field", err)
	}
}

func TestNotificationsNotPartOfStackHashInput(t *testing.T) {
	// Notifications are host-global: they must not leak into a stack
	// definition. A stack file carrying the key is a load error (strict
	// decoding), and no stack field mirrors it.
	rootFile, hostDir, _ := newRepo(t)
	writeFile(t, filepath.Join(hostDir, "web", "stack.yaml"), "name: web\nnotifications:\n  destinations: []\n")
	writeFile(t, filepath.Join(hostDir, "web", "compose.yaml"), "services:\n  app:\n    image: nginx\n")
	if _, err := Load(rootFile); err == nil {
		t.Fatal("Load() error = nil, want unknown field error for a stack-level notifications key")
	}
}
