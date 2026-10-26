package reconciler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/notify"
	"github.com/local/dockops/internal/registry"
	"github.com/local/dockops/internal/selfupdate"
	"github.com/local/dockops/internal/state"
)

// testSelfContainer is the agent container identity the fixture hands to the
// helper's --volumes-from, so assertions do not depend on the test hostname.
const testSelfContainer = "selfcontainer-fixture"

// selfStackYAML is the phase-12 self stack: the fixture's project name must
// equal the reconciler's self project.
const selfStackYAML = `name: dockops
update_policy: apply
`

// helperRun records the docker run argument lists of the self-update hand-over
// and can fail or observe the call. no real container is started.
type helperRun struct {
	mu    sync.Mutex
	calls [][]string
	err   error
}

func (h *helperRun) run(_ context.Context, args []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, append([]string(nil), args...))
	return h.err
}

func (h *helperRun) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func (h *helperRun) lastArgs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) == 0 {
		return nil
	}
	return h.calls[len(h.calls)-1]
}

// phase12Reconciler wires the fixture with the self-update path enabled and
// the helper injected; selfProject is passed verbatim so the empty value's
// default can be exercised too.
func (f *fixture) phase12Reconciler(composeExec ComposeExecutor, resolver registry.Resolver, rec notify.Notifier, helper func(context.Context, []string) error, selfProject string) *Reconciler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(Deps{
		RepoURL:     "https://example.invalid/host.git",
		Branch:      "main",
		WorkDir:     f.workDir,
		State:       f.state,
		Logger:      logger,
		Git:         f.git,
		Compose:     composeExec,
		Hooks:       f.hooks,
		Sops:        f.sops.decrypt,
		NewVault:    f.vault.make,
		Notifier:    rec,
		Registry:    resolver,
		SelfProject: selfProject,
		SelfUpdate:  true,
	})
	r.healthPoll = time.Millisecond
	// Fixed identity: the helper inherits this container's mounts.
	r.selfContainer = testSelfContainer
	r.runHelper = helper
	return r
}

// selfFixture builds the recurring phase-12 setup: the self stack's image
// moved from sha256:old to sha256:new.
func selfFixture(t *testing.T) (*fixture, *fakeResolver, *recordingNotifier, *helperRun) {
	t.Helper()
	f := newFixture(t, updateRoot)
	f.addLocalStack("dockops", selfStackYAML)
	f.compose.images = map[string]string{"app": applyImage}
	f.compose.digests = map[string]string{applyImage: "sha256:old"}
	f.compose.containers = []compose.Container{
		{Service: "app", Name: "dockops-app-1", State: "running", Health: "healthy"},
	}
	resolver := &fakeResolver{digests: map[string]string{applyImage: "sha256:new"}}
	return f, resolver, &recordingNotifier{}, &helperRun{}
}

// recordOf returns one image record of a stack.
func recordOf(t *testing.T, f *fixture, stack string) state.ImageRecord {
	t.Helper()
	recs := imageState(t, f, stack)
	rec, ok := recs[applyImage]
	if !ok {
		t.Fatalf("no image record for %s (records: %+v)", applyImage, recs)
	}
	return rec
}

// planPath is the plan file of the fixture's state directory.
func planPath(f *fixture) string {
	return selfupdate.PlanPath(filepath.Dir(f.state.Path()))
}

