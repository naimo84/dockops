package reconciler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/local/dockops/internal/compose"
	"github.com/local/dockops/internal/config"
	"github.com/local/dockops/internal/notify"
	"github.com/local/dockops/internal/registry"
	"github.com/local/dockops/internal/state"
)

// applyImage is the image of the phase-11 apply tests; the fake compose
// resolves the stack's single service "app" to it.
const applyImage = "ghcr.io/x/app:latest"

// applyStackYAML is a stack that applies updates, due on every pass (the
// fixture's root uses update_check_interval: 1ns).
const applyStackYAML = `name: web
update_policy: apply
`

// phase11Reconciler wires the fixture's fakes with an injected registry, a
// recording notifier and the given compose executor; the health gate polls
// every millisecond so no test waits for the production interval.
func (f *fixture) phase11Reconciler(composeExec ComposeExecutor, resolver registry.Resolver, rec notify.Notifier) *Reconciler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(Deps{
		RepoURL:  "https://example.invalid/host.git",
		Branch:   "main",
		WorkDir:  f.workDir,
		State:    f.state,
		Logger:   logger,
		Git:      f.git,
		Compose:  composeExec,
		Hooks:    f.hooks,
		Sops:     f.sops.decrypt,
		NewVault: f.vault.make,
		Notifier: rec,
		Registry: resolver,
	})
	r.healthPoll = time.Millisecond
	return r
}

// applyFixture builds the recurring setup: one apply stack whose image moved
// from sha256:old to sha256:new.
func applyFixture(t *testing.T, stackYAML string) (*fixture, *fakeResolver, *recordingNotifier) {
	t.Helper()
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", stackYAML)
	f.compose.images = map[string]string{"app": applyImage}
	f.compose.digests = map[string]string{applyImage: "sha256:old"}
	resolver := &fakeResolver{digests: map[string]string{applyImage: "sha256:new"}}
	return f, resolver, &recordingNotifier{}
}

// gateContainers builds one ps answer for service app.
func gateContainers(health string) []compose.Container {
	return []compose.Container{{Service: "app", Name: "web-app-1", State: "running", Health: health}}
}

// applyRecord returns the image record of the fixture's stack.
func applyRecord(t *testing.T, f *fixture, ref string) state.ImageRecord {
	t.Helper()
	recs := imageState(t, f, "web")
	rec, ok := recs[ref]
	if !ok {
		t.Fatalf("no image record for %s (records: %+v)", ref, recs)
	}
	return rec
}

// singleEvent asserts that exactly one event of kind was recorded and
// returns it. Other kinds are ignored on purpose: the first check of a moved
// tag also raises update_available, which is the phase-10 behaviour and stays
// untouched by the apply step.
func singleEvent(t *testing.T, rec *recordingNotifier, kind string) notify.Event {
	t.Helper()
	events := eventsOfKind(t, rec, kind)
	if len(events) != 1 {
		all, _ := rec.snapshot()
		t.Fatalf("events of kind %s = %+v, want exactly one (all events: %+v)", kind, events, all)
	}
	return events[0]
}

