package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/notify"
	"github.com/local/dockops/internal/selfupdate"
	"github.com/local/dockops/internal/state"
)

// phase13SelfImage is the helper image the phase-13 fixtures hand over with —
// the agent's own build in production (DOCKOPS_SELF_IMAGE or the inspect
// answer at startup).
const phase13SelfImage = "ghcr.io/x/dockops:test"

// selfDeployStackYAML is the self stack of the deploy tests: a profile and an
// ignored service, so the plan has to carry both (the profile as --profile,
// the service filter as the positional list).
const selfDeployStackYAML = `name: dockops
profiles:
  - sops
ignore_services:
  - db
`

// phase13Reconciler wires the fixture's fakes into a reconciler with the
// deploy hand-over configured: self-update on, a known helper image and the
// helper runner injected (no real docker).
func (f *fixture) phase13Reconciler(rec notify.Notifier, helper func(context.Context, []string) error, selfImage string) *Reconciler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(Deps{
		RepoURL:     "https://example.invalid/host.git",
		Branch:      "main",
		WorkDir:     f.workDir,
		State:       f.state,
		Logger:      logger,
		Git:         f.git,
		Compose:     f.compose,
		Hooks:       f.hooks,
		Sops:        f.sops.decrypt,
		NewVault:    f.vault.make,
		Notifier:    rec,
		Registry:    &fakeResolver{},
		SelfProject: "dockops",
		SelfUpdate:  true,
		SelfImage:   selfImage,
	})
	r.healthPoll = time.Millisecond
	r.selfContainer = testSelfContainer
	r.runHelper = helper
	return r
}

// selfDeployFixture is the recurring phase-13 setup: the self stack exists in
// the configuration and has never been deployed (the adoption case — the
// dangerous one, because compose removes the old container before it creates
// the new one).
func selfDeployFixture(t *testing.T) (*fixture, *recordingNotifier, *helperRun) {
	t.Helper()
	f := newFixture(t, updateRoot)
	f.addLocalStack("dockops", selfDeployStackYAML)
	return f, &recordingNotifier{}, &helperRun{}
}

// mustEvents returns the recorded notification events (nil when none).
func mustEvents(t *testing.T, rec *recordingNotifier) []notify.Event {
	t.Helper()
	events, _ := rec.snapshot()
	return events
}

// deployState reads the self stack's state entry, failing when it is missing.
func deployState(t *testing.T, f *fixture) state.State {
	t.Helper()
	st, ok := f.state.Get("dockops")
	if !ok {
		t.Fatal("state has no entry for dockops")
	}
	return st
}