func TestPhase12SelfStackHandsOverToHelper(t *testing.T) {
	f, resolver, rec, helper := selfFixture(t)
	r := f.phase12Reconciler(f.compose, resolver, rec, helper.run, "dockops")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// Exactly one pull, no recreate by the agent itself.
	if len(f.compose.pulls) != 1 {
		t.Errorf("pulls = %+v, want exactly one", f.compose.pulls)
	}
	if len(f.compose.upOverrides) != 0 {
		t.Errorf("UpOverride calls = %+v, want none (the helper recreates, not the agent)", f.compose.upOverrides)
	}
	if got := helper.count(); got != 1 {
		t.Fatalf("helper runs = %d, want exactly one", got)
	}

	// The argument list is the mandated docker run invocation.
	args := helper.lastArgs()
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run -d --rm",
		"--name dockops-selfupdate-dockops-",
		"--volumes-from " + testSelfContainer,
		applyImage + " -selfupdate-helper -plan " + planPath(f),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("helper args = %q, want it to contain %q", joined, want)
		}
	}

	// The plan is on disk and describes exactly this move.
	plan, err := selfupdate.ReadPlan(planPath(f))
	if err != nil {
		t.Fatalf("ReadPlan() error = %v", err)
	}
	if plan.Project != "dockops" || plan.StackDir != filepath.Join(f.workDir, "host", "dockops") {
		t.Errorf("plan = %+v, want project dockops at the stack dir", plan)
	}
	if len(plan.Services) != 1 || plan.Services[0] != "app" {
		t.Errorf("plan.Services = %v, want [app]", plan.Services)
	}
	if got, want := plan.EnvFile, filepath.Join(f.workDir, "host", "dockops", ".env"); got != want {
		t.Errorf("plan.EnvFile = %q, want %q", got, want)
	}
	if len(plan.Containers) != 1 || plan.Containers[0] != "dockops-app-1" {
		t.Errorf("plan.Containers = %v, want the project's container name", plan.Containers)
	}
	if plan.GraceSeconds != selfUpdateGraceSeconds {
		t.Errorf("plan.GraceSeconds = %d, want %d", plan.GraceSeconds, selfUpdateGraceSeconds)
	}
	if plan.HealthTimeoutSeconds <= 0 || plan.HealthPollSeconds <= 0 {
		t.Errorf("plan timeouts = %d/%d, want > 0", plan.HealthTimeoutSeconds, plan.HealthPollSeconds)
	}
	if len(plan.Targets) != 1 {
		t.Fatalf("plan.Targets = %+v, want one", plan.Targets)
	}
	target := plan.Targets[0]
	if target.Image != applyImage || target.Repository != "ghcr.io/x/app" ||
		target.Digest != "sha256:new" || target.Previous != "sha256:old" || target.Service != "app" {
		t.Errorf("plan target = %+v, want the app image pinned to new/previous", target)
	}

	// The state carries the honest interim picture.
	got := recordOf(t, f, "dockops")
	if got.ApplyPendingDigest != "sha256:new" {
		t.Errorf("ApplyPendingDigest = %q, want sha256:new", got.ApplyPendingDigest)
	}
	if got.PreviousDigest != "sha256:old" {
		t.Errorf("PreviousDigest = %q, want sha256:old", got.PreviousDigest)
	}
	if got.CurrentDigest != "sha256:old" {
		t.Errorf("CurrentDigest = %q, want the still running sha256:old", got.CurrentDigest)
	}
	if got.PendingAt.IsZero() {
		t.Error("PendingAt is zero, want the hand-over time")
	}
	if got.ApplyError != "" {
		t.Errorf("ApplyError = %q, want empty on a clean hand-over", got.ApplyError)
	}

	// Nothing about the apply is reported yet: the update_available event of
	// the check is fine, the outcome is the next start's business.
	events, _ := rec.snapshot()
	for _, ev := range events {
		if ev.Kind == config.NotificationUpdateApplied || ev.Kind == config.NotificationUpdateFailed {
			t.Errorf("event = %+v, want no apply verdict before the helper reported", ev)
		}
	}
}

func TestPhase12SelfProjectDefaultsToDockops(t *testing.T) {
	f, resolver, rec, helper := selfFixture(t)
	// Empty SelfProject: the default "dockops" must still match.
	r := f.phase12Reconciler(f.compose, resolver, rec, helper.run, "")
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := helper.count(); got != 1 {
		t.Errorf("helper runs = %d, want one (an unset self project means dockops)", got)
	}
}

func TestPhase12NonSelfStackAppliesInProcess(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", applyStackYAML)
	f.compose.images = map[string]string{"app": applyImage}
	f.compose.digests = map[string]string{applyImage: "sha256:old"}
	f.compose.containers = gateContainers("healthy")
	resolver := &fakeResolver{digests: map[string]string{applyImage: "sha256:new"}}
	helper := &helperRun{}
	r := f.phase12Reconciler(f.compose, resolver, &recordingNotifier{}, helper.run, "dockops")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := helper.count(); got != 0 {
		t.Errorf("helper runs = %d, want none for a non-self stack", got)
	}
	if len(f.compose.upOverrides) != 1 {
		t.Errorf("UpOverride calls = %+v, want the in-process recreate", f.compose.upOverrides)
	}
	if _, err := os.Stat(planPath(f)); !os.IsNotExist(err) {
		t.Errorf("plan file exists (stat err = %v), want none for a non-self stack", err)
	}
	if got := recordOf(t, f, "web"); got.CurrentDigest != "sha256:new" || got.ApplyPendingDigest != "" {
		t.Errorf("record = %+v, want an in-process apply without a pending marker", got)
	}
}

