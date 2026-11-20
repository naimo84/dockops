package reconciler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/selfupdate"
)

// The update check compares what the containers run, not where the tag
// points: a pull without a recreate (by hand, or an interrupted apply) moves
// the tag, while the stack keeps running the old image.
func TestUpdateCheckComparesRunningContainers(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.images = map[string]string{"app": applyImage}
	f.compose.digests = map[string]string{applyImage: "sha256:new"} // the tag, already pulled
	f.compose.serviceImages = map[string][]string{"app": {"sha256:img-old"}}
	f.compose.repoDigests = map[string]string{"sha256:img-old": "sha256:old"}
	resolver := &fakeResolver{digests: map[string]string{applyImage: "sha256:new"}}
	r := f.phase11Reconciler(f.compose, resolver, &recordingNotifier{})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := applyRecord(t, f, applyImage)
	if got.CurrentDigest != "sha256:old" || !got.UpdateAvailable {
		t.Errorf("record = %+v, want the running digest sha256:old and a pending update", got)
	}
}

// After a rollback the tag still points at the image that failed (the pull
// moved it). The rollback must move it back — otherwise the next deploy of
// the stack rolls the failed image out without any gate — and the next check
// must keep the digest blocked instead of reading the tag as "in sync".
func TestRollbackRestoresTagAndKeepsTheBlock(t *testing.T) {
	f, resolver, rec := applyFixture(t, applyStackYAML)
	f.compose.serviceImages = map[string][]string{"app": {"sha256:img-old"}}
	f.compose.repoDigests = map[string]string{"sha256:img-old": "sha256:old"}
	f.compose.psFn = func(call int) []compose.Container {
		if call == 1 {
			return gateContainers("unhealthy")
		}
		return gateContainers("healthy")
	}
	r := f.phase11Reconciler(f.compose, resolver, rec)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	wantTag := [2]string{"ghcr.io/x/app@sha256:old", applyImage}
	if len(f.compose.tags) != 1 || f.compose.tags[0] != wantTag {
		t.Fatalf("tags = %v, want exactly %v (tag back on the rolled-back digest)", f.compose.tags, wantTag)
	}

	// The fake does not model the retag: the tag still answers with the
	// pulled digest, as it would if the retag had not happened.
	f.compose.digests[applyImage] = "sha256:new"
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	got := applyRecord(t, f, applyImage)
	if got.ApplyBlockedDigest != "sha256:new" || !got.UpdateAvailable || got.CurrentDigest != "sha256:old" {
		t.Errorf("record after the second check = %+v, want sha256:new still blocked and pending", got)
	}
	if len(f.compose.pulls) != 1 {
		t.Errorf("pulls = %+v, want no second attempt on the blocked digest", f.compose.pulls)
	}
}

// Services sharing one image are updated, gated and rolled back together.
// Recreating only one of them leaves the stack on two versions, which the
// tag-based check then even reports as "in sync".
func TestApplyUpdatesEveryServiceOfASharedImage(t *testing.T) {
	f, resolver, rec := applyFixture(t, applyStackYAML)
	f.compose.images = map[string]string{"app": applyImage, "worker": applyImage}
	f.compose.psFn = func(call int) []compose.Container {
		return []compose.Container{
			{Service: "app", Name: "web-app-1", State: "running", Health: "healthy"},
			{Service: "worker", Name: "web-worker-1", State: "running"},
		}
	}
	r := f.phase11Reconciler(f.compose, resolver, rec)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	both := []string{"app", "worker"}
	if len(f.compose.pulls) != 1 || !equalStrings(f.compose.pulls[0].services, both) {
		t.Errorf("pulls = %+v, want one pull of %v", f.compose.pulls, both)
	}
	if len(f.compose.upOverrides) != 1 || !equalStrings(f.compose.upOverrides[0].services, both) {
		t.Errorf("UpOverride calls = %+v, want one up of %v", f.compose.upOverrides, both)
	}
	got := applyRecord(t, f, applyImage)
	if !equalStrings(got.Services, both) || got.Service != "app" {
		t.Errorf("record services = %q/%v, want app/%v", got.Service, got.Services, both)
	}
}

func TestRollbackPinsEveryServiceOfASharedImage(t *testing.T) {
	f, resolver, rec := applyFixture(t, applyStackYAML)
	f.compose.images = map[string]string{"app": applyImage, "worker": applyImage}
	f.compose.psFn = func(call int) []compose.Container {
		worker := "running"
		if call == 1 {
			worker = "exited"
		}
		return []compose.Container{
			{Service: "app", Name: "web-app-1", State: "running"},
			{Service: "worker", Name: "web-worker-1", State: worker},
		}
	}
	r := f.phase11Reconciler(f.compose, resolver, rec)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(f.compose.upOverrides) != 2 {
		t.Fatalf("UpOverride calls = %+v, want forward and rollback", f.compose.upOverrides)
	}
	override := f.compose.upOverrides[1].contents[0]
	for _, service := range []string{"app:", "worker:"} {
		if !strings.Contains(override, service) {
			t.Errorf("rollback override does not pin %s\n%s", service, override)
		}
	}
	if got := f.compose.upOverrides[1].services; !equalStrings(got, []string{"app", "worker"}) {
		t.Errorf("rollback up services = %v, want both", got)
	}
}