func TestPhase13SelfDeployHandsOverToHelper(t *testing.T) {
	f, rec, helper := selfDeployFixture(t)
	r := f.phase13Reconciler(rec, helper.run, phase13SelfImage)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// The agent itself recreates nothing: no in-process up.
	if len(f.compose.ups) != 0 {
		t.Errorf("compose.Up calls = %+v, want none (the helper deploys, not the agent)", f.compose.ups)
	}
	if got := helper.count(); got != 1 {
		t.Fatalf("helper runs = %d, want exactly one", got)
	}
	args := helper.lastArgs()
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run -d --rm",
		"--name dockops-selfupdate-dockops-",
		"--volumes-from " + testSelfContainer,
		phase13SelfImage + " -selfupdate-helper -plan " + planPath(f),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("helper args = %q, want it to contain %q", joined, want)
		}
	}
	// The helper image is the agent's own build, never the stack's image.
	if strings.Contains(joined, "ghcr.io/x/app") {
		t.Errorf("helper args = %q, want the agent image, not a stack target", joined)
	}

	// The plan describes the deploy completely.
	plan, err := selfupdate.ReadPlan(planPath(f))
	if err != nil {
		t.Fatalf("ReadPlan() error = %v", err)
	}
	stackDir := filepath.Join(f.workDir, "host", "dockops")
	if plan.Kind != selfupdate.KindDeploy {
		t.Errorf("plan.Kind = %q, want %q", plan.Kind, selfupdate.KindDeploy)
	}
	if plan.Project != "dockops" || plan.StackDir != stackDir {
		t.Errorf("plan = %+v, want project dockops at %s", plan, stackDir)
	}
	if got, want := plan.EnvFile, filepath.Join(stackDir, ".env"); got != want {
		t.Errorf("plan.EnvFile = %q, want %q", got, want)
	}
	if len(plan.Profiles) != 1 || plan.Profiles[0] != "sops" {
		t.Errorf("plan.Profiles = %v, want [sops]", plan.Profiles)
	}
	// The fixture's compose file has one service (app); ignore_services names
	// a service that does not exist, so the filtered list is exactly [app] —
	// the same list compose.Up would have passed.
	if len(plan.Services) != 1 || plan.Services[0] != "app" {
		t.Errorf("plan.Services = %v, want [app] (the upServices result)", plan.Services)
	}
	if len(plan.Targets) != 0 {
		t.Errorf("plan.Targets = %+v, want none for a deploy", plan.Targets)
	}
	if plan.DeployHash == "" {
		t.Error("plan.DeployHash is empty, want the stack hash")
	}
	if plan.GraceSeconds != selfUpdateGraceSeconds {
		t.Errorf("plan.GraceSeconds = %d, want %d", plan.GraceSeconds, selfUpdateGraceSeconds)
	}
	if plan.HealthTimeoutSeconds <= 0 || plan.HealthPollSeconds <= 0 {
		t.Errorf("plan health deadlines = %d/%d, want > 0", plan.HealthTimeoutSeconds, plan.HealthPollSeconds)
	}
	// The generated .env is what the helper will read; it must already exist.
	if _, err := os.Stat(filepath.Join(stackDir, ".env")); err != nil {
		t.Errorf("stack .env stat = %v, want the generated file (the helper reads it)", err)
	}

	// The honest interim state: pending hash and time, no deployment booked.
	st := deployState(t, f)
	if st.DeployPendingHash != plan.DeployHash {
		t.Errorf("DeployPendingHash = %q, want %q", st.DeployPendingHash, plan.DeployHash)
	}
	if st.DeployPendingAt.IsZero() {
		t.Error("DeployPendingAt is zero, want the hand-over time")
	}
	if st.Hash == plan.DeployHash || st.Status == state.StatusDeployed {
		t.Errorf("state = %+v, want the deploy NOT booked as done", st)
	}
	if st.StackDir != stackDir || st.ComposeFile != filepath.Join(stackDir, "compose.yaml") {
		t.Errorf("state traces = %q/%q, want the stack's files for prune", st.StackDir, st.ComposeFile)
	}
	// No verdict is reported: only the process that knows the outcome may
	// report it, and that is the next agent start.
	events, _ := rec.snapshot()
	for _, ev := range events {
		if ev.Kind == config.NotificationDeployFailed {
			t.Errorf("event = %+v, want no deploy_failed on a clean hand-over", ev)
		}
	}
}

// TestPhase13ChangedHashIsHandedOver is the second phase-13 case next to the
// adoption: the stack is deployed already and its hash moved (compose file,
// env, profile). The recreate still belongs to the helper.
func TestPhase13ChangedHashIsHandedOver(t *testing.T) {
	f, rec, helper := selfDeployFixture(t)
	dir := filepath.Join(f.workDir, "host", "dockops")
	if err := f.state.Set("dockops", state.State{
		Hash:         "hash-old",
		Status:       state.StatusDeployed,
		StackDir:     dir,
		ComposeFiles: []string{filepath.Join(dir, "compose.yaml")},
	}); err != nil {
		t.Fatalf("state.Set() error = %v", err)
	}
	r := f.phase13Reconciler(rec, helper.run, phase13SelfImage)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(f.compose.ups) != 0 {
		t.Errorf("compose.Up calls = %+v, want none for the self stack", f.compose.ups)
	}
	if got := helper.count(); got != 1 {
		t.Fatalf("helper runs = %d, want one", got)
	}
	plan, err := selfupdate.ReadPlan(planPath(f))
	if err != nil {
		t.Fatalf("ReadPlan() error = %v", err)
	}
	st := deployState(t, f)
	if plan.DeployHash == "" || plan.DeployHash == "hash-old" {
		t.Errorf("plan.DeployHash = %q, want the new hash", plan.DeployHash)
	}
	if st.Hash != "hash-old" || st.Status != state.StatusDeployed {
		t.Errorf("state = %+v, want the previous deployment still recorded", st)
	}
	if st.DeployPendingHash != plan.DeployHash {
		t.Errorf("DeployPendingHash = %q, want %q", st.DeployPendingHash, plan.DeployHash)
	}
}