func TestPhase12HelperFailureRollsPendingBack(t *testing.T) {
	f, resolver, rec, helper := selfFixture(t)
	helper.err = context.DeadlineExceeded
	r := f.phase12Reconciler(f.compose, resolver, rec, helper.run, "dockops")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v, want nil (a failed hand-over stays in the record)", err)
	}
	got := recordOf(t, f, "dockops")
	if got.ApplyPendingDigest != "" {
		t.Errorf("ApplyPendingDigest = %q, want cleared (no helper will report)", got.ApplyPendingDigest)
	}
	if !strings.Contains(got.ApplyError, "start helper") {
		t.Errorf("ApplyError = %q, want the hand-over failure", got.ApplyError)
	}
	if got.ApplyBlockedDigest != "" {
		t.Errorf("ApplyBlockedDigest = %q, want empty (nothing was recreated)", got.ApplyBlockedDigest)
	}
	if got.PreviousDigest != "sha256:old" {
		t.Errorf("PreviousDigest = %q, want the rollback target kept", got.PreviousDigest)
	}
	if len(f.compose.upOverrides) != 0 {
		t.Errorf("UpOverride calls = %+v, want none", f.compose.upOverrides)
	}
}

func TestPhase12PendingUpdateIsNotHandedOverTwice(t *testing.T) {
	f, resolver, rec, helper := selfFixture(t)
	r := f.phase12Reconciler(f.compose, resolver, rec, helper.run, "dockops")
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if got := helper.count(); got != 1 {
		t.Fatalf("helper runs after the first pass = %d, want 1", got)
	}

	// The helper is still working: the tag moves to a digest that is
	// neither the target nor the previous one, so the finalization keeps
	// the update pending and the second pass must not start a second helper.
	f.compose.digests[applyImage] = "sha256:third"
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if got := helper.count(); got != 1 {
		t.Errorf("helper runs after the second pass = %d, want still 1 (pending guard)", got)
	}
	if got := recordOf(t, f, "dockops"); got.ApplyPendingDigest != "sha256:new" {
		t.Errorf("ApplyPendingDigest = %q, want the pending marker preserved", got.ApplyPendingDigest)
	}
}

// ---- finalization ----

// selfState installs the self stack's state entry with one image record.
func selfState(t *testing.T, f *fixture, record state.ImageRecord) {
	t.Helper()
	dir := filepath.Join(f.workDir, "host", "dockops")
	st := state.State{
		Hash:         "hash12",
		Status:       state.StatusDeployed,
		StackDir:     dir,
		ComposeFiles: []string{filepath.Join(dir, "compose.yaml")},
		UpdatePolicy: config.UpdatePolicyApply,
		Images:       map[string]state.ImageRecord{applyImage: record},
	}
	if err := f.state.Set("dockops", st); err != nil {
		t.Fatalf("state.Set() error = %v", err)
	}
}

// pendingRecord is the state a hand-over leaves behind.
func pendingRecord() state.ImageRecord {
	return state.ImageRecord{
		Service:            "app",
		CurrentDigest:      "sha256:old",
		RemoteDigest:       "sha256:new",
		UpdateAvailable:    true,
		CheckedAt:          time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC),
		PreviousDigest:     "sha256:old",
		ApplyPendingDigest: "sha256:new",
		PendingAt:          time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC),
	}
}

func writeSelfResult(t *testing.T, f *fixture, res selfupdate.Result) string {
	t.Helper()
	path := selfupdate.ResultPath(filepath.Dir(f.state.Path()))
	if err := selfupdate.WriteAtomic(path, res); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	return path
}

func TestPhase12FinalizeOK(t *testing.T) {
	f := newFixture(t, updateRoot)
	selfState(t, f, pendingRecord())
	rec := &recordingNotifier{}
	r := f.phase12Reconciler(f.compose, &fakeResolver{}, rec, nil, "dockops")
	resultPath := writeSelfResult(t, f, selfupdate.Result{Status: selfupdate.StatusOK, Digest: "sha256:new", At: time.Now().UTC()})

	r.finalizeSelfUpdate(context.Background())

	got := recordOf(t, f, "dockops")
	if got.CurrentDigest != "sha256:new" || got.UpdateAvailable {
		t.Errorf("record = %+v, want the new digest and no pending update", got)
	}
	if got.ApplyPendingDigest != "" || !got.PendingAt.IsZero() {
		t.Errorf("pending fields = %q/%v, want cleared", got.ApplyPendingDigest, got.PendingAt)
	}
	if got.AppliedAt.IsZero() {
		t.Error("AppliedAt is zero, want the finalization time")
	}
	if got.ApplyError != "" || got.ApplyBlockedDigest != "" {
		t.Errorf("record = %+v, want no error and no blocked digest", got)
	}
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Errorf("result file still exists (stat err = %v), want it removed", err)
	}
	events, _ := rec.snapshot()
	if len(events) != 1 || events[0].Kind != config.NotificationUpdateApplied {
		t.Fatalf("events = %+v, want exactly one update_applied", events)
	}
	if events[0].Stack != "dockops" || events[0].Stage != stageApply || events[0].Hash != "hash12" {
		t.Errorf("event = %+v, want stack dockops at stage apply with the state hash", events[0])
	}
	if !strings.Contains(events[0].Err, "sha256:old") || !strings.Contains(events[0].Err, "sha256:new") {
		t.Errorf("event text = %q, want both digests", events[0].Err)
	}
}