// eventsOfKind filters the recorded events by kind.
func eventsOfKind(t *testing.T, rec *recordingNotifier, kind string) []notify.Event {
	t.Helper()
	all, _ := rec.snapshot()
	out := make([]notify.Event, 0, len(all))
	for _, ev := range all {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// TestPhase11ApplySuccess is the happy path end to end: pull, targeted up,
// gate healthy after two polls, record updated, exactly one update_applied
// event — and a second pass that neither pulls nor up's again.
func TestPhase11ApplySuccess(t *testing.T) {
	f, resolver, rec := applyFixture(t, applyStackYAML)
	f.compose.psFn = func(call int) []compose.Container {
		if call <= 2 {
			return gateContainers("starting")
		}
		return gateContainers("healthy")
	}
	r := f.phase11Reconciler(f.compose, resolver, rec)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if len(f.compose.pulls) != 1 {
		t.Fatalf("pulls = %+v, want exactly one", f.compose.pulls)
	}
	if got, want := f.compose.pulls[0].services, []string{"app"}; !equalStrings(got, want) {
		t.Errorf("pull services = %v, want %v (only the affected service)", got, want)
	}
	if got := f.compose.pulls[0].project; got != "web" {
		t.Errorf("pull project = %q, want web", got)
	}
	if len(f.compose.upOverrides) != 1 {
		t.Fatalf("UpOverride calls = %+v, want exactly one", f.compose.upOverrides)
	}
	up := f.compose.upOverrides[0]
	if len(up.extraFiles) != 0 {
		t.Errorf("up override files = %v, want none for the forward direction", up.extraFiles)
	}
	if !equalStrings(up.services, []string{"app"}) {
		t.Errorf("up services = %v, want [app]", up.services)
	}
	if got := len(f.compose.psCalls); got != 3 {
		t.Errorf("health gate polls = %d, want 3 (two starting, one healthy)", got)
	}

	rec11 := applyRecord(t, f, applyImage)
	if rec11.CurrentDigest != "sha256:new" || rec11.UpdateAvailable {
		t.Errorf("record = %+v, want the new digest and no pending update", rec11)
	}
	if rec11.AppliedAt.IsZero() {
		t.Error("AppliedAt is zero, want the apply time")
	}
	if rec11.ApplyError != "" || rec11.ApplyBlockedDigest != "" || !rec11.RolledBackAt.IsZero() {
		t.Errorf("record = %+v, want no error/block and no rollback", rec11)
	}
	if rec11.PreviousDigest != "sha256:old" {
		t.Errorf("PreviousDigest = %q, want the digest before the apply", rec11.PreviousDigest)
	}

	ev := singleEvent(t, rec, config.NotificationUpdateApplied)
	if ev.Stage != "apply" || ev.Stack != "web" {
		t.Errorf("event = %+v, want update_applied for web at stage apply", ev)
	}
	if !strings.Contains(ev.Err, "sha256:new") {
		t.Errorf("event text = %q, want the applied digest", ev.Err)
	}
	if recoveries, _ := rec.snapshot(); len(recoveries) == 0 {
		t.Error("no recovery recorded, want update_failed cleared after a successful apply")
	}

	// Second pass: the local image now carries the pulled digest, so the
	// check finds nothing to do and the apply step stays silent.
	f.compose.digests[applyImage] = "sha256:new"
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(f.compose.pulls) != 1 {
		t.Errorf("pulls after the second pass = %+v, want still one", f.compose.pulls)
	}
	if len(f.compose.upOverrides) != 1 {
		t.Errorf("UpOverride calls after the second pass = %+v, want still one", f.compose.upOverrides)
	}
	if events := eventsOfKind(t, rec, config.NotificationUpdateApplied); len(events) != 1 {
		t.Errorf("update_applied events after the second pass = %+v, want still one", events)
	}
}

// TestPhase11ApplyRollbackOnUnhealthy: an unhealthy container is a hard gate
// failure — exactly one rollback up with an override file pinning the
// previous digest, the file is removed again, the digest is blocked and the
// event names the health gate.
func TestPhase11ApplyRollbackOnUnhealthy(t *testing.T) {
	f, resolver, rec := applyFixture(t, applyStackYAML)
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

	if len(f.compose.upOverrides) != 2 {
		t.Fatalf("UpOverride calls = %+v, want a forward up and exactly one rollback up", f.compose.upOverrides)
	}
	rollback := f.compose.upOverrides[1]
	if len(rollback.extraFiles) != 1 {
		t.Fatalf("rollback override files = %v, want exactly one", rollback.extraFiles)
	}
	if !equalStrings(rollback.services, []string{"app"}) {
		t.Errorf("rollback services = %v, want [app]", rollback.services)
	}
	if len(rollback.contents) != 1 {
		t.Fatalf("rollback override content = %v, want one snapshot", rollback.contents)
	}
	content := rollback.contents[0]
	if !strings.Contains(content, "image: ghcr.io/x/app@sha256:old") {
		t.Errorf("rollback override =\n%s\nwant an image pin to ghcr.io/x/app@sha256:old", content)
	}
	if !strings.Contains(content, "app:") {
		t.Errorf("rollback override =\n%s\nwant the service as the mapping key", content)
	}
	overridePath := filepath.Join(f.workDir, "host", "web", "dockops-rollback-web.yaml")
	if _, err := os.Stat(overridePath); !os.IsNotExist(err) {
		t.Errorf("rollback override %s still exists, want it removed after the attempt", overridePath)
	}

	got := applyRecord(t, f, applyImage)
	if got.CurrentDigest != "sha256:old" {
		t.Errorf("CurrentDigest = %q, want the old digest (rolled back)", got.CurrentDigest)
	}
	if !got.UpdateAvailable {
		t.Error("UpdateAvailable = false, want true after a failed apply")
	}
	if got.ApplyBlockedDigest != "sha256:new" {
		t.Errorf("ApplyBlockedDigest = %q, want the digest that failed", got.ApplyBlockedDigest)
	}
	if got.RolledBackAt.IsZero() {
		t.Error("RolledBackAt is zero, want the rollback time")
	}
	if got.ApplyError == "" {
		t.Error("ApplyError is empty, want the gate failure")
	}

	ev := singleEvent(t, rec, config.NotificationUpdateFailed)
	if ev.Stage != "health_gate" {
		t.Errorf("event = %+v, want update_failed at stage health_gate", ev)
	}
	if !strings.Contains(ev.Err, "rollback to sha256:old") {
		t.Errorf("event text = %q, want the rollback target", ev.Err)
	}

	// Third pass: the blocked digest degrades apply to notify — no second
	// pull, no second up, no repeated alarm.
	pullsBefore := len(f.compose.pulls)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(f.compose.pulls) != pullsBefore {
		t.Errorf("pulls after the blocked pass = %+v, want no new pull", f.compose.pulls)
	}
	if len(f.compose.upOverrides) != 2 {
		t.Errorf("UpOverride calls after the blocked pass = %+v, want no new up", f.compose.upOverrides)
	}
	if events := eventsOfKind(t, rec, config.NotificationUpdateFailed); len(events) != 1 {
		t.Errorf("update_failed events after the blocked pass = %+v, want no duplicate", events)
	}
}

// TestPhase11ApplyGateTimeout: a gate that never leaves "starting" times out
// and rolls back with stage health_gate, like the hard failure.
func TestPhase11ApplyGateTimeout(t *testing.T) {
	f, resolver, rec := applyFixture(t, applyStackYAML+"health_timeout: 5ms\n")
	rollbackStarted := false
	f.compose.onUpOverride = func(call int) {
		if call == 2 {
			rollbackStarted = true
		}
	}
	f.compose.psFn = func(int) []compose.Container {
		if rollbackStarted {
			return gateContainers("healthy")
		}
		return gateContainers("starting")
	}
	r := f.phase11Reconciler(f.compose, resolver, rec)

	start := time.Now()
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("pass took %v, want the 5ms health_timeout to bound the gate", elapsed)
	}

	if len(f.compose.upOverrides) != 2 {
		t.Fatalf("UpOverride calls = %+v, want a forward up and a rollback", f.compose.upOverrides)
	}
	got := applyRecord(t, f, applyImage)
	if got.ApplyBlockedDigest != "sha256:new" || !strings.Contains(got.ApplyError, "timed out") {
		t.Errorf("record = %+v, want the digest blocked with a timeout error", got)
	}
	ev := singleEvent(t, rec, config.NotificationUpdateFailed)
	if ev.Stage != "health_gate" {
		t.Errorf("event = %+v, want update_failed at stage health_gate", ev)
	}
}

// TestPhase11ApplyRollbackFails: the case a human is needed — the rollback
// itself fails, so the event escalates to stage rollback and the record keeps
// both errors.
func TestPhase11ApplyRollbackFails(t *testing.T) {
	f, resolver, rec := applyFixture(t, applyStackYAML)
	f.compose.psFn = func(int) []compose.Container { return gateContainers("unhealthy") }
	f.compose.upOverrideErrs = []error{nil, io.ErrUnexpectedEOF}
	r := f.phase11Reconciler(f.compose, resolver, rec)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := applyRecord(t, f, applyImage)
	if !strings.Contains(got.ApplyError, "rollback FAILED") {
		t.Errorf("ApplyError = %q, want the failed rollback named", got.ApplyError)
	}
	if got.ApplyBlockedDigest != "sha256:new" {
		t.Errorf("ApplyBlockedDigest = %q, want the failed digest blocked", got.ApplyBlockedDigest)
	}
	ev := singleEvent(t, rec, config.NotificationUpdateFailed)
	if ev.Stage != "rollback" {
		t.Errorf("event = %+v, want update_failed at stage rollback", ev)
	}
	if !strings.Contains(ev.Err, "rollback FAILED") {
		t.Errorf("event text = %q, want it marked as a failed rollback", ev.Err)
	}
}

// TestPhase11ApplyBlockedDigestNoRetry: after a failed attempt the blocked
// digest survives the next re-check (the record is rebuilt on every check)
// and update_policy: apply degrades to notify — no pull, no up, no alarm.
func TestPhase11ApplyBlockedDigestNoRetry(t *testing.T) {
	f, resolver, rec := applyFixture(t, applyStackYAML)
	f.compose.psFn = func(call int) []compose.Container {
		if call == 1 {
			return gateContainers("unhealthy")
		}
		return gateContainers("healthy")
	}
	r := f.phase11Reconciler(f.compose, resolver, rec)

	// First pass: the gate fails, the rollback succeeds, the digest is
	// blocked.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := applyRecord(t, f, applyImage); got.ApplyBlockedDigest != "sha256:new" {
		t.Fatalf("ApplyBlockedDigest = %q, want the failed digest", got.ApplyBlockedDigest)
	}

	// Second pass: same digest, same records — but nothing happens.
	f.compose.pulls = nil
	f.compose.upOverrides = nil
	f.compose.psCalls = nil
	f.compose.failUpFor = ""
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(f.compose.pulls) != 0 {
		t.Errorf("pulls = %+v, want none for a blocked digest", f.compose.pulls)
	}
	if len(f.compose.upOverrides) != 0 {
		t.Errorf("UpOverride calls = %+v, want none for a blocked digest", f.compose.upOverrides)
	}
	if got := len(f.compose.psCalls); got != 0 {
		t.Errorf("health gate polls = %d, want none", got)
	}
	if events := eventsOfKind(t, rec, config.NotificationUpdateFailed); len(events) != 1 {
		t.Errorf("update_failed events = %+v, want no repeated alarm", events)
	}
	got := applyRecord(t, f, applyImage)
	if got.ApplyBlockedDigest != "sha256:new" {
		t.Errorf("ApplyBlockedDigest = %q, want the block to survive the re-check", got.ApplyBlockedDigest)
	}
	if got.ApplyError == "" {
		t.Error("ApplyError is empty, want the recorded failure to survive the re-check")
	}
}