func TestPhase13PendingDeployIsNotHandedOverTwice(t *testing.T) {
	f, rec, helper := selfDeployFixture(t)
	r := f.phase13Reconciler(rec, helper.run, phase13SelfImage)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if got := helper.count(); got != 1 {
		t.Fatalf("helper runs after the first pass = %d, want 1", got)
	}
	before := deployState(t, f)

	// The helper is still working (no result file): the second pass must
	// neither start a second helper nor touch the stack.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if got := helper.count(); got != 1 {
		t.Errorf("helper runs after the second pass = %d, want still 1 (pending guard)", got)
	}
	if len(f.compose.ups) != 0 {
		t.Errorf("compose.Up calls = %+v, want none while the deploy is pending", f.compose.ups)
	}
	after := deployState(t, f)
	if after.DeployPendingHash != before.DeployPendingHash || !after.DeployPendingAt.Equal(before.DeployPendingAt) {
		t.Errorf("pending = %q/%s, want the first hand-over untouched (%q/%s)",
			after.DeployPendingHash, after.DeployPendingAt, before.DeployPendingHash, before.DeployPendingAt)
	}
}

func TestPhase13HelperStartFailureClearsPending(t *testing.T) {
	f, rec, helper := selfDeployFixture(t)
	helper.err = errors.New("docker run boom")
	r := f.phase13Reconciler(rec, helper.run, phase13SelfImage)

	err := r.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile() error = nil, want the hand-over failure")
	}
	if !strings.Contains(err.Error(), "start helper") {
		t.Errorf("Reconcile() error = %v, want it to name the failed helper start", err)
	}
	st := deployState(t, f)
	if st.DeployPendingHash != "" || !st.DeployPendingAt.IsZero() {
		t.Errorf("pending = %q/%s, want it rolled back (nothing was recreated)",
			st.DeployPendingHash, st.DeployPendingAt)
	}
	if st.Status == state.StatusDeployed {
		t.Errorf("status = %q, want a retryable, not-deployed entry", st.Status)
	}
	if len(f.compose.ups) != 0 {
		t.Errorf("compose.Up calls = %+v, want none", f.compose.ups)
	}
	events, _ := rec.snapshot()
	var failed []string
	for _, ev := range events {
		if ev.Kind == config.NotificationDeployFailed {
			failed = append(failed, ev.Stage+": "+ev.Err)
		}
	}
	if len(failed) != 1 || !strings.Contains(failed[0], stageComposeUp) {
		t.Fatalf("deploy_failed events = %v, want exactly one at stage %s", failed, stageComposeUp)
	}
	if !strings.Contains(failed[0], "docker run boom") {
		t.Errorf("event = %q, want the helper failure text", failed[0])
	}
}

func TestPhase13NonSelfStackIsDeployedInProcess(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", "name: web\n")
	rec := &recordingNotifier{}
	helper := &helperRun{}
	r := f.phase13Reconciler(rec, helper.run, phase13SelfImage)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got, want := len(f.compose.ups), 1; got != want {
		t.Errorf("compose.Up calls = %d, want %d (in-process deploy)", got, want)
	}
	if got := helper.count(); got != 0 {
		t.Errorf("helper runs = %d, want none for a non-self stack", got)
	}
	if _, err := os.Stat(planPath(f)); !os.IsNotExist(err) {
		t.Errorf("plan file exists (stat err = %v), want none for a non-self stack", err)
	}
	entry, _ := f.state.Get("web")
	if entry.Status != state.StatusDeployed || entry.DeployPendingHash != "" {
		t.Errorf("state = %+v, want the ordinary deployed entry", entry)
	}
}