func TestPhase12FinalizeRolledBack(t *testing.T) {
	f := newFixture(t, updateRoot)
	selfState(t, f, pendingRecord())
	rec := &recordingNotifier{}
	r := f.phase12Reconciler(f.compose, &fakeResolver{}, rec, nil, "dockops")
	resultPath := writeSelfResult(t, f, selfupdate.Result{
		Status: selfupdate.StatusRolledBack, Digest: "sha256:new",
		Error: "container dockops-app-1 is unhealthy", At: time.Now().UTC(),
	})

	r.finalizeSelfUpdate(context.Background())

	got := recordOf(t, f, "dockops")
	if got.ApplyBlockedDigest != "sha256:new" {
		t.Errorf("ApplyBlockedDigest = %q, want the failed digest", got.ApplyBlockedDigest)
	}
	if !strings.Contains(got.ApplyError, "unhealthy") {
		t.Errorf("ApplyError = %q, want the helper's error text", got.ApplyError)
	}
	if got.ApplyPendingDigest != "" {
		t.Errorf("ApplyPendingDigest = %q, want cleared", got.ApplyPendingDigest)
	}
	if got.RolledBackAt.IsZero() {
		t.Error("RolledBackAt is zero, want the rollback time")
	}
	if got.CurrentDigest != "sha256:old" {
		t.Errorf("CurrentDigest = %q, want the digest the rollback restored", got.CurrentDigest)
	}
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Errorf("result file still exists (stat err = %v), want it removed", err)
	}
	events, _ := rec.snapshot()
	if len(events) != 1 || events[0].Kind != config.NotificationUpdateFailed || events[0].Stage != stageHealthGate {
		t.Fatalf("events = %+v, want one update_failed at stage health_gate", events)
	}
	if !strings.Contains(events[0].Err, "unhealthy") {
		t.Errorf("event text = %q, want the failure", events[0].Err)
	}
}

func TestPhase12FinalizeRollbackFailed(t *testing.T) {
	f := newFixture(t, updateRoot)
	selfState(t, f, pendingRecord())
	rec := &recordingNotifier{}
	r := f.phase12Reconciler(f.compose, &fakeResolver{}, rec, nil, "dockops")
	writeSelfResult(t, f, selfupdate.Result{
		Status: selfupdate.StatusRollbackFailed, Digest: "sha256:new",
		Error: "container dockops-app-1 is unhealthy; rollback: compose up failed", At: time.Now().UTC(),
	})

	r.finalizeSelfUpdate(context.Background())

	got := recordOf(t, f, "dockops")
	if got.ApplyBlockedDigest != "sha256:new" {
		t.Errorf("ApplyBlockedDigest = %q, want the failed digest", got.ApplyBlockedDigest)
	}
	if !strings.Contains(got.ApplyError, selfupdate.RollbackFailedTag) {
		t.Errorf("ApplyError = %q, want the %q marker", got.ApplyError, selfupdate.RollbackFailedTag)
	}
	if got.ApplyPendingDigest != "" {
		t.Errorf("ApplyPendingDigest = %q, want cleared", got.ApplyPendingDigest)
	}
	events, _ := rec.snapshot()
	if len(events) != 1 || events[0].Kind != config.NotificationUpdateFailed || events[0].Stage != stageRollback {
		t.Fatalf("events = %+v, want one update_failed at stage rollback", events)
	}
	if !strings.Contains(events[0].Err, selfupdate.RollbackFailedTag) {
		t.Errorf("event text = %q, want the rollback marker", events[0].Err)
	}
}

