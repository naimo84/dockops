package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeEnv adapts a map to the getenv callback of parseFlags.
func fakeEnv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestParseFlagsDefaultsFromEnv(t *testing.T) {
	env := map[string]string{
		envRepoURL:       "https://example.invalid/host.git",
		envBranch:        "dev",
		envWebhookSecret: "s3cr3t",
	}
	opts, err := parseFlags(nil, fakeEnv(env))
	if err != nil {
		t.Fatalf("parseFlags() error = %v", err)
	}
	want := options{
		RepoURL:       "https://example.invalid/host.git",
		Branch:        "dev",
		StatePath:     defaultStatePath,
		WebhookSecret: "s3cr3t",
		Addr:          defaultAddr,
	}
	if !reflect.DeepEqual(opts, want) {
		t.Errorf("parseFlags() = %+v, want %+v", opts, want)
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	opts, err := parseFlags([]string{"-repo", "https://example.invalid/host.git"}, fakeEnv(nil))
	if err != nil {
		t.Fatalf("parseFlags() error = %v", err)
	}
	if opts.Branch != defaultBranch {
		t.Errorf("Branch = %q, want default %q when neither flag nor env set it", opts.Branch, defaultBranch)
	}
	if opts.StatePath != defaultStatePath || opts.Addr != defaultAddr || opts.Once || opts.Interval != 0 {
		t.Errorf("parseFlags() = %+v, want flag defaults", opts)
	}
}

func TestParseFlagsArgsOverrideEnv(t *testing.T) {
	env := map[string]string{
		envRepoURL:       "https://env.invalid/host.git",
		envBranch:        "dev",
		envWebhookSecret: "from-env",
	}
	args := []string{
		"-repo", "https://args.invalid/host.git",
		"-branch", "feature",
		"-state", "/tmp/other-state.json",
		"-webhook-secret", "from-args",
		"-addr", ":9090",
		"-once",
		"-interval", "90s",
	}
	opts, err := parseFlags(args, fakeEnv(env))
	if err != nil {
		t.Fatalf("parseFlags() error = %v", err)
	}
	want := options{
		RepoURL:       "https://args.invalid/host.git",
		Branch:        "feature",
		StatePath:     "/tmp/other-state.json",
		WebhookSecret: "from-args",
		Addr:          ":9090",
		Once:          true,
		Interval:      90 * time.Second,
	}
	if !reflect.DeepEqual(opts, want) {
		t.Errorf("parseFlags() = %+v, want %+v", opts, want)
	}
}

func TestParseFlagsMissingRepo(t *testing.T) {
	_, err := parseFlags(nil, fakeEnv(nil))
	if err == nil || !strings.Contains(err.Error(), envRepoURL) {
		t.Errorf("parseFlags() without repo error = %v, want error naming %s", err, envRepoURL)
	}
}

func TestParseFlagsBadInterval(t *testing.T) {
	_, err := parseFlags([]string{"-repo", "u", "-interval", "nope"}, fakeEnv(nil))
	if err == nil || !strings.Contains(err.Error(), "-interval") {
		t.Errorf("parseFlags() with bad interval error = %v, want -interval error", err)
	}
	_, err = parseFlags([]string{"-repo", "u", "-interval", "-5m"}, fakeEnv(nil))
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("parseFlags() with negative interval error = %v, want negative error", err)
	}
}

func TestParseFlagsZeroIntervalMeansUnset(t *testing.T) {
	opts, err := parseFlags([]string{"-repo", "u", "-interval", "0s"}, fakeEnv(nil))
	if err != nil {
		t.Fatalf("parseFlags() error = %v", err)
	}
	if opts.Interval != 0 {
		t.Errorf("Interval = %v, want 0 (unset; config poll_interval applies)", opts.Interval)
	}
}

func TestIntervalSourceEffective(t *testing.T) {
	src := &intervalSource{}

	// Nothing loaded yet: the default applies; an override wins over it.
	if got := src.effective(options{}); got != defaultPollInterval {
		t.Errorf("effective() = %v, want default %v", got, defaultPollInterval)
	}
	override := options{Interval: time.Minute}
	if got := src.effective(override); got != time.Minute {
		t.Errorf("effective() with override = %v, want 1m", got)
	}

	// With a loaded config, its interval applies unless overridden.
	src.set(30 * time.Second)
	if got := src.effective(options{}); got != 30*time.Second {
		t.Errorf("effective() with config = %v, want 30s", got)
	}
	if got := src.effective(override); got != time.Minute {
		t.Errorf("effective() with override and config = %v, want 1m (override wins)", got)
	}
}