// TestPhase11NotifyPolicyNeverApplies: update_policy notify keeps the pass
// read-only.
func TestPhase11NotifyPolicyNeverApplies(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", "name: web\n")
	f.compose.images = map[string]string{"app": applyImage}
	f.compose.digests = map[string]string{applyImage: "sha256:old"}
	resolver := &fakeResolver{digests: map[string]string{applyImage: "sha256:new"}}
	rec := &recordingNotifier{}
	r := f.phase11Reconciler(f.compose, resolver, rec)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(f.compose.pulls) != 0 || len(f.compose.upOverrides) != 0 || len(f.compose.psCalls) != 0 {
		t.Errorf("apply calls = pulls %v, ups %v, ps %v; want none for notify",
			f.compose.pulls, f.compose.upOverrides, f.compose.psCalls)
	}
	// The update is reported, not applied.
	rec11 := applyRecord(t, f, applyImage)
	if !rec11.UpdateAvailable || rec11.CurrentDigest != "sha256:old" {
		t.Errorf("record = %+v, want the pending update untouched", rec11)
	}
}

// TestPhase11RegistryPortKeepsRollbackReference: the rollback pin must not
// lose a registry port to the tag-stripping rule.
func TestPhase11RegistryPortKeepsRollbackReference(t *testing.T) {
	const portImage = "gitea.naimo84.dev:3000/a/b:latest"
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", applyStackYAML)
	f.compose.images = map[string]string{"app": portImage}
	f.compose.digests = map[string]string{portImage: "sha256:old"}
	f.compose.psFn = func(call int) []compose.Container {
		if call == 1 {
			return gateContainers("unhealthy")
		}
		return gateContainers("healthy")
	}
	resolver := &fakeResolver{digests: map[string]string{portImage: "sha256:new"}}
	r := f.phase11Reconciler(f.compose, resolver, &recordingNotifier{})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(f.compose.upOverrides) != 2 || len(f.compose.upOverrides[1].contents) != 1 {
		t.Fatalf("UpOverride calls = %+v, want a rollback with one override snapshot", f.compose.upOverrides)
	}
	if got, want := f.compose.upOverrides[1].contents[0], "gitea.naimo84.dev:3000/a/b@sha256:old"; !strings.Contains(got, want) {
		t.Errorf("rollback override =\n%s\nwant it to contain %s", got, want)
	}
}