func TestPhase12FinalizeNoResultDigestMatchesTarget(t *testing.T) {
	f := newFixture(t, updateRoot)
	selfState(t, f, pendingRecord())
	// The helper died, but the tag already points at the target.
	f.compose.digests = map[string]string{applyImage: "sha256:new"}
	rec := &recordingNotifier{}
	r := f.phase12Reconciler(f.compose, &fakeResolver{}, rec, nil, "dockops")

	r.finalizeSelfUpdate(context.Background())

	got := recordOf(t, f, "dockops")
	if got.CurrentDigest != "sha256:new" || got.UpdateAvailable {
		t.Errorf("record = %+v, want the target digest as current", got)
	}
	if got.ApplyPendingDigest != "" || got.AppliedAt.IsZero() {
		t.Errorf("record = %+v, want finalized and applied_at set", got)
	}
	events, _ := rec.snapshot()
	if len(events) != 1 || events[0].Kind != config.NotificationUpdateApplied {
		t.Fatalf("events = %+v, want one update_applied", events)
	}
}

func TestPhase12FinalizeNoResultDigestIsPrevious(t *testing.T) {
	f := newFixture(t, updateRoot)
	selfState(t, f, pendingRecord())
	// Nothing moved: the helper never recreated the stack.
	f.compose.digests = map[string]string{applyImage: "sha256:old"}
	rec := &recordingNotifier{}
	r := f.phase12Reconciler(f.compose, &fakeResolver{}, rec, nil, "dockops")

	r.finalizeSelfUpdate(context.Background())

	got := recordOf(t, f, "dockops")
	if got.ApplyBlockedDigest != "sha256:new" {
		t.Errorf("ApplyBlockedDigest = %q, want the pending digest blocked", got.ApplyBlockedDigest)
	}
	if got.ApplyPendingDigest != "" {
		t.Errorf("ApplyPendingDigest = %q, want cleared (the attempt is over)", got.ApplyPendingDigest)
	}
	if got.ApplyError == "" {
		t.Error("ApplyError is empty, want the reason")
	}
	if got.CurrentDigest != "sha256:old" {
		t.Errorf("CurrentDigest = %q, want unchanged", got.CurrentDigest)
	}
	events, _ := rec.snapshot()
	if len(events) != 1 || events[0].Kind != config.NotificationUpdateFailed || events[0].Stage != stageComposeUp {
		t.Fatalf("events = %+v, want one update_failed at stage compose_up", events)
	}
}

func TestPhase12FinalizeUnknownDigestStaysPending(t *testing.T) {
	f := newFixture(t, updateRoot)
	selfState(t, f, pendingRecord())
	// Neither digest: the helper may still be recreating.
	f.compose.digests = map[string]string{applyImage: "sha256:third"}
	r := f.phase12Reconciler(f.compose, &fakeResolver{}, &recordingNotifier{}, nil, "dockops")

	r.finalizeSelfUpdate(context.Background())

	got := recordOf(t, f, "dockops")
	if got.ApplyPendingDigest != "sha256:new" {
		t.Errorf("ApplyPendingDigest = %q, want it kept", got.ApplyPendingDigest)
	}
	if got.ApplyBlockedDigest != "" || got.ApplyError != "" {
		t.Errorf("record = %+v, want no verdict while the digest is unknown", got)
	}
}

func TestPhase12FinalizeWithoutStateEntryKeepsResult(t *testing.T) {
	f := newFixture(t, updateRoot)
	// No state entry at all: the result cannot be attributed to a record and
	// is therefore left untouched (nothing is lost).
	resultPath := writeSelfResult(t, f, selfupdate.Result{Status: selfupdate.StatusOK, Digest: "sha256:new"})
	r := f.phase12Reconciler(f.compose, &fakeResolver{}, &recordingNotifier{}, nil, "dockops")

	r.finalizeSelfUpdate(context.Background())

	if _, err := os.Stat(resultPath); err != nil {
		t.Errorf("stat result = %v, want the file kept without a state entry", err)
	}
}

func TestPhase12FinalizeNeverFailsThePass(t *testing.T) {
	f := newFixture(t, updateRoot)
	selfState(t, f, pendingRecord())
	// Both digest sources fail: the finalization must log and give up.
	f.compose.digestErr = context.DeadlineExceeded
	r := f.phase12Reconciler(f.compose, &fakeResolver{}, &recordingNotifier{}, nil, "dockops")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v, want the finalization to swallow its problems", err)
	}
	if got := recordOf(t, f, "dockops"); got.ApplyPendingDigest != "sha256:new" {
		t.Errorf("ApplyPendingDigest = %q, want it kept when the digest is unreadable", got.ApplyPendingDigest)
	}
}