func TestPhase13UnknownHelperImageDeploysInProcess(t *testing.T) {
	// Documented boundary (the one deliberate deviation from the spec's
	// "unknown image is a hard error"): without a helper image no correct
	// helper can start, and the image path of the same pass depends on the
	// records a deploy writes — so the deploy falls back to the in-process
	// recreate it always used, loudly, instead of leaving the stack stale.
	// What must never happen: a helper started with a guessed image.
	f, rec, helper := selfDeployFixture(t)
	r := f.phase13Reconciler(rec, helper.run, "")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := helper.count(); got != 0 {
		t.Errorf("helper runs = %d, want none without a helper image", got)
	}
	if got := len(f.compose.ups); got != 1 {
		t.Errorf("compose.Up calls = %d, want the in-process fallback", got)
	}
	if _, err := os.Stat(planPath(f)); !os.IsNotExist(err) {
		t.Errorf("plan file exists (stat err = %v), want none", err)
	}
	if st, ok := f.state.Get("dockops"); ok && (st.DeployPendingHash != "" || !st.DeployPendingAt.IsZero()) {
		t.Errorf("pending = %q/%s, want none", st.DeployPendingHash, st.DeployPendingAt)
	}
	for _, ev := range mustEvents(t, rec) {
		if ev.Kind == config.NotificationDeployFailed {
			t.Errorf("event = %+v, want none (the fallback deployed)", ev)
		}
	}
}

// TestPhase13DeployPendingBlocksTheImageHandOver: while a deploy is in
// flight, the same stack must not get a second helper — the image path has to
// wait for the deploy's result.
func TestPhase13DeployPendingBlocksTheImageHandOver(t *testing.T) {
	f, resolver, rec, helper := selfFixture(t)
	r := f.phase12Reconciler(f.compose, resolver, rec, helper.run, "dockops")
	r.selfImage = phase13SelfImage
	selfState(t, f, state.ImageRecord{
		Service: "app", CurrentDigest: "sha256:old", RemoteDigest: "sha256:old",
	})
	st, _ := f.state.Get("dockops")
	st.DeployPendingHash = st.Hash
	st.DeployPendingAt = time.Now().UTC()
	if err := f.state.Set("dockops", st); err != nil {
		t.Fatalf("state.Set() error = %v", err)
	}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := helper.count(); got != 0 {
		t.Errorf("helper runs = %d, want none (the deploy is still in flight)", got)
	}
	if len(f.compose.pulls) != 0 {
		t.Errorf("pulls = %+v, want none while a deploy is pending", f.compose.pulls)
	}
	if len(f.compose.upOverrides) != 0 {
		t.Errorf("UpOverride calls = %+v, want none while a deploy is pending", f.compose.upOverrides)
	}
	if got, _ := f.state.Get("dockops"); got.DeployPendingHash == "" {
		t.Error("DeployPendingHash was cleared, want it kept until the helper reports")
	}
}

// ---- finalization ----

// deployPending installs the self stack's state as a hand-over leaves it.
func deployPending(t *testing.T, f *fixture, pendingAt time.Time) state.State {
	t.Helper()
	dir := filepath.Join(f.workDir, "host", "dockops")
	st := state.State{
		Hash:              "hash-old",
		Status:            state.StatusDeployed,
		StackDir:          dir,
		ComposeFiles:      []string{filepath.Join(dir, "compose.yaml")},
		UpdatePolicy:      config.UpdatePolicyApply,
		DeployPendingHash: "hash-new",
		DeployPendingAt:   pendingAt,
	}
	if err := f.state.Set("dockops", st); err != nil {
		t.Fatalf("state.Set() error = %v", err)
	}
	return st
}

func TestPhase13FinalizeDeployOK(t *testing.T) {
	f := newFixture(t, updateRoot)
	deployPending(t, f, time.Now().UTC())
	rec := &recordingNotifier{}
	r := f.phase13Reconciler(rec, nil, phase13SelfImage)
	resultPath := writeSelfResult(t, f, selfupdate.Result{
		Kind: selfupdate.KindDeploy, Status: selfupdate.StatusOK,
		DeployHash: "hash-new", At: time.Now().UTC(),
	})

	r.finalizeSelfUpdate(context.Background())

	st := deployState(t, f)
	if st.Hash != "hash-new" || st.Status != state.StatusDeployed {
		t.Errorf("state = %+v, want hash-new deployed", st)
	}
	if st.DeployPendingHash != "" || !st.DeployPendingAt.IsZero() {
		t.Errorf("pending = %q/%s, want cleared", st.DeployPendingHash, st.DeployPendingAt)
	}
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Errorf("result file still exists (stat err = %v), want it removed", err)
	}
	events, recoveries := rec.snapshot()
	for _, ev := range events {
		if ev.Kind == config.NotificationDeployFailed {
			t.Errorf("event = %+v, want no deploy_failed after a successful deploy", ev)
		}
	}
	if len(recoveries) != 1 || recoveries[0] != config.NotificationDeployFailed+"/dockops" {
		t.Errorf("recoveries = %v, want the deploy_failed key cleared", recoveries)
	}
}