// selfRunning makes the fake answer that the self stack's container runs the
// image with the given repository digest.
func selfRunning(f *fixture, digest string) {
	f.compose.serviceImages = map[string][]string{"app": {"sha256:img-" + digest}}
	f.compose.repoDigests = map[string]string{"sha256:img-" + digest: digest}
}

// Without a result file the fallback must judge by the container, not by
// the tag: the agent pulled before the hand-over, so the tag always points
// at the target — a helper that died before recreating anything would be
// booked as a success.
func TestSelfUpdateFallbackJudgesTheContainerNotTheTag(t *testing.T) {
	f := newFixture(t, updateRoot)
	selfState(t, f, pendingRecord())
	f.compose.digests = map[string]string{applyImage: "sha256:new"} // pulled tag
	selfRunning(f, "sha256:old")                                    // never recreated
	rec := &recordingNotifier{}
	r := f.phase12Reconciler(f.compose, &fakeResolver{}, rec, nil, "dockops")

	r.finalizeSelfUpdate(context.Background())

	got := recordOf(t, f, "dockops")
	if got.ApplyBlockedDigest != "sha256:new" || got.CurrentDigest != "sha256:old" || !got.AppliedAt.IsZero() {
		t.Errorf("record = %+v, want the attempt blocked, not applied", got)
	}
	if events := eventsOfKind(t, rec, config.NotificationUpdateApplied); len(events) != 0 {
		t.Errorf("update_applied events = %+v, want none", events)
	}
}

// While the helper may still be working (grace period, recreate, health
// gate), a missing result is no verdict — a pass triggered by a webhook in
// that window must not book anything.
func TestSelfUpdateFallbackWaitsForTheHelper(t *testing.T) {
	f := newFixture(t, updateRoot)
	pending := pendingRecord()
	pending.PendingAt = time.Now().UTC()
	selfState(t, f, pending)
	f.compose.digests = map[string]string{applyImage: "sha256:new"} // pulled before the hand-over
	selfRunning(f, "sha256:old")                                    // not recreated yet
	r := f.phase12Reconciler(f.compose, &fakeResolver{}, &recordingNotifier{}, nil, "dockops")

	r.finalizeSelfUpdate(context.Background())

	got := recordOf(t, f, "dockops")
	if got.ApplyPendingDigest != "sha256:new" || got.ApplyBlockedDigest != "" {
		t.Errorf("record = %+v, want it still pending inside the helper's window", got)
	}
}

// The helper runs the agent's own image, not the target: the pull right
// before the hand-over moved the target's tag, and a broken new build must
// not be the one that has to roll itself back.
func TestSelfUpdateHelperRunsTheAgentsOwnImage(t *testing.T) {
	f, resolver, rec, helper := selfFixture(t)
	r := f.phase12Reconciler(f.compose, resolver, rec, helper.run, "dockops")
	r.selfImage = "sha256:agent-image"

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if helper.count() != 1 {
		t.Fatalf("helper starts = %d, want 1", helper.count())
	}
	args := helper.lastArgs()
	image := ""
	for i, arg := range args {
		if arg == "-selfupdate-helper" && i > 0 {
			image = args[i-1]
		}
	}
	if image != "sha256:agent-image" {
		t.Errorf("helper image = %q, want the agent's own image (args %v)", image, args)
	}
}

// A rolled-back self update leaves the tag on the image that failed; the
// finalization moves it back like the in-process rollback does.
func TestSelfUpdateRolledBackRestoresTag(t *testing.T) {
	f := newFixture(t, updateRoot)
	selfState(t, f, pendingRecord())
	writeSelfResult(t, f, selfupdate.Result{
		Version: selfupdate.Version,
		Status:  selfupdate.StatusRolledBack,
		Digest:  "sha256:new",
		Error:   "health gate: unhealthy",
	})
	r := f.phase12Reconciler(f.compose, &fakeResolver{}, &recordingNotifier{}, nil, "dockops")

	r.finalizeSelfUpdate(context.Background())

	wantTag := [2]string{"ghcr.io/x/app@sha256:old", applyImage}
	if len(f.compose.tags) != 1 || f.compose.tags[0] != wantTag {
		t.Errorf("tags = %v, want %v", f.compose.tags, wantTag)
	}
}
