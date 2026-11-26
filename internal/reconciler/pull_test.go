package reconciler

import (
	"context"
	"reflect"
	"testing"

	"github.com/local/dockops/internal/config"
)

// POST /pull makes a stack due although the interval has not elapsed; the
// request is consumed by that pass, so the next pass is gated again.
func TestPullForcesUpdateCheck(t *testing.T) {
	f := newFixture(t, defaultRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.images = map[string]string{"app": "ghcr.io/x/app:latest"}
	f.compose.digests = map[string]string{"ghcr.io/x/app:latest": "sha256:old"}

	resolver := &fakeResolver{digests: map[string]string{"ghcr.io/x/app:latest": "sha256:old"}}
	r := f.phase10Reconciler(resolver, &recordingNotifier{})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 1: Reconcile() error = %v", err)
	}

	queued, skipped, unknown, err := r.RequestUpdateCheck([]string{"web"})
	if err != nil || len(unknown) != 0 || len(skipped) != 0 || !reflect.DeepEqual(queued, []string{"web"}) {
		t.Fatalf("RequestUpdateCheck() = %v %v %v %v", queued, skipped, unknown, err)
	}
	for pass := 2; pass <= 3; pass++ {
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("pass %d: Reconcile() error = %v", pass, err)
		}
	}
	if calls := resolver.called(); len(calls) != 2 {
		t.Errorf("registry calls = %v, want two (pass 1 due, pass 2 forced, pass 3 gated)", calls)
	}
}

// update_check_interval: 0 stays the emergency brake: a forced request is
// dropped, not kept for later.
func TestPullRespectsDisabledCheck(t *testing.T) {
	f := newFixture(t, `global:
  work_dir: WORKDIR
  update_check_interval: 0
`)
	f.addLocalStack("web", "name: web\n")
	f.compose.images = map[string]string{"app": "ghcr.io/x/app:latest"}
	f.compose.digests = map[string]string{"ghcr.io/x/app:latest": "sha256:old"}
	resolver := &fakeResolver{digests: map[string]string{"ghcr.io/x/app:latest": "sha256:new"}}
	r := f.phase10Reconciler(resolver, &recordingNotifier{})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, _, _, err := r.RequestUpdateCheck(nil); err != nil {
		t.Fatalf("RequestUpdateCheck() error = %v", err)
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if calls := resolver.called(); len(calls) != 0 {
		t.Errorf("registry calls = %v, want none with the check disabled", calls)
	}
	if left := r.takeForcedChecks(); len(left) != 0 {
		t.Errorf("forced set = %v, want drained", left)
	}
}

// Selection: unknown names reject the request, pinned and disabled stacks
// are skipped with a reason, an empty selection means all.
func TestRequestUpdateCheckSelection(t *testing.T) {
	r := newRestartReconciler(t, []config.Stack{
		{Name: "web", Enabled: true},
		{Name: "db", Enabled: true, UpdatePolicy: "pinned"},
		{Name: "off", Enabled: false},
	}, nil, nil)

	_, _, unknown, err := r.RequestUpdateCheck([]string{"web", "nope"})
	if err != nil || !reflect.DeepEqual(unknown, []string{"nope"}) {
		t.Fatalf("unknown = %v, err = %v", unknown, err)
	}
	if left := r.takeForcedChecks(); len(left) != 0 {
		t.Errorf("forced set = %v after a rejected request, want empty", left)
	}

	queued, skipped, _, err := r.RequestUpdateCheck(nil)
	if err != nil {
		t.Fatalf("RequestUpdateCheck() error = %v", err)
	}
	if !reflect.DeepEqual(queued, []string{"web"}) {
		t.Errorf("queued = %v, want [web]", queued)
	}
	want := map[string]string{"db": "update_policy: pinned", "off": "stack disabled"}
	if !reflect.DeepEqual(skipped, want) {
		t.Errorf("skipped = %v, want %v", skipped, want)
	}

	empty := New(Deps{RepoURL: "x", Branch: "main", Logger: discardLogger()})
	if _, _, _, err := empty.RequestUpdateCheck(nil); err == nil {
		t.Error("RequestUpdateCheck() before the first pass succeeded, want an error")
	}
}