func TestPhase13FinalizeDeployFailed(t *testing.T) {
	f := newFixture(t, updateRoot)
	deployPending(t, f, time.Now().UTC())
	rec := &recordingNotifier{}
	r := f.phase13Reconciler(rec, nil, phase13SelfImage)
	resultPath := writeSelfResult(t, f, selfupdate.Result{
		Kind: selfupdate.KindDeploy, Status: selfupdate.StatusFailed,
		DeployHash: "hash-new", Error: "compose up: service dockops failed to start",
		At: time.Now().UTC(),
	})

	r.finalizeSelfUpdate(context.Background())

	st := deployState(t, f)
	if st.Hash != "hash-new" {
		t.Errorf("Hash = %q, want the pending hash (the next pass retries it)", st.Hash)
	}
	if st.Status != state.StatusFailed {
		t.Errorf("Status = %q, want %q", st.Status, state.StatusFailed)
	}
	if st.DeployPendingHash != "" {
		t.Errorf("DeployPendingHash = %q, want cleared", st.DeployPendingHash)
	}
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Errorf("result file still exists (stat err = %v), want it consumed", err)
	}
	events, _ := rec.snapshot()
	if len(events) != 1 || events[0].Kind != config.NotificationDeployFailed {
		t.Fatalf("events = %+v, want exactly one deploy_failed", events)
	}
	if events[0].Stage != stageComposeUp || events[0].Hash != "hash-new" || events[0].Stack != "dockops" {
		t.Errorf("event = %+v, want stack dockops at stage %s for hash-new", events[0], stageComposeUp)
	}
	if !strings.Contains(events[0].Err, "failed to start") {
		t.Errorf("event text = %q, want the helper's error", events[0].Err)
	}
}

func TestPhase13FinalizeDeployRolledBackReportIsLoud(t *testing.T) {
	// A deploy has nothing to roll back to: a rollback verdict for one is a
	// bug, and it must read like one.
	f := newFixture(t, updateRoot)
	deployPending(t, f, time.Now().UTC())
	rec := &recordingNotifier{}
	r := f.phase13Reconciler(rec, nil, phase13SelfImage)
	writeSelfResult(t, f, selfupdate.Result{
		Kind: selfupdate.KindDeploy, Status: selfupdate.StatusRolledBack,
		DeployHash: "hash-new", Error: "compose up: boom", At: time.Now().UTC(),
	})

	r.finalizeSelfUpdate(context.Background())

	st := deployState(t, f)
	if st.Status != state.StatusFailed {
		t.Errorf("Status = %q, want %q", st.Status, state.StatusFailed)
	}
	events, _ := rec.snapshot()
	if len(events) != 1 || events[0].Kind != config.NotificationDeployFailed {
		t.Fatalf("events = %+v, want one deploy_failed", events)
	}
	if !strings.Contains(events[0].Err, selfupdate.RollbackFailedTag) {
		t.Errorf("event text = %q, want the %q marker", events[0].Err, selfupdate.RollbackFailedTag)
	}
}

func TestPhase13FinalizeDeployWithoutResultStaysPendingInsideWindow(t *testing.T) {
	f := newFixture(t, updateRoot)
	deployPending(t, f, time.Now().UTC().Add(-time.Minute))
	rec := &recordingNotifier{}
	r := f.phase13Reconciler(rec, nil, phase13SelfImage)

	r.finalizeSelfUpdate(context.Background())

	st := deployState(t, f)
	if st.DeployPendingHash != "hash-new" {
		t.Errorf("DeployPendingHash = %q, want it kept (the helper may still be working)", st.DeployPendingHash)
	}
	if st.Status != state.StatusDeployed || st.Hash != "hash-old" {
		t.Errorf("state = %+v, want the previous deployment untouched", st)
	}
	if events, _ := rec.snapshot(); len(events) != 0 {
		t.Errorf("events = %+v, want none while the deploy stays pending", events)
	}
}

