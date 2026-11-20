package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/local/dockops/internal/state"
)

// A stack that keeps failing with the same configuration is not redeployed
// on every poll — hooks, compose up and the notification machinery included.
// The first retry is immediate (a transient failure heals quickly), then the
// wait doubles; a configuration change retries at once.
func TestFailedDeployBacksOff(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.failUpFor = "web"
	r := f.reconciler()

	for pass := 1; pass <= 3; pass++ {
		_ = r.Reconcile(context.Background())
	}
	// Pass 1 fails, pass 2 retries immediately and fails, pass 3 waits.
	if got := len(f.compose.ups); got != 2 {
		t.Fatalf("Up calls after three immediate passes = %d, want 2 (first retry immediate, then back-off)", got)
	}
	st, _ := f.state.Get("web")
	if st.Status != state.StatusFailed || st.Failures != 2 {
		t.Fatalf("state = %+v, want failed with two consecutive failures", st)
	}

	// The wait (one minute after the second failure) has passed.
	st.UpdatedAt = time.Now().Add(-2 * time.Minute)
	if err := f.state.Set("web", st); err != nil {
		t.Fatalf("state.Set() error = %v", err)
	}
	f.compose.failUpFor = ""
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() after the back-off error = %v", err)
	}
	if got := len(f.compose.ups); got != 3 {
		t.Errorf("Up calls = %d, want the retry after the back-off", got)
	}
	if st, _ := f.state.Get("web"); st.Status != state.StatusDeployed || st.Failures != 0 {
		t.Errorf("state = %+v, want deployed and the failure count reset", st)
	}
}

func TestFailedDeployRetriesAtOnceOnConfigChange(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.failUpFor = "web"
	r := f.reconciler()
	_ = r.Reconcile(context.Background())
	_ = r.Reconcile(context.Background())

	// A fix arrives: the stack definition changes, so does the hash.
	f.addLocalStack("web", "name: web\nprofiles:\n  - fixed\n")
	f.compose.failUpFor = ""
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := len(f.compose.ups); got != 3 {
		t.Errorf("Up calls = %d, want an immediate attempt with the new configuration", got)
	}
}

func TestRetryDelay(t *testing.T) {
	for failures, want := range map[int]time.Duration{
		0: 0, 1: 0, 2: time.Minute, 3: 2 * time.Minute, 4: 4 * time.Minute,
		6: 16 * time.Minute, 7: 30 * time.Minute, 8: 30 * time.Minute, 40: 30 * time.Minute,
	} {
		if got := retryDelay(failures); got != want {
			t.Errorf("retryDelay(%d) = %s, want %s", failures, got, want)
		}
	}
}

// A failed pull says nothing about the image — registries hiccup, disks run
// full. It must not block the digest until the tag moves; the next due check
// tries again.
func TestPullFailureDoesNotBlockTheDigest(t *testing.T) {
	f, resolver, rec := applyFixture(t, applyStackYAML)
	f.compose.pullErr = errors.New("registry: connection reset")
	r := f.phase11Reconciler(f.compose, resolver, rec)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := applyRecord(t, f, applyImage)
	if got.ApplyBlockedDigest != "" {
		t.Errorf("ApplyBlockedDigest = %q after a pull failure, want none", got.ApplyBlockedDigest)
	}
	if !strings.Contains(got.ApplyError, "pull") {
		t.Errorf("ApplyError = %q, want the pull failure recorded", got.ApplyError)
	}

	f.compose.pullErr = nil
	f.compose.containers = gateContainers("healthy")
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(f.compose.pulls) != 2 || len(f.compose.upOverrides) != 1 {
		t.Errorf("pulls = %d, ups = %d; want the next due pass to try again and apply",
			len(f.compose.pulls), len(f.compose.upOverrides))
	}
}

// A pass a webhook triggered is an explicit "try now": failed stacks skip
// their back-off (the fix may not change the hash, e.g. a secret added in
// Vault).
func TestRetryNowSkipsTheBackOff(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.failUpFor = "web"
	r := f.reconciler()
	_ = r.Reconcile(context.Background())
	_ = r.Reconcile(context.Background())
	_ = r.Reconcile(context.Background()) // backing off
	if got := len(f.compose.ups); got != 2 {
		t.Fatalf("Up calls = %d, want 2 before the forced pass", got)
	}
	f.compose.failUpFor = ""
	if err := r.Reconcile(RetryNow(context.Background())); err != nil {
		t.Fatalf("forced Reconcile() error = %v", err)
	}
	if got := len(f.compose.ups); got != 3 {
		t.Errorf("Up calls = %d, want the forced retry", got)
	}
}