// TestPhase11MissingServiceFieldIsNotGreen: a ps answer without service names
// cannot attribute its containers. The gate must fail (and roll back) instead
// of accepting an unchecked service.
func TestPhase11MissingServiceFieldIsNotGreen(t *testing.T) {
	f, resolver, rec := applyFixture(t, applyStackYAML)
	f.compose.psFn = func(call int) []compose.Container {
		if call == 1 {
			// Old compose version: no Service key, container running and
			// healthy — the trap the briefing warns about.
			return []compose.Container{{Name: "web-app-1", State: "running", Health: "healthy"}}
		}
		return gateContainers("healthy")
	}
	r := f.phase11Reconciler(f.compose, resolver, rec)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := applyRecord(t, f, applyImage)
	if got.ApplyBlockedDigest != "sha256:new" || !strings.Contains(got.ApplyError, "no container") {
		t.Errorf("record = %+v, want the gate to fail with 'no container'", got)
	}
	ev := singleEvent(t, rec, config.NotificationUpdateFailed)
	if ev.Stage != "health_gate" {
		t.Errorf("event = %+v, want stage health_gate", ev)
	}
}

// TestPhase11SkipsApplyWhenDeployFailed: a stack whose deployment failed in
// this pass has no running image set to compare — no pull, no up.
func TestPhase11SkipsApplyWhenDeployFailed(t *testing.T) {
	f, resolver, rec := applyFixture(t, applyStackYAML)
	f.compose.failUpFor = "web"
	r := f.phase11Reconciler(f.compose, resolver, rec)

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want the deployment failure")
	}
	if len(f.compose.pulls) != 0 {
		t.Errorf("pulls = %+v, want none after a failed deployment", f.compose.pulls)
	}
	if len(f.compose.upOverrides) != 0 {
		t.Errorf("UpOverride calls = %+v, want none after a failed deployment", f.compose.upOverrides)
	}
	st, _ := f.state.Get("web")
	if st.Status != state.StatusFailed {
		t.Errorf("state status = %q, want failed", st.Status)
	}
	if events := eventsOfKind(t, rec, config.NotificationDeployFailed); len(events) != 1 {
		t.Errorf("deploy_failed events = %+v, want exactly one", events)
	}
	if events := eventsOfKind(t, rec, config.NotificationUpdateApplied); len(events) != 0 {
		t.Errorf("update_applied events = %+v, want none", events)
	}
}