func TestPhase13FinalizeDeployWithoutResultExpiresOutsideWindow(t *testing.T) {
	f := newFixture(t, updateRoot)
	deployPending(t, f, time.Now().UTC().Add(-time.Hour))
	rec := &recordingNotifier{}
	r := f.phase13Reconciler(rec, nil, phase13SelfImage)

	r.finalizeSelfUpdate(context.Background())

	st := deployState(t, f)
	if st.DeployPendingHash != "" {
		t.Errorf("DeployPendingHash = %q, want cleared (the helper is gone)", st.DeployPendingHash)
	}
	if st.Hash != "hash-new" || st.Status != state.StatusFailed {
		t.Errorf("state = %+v, want hash-new as a failed attempt", st)
	}
	events, _ := rec.snapshot()
	if len(events) != 1 || events[0].Kind != config.NotificationDeployFailed {
		t.Fatalf("events = %+v, want one deploy_failed", events)
	}
	if !strings.Contains(events[0].Err, "no result") {
		t.Errorf("event text = %q, want it to explain the missing result", events[0].Err)
	}
}

func TestPhase13DeployWaitWindowFollowsThePlan(t *testing.T) {
	f := newFixture(t, updateRoot)
	r := f.phase13Reconciler(&recordingNotifier{}, nil, phase13SelfImage)
	stateDir := filepath.Dir(f.state.Path())

	// Without a plan the window falls back to the agent's own deadlines.
	if got, want := r.deployWaitWindow(stateDir), time.Duration(selfUpdateGraceSeconds)*time.Second+config.DefaultHealthTimeout+deployFinalizeSlack; got != want {
		t.Errorf("deployWaitWindow() without a plan = %s, want %s", got, want)
	}

	plan := selfupdate.Plan{
		Version: selfupdate.Version, Kind: selfupdate.KindDeploy,
		Project: "dockops", StackDir: "/s", ComposeFiles: []string{"compose.yaml"},
		GraceSeconds: 30, HealthTimeoutSeconds: 45, HealthPollSeconds: 3,
		DeployHash: "h2",
	}
	if err := selfupdate.WriteAtomic(selfupdate.PlanPath(stateDir), plan); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	if got, want := r.deployWaitWindow(stateDir), 30*time.Second+45*time.Second+deployFinalizeSlack; got != want {
		t.Errorf("deployWaitWindow() = %s, want the plan's deadlines (%s)", got, want)
	}
}

// TestPhase13DeployFinalizationRunsBeforeImagePending pins the order of the
// two kinds of pending: a deploy pending is booked first, even when an image
// record happens to be pending as well.
func TestPhase13DeployFinalizationRunsBeforeImagePending(t *testing.T) {
	f := newFixture(t, updateRoot)
	st := deployPending(t, f, time.Now().UTC())
	st.Images = map[string]state.ImageRecord{
		applyImage: {Service: "app", ApplyPendingDigest: "sha256:new", PreviousDigest: "sha256:old"},
	}
	if err := f.state.Set("dockops", st); err != nil {
		t.Fatalf("state.Set() error = %v", err)
	}
	rec := &recordingNotifier{}
	r := f.phase13Reconciler(rec, nil, phase13SelfImage)
	writeSelfResult(t, f, selfupdate.Result{
		Kind: selfupdate.KindDeploy, Status: selfupdate.StatusOK, DeployHash: "hash-new",
		At: time.Now().UTC(),
	})

	r.finalizeSelfUpdate(context.Background())

	got := deployState(t, f)
	if got.Hash != "hash-new" || got.DeployPendingHash != "" {
		t.Errorf("state = %+v, want the deploy booked and its marker gone", got)
	}
	// The image pending is untouched by the deploy bookkeeping: it is the
	// next pass's business (and needs its own result).
	if img, ok := got.Images[applyImage]; !ok || img.ApplyPendingDigest != "sha256:new" {
		t.Errorf("image record = %+v, want the image pending preserved", got.Images[applyImage])
	}
}