// TestPhase11ApplyViaRealExecutorDockerArgv drives the real compose executor
// against a docker binary shadowed through PATH (the e2e pattern). It proves
// the command lines the apply step really emits: pull with the service, a
// targeted up --no-deps, and the ps polls of the gate.
func TestPhase11ApplyViaRealExecutorDockerArgv(t *testing.T) {
	f := newFixture(t, updateRoot)
	f.addLocalStack("web", applyStackYAML)

	binDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "docker.log")
	psCount := filepath.Join(t.TempDir(), "ps.count")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DOCKER_LOG"
case "$*" in
*" ps --all --format json")
	n=$(cat "$FAKE_DOCKER_PS" 2>/dev/null || echo 0)
	n=$((n+1))
	printf '%s' "$n" > "$FAKE_DOCKER_PS"
	if [ "$n" -le 1 ]; then
		printf '%s' '[{"Service":"app","Name":"web-app-1","State":"running","Health":"starting"}]'
	else
		printf '%s' '[{"Service":"app","Name":"web-app-1","State":"running","Health":"healthy"}]'
	fi
	;;
*"image inspect"*)
	printf '%s' '["ghcr.io/x/app@sha256:old"]'
	;;
*" config --format json")
	printf '%s' '{"services":{"app":{"image":"ghcr.io/x/app:latest"}}}'
	;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_LOG", logFile)
	t.Setenv("FAKE_DOCKER_PS", psCount)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	resolver := &fakeResolver{digests: map[string]string{applyImage: "sha256:new"}}
	rec := &recordingNotifier{}
	r := f.phase11Reconciler(compose.New(logger, ""), resolver, rec)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read fake docker log: %v", err)
	}
	calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
	stackDir := filepath.Join(f.workDir, "host", "web")
	composeFile := filepath.Join(stackDir, "compose.yaml")
	prefix := "compose -p web -f " + composeFile
	// The deployment's own up carries the shm env file; the apply up must
	// not, and must be targeted (--no-deps, positional service).
	for _, want := range []string{
		prefix + " pull app",
		prefix + " up -d --remove-orphans --no-deps app",
		"compose -p web ps --all --format json",
	} {
		if !containsLine(calls, want) {
			t.Errorf("docker calls do not contain %q:\n%s", want, strings.Join(calls, "\n"))
		}
	}
	if !containsLine(calls, prefix+" --env-file") {
		t.Errorf("no deployment up with --env-file in:\n%s", strings.Join(calls, "\n"))
	}
	// The apply up must come after the pull.
	if indexOfLine(calls, " pull app") > indexOfLine(calls, "--no-deps app") {
		t.Errorf("pull did not run before the targeted up:\n%s", strings.Join(calls, "\n"))
	}

	got := applyRecord(t, f, applyImage)
	if got.CurrentDigest != "sha256:new" || got.UpdateAvailable {
		t.Errorf("record = %+v, want the applied digest", got)
	}
	if ev := singleEvent(t, rec, config.NotificationUpdateApplied); ev.Stage != "apply" {
		t.Errorf("event = %+v, want update_applied at stage apply", ev)
	}
}

// equalStrings compares two string slices element-wise.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// containsLine reports whether one of the recorded docker calls equals want.
func containsLine(calls []string, want string) bool {
	return indexOfLine(calls, want) >= 0
}

// indexOfLine returns the index of the first call containing fragment, or
// -1. Substring matching keeps the assertions readable (the deployment up
// carries an unpredictable shm env file path); the interesting prefixes are
// spelled out at the call sites.
func indexOfLine(calls []string, fragment string) int {
	for i, call := range calls {
		if strings.Contains(call, fragment) {
			return i
		}
	}
	return -1
}
