# dockops Log-API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** dockops bekommt eine per Bearer-Token geschützte Log-API (Container-Inventar, Log-Snapshot mit serverseitiger Suche, SSE-Live-Tail), die Dozzle ersetzt.

**Architecture:** Drei Schichten wie im Bestand — `internal/compose` (docker-CLI-Aufrufe via `exec.CommandContext`), `internal/logs` (Query-Validierung, Zeilenfilter, Limits, Encoding; kein Docker-Wissen), `internal/webhook` (Handler, Token-Gate, SSE-Plumbing). Kein Docker-SDK, keine neuen Dependencies.

**Tech Stack:** Go 1.25, stdlib (`net/http`, `log/slog`, `testing`, `regexp`, `encoding/json`), Docker-CLI + compose-Plugin (im Image vorhanden).

**Spec:** `docs/specs/2026-10-25-dockops-log-api-design.md` (PR #8) — der Plan argumentiert aus dem Spec; beide zusammen lesen.

## Global Constraints

- Go 1.25, `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH` vor jedem Build/Test.
- **Keine neuen Dependencies** — stdlib only (`log/slog`, `testing`, kein testify), `gopkg.in/yaml.v3` ist die einzige bestehende Fremd-Dependency. Kein Docker-SDK.
- Kein `panic` in Library-Code (Konvention `CLAUDE.md`).
- Code-Kommentare **Englisch** (Bestandsstil), Doku/Commits/Reply **Deutsch**.
- Commit-Stil: Conventional Commits, Scope `logs` (`feat(logs): …`, `test(logs): …`).
- Nach jeder Task: `go build ./... && go vet ./... && go test ./... -count=1` grün, Testanzahl darf nicht sinken.
- Repo: `/home/hermes/projects/dockops`, Branch `feat/log-api` von `github/main` (nicht von lokalem `main`).
- Alle Datei-Schreibzugriffe aufs Repo per Shell (`python3`/`cp`) — `write_file`/`patch` sind auf `/opt/data` gejailt.

## Review Focus

Diese fünf Eingabeklassen impliziert das Spec, ohne dass ein Task-Schnitt sie offensichtlich abdeckt. Jede Zeile hat ihren Test in der genannten Task.

1. **Containername, der mit `-` beginnt** (`--help`, `-f`) → landet als Flag bei `docker logs`, statt als Name. Erwartung: `400`, nichts wird ausgeführt. → Task 3.
2. **Einzelne Log-Zeile ohne Newline, größer als das Limit** (Crash-Dump eines Containers) → ungebremster Buffer im Filter, OOM-Risiko. Erwartung: Zeile wird bei `MaxLineBytes` gekürzt, Rest verworfen, ein Marker geschrieben. → Task 4.
3. **Container mit `journald`-Log-Treiber** → `docker logs` antwortet `configured logging driver does not support reading`. Erwartung: `422` mit dieser Meldung, nicht `404`/`500`. → Task 6.
4. **Client trennt mitten im Stream** → `docker logs --follow`-Subprozess muss sterben (sonst ein `docker`-Prozess pro abgebrochenem Request im Container). Erwartung: Prozess weg, `Stream` kehrt zurück. → Task 2 (Kill) + Task 7 (Handler).
5. **`DOCKOPS_LOGS_TOKEN` nicht gesetzt** → Endpoint darf nicht offen sein. Erwartung: `503`, kein Log-Zugriff. → Task 6.

---

## File Structure

| Datei | Verantwortung |
|---|---|
| `internal/compose/logs.go` (neu) | `LogsOptions`, `ContainerInfo`, `Logs`, `PsContainers`, `logArgs`, `stream` |
| `internal/compose/logs_test.go` (neu) | argv je Fall, Context-Kill, `PsContainers`-Parsing |
| `internal/logs/options.go` (neu) | `Kind`, `Format`, `Options`, `ParseQuery`, `ParseStreamQuery`, `parseSince` |
| `internal/logs/options_test.go` (neu) | Validierungsmatrix |
| `internal/logs/filter.go` (neu) | `lineSink`, `writeSink`, `filter` (grep, Limits, Zeilen-Splitting) |
| `internal/logs/copy.go` (neu) | `Source`, `Copy` |
| `internal/logs/stream.go` (neu) | `Event`, `Stream`, `sseSink` |
| `internal/logs/logs_test.go` (neu) | `Copy`/`Stream` gegen Fake-Source |
| `internal/webhook/logs.go` (neu) | `handleLogs`, Token-Gate, die drei Handler, Error-Mapping |
| `internal/webhook/logs_test.go` (neu) | 503/401/400/404/422/429, SSE-Framing, Disconnect |
| `internal/webhook/webhook.go` (ändern) | `Deps.Logs`, `Deps.LogsToken`, Routing-Case `/logs*` |
| `cmd/dockops/main.go` (ändern) | `envLogsToken`, Verdrahtung, Start-Warnung, Endpoint-Liste im Startlog |
| `README.md` (ändern) | Abschnitt „Log API" |

---

### Task 1: `compose.PsContainers` — Container-Inventar

**Files:**
- Create: `internal/compose/logs.go`
- Test: `internal/compose/logs_test.go`

**Interfaces:**
- Consumes: `Executor.output(ctx, args)` (bestehend, `internal/compose/ps.go:244`), Test-Helper `fakeDocker`, `setDockerStdout`, `recordedArgs`, `quietExecutor` (`internal/compose/compose_test.go`).
- Produces: `compose.ContainerInfo{Name, Project, Service, Image, State, Health, Created string}` (JSON-Tags wie im Spec §3.1), `(*Executor).PsContainers(ctx) ([]ContainerInfo, error)`.

- [ ] **Step 1: Write the failing test**

```go
package compose

import (
	"context"
	"testing"
)

// equalArgs compares argument lists positionally: the argv order is part of
// the contract, so a set comparison would hide regressions.
func equalArgs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestPsContainersArguments(t *testing.T) {
	argsFile := fakeDocker(t)
	setDockerStdout(t, "")

	if _, err := quietExecutor().PsContainers(context.Background()); err != nil {
		t.Fatalf("PsContainers() error = %v", err)
	}
	want := []string{"ps", "-a", "--format", "{{json .}}"}
	if got := recordedArgs(t, argsFile); !equalArgs(got, want) {
		t.Errorf("PsContainers() args = %v, want %v", got, want)
	}
}

func TestPsContainersParsesLabelsAndHealth(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, `{"Names":"n8n-1","Image":"n8nio/n8n:1.9","State":"running","Status":"Up 2 hours (healthy)","CreatedAt":"2026-10-20 09:12:00 +0200 CEST","Labels":"com.docker.compose.project=n8n,com.docker.compose.service=n8n,other=1"}
{"Names":"traefik","Image":"traefik:v3","State":"running","Status":"Up 3 days","CreatedAt":"2026-09-01 04:00:00 +0200 CEST","Labels":""}
`)

	got, err := quietExecutor().PsContainers(context.Background())
	if err != nil {
		t.Fatalf("PsContainers() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("PsContainers() len = %d, want 2: %+v", len(got), got)
	}
	want := ContainerInfo{
		Name: "n8n-1", Project: "n8n", Service: "n8n", Image: "n8nio/n8n:1.9",
		State: "running", Health: "healthy", Created: "2026-10-20T09:12:00+02:00",
	}
	if got[0] != want {
		t.Errorf("PsContainers()[0] = %+v, want %+v", got[0], want)
	}
	if got[1].Project != "" || got[1].Health != "" {
		t.Errorf("standalone container = %+v, want empty project/health", got[1])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/ -run TestPsContainers -v`
Expected: FAIL — `undefined: ContainerInfo`, `PsContainers not declared`.

- [ ] **Step 3: Write minimal implementation**

`internal/compose/logs.go`:

```go
// Reading container logs and the container inventory for the HTTP log API
// (internal/webhook/logs.go). Like ps.go, this is a read-only exception to
// the "compose drives deployments" rule.
package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// dockerPsRow is one line of `docker ps -a --format {{json .}}`. Only the
// fields the log API reports are decoded; everything else is ignored on
// purpose — the shape of that output is a CLI detail.
type dockerPsRow struct {
	Names     string `json:"Names"`
	Image     string `json:"Image"`
	State     string `json:"State"`
	Status    string `json:"Status"`
	CreatedAt string `json:"CreatedAt"`
	Labels    string `json:"Labels"`
}

// ContainerInfo is one row of the container inventory served by
// GET /logs/containers. Project and Service are empty for containers docker
// compose did not create; Health is empty when the container has no
// healthcheck.
type ContainerInfo struct {
	Name    string `json:"container"`
	Project string `json:"project"`
	Service string `json:"service"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Health  string `json:"health"`
	Created string `json:"created"`
}

// PsContainers lists every container of the host, running or not:
//
//	<binary> ps -a --format {{json .}}
//
// The template form works on every docker CLI version and emits one JSON
// object per line. Rows are sorted project, service, container; rows without
// a compose project come last.
func (e *Executor) PsContainers(ctx context.Context) ([]ContainerInfo, error) {
	out, err := e.output(ctx, []string{"ps", "-a", "--format", "{{json .}}"})
	if err != nil {
		return nil, err
	}
	infos := make([]ContainerInfo, 0, 16)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row dockerPsRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("docker ps -a --format {{json .}}: %w", err)
		}
		if row.Names == "" {
			continue
		}
		info := ContainerInfo{
			Name:    row.Names,
			Image:   row.Image,
			State:   row.State,
			Health:  healthFromStatus(row.Status),
			Created: normalizeCreatedAt(row.CreatedAt),
		}
		for _, label := range strings.Split(row.Labels, ",") {
			key, value, ok := strings.Cut(label, "=")
			if !ok {
				continue
			}
			switch strings.TrimSpace(key) {
			case composeProjectLabel:
				info.Project = value
			case "com.docker.compose.service":
				info.Service = value
			}
		}
		infos = append(infos, info)
	}
	sort.SliceStable(infos, func(i, j int) bool {
		a, b := infos[i], infos[j]
		if (a.Project == "") != (b.Project == "") {
			return b.Project == ""
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Name < b.Name
	})
	return infos, nil
}

// healthFromStatus extracts the healthcheck verdict docker renders into the
// human-readable Status column ("Up 2 hours (unhealthy)"); empty when the
// container has no healthcheck.
func healthFromStatus(status string) string {
	for _, verdict := range []string{"healthy", "unhealthy", "starting"} {
		if strings.Contains(status, "("+verdict+")") {
			return verdict
		}
	}
	return ""
}

// normalizeCreatedAt turns the human-format CreatedAt ("2026-10-20 09:12:00
// +0200 CEST") into RFC3339; unparseable input degrades to "" rather than
// failing the whole inventory.
func normalizeCreatedAt(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := time.Parse("2006-01-02 15:04:05 -0700 MST", raw)
	if err != nil {
		return ""
	}
	return parsed.Format(time.RFC3339)
}

```

Task 2 erweitert den Import-Block dann um `bytes`, `errors`, `io`, `os/exec`, `strconv`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/compose/ -run TestPsContainers -v`
Expected: PASS (2 Tests). Zusätzlich `gofmt -l internal/compose/` → leer.

- [ ] **Step 5: Commit**

```bash
git add internal/compose/logs.go internal/compose/logs_test.go
git commit -m "feat(logs): container inventory via docker ps for the log API"
```

---

### Task 2: `compose.Logs` — Snapshot und Follow

**Files:**
- Modify: `internal/compose/logs.go` (append)
- Test: `internal/compose/logs_test.go` (append)

**Interfaces:**
- Consumes: `Executor.binary`, `e.logger`, `snippet()`, `outputSuffix()` (bestehend).
- Produces: `compose.LogsOptions{Container, Project string; Since time.Time; Tail int; Follow bool}`, `(*Executor).Logs(ctx, opts LogsOptions, w io.Writer) error`.

- [ ] **Step 1: Write the failing test**

```go
func TestLogsArguments(t *testing.T) {
	since := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		opts LogsOptions
		want []string
	}{
		{
			name: "container snapshot",
			opts: LogsOptions{Container: "n8n-1", Since: since, Tail: 200},
			want: []string{"logs", "--timestamps", "--since", "2026-10-25T08:00:00Z", "--tail", "200", "n8n-1"},
		},
		{
			name: "container follow without tail",
			opts: LogsOptions{Container: "n8n-1", Since: since, Follow: true},
			want: []string{"logs", "--timestamps", "--since", "2026-10-25T08:00:00Z", "--follow", "n8n-1"},
		},
		{
			name: "project snapshot",
			opts: LogsOptions{Project: "n8n", Since: since, Tail: 50},
			want: []string{"compose", "-p", "n8n", "logs", "--no-color", "--timestamps", "--since", "2026-10-25T08:00:00Z", "--tail", "50"},
		},
		{
			name: "project follow",
			opts: LogsOptions{Project: "n8n", Since: since, Follow: true},
			want: []string{"compose", "-p", "n8n", "logs", "--no-color", "--timestamps", "--since", "2026-10-25T08:00:00Z", "--follow"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := fakeDocker(t)
			setDockerStdout(t, "")
			if err := quietExecutor().Logs(context.Background(), tc.opts, io.Discard); err != nil {
				t.Fatalf("Logs() error = %v", err)
			}
			if got := recordedArgs(t, argsFile); !equalArgs(got, tc.want) {
				t.Errorf("Logs() args = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLogsRejectsBothAndNeither(t *testing.T) {
	fakeDocker(t)
	since := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	if err := quietExecutor().Logs(context.Background(), LogsOptions{Container: "a", Project: "b", Since: since}, io.Discard); err == nil {
		t.Error("Logs() with container and project: want error, got nil")
	}
	if err := quietExecutor().Logs(context.Background(), LogsOptions{Since: since}, io.Discard); err == nil {
		t.Error("Logs() with neither: want error, got nil")
	}
}

// fakeDockerSpewing writes a docker binary that emits one line per 10ms
// until it is killed, so a context cancel can be observed.
func fakeDockerSpewing(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\nwhile true; do echo \"2026-10-25T08:00:00Z spam\"; sleep 0.01; done\n"
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("write spewing docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestLogsFollowStopsOnContextCancel(t *testing.T) {
	fakeDockerSpewing(t)
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- quietExecutor().Logs(ctx, LogsOptions{Container: "n8n-1", Since: time.Now().Add(-time.Minute), Follow: true}, pw)
		_ = pw.Close()
	}()
	buf := make([]byte, 64)
	if _, err := pr.Read(buf); err != nil {
		t.Fatalf("read first output: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Logs() after cancel = %v, want nil or context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Logs() did not return within 10s after cancel — subprocess leaked")
	}
}
```

`equalArgs` steht schon in Task 1. Für diesen Test zusätzlich in den Import-Block von
`internal/compose/logs_test.go` aufnehmen: `errors`, `io`, `os`, `path/filepath`, `time`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/ -run 'TestLogs' -v`
Expected: FAIL — `undefined: LogsOptions`.

- [ ] **Step 3: Write minimal implementation** (an `internal/compose/logs.go` anhängen)

```go
// LogsOptions describes one log read. Exactly one of Container and Project is
// set: a single container, or every container of a compose project (docker
// compose merges and prefixes those itself). Tail 0 omits --tail (follow mode
// starts with new lines only); Since is always sent.
type LogsOptions struct {
	Container string
	Project   string
	Since     time.Time
	Tail      int
	Follow    bool
}

// Logs writes the container's (or project's) log stream to w:
//
//	<binary> logs --timestamps --since <t> [--tail n] [--follow] <container>
//	<binary> compose -p <project> logs --no-color --timestamps --since <t> [--tail n] [--follow]
//
// w is written as the process produces output, so a follow mode read streams
// straight through. Filters and limits belong to internal/logs, not here.
func (e *Executor) Logs(ctx context.Context, opts LogsOptions, w io.Writer) error {
	args, err := logArgs(opts)
	if err != nil {
		return err
	}
	return e.stream(ctx, args, w)
}

// logArgs builds the command line; the container name is the last positional
// argument, every flag precedes it.
func logArgs(o LogsOptions) ([]string, error) {
	switch {
	case o.Container != "" && o.Project != "":
		return nil, errors.New("container and project are mutually exclusive")
	case o.Container == "" && o.Project == "":
		return nil, errors.New("one of container or project is required")
	}
	since := o.Since.UTC().Format(time.RFC3339)
	var args []string
	if o.Project != "" {
		args = []string{"compose", "-p", o.Project, "logs", "--no-color", "--timestamps", "--since", since}
	} else {
		args = []string{"logs", "--timestamps", "--since", since}
	}
	if o.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(o.Tail))
	}
	if o.Follow {
		args = append(args, "--follow")
	}
	if o.Container != "" {
		args = append(args, o.Container)
	}
	return args, nil
}

// stream runs the composed command and copies stdout to w. Unlike output() it
// does not buffer stdout — log output can be arbitrarily large — but it does
// capture stderr, so docker's own diagnosis ("No such container", "configured
// logging driver does not support reading") survives into the error and can be
// mapped to an HTTP status by the caller. WaitDelay guarantees a SIGKILL five
// seconds after the context is cancelled, so a follow-mode subprocess cannot
// outlive its request.
func (e *Executor) stream(ctx context.Context, args []string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, e.binary, args...)
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	command := strings.Join(args, " ")
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		e.logger.Error("docker command failed",
			"command", command, "error", err.Error(), "output", snippet(stderr.Bytes()))
		return fmt.Errorf("docker %s: %w%s", command, err, outputSuffix(stderr.Bytes()))
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/compose/ -run 'TestLogs' -v`
Expected: PASS (4 Subtests + 2). Der Cancel-Test muss in < 10 s durchlaufen.

- [ ] **Step 5: Commit**

```bash
git add internal/compose/logs.go internal/compose/logs_test.go
git commit -m "feat(logs): docker logs snapshot and follow with context-bound subprocess"
```

---

### Task 3: `internal/logs` — Query-Validierung

**Files:**
- Create: `internal/logs/options.go`
- Test: `internal/logs/options_test.go`

**Interfaces:**
- Consumes: `compose.LogsOptions` (Task 2).
- Produces: `logs.Kind` (`KindContainer`/`KindProject`), `logs.Format` (`FormatText`/`FormatJSON`), `logs.Options{Kind, Name, Since, Tail, Grep, Regex, Format, Follow}`, `logs.ParseQuery(q, now)`, `logs.ParseStreamQuery(q, now)`, `(Options).Compose() compose.LogsOptions`, Konstanten `MaxTail`, `MaxSince`, `MaxBytes`, `MaxStreamBytes`, `MaxStreamDuration`, `MaxConcurrentStreams`, `MaxGrepLen`, `MaxLineBytes`.

- [ ] **Step 1: Write the failing test**

```go
package logs

import (
	"net/url"
	"testing"
	"time"
)

func TestParseQuery(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		query   string
		stream  bool
		wantErr bool
		want    Options
	}{
		{name: "container defaults", query: "container=n8n-1", want: Options{Kind: KindContainer, Name: "n8n-1", Since: now.Add(-time.Hour), Tail: 200}},
		{name: "project defaults", query: "project=n8n", want: Options{Kind: KindProject, Name: "n8n", Since: now.Add(-time.Hour), Tail: 200}},
		{name: "full", query: "container=api-1&since=7d&tail=50&grep=ERROR&regex=1&format=json", want: Options{Kind: KindContainer, Name: "api-1", Since: now.Add(-7 * 24 * time.Hour), Tail: 50, Grep: "ERROR", Regex: true, Format: FormatJSON}},
		{name: "since rfc3339", query: "container=a&since=2026-10-25T10:30:00Z", want: Options{Kind: KindContainer, Name: "a", Since: time.Date(2026, 10, 25, 10, 30, 0, 0, time.UTC), Tail: 200}},
		{name: "since hours and minutes", query: "container=a&since=2h30m", want: Options{Kind: KindContainer, Name: "a", Since: now.Add(-150 * time.Minute), Tail: 200}},
		{name: "stream defaults to tail 0", query: "container=a", stream: true, want: Options{Kind: KindContainer, Name: "a", Since: now.Add(-time.Hour), Follow: true}},
		{name: "stream tail allowed", query: "container=a&tail=10", stream: true, want: Options{Kind: KindContainer, Name: "a", Since: now.Add(-time.Hour), Tail: 10, Follow: true}},

		{name: "neither", query: "", wantErr: true},
		{name: "both", query: "container=a&project=b", wantErr: true},
		{name: "container with leading dash", query: "container=-f", wantErr: true},
		{name: "container with slash", query: "container=a%2Fb", wantErr: true},
		{name: "container with space", query: "container=a%20b", wantErr: true},
		{name: "project with leading dash", query: "project=--help", wantErr: true},
		{name: "tail zero in snapshot", query: "container=a&tail=0", wantErr: true},
		{name: "tail negative", query: "container=a&tail=-3", wantErr: true},
		{name: "tail not a number", query: "container=a&tail=abc", wantErr: true},
		{name: "tail over maximum", query: "container=a&tail=5001", wantErr: true},
		{name: "since over maximum", query: "container=a&since=200h", wantErr: true},
		{name: "since garbage", query: "container=a&since=yesterday", wantErr: true},
		{name: "since zero", query: "container=a&since=0s", wantErr: true},
		{name: "regex without grep", query: "container=a&regex=1", wantErr: true},
		{name: "regex broken", query: "container=a&grep=%28&regex=1", wantErr: true},
		{name: "regex value invalid", query: "container=a&grep=x&regex=yes", wantErr: true},
		{name: "grep too long", query: "container=a&grep=" + strings.Repeat("x", 257), wantErr: true},
		{name: "format invalid", query: "container=a&format=xml", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("parse query: %v", err)
			}
			var (
				got Options
				perr error
			)
			if tc.stream {
				got, perr = ParseStreamQuery(values, now)
			} else {
				got, perr = ParseQuery(values, now)
			}
			if tc.wantErr {
				if perr == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if perr != nil {
				t.Fatalf("unexpected error: %v", perr)
			}
			if got != tc.want {
				t.Errorf("Options = %+v, want %+v", got, tc.want)
			}
		})
	}
}
```

`strings` in den Import-Block von `options_test.go` aufnehmen.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/logs/ -v`
Expected: FAIL — `no Go files` / `undefined: ParseQuery`.

- [ ] **Step 3: Write minimal implementation**

`internal/logs/options.go`:

```go
// Package logs turns the raw docker log stream into a bounded, filtered
// answer for the HTTP log API: query validation, line filtering and the
// limits that keep one noisy container from flooding the caller. It knows
// nothing about docker itself — the command lines live in internal/compose.
package logs

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/local/dockops/internal/compose"
)

// Limits of the log API. Deliberately constants, not configuration: there is
// exactly one kind of caller (an operator or an agent reading logs), so a
// config surface would only add states nobody asked for.
const (
	// MaxTail bounds one snapshot's line count.
	MaxTail = 5000
	// MaxSince bounds how far back a request may look.
	MaxSince = 168 * time.Hour
	// MaxBytes bounds one snapshot response in bytes.
	MaxBytes = 2 << 20
	// MaxStreamBytes bounds one live stream in bytes.
	MaxStreamBytes = 10 << 20
	// MaxStreamDuration bounds one live stream in time.
	MaxStreamDuration = 15 * time.Minute
	// MaxConcurrentStreams bounds concurrent live streams per agent.
	MaxConcurrentStreams = 8
	// MaxGrepLen bounds the search term's length.
	MaxGrepLen = 256
	// MaxLineBytes bounds a single log line; a longer one is cut and its
	// remainder dropped (a crashing container can emit a multi-megabyte line
	// without a newline).
	MaxLineBytes = 64 << 10
)

// Kind selects what a request reads.
type Kind int

const (
	// KindContainer reads one container.
	KindContainer Kind = iota
	// KindProject reads every container of a compose project.
	KindProject
)

// Format selects the wire format of the answer.
type Format int

const (
	// FormatText writes the raw log lines.
	FormatText Format = iota
	// FormatJSON writes one JSON object per line (NDJSON).
	FormatJSON
)

// Options is a validated log request.
type Options struct {
	Kind   Kind
	Name   string
	Since  time.Time
	Tail   int
	Grep   string
	Regex  bool
	Format Format
	Follow bool
}

// nameRe is the allowlist for container and project names. The leading
// character requirement matters: a name starting with "-" would otherwise be
// taken as a flag by docker logs.
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// daySuffix matches a leading "<n>d" so that "7d12h" works; time.ParseDuration
// has no day unit.
var daySuffix = regexp.MustCompile(`^(\d+)d(.*)$`)

// ParseQuery validates the query parameters of a snapshot request.
func ParseQuery(q url.Values, now time.Time) (Options, error) {
	return parse(q, now, false)
}

// ParseStreamQuery validates the query parameters of a live stream: Follow is
// set, Tail defaults to 0 (new lines only) and 0 stays allowed.
func ParseStreamQuery(q url.Values, now time.Time) (Options, error) {
	return parse(q, now, true)
}

func parse(q url.Values, now time.Time, stream bool) (Options, error) {
	var opts Options
	opts.Follow = stream

	container, project := q.Get("container"), q.Get("project")
	switch {
	case container == "" && project == "":
		return opts, errors.New("one of container or project is required")
	case container != "" && project != "":
		return opts, errors.New("container and project are mutually exclusive")
	case container != "":
		if !nameRe.MatchString(container) {
			return opts, fmt.Errorf("invalid container name %q", container)
		}
		opts.Kind, opts.Name = KindContainer, container
	default:
		if !nameRe.MatchString(project) {
			return opts, fmt.Errorf("invalid project name %q", project)
		}
		opts.Kind, opts.Name = KindProject, project
	}

	opts.Since = now.Add(-time.Hour)
	if raw := q.Get("since"); raw != "" {
		since, err := parseSince(raw, now)
		if err != nil {
			return opts, err
		}
		opts.Since = since
	}

	opts.Tail = 200
	if stream {
		opts.Tail = 0
	}
	if raw := q.Get("tail"); raw != "" {
		tail, err := strconv.Atoi(raw)
		if err != nil {
			return opts, fmt.Errorf("invalid tail %q: not a number", raw)
		}
		switch {
		case tail > MaxTail:
			return opts, fmt.Errorf("invalid tail %d: maximum is %d", tail, MaxTail)
		case tail < 0:
			return opts, fmt.Errorf("invalid tail %d: must not be negative", tail)
		case tail == 0 && !stream:
			return opts, errors.New("invalid tail 0: only a stream may start without tail")
		}
		opts.Tail = tail
	}

	opts.Grep = q.Get("grep")
	if len(opts.Grep) > MaxGrepLen {
		return opts, fmt.Errorf("grep too long: %d characters, maximum is %d", len(opts.Grep), MaxGrepLen)
	}
	switch q.Get("regex") {
	case "", "0":
	case "1":
		opts.Regex = true
		if opts.Grep == "" {
			return opts, errors.New("regex=1 requires grep")
		}
		if _, err := regexp.Compile(opts.Grep); err != nil {
			return opts, fmt.Errorf("invalid grep regex %q: %w", opts.Grep, err)
		}
	default:
		return opts, fmt.Errorf("invalid regex %q: want 0 or 1", q.Get("regex"))
	}

	switch q.Get("format") {
	case "", "text":
	case "json":
		opts.Format = FormatJSON
	default:
		return opts, fmt.Errorf("invalid format %q: want text or json", q.Get("format"))
	}
	return opts, nil
}

// parseSince accepts RFC3339 or a duration ("90s", "2h30m", "7d").
func parseSince(raw string, now time.Time) (time.Time, error) {
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		if ts.Before(now.Add(-MaxSince)) {
			return time.Time{}, fmt.Errorf("since %q is older than the maximum of %s", raw, MaxSince)
		}
		return ts, nil
	}
	d, err := parseDuration(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid since %q: want RFC3339 or a duration like 90m or 7d", raw)
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("invalid since %q: must be positive", raw)
	}
	if d > MaxSince {
		return time.Time{}, fmt.Errorf("invalid since %q: maximum is %s", raw, MaxSince)
	}
	return now.Add(-d), nil
}

func parseDuration(raw string) (time.Duration, error) {
	if m := daySuffix.FindStringSubmatch(raw); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, err
		}
		rest := time.Duration(0)
		if m[2] != "" {
			parsed, err := time.ParseDuration(m[2])
			if err != nil {
				return 0, err
			}
			rest = parsed
		}
		return time.Duration(days)*24*time.Hour + rest, nil
	}
	return time.ParseDuration(raw)
}

// Compose renders the docker-facing request.
func (o Options) Compose() compose.LogsOptions {
	out := compose.LogsOptions{Since: o.Since, Tail: o.Tail, Follow: o.Follow}
	if o.Kind == KindProject {
		out.Project = o.Name
	} else {
		out.Container = o.Name
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/logs/ -v`
Expected: PASS (alle ~25 Subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/logs/options.go internal/logs/options_test.go
git commit -m "feat(logs): query validation with strict allowlist and bounds"
```

---

### Task 4: `internal/logs` — Filter, Limits, `Copy`

**Files:**
- Create: `internal/logs/filter.go`, `internal/logs/copy.go`
- Test: `internal/logs/logs_test.go`

**Interfaces:**
- Consumes: `Options` (Task 3), `compose.LogsOptions` (Task 2).
- Produces: `logs.Source` interface (`Logs(ctx, compose.LogsOptions, io.Writer) error`, `PsContainers(ctx) ([]compose.ContainerInfo, error)`), `logs.Copy(ctx, src, opts, dst) (lines int, truncated bool, err error)`.

- [ ] **Step 1: Write the failing test**

```go
package logs

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeSource feeds a canned log stream and records the request it saw.
type fakeSource struct {
	output   string
	err      error
	got      compose.LogsOptions
	requests int
}

func (f *fakeSource) Logs(ctx context.Context, opts compose.LogsOptions, w io.Writer) error {
	f.got = opts
	f.requests++
	if f.err != nil {
		return f.err
	}
	_, err := io.WriteString(w, f.output)
	return err
}

func (f *fakeSource) PsContainers(ctx context.Context) ([]compose.ContainerInfo, error) {
	return nil, nil
}

func snapshotOpts() Options {
	return Options{Kind: KindContainer, Name: "n8n-1", Since: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC), Tail: 200}
}

func TestCopyTextKeepsRawLines(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01.123456789Z hello\n2026-10-25T08:00:02Z world\n"}
	var buf strings.Builder
	lines, truncated, err := Copy(context.Background(), src, snapshotOpts(), &buf)
	if err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	if lines != 2 || truncated {
		t.Errorf("lines/truncated = %d/%v, want 2/false", lines, truncated)
	}
	if want := "2026-10-25T08:00:01.123456789Z hello\n2026-10-25T08:00:02Z world\n"; buf.String() != want {
		t.Errorf("body = %q, want %q", buf.String(), want)
	}
	if src.got.Container != "n8n-1" || src.got.Project != "" || src.got.Tail != 200 {
		t.Errorf("source saw %+v, want container n8n-1 tail 200", src.got)
	}
}

func TestCopyJSONSplitsTimestamp(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01.123456789Z Editor is now accessible\nno-timestamp-line\n"}
	var buf strings.Builder
	opts := snapshotOpts()
	opts.Format = FormatJSON
	if _, _, err := Copy(context.Background(), src, opts, &buf); err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	want := "{\"t\":\"2026-10-25T08:00:01.123456789Z\",\"c\":\"n8n-1\",\"m\":\"Editor is now accessible\"}\n" +
		"{\"t\":\"\",\"c\":\"n8n-1\",\"m\":\"no-timestamp-line\"}\n"
	if buf.String() != want {
		t.Errorf("body = %q, want %q", buf.String(), want)
	}
}

func TestCopyGrepLiteralAndRegex(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01Z ERROR boom\n2026-10-25T08:00:02Z INFO fine\n2026-10-25T08:00:03Z error lower\n"}
	for _, tc := range []struct {
		name string
		opts func(Options) Options
		want string
	}{
		{
			name: "literal case sensitive",
			opts: func(o Options) Options { o.Grep = "ERROR"; return o },
			want: "2026-10-25T08:00:01Z ERROR boom\n",
		},
		{
			name: "regex",
			opts: func(o Options) Options { o.Grep = "(?i)error"; o.Regex = true; return o },
			want: "2026-10-25T08:00:01Z ERROR boom\n2026-10-25T08:00:03Z error lower\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{output: src.output}
			var buf strings.Builder
			if _, _, err := Copy(context.Background(), src, tc.opts(snapshotOpts()), &buf); err != nil {
				t.Fatalf("Copy() error = %v", err)
			}
			if buf.String() != tc.want {
				t.Errorf("body = %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

func TestCopyTruncatesAtByteLimit(t *testing.T) {
	line := "2026-10-25T08:00:01Z " + strings.Repeat("x", 200) + "\n"
	src := &fakeSource{output: strings.Repeat(line, 40)} // ~8.9 KB
	var buf strings.Builder
	lines, truncated, err := Copy(context.Background(), src, snapshotOpts(), &buf)
	if err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	if !truncated {
		t.Fatal("truncated = false, want true at the byte limit")
	}
	if buf.Len() > MaxBytes+64 {
		t.Errorf("body = %d bytes, want <= %d+marker", buf.Len(), MaxBytes)
	}
	if lines == 0 || lines >= 40 {
		t.Errorf("lines = %d, want 1..39", lines)
	}
	if !strings.HasSuffix(buf.String(), "… [dockops: truncated]\n") {
		t.Errorf("body does not end with the truncation marker: %q", buf.String())
	}
}

// TestCopyCutsUnterminatedLongLine pins Review Focus #2: a container writing a
// single huge line without a newline must not buffer without bound.
func TestCopyCutsUnterminatedLongLine(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01Z " + strings.Repeat("y", 3*MaxLineBytes)}
	var buf strings.Builder
	lines, _, err := Copy(context.Background(), src, snapshotOpts(), &buf)
	if err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	if lines != 1 {
		t.Errorf("lines = %d, want exactly 1 (cut prefix)", lines)
	}
	if buf.Len() > MaxLineBytes+128 {
		t.Errorf("body = %d bytes, want around MaxLineBytes", buf.Len())
	}
	if !strings.Contains(buf.String(), "line truncated") {
		t.Errorf("body lacks the line-truncation marker: %q", buf.String()[:80])
	}
}

func TestCopyPropagatesSourceError(t *testing.T) {
	src := &fakeSource{err: errors.New("No such container: nope")}
	var buf strings.Builder
	if _, _, err := Copy(context.Background(), src, snapshotOpts(), &buf); err == nil {
		t.Fatal("Copy() error = nil, want the source error")
	}
}
```

Import `"github.com/local/dockops/internal/compose"` ergänzen.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/logs/ -run TestCopy -v`
Expected: FAIL — `undefined: Copy`, `undefined: Source`.

- [ ] **Step 3: Write minimal implementation**

`internal/logs/filter.go`:

```go
package logs

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
)

// lineSink receives filtered log lines. One line, one call.
type lineSink interface {
	// Line gets the raw line (with docker's timestamp prefix), the parsed
	// timestamp and message, and the container name (empty in project mode).
	Line(raw, ts, container, msg string) error
	// Note receives a marker that belongs to the stream itself, not to a
	// container's output (truncation).
	Note(reason string) error
}

// filter implements io.Writer: it splits the incoming byte stream into lines,
// applies the grep filter and the limits, and hands each surviving line to a
// sink. Splitting on bytes rather than using bufio.Scanner keeps an
// unterminated (and possibly huge) line from being buffered without bound.
type filter struct {
	sink      lineSink
	container string
	grep      string
	re        *regexp.Regexp
	maxBytes  int
	buf       []byte
	dropping  bool
	lines     int
	written   int
	truncated bool
}

func newFilter(opts Options, sink lineSink, maxBytes int) (*filter, error) {
	f := &filter{sink: sink, grep: opts.Grep, maxBytes: maxBytes}
	if opts.Kind == KindContainer {
		f.container = opts.Name
	}
	if opts.Regex && opts.Grep != "" {
		re, err := regexp.Compile(opts.Grep)
		if err != nil {
			return nil, err
		}
		f.re = re
	}
	return f, nil
}

func (f *filter) Write(p []byte) (int, error) {
	if f.truncated {
		return len(p), nil // keep draining, the answer is already cut
	}
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			if len(f.buf) <= MaxLineBytes {
				return len(p), nil
			}
			// Unterminated line beyond the limit: emit the prefix once, drop
			// the rest as it arrives.
			if err := f.line(string(f.buf[:MaxLineBytes]) + " … [dockops: line truncated]"); err != nil {
				return 0, err
			}
			f.buf = f.buf[:0]
			f.dropping = true
			return len(p), nil
		}
		if f.dropping {
			f.buf = f.buf[i+1:]
			f.dropping = false
			continue
		}
		line := f.buf[:i]
		if len(line) > MaxLineBytes {
			line = append(line[:MaxLineBytes:MaxLineBytes], " … [dockops: line truncated]"...)
		}
		if err := f.line(string(line)); err != nil {
			return 0, err
		}
		f.buf = f.buf[i+1:]
		if f.truncated {
			return len(p), nil
		}
	}
}

// Flush emits a trailing line that never got its newline.
func (f *filter) Flush() error {
	if f.truncated || f.dropping || len(f.buf) == 0 {
		return nil
	}
	line := string(f.buf)
	f.buf = nil
	return f.line(line)
}

func (f *filter) line(raw string) error {
	if raw == "" {
		return nil
	}
	if !f.matches(raw) {
		return nil
	}
	if f.written+len(raw)+1 > f.maxBytes {
		f.truncated = true
		return f.sink.Note("truncated")
	}
	ts, msg := splitTimestamp(raw)
	if err := f.sink.Line(raw, ts, f.container, msg); err != nil {
		return err
	}
	f.lines++
	f.written += len(raw) + 1
	return nil
}

func (f *filter) matches(raw string) bool {
	if f.grep == "" {
		return true
	}
	if f.re != nil {
		return f.re.MatchString(raw)
	}
	return strings.Contains(raw, f.grep)
}

// splitTimestamp separates docker's --timestamps prefix from the message. A
// line without a recognizable prefix is passed through untouched.
func splitTimestamp(raw string) (ts, msg string) {
	i := strings.IndexByte(raw, ' ')
	if i <= 0 {
		return "", raw
	}
	if _, err := time.Parse(time.RFC3339Nano, raw[:i]); err != nil {
		return "", raw
	}
	return raw[:i], raw[i+1:]
}

// writeSink writes lines to an io.Writer: raw in text mode, NDJSON in JSON
// mode. The truncation marker is only written in text mode — a JSON caller
// learns about truncation from the X-Dockops-Truncated header, and a plain
// marker line would break NDJSON parsing.
type writeSink struct {
	dst    io.Writer
	format Format
}

func (s *writeSink) Line(raw, ts, container, msg string) error {
	if s.format == FormatText {
		_, err := io.WriteString(s.dst, raw+"\n")
		return err
	}
	record, err := json.Marshal(struct {
		T string `json:"t"`
		C string `json:"c"`
		M string `json:"m"`
	}{ts, container, msg})
	if err != nil {
		return err
	}
	if _, err := s.dst.Write(append(record, '\n')); err != nil {
		return err
	}
	return nil
}

func (s *writeSink) Note(reason string) error {
	if s.format == FormatJSON {
		return nil
	}
	_, err := io.WriteString(s.dst, "… [dockops: "+reason+"]\n")
	return err
}
```

`internal/logs/copy.go`:

```go
package logs

import (
	"context"
	"io"

	"github.com/local/dockops/internal/compose"
)

// Source is everything the log API needs from the docker side. The real
// implementation is *compose.Executor; tests inject a fake.
type Source interface {
	Logs(ctx context.Context, opts compose.LogsOptions, w io.Writer) error
	PsContainers(ctx context.Context) ([]compose.ContainerInfo, error)
}

// Copy reads one snapshot from src, applies the grep filter and the byte
// limit, and writes the result to dst. The snapshot is streamed through the
// filter, not buffered beyond the current line, so a huge container history
// cannot exhaust memory — the byte limit bounds the answer, not the read.
func Copy(ctx context.Context, src Source, opts Options, dst io.Writer) (lines int, truncated bool, err error) {
	f, err := newFilter(opts, &writeSink{dst: dst, format: opts.Format}, MaxBytes)
	if err != nil {
		return 0, false, err
	}
	err = src.Logs(ctx, opts.Compose(), f)
	if ferr := f.Flush(); err == nil {
		err = ferr
	}
	return f.lines, f.truncated, err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/logs/ -run TestCopy -v`
Expected: PASS (6 Tests). `TestCopyTruncatesAtByteLimit` muss den Marker am Ende haben — schlägt er fehl, prüfen, ob `Note` im JSON-Modus fälschlich schreibt.

- [ ] **Step 5: Commit**

```bash
git add internal/logs/filter.go internal/logs/copy.go internal/logs/logs_test.go
git commit -m "feat(logs): line filter, grep and bounded snapshot copy"
```

---

### Task 5: `internal/logs` — SSE-`Stream`

**Files:**
- Create: `internal/logs/stream.go`
- Test: `internal/logs/logs_test.go` (append)

**Interfaces:**
- Consumes: `filter`, `lineSink`, `Source`, `Options`.
- Produces: `logs.Event{Name string; Data []byte}`, `logs.Stream(ctx, src, opts, emit func(Event) error) error`.

- [ ] **Step 1: Write the failing test**

```go
func TestStreamEmitsLogAndEndEvents(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01Z first\n2026-10-25T08:00:02Z second\n"}
	var got []Event
	err := Stream(context.Background(), src, snapshotOpts(), func(ev Event) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(got), got)
	}
	if got[0].Name != "log" || string(got[0].Data) != "2026-10-25T08:00:01Z first" {
		t.Errorf("event 0 = %+v, want log line", got[0])
	}
	if !src.got.Follow {
		t.Error("Stream() did not request follow mode")
	}
}

func TestStreamJSONPayload(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01Z hello\n"}
	var got []Event
	opts := snapshotOpts()
	opts.Format = FormatJSON
	if err := Stream(context.Background(), src, opts, func(ev Event) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	want := `{"t":"2026-10-25T08:00:01Z","c":"n8n-1","m":"hello"}`
	if len(got) != 1 || string(got[0].Data) != want {
		t.Errorf("events = %+v, want one with data %s", got, want)
	}
}

func TestStreamStopsAtByteLimit(t *testing.T) {
	line := "2026-10-25T08:00:01Z " + strings.Repeat("z", 500) + "\n"
	src := &fakeSource{output: strings.Repeat(line, 200)}
	events := 0
	var last Event
	err := Stream(context.Background(), src, snapshotOpts(), func(ev Event) error {
		events++
		last = ev
		return nil
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if last.Name != "end" || !strings.Contains(string(last.Data), "limit") {
		t.Errorf("last event = %+v, want end{limit}", last)
	}
	if events > 300 {
		t.Errorf("events = %d, want the byte limit to cut the stream", events)
	}
}

func TestStreamPropagatesEmitError(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01Z a\n2026-10-25T08:00:02Z b\n"}
	stop := errors.New("client gone")
	calls := 0
	err := Stream(context.Background(), src, snapshotOpts(), func(ev Event) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Stream() error = %v, want %v", err, stop)
	}
	if calls != 1 {
		t.Errorf("emit calls = %d, want 1 (stop at the first error)", calls)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/logs/ -run TestStream -v`
Expected: FAIL — `undefined: Stream`, `undefined: Event`.

- [ ] **Step 3: Write minimal implementation**

`internal/logs/stream.go`:

```go
package logs

import (
	"context"
	"encoding/json"
)

// Event is one server-sent event of a live stream.
type Event struct {
	// Name is "log" or "end"; the HTTP layer adds "ping" itself.
	Name string
	// Data is the payload: the raw line in text mode, the NDJSON object in
	// JSON mode, and {"reason":"…"} for the end event.
	Data []byte
}

// Stream follows the source and hands every surviving line to emit. emit
// blocks, so backpressure is the client's — there is no queue that could grow
// while a slow reader stalls. A non-nil emit error aborts the stream (the
// caller's connection is gone), a source error is returned after the last
// line. The stream ends with an "end" event carrying the reason: "limit" when
// the byte limit stopped it, "docker_exit" when docker ended the follow (or
// failed).
//
// Time limits and ping keepalives are the HTTP layer's job — they need the
// ResponseWriter.
func Stream(ctx context.Context, src Source, opts Options, emit func(Event) error) error {
	opts.Follow = true
	sink := &eventSink{emit: emit, format: opts.Format}
	f, err := newFilter(opts, sink, MaxStreamBytes)
	if err != nil {
		return err
	}
	srcErr := src.Logs(ctx, opts.Compose(), f)
	if ferr := f.Flush(); srcErr == nil {
		srcErr = ferr
	}
	reason := "docker_exit"
	if f.truncated {
		reason = "limit"
	}
	end, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{reason})
	if err != nil {
		return err
	}
	if emitErr := emit(Event{Name: "end", Data: end}); emitErr != nil && srcErr == nil {
		return emitErr
	}
	return srcErr
}

// eventSink turns filtered lines into log events.
type eventSink struct {
	emit   func(Event) error
	format Format
}

func (s *eventSink) Line(raw, ts, container, msg string) error {
	if s.format == FormatText {
		return s.emit(Event{Name: "log", Data: []byte(raw)})
	}
	record, err := json.Marshal(struct {
		T string `json:"t"`
		C string `json:"c"`
		M string `json:"m"`
	}{ts, container, msg})
	if err != nil {
		return err
	}
	return s.emit(Event{Name: "log", Data: record})
}

// Note is a no-op for streams: the byte limit turns into the end event's
// reason, and a marker line inside a live tail would be noise.
func (s *eventSink) Note(reason string) error { return nil }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/logs/ -v`
Expected: PASS (alle Tests des Pakets).

- [ ] **Step 5: Commit**

```bash
git add internal/logs/stream.go internal/logs/logs_test.go
git commit -m "feat(logs): SSE stream with byte limit and end-reason"
```

---

### Task 6: HTTP-Handler für Inventar und Snapshot

**Files:**
- Create: `internal/webhook/logs.go`
- Modify: `internal/webhook/webhook.go` (Deps + ServeHTTP-Case)
- Test: `internal/webhook/logs_test.go`

**Interfaces:**
- Consumes: `logs.Source`, `logs.ParseQuery`, `logs.Copy`, `logs.MaxBytes`, `logs.FormatJSON`; `compose.ContainerInfo`.
- Produces: `webhook.Deps.Logs logs.Source`, `webhook.Deps.LogsToken string`; intern `(*Handler).handleLogs`, `handleLogContainers`, `handleLogSnapshot`, `tokenMatches(got, want string) bool`, `logStatus(err error) int`.

- [ ] **Step 1: Write the failing test**

```go
package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/local/dockops/internal/compose"
)

type fakeLogSource struct {
	logsOutput string
	logsErr    error
	containers []compose.ContainerInfo
	got        compose.LogsOptions
}

func (f *fakeLogSource) Logs(ctx context.Context, opts compose.LogsOptions, w io.Writer) error {
	f.got = opts
	if f.logsErr != nil {
		return f.logsErr
	}
	_, err := io.WriteString(w, f.logsOutput)
	return err
}

func (f *fakeLogSource) PsContainers(ctx context.Context) ([]compose.ContainerInfo, error) {
	return f.containers, nil
}

func logHandler(src logs.Source, token string) *Handler {
	return New(Deps{Secret: "s3cret", Trigger: func() {}, Logs: src, LogsToken: token})
}

func doLogs(t *testing.T, h http.Handler, target, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestLogsDisabledWithoutToken pins Review Focus #5: no token, no access.
func TestLogsDisabledWithoutToken(t *testing.T) {
	src := &fakeLogSource{logsOutput: "secret\n"}
	for _, target := range []string{"/logs?container=a", "/logs/containers", "/logs/stream?container=a"} {
		rec := doLogs(t, logHandler(src, ""), target, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without configured token = %d, want 503", target, rec.Code)
		}
		if src.got.Container != "" {
			t.Errorf("%s reached the docker side", target)
		}
	}
}

func TestLogsRejectsWrongToken(t *testing.T) {
	src := &fakeLogSource{logsOutput: "x\n"}
	rec := doLogs(t, logHandler(src, "right"), "/logs?container=a", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if src.got.Container != "" {
		t.Error("docker side was reached with a wrong token")
	}
}

func TestLogsSnapshotAuth(t *testing.T) {
	src := &fakeLogSource{logsOutput: "2026-10-25T08:00:01Z hello\n"}
	rec := doLogs(t, logHandler(src, "right"), "/logs?container=n8n-1&since=10m&tail=5", "right")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "2026-10-25T08:00:01Z hello\n" {
		t.Errorf("body = %q", got)
	}
	if rec.Header().Get("X-Dockops-Lines") != "1" {
		t.Errorf("X-Dockops-Lines = %q, want 1", rec.Header().Get("X-Dockops-Lines"))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if src.got.Container != "n8n-1" || src.got.Tail != 5 {
		t.Errorf("source saw %+v", src.got)
	}
}

// TestLogsSnapshotRejectsBadQuery pins Review Focus #1 at the HTTP layer.
func TestLogsSnapshotRejectsBadQuery(t *testing.T) {
	src := &fakeLogSource{}
	rec := doLogs(t, logHandler(src, "right"), "/logs?container=-f", "right")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for container=-f", rec.Code)
	}
	rec = doLogs(t, logHandler(src, "right"), "/logs?container=a&project=b", "right")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for container+project", rec.Code)
	}
}

// TestLogsSnapshotErrorMapping pins Review Focus #3: a logging driver that
// cannot be read is a 422, an unknown container a 404.
func TestLogsSnapshotErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "unknown container", err: errors.New("docker logs --timestamps nope: exit status 1: Error: No such container: nope"), want: http.StatusNotFound},
		{name: "unknown project", err: errors.New("docker compose -p nope logs: exit status 1: no such project: nope"), want: http.StatusNotFound},
		{name: "logging driver", err: errors.New("docker logs --timestamps db: exit status 1: Error response from daemon: configured logging driver does not support reading"), want: http.StatusUnprocessableEntity},
		{name: "other", err: errors.New("docker logs --timestamps x: signal: killed"), want: http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeLogSource{logsErr: tc.err}
			rec := doLogs(t, logHandler(src, "right"), "/logs?container=x", "right")
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestLogsContainers(t *testing.T) {
	src := &fakeLogSource{containers: []compose.ContainerInfo{
		{Name: "n8n-1", Project: "n8n", Service: "n8n", Image: "n8nio/n8n:1.9", State: "running", Health: "healthy"},
	}}
	rec := doLogs(t, logHandler(src, "right"), "/logs/containers", "right")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), `"container":"n8n-1"`) {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestLogsRejectsWrongMethod(t *testing.T) {
	rec := doLogs(t, logHandler(&fakeLogSource{}, "right"), "/logs/containers", "right")
	if rec.Code != http.StatusOK {
		t.Fatalf("precondition: GET = %d, want 200", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/logs/containers", nil)
	req.Header.Set("Authorization", "Bearer right")
	post := httptest.NewRecorder()
	logHandler(&fakeLogSource{}, "right").ServeHTTP(post, req)
	if post.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", post.Code)
	}
}
```

Import `"github.com/local/dockops/internal/logs"` ergänzen.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webhook/ -run TestLogs -v`
Expected: FAIL — `unknown field Logs in struct literal`, `undefined: fakeLogSource` compiles but handlers 404 (`unknown field` guard prevents build).

- [ ] **Step 3: Write minimal implementation**

In `internal/webhook/webhook.go`:

1. Imports ergänzen: `"encoding/json"`, `"github.com/local/dockops/internal/logs"`.
2. An `Deps` anhängen:

```go
	// Logs is the docker side of the log API (GET /logs, /logs/containers,
	// /logs/stream). Nil disables every /logs endpoint with 503.
	Logs logs.Source

	// LogsToken is the mandatory Bearer token for /logs*. Unlike Token
	// (/status) an empty value does NOT open the endpoint: log lines carry
	// secrets, so an unset token disables it (503).
	LogsToken string
```

3. `Handler` bekommt Felder `logSrc logs.Source`, `logsToken string`, `streams chan struct{}` (Task 7 füllt `streams`; in Task 6 mit `make(chan struct{}, logs.MaxConcurrentStreams)` initialisieren, damit Task 7 nur noch nutzt).
4. `New()` setzt `logSrc: deps.Logs, logsToken: deps.LogsToken, streams: make(chan struct{}, logs.MaxConcurrentStreams)`.
5. In `ServeHTTP` vor `default:` einfügen:

```go
	case "/logs", "/logs/containers", "/logs/stream":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleLogs(w, r)
```

`internal/webhook/logs.go`:

```go
// Log API: container inventory, log snapshots and live tails. The endpoints
// exist for machines (agent, curl), not for a browser — there is no UI and no
// session: every request must carry the Bearer token from DOCKOPS_LOGS_TOKEN,
// which is mandatory. An unset token disables the endpoints with 503 instead
// of opening them: log lines routinely contain secrets.
package webhook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"crypto/subtle"

	"github.com/local/dockops/internal/logs"
)

// logsDisabledBody is answered when the feature is switched off.
const logsDisabledBody = "logs endpoint disabled: DOCKOPS_LOGS_TOKEN unset"

// handleLogs is the single entry point of the /logs* endpoints: it enforces
// the feature switch and the token, then dispatches by path.
func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	if h.logsToken == "" {
		http.Error(w, logsDisabledBody, http.StatusServiceUnavailable)
		return
	}
	if h.logSrc == nil {
		http.Error(w, "logs endpoint unavailable: no log source", http.StatusServiceUnavailable)
		return
	}
	if !tokenMatches(r.Header.Get("Authorization"), h.logsToken) {
		h.logger.Warn("log request rejected: authentication failed",
			"remote", r.RemoteAddr, "path", r.URL.Path)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/logs/containers":
		h.handleLogContainers(w, r)
	case "/logs/stream":
		h.handleLogStream(w, r)
	default:
		h.handleLogSnapshot(w, r)
	}
}

// tokenMatches compares "Bearer <token>" against want in constant time.
func tokenMatches(header, want string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

Die beiden Handler:

```go
func (h *Handler) handleLogContainers(w http.ResponseWriter, r *http.Request) {
	infos, err := h.logSrc.PsContainers(r.Context())
	if err != nil {
		h.logger.Error("container inventory failed", "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	body, err := json.Marshal(infos)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleLogSnapshot answers one bounded snapshot. The answer is buffered
// before the first byte goes out: the line count and the truncation flag must
// travel as headers, and the buffer is bounded by logs.MaxBytes.
func (h *Handler) handleLogSnapshot(w http.ResponseWriter, r *http.Request) {
	opts, err := logs.ParseQuery(r.URL.Query(), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var buf bytes.Buffer
	lines, truncated, err := logs.Copy(r.Context(), h.logSrc, opts, &buf)
	if err != nil {
		status := logStatus(err)
		h.logger.Warn("log snapshot failed", "status", status, "error", err.Error())
		http.Error(w, err.Error(), status)
		return
	}
	if opts.Format == logs.FormatJSON {
		w.Header().Set("Content-Type", "application/x-ndjson")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("X-Dockops-Lines", strconv.Itoa(lines))
	if truncated {
		w.Header().Set("X-Dockops-Truncated", "true")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// logStatus maps a docker-side error to an HTTP status: a missing container
// or project is a client error (404), an unreadable logging driver is a
// documented limitation (422), everything else is upstream (502).
func logStatus(err error) int {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "no such container"), strings.Contains(message, "no such project"):
		return http.StatusNotFound
	case strings.Contains(message, "logging driver does not support reading"):
		return http.StatusUnprocessableEntity
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}
```

Fehlende Imports in `logs.go`: `context`, `errors`, `strconv`, `time`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/webhook/ -run TestLogs -v`
Expected: PASS. `TestLogsRejectsWrongMethod` braucht den ServeHTTP-Case aus Schritt 3.

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/logs.go internal/webhook/logs_test.go internal/webhook/webhook.go
git commit -m "feat(logs): token-guarded HTTP handlers for inventory and snapshots"
```

---

### Task 7: SSE-Handler `/logs/stream`

**Files:**
- Modify: `internal/webhook/logs.go` (append)
- Test: `internal/webhook/logs_test.go` (append)

**Interfaces:**
- Consumes: `logs.ParseStreamQuery`, `logs.Stream`, `logs.Event`, `logs.MaxStreamDuration`, `logs.MaxConcurrentStreams`, `h.streams`.
- Produces: `(*Handler).handleLogStream`, `(*Handler).streamSlot() bool`, Hilfsfunktion `sseEvent(name string, data []byte) string`.

- [ ] **Step 1: Write the failing test**

```go
func TestLogsStreamFramesEvents(t *testing.T) {
	src := &fakeLogSource{logsOutput: "2026-10-25T08:00:01Z first\n2026-10-25T08:00:02Z second\n"}
	rec := doLogs(t, logHandler(src, "right"), "/logs/stream?container=n8n-1", "right")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: log\ndata: 2026-10-25T08:00:01Z first\n\n",
		"event: log\ndata: 2026-10-25T08:00:02Z second\n\n",
		`event: end
data: {"reason":"docker_exit"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("stream does not end with a blank line: %q", body)
	}
	if !src.got.Follow {
		t.Error("stream did not request follow mode")
	}
}

func TestLogsStreamRejectsBadQuery(t *testing.T) {
	rec := doLogs(t, logHandler(&fakeLogSource{}, "right"), "/logs/stream?container=a&tail=abc", "right")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestLogsStreamConcurrencyLimit pins the semaphore: the ninth concurrent
// stream is rejected with 429.
func TestLogsStreamConcurrencyLimit(t *testing.T) {
	h := logHandler(&blockingLogSource{release: make(chan struct{})}, "right")
	for i := 0; i < logs.MaxConcurrentStreams; i++ {
		if !h.streamSlot() {
			t.Fatalf("slot %d refused, want granted", i)
		}
	}
	if h.streamSlot() {
		t.Fatal("slot granted beyond MaxConcurrentStreams, want refusal")
	}
	for i := 0; i < logs.MaxConcurrentStreams; i++ {
		<-h.streams
	}
	if !h.streamSlot() {
		t.Fatal("slot not available again after release")
	}
}

// blockingLogSource blocks until released so a stream occupies its slot.
type blockingLogSource struct {
	release chan struct{}
}

func (b *blockingLogSource) Logs(ctx context.Context, opts compose.LogsOptions, w io.Writer) error {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return ctx.Err()
}

func (b *blockingLogSource) PsContainers(ctx context.Context) ([]compose.ContainerInfo, error) {
	return nil, nil
}

func TestLogsStreamEndsOnClientDisconnect(t *testing.T) {
	cancelled := make(chan struct{})
	src := &disconnectLogSource{cancelled: cancelled}
	h := logHandler(src, "right")
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/logs/stream?container=a", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer right")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("log source was not cancelled — subprocess would leak")
	}
}

// disconnectLogSource writes one line, then blocks until its context dies.
type disconnectLogSource struct{ cancelled chan struct{} }

func (d *disconnectLogSource) Logs(ctx context.Context, opts compose.LogsOptions, w io.Writer) error {
	if _, err := io.WriteString(w, "2026-10-25T08:00:01Z hello\n"); err != nil {
		return err
	}
	<-ctx.Done()
	close(d.cancelled)
	return ctx.Err()
}

func (d *disconnectLogSource) PsContainers(ctx context.Context) ([]compose.ContainerInfo, error) {
	return nil, nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webhook/ -run 'TestLogsStream' -v`
Expected: FAIL — `h.streamSlot undefined`, `/logs/stream` liefert 404 (Snapshot-Handler greift).

- [ ] **Step 3: Write minimal implementation** (an `internal/webhook/logs.go` anhängen)

```go
// streamSlot takes one concurrency slot; false means the limit is reached.
// The channel is the semaphore: capacity MaxConcurrentStreams, one entry per
// running stream.
func (h *Handler) streamSlot() bool {
	select {
	case h.streams <- struct{}{}:
		return true
	default:
		return false
	}
}

// sseEvent renders one server-sent event. Log lines never contain newlines
// (the filter splits them), so a single data field per event is enough.
func sseEvent(name string, data []byte) string {
	return "event: " + name + "\ndata: " + string(data) + "\n\n"
}

// handleLogStream answers a live tail as Server-Sent Events. Writes from the
// log goroutine and the keepalive goroutine are serialized by mu — the
// ResponseWriter is not safe for concurrent use. The request context bounds
// the stream: the client's disconnect cancels it, MaxStreamDuration ends it,
// and both kill the docker subprocess (internal/compose sets WaitDelay).
func (h *Handler) handleLogStream(w http.ResponseWriter, r *http.Request) {
	opts, err := logs.ParseStreamQuery(r.URL.Query(), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if !h.streamSlot() {
		http.Error(w, "too many concurrent streams", http.StatusTooManyRequests)
		return
	}
	defer func() { <-h.streams }()

	ctx, cancel := context.WithTimeout(r.Context(), logs.MaxStreamDuration)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var mu sync.Mutex
	send := func(ev logs.Event) error {
		mu.Lock()
		defer mu.Unlock()
		if _, err := io.WriteString(w, sseEvent(ev.Name, ev.Data)); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := send(logs.Event{Name: "ping", Data: []byte(`{}`)}); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	streamErr := logs.Stream(ctx, h.logSrc, opts, send)
	cancel()
	<-pingDone
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		h.logger.Warn("log stream ended", "error", streamErr.Error())
	}
}
```

Fehlende Imports: `"sync"`, `"io"`.

Hinweis zur `end`-Semantik: das `end`-Event kommt aus `logs.Stream` (Task 5) und geht durch `send` — deshalb erscheint es im Test-Body. Bei Client-Disconnect schlägt `send` fehl, `Stream` bricht ab, der Handler kehrt zurück; es wird kein `end` mehr geschrieben (die Verbindung ist weg). Das ist gewollt und deckt `server_shutdown`/`since_exhausted` mit ab — eigene Reasons dafür gibt es im MVP nicht.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/webhook/ -v`
Expected: PASS (alle Handler-Tests).

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/logs.go internal/webhook/logs_test.go
git commit -m "feat(logs): SSE live tail with ping, duration bound and slot limit"
```

---

### Task 8: CLI-Verdrahtung, README

**Files:**
- Modify: `cmd/dockops/main.go:43` (Konstante), `cmd/dockops/main.go:385-397` (Deps), `cmd/dockops/main.go:413` (Startlog)
- Modify: `README.md` (neuer Abschnitt)
- Test: `cmd/dockops/main_test.go` (append)

**Interfaces:**
- Consumes: `webhook.Deps.Logs`, `webhook.Deps.LogsToken`, `collab.compose`.
- Produces: Env `DOCKOPS_LOGS_TOKEN`, Startwarnung, README-Abschnitt.

- [ ] **Step 1: Write the failing test**

```go
func TestLogsTokenWiring(t *testing.T) {
	// The endpoint list in the start log must mention the log API once the
	// token is configured — this is the only place the operator sees it.
	if !strings.Contains(endpointList(true), "/logs") {
		t.Errorf("endpointList(true) = %q, want /logs mentioned", endpointList(true))
	}
	if strings.Contains(endpointList(false), "/logs") {
		t.Errorf("endpointList(false) = %q, want no /logs when disabled", endpointList(false))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/dockops/ -run TestLogsTokenWiring -v`
Expected: FAIL — `undefined: endpointList`.

- [ ] **Step 3: Write minimal implementation**

In `cmd/dockops/main.go`:

```go
const (
	// ... bestehende Konstanten
	envLogsToken = "DOCKOPS_LOGS_TOKEN"
)

// endpointList names the HTTP endpoints in the start log; the log API only
// appears when its token is configured, because without it every /logs call
// is a 503.
func endpointList(logsEnabled bool) string {
	endpoints := "POST /webhook, GET /healthz, GET /status"
	if logsEnabled {
		endpoints += ", GET /logs, /logs/containers, /logs/stream"
	}
	return endpoints
}
```

Beim Server-Aufbau:

```go
	logsToken := os.Getenv(envLogsToken)
	if logsToken == "" {
		logger.Warn("logs API disabled: " + envLogsToken + " unset")
	}
	server := &http.Server{
		Addr: opts.Addr,
		Handler: webhook.New(webhook.Deps{
			// ... bestehende Felder
			Logs:      collab.compose,
			LogsToken: logsToken,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
```

Und im Startlog `"endpoints", endpointList(logsToken != "")` statt des Literals.

`README.md` — Abschnitt nach dem `/status`-Abschnitt:

```markdown
## Log API

`GET /logs/containers`, `GET /logs` und `GET /logs/stream` liefern Container-Inventar,
Log-Snapshots und einen Live-Tail (Server-Sent Events) — gedacht für Agenten und `curl`,
es gibt bewusst kein Web-UI.

Alle drei Endpoints verlangen `Authorization: Bearer $DOCKOPS_LOGS_TOKEN`. Ist die Variable
nicht gesetzt, antworten sie mit `503` (Logs enthalten Secrets — der Endpoint ist dann aus,
nicht offen).

```bash
# Inventar
curl -sH "Authorization: Bearer $TOKEN" https://dockops.<host>.naimo84.dev/logs/containers | jq

# letzte 200 Zeilen eines Containers aus der letzten Stunde
curl -sH "Authorization: Bearer $TOKEN" "https://dockops.<host>.naimo84.dev/logs?container=n8n-1&since=1h&tail=200"

# im ganzen Projekt nach ERROR suchen (json = NDJSON-Ausgabe)
curl -sH "Authorization: Bearer $TOKEN" "https://dockops.<host>.naimo84.dev/logs?project=n8n&since=6h&grep=ERROR&format=json"

# Live-Tail
curl -NsH "Authorization: Bearer $TOKEN" "https://dockops.<host>.naimo84.dev/logs/stream?container=n8n-1"
```

Limits: `tail` ≤ 5000, `since` ≤ 168 h, 2 MiB pro Snapshot, 10 MiB / 15 min pro Stream,
8 gleichzeitige Streams. Container mit `journald`-Log-Treiber sind über `docker logs` nicht
lesbar (`422`).
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/dockops/ -run TestLogsTokenWiring -v` und `go build ./...`
Expected: PASS, Build grün.

- [ ] **Step 5: Commit**

```bash
git add cmd/dockops/main.go cmd/dockops/main_test.go README.md
git commit -m "feat(logs): wire DOCKOPS_LOGS_TOKEN into the daemon and document the API"
```

---

### Task 9: Gesamtverifikation und PR

**Files:**
- Keine neuen Dateien.

**Interfaces:**
- Consumes: alles aus Task 1–8.
- Produces: grüner Branch + PR gegen `github/main`.

- [ ] **Step 1: Vollständige Prüfung**

```bash
cd /home/hermes/projects/dockops
export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH
gofmt -l ./cmd ./internal
go build ./... && go vet ./... && go test ./... -count=1
```
Expected: `gofmt -l` leer, alle Pakete `ok`, kein Test rot, Testanzahl ≥ vorher.

- [ ] **Step 2: Diff-Kontrolle**

```bash
git fetch github main -q
git diff --stat github/main...HEAD
```
Expected: genau `internal/compose/logs.go`, `internal/compose/logs_test.go`, `internal/logs/*.go`,
`internal/webhook/logs.go`, `internal/webhook/logs_test.go`, `internal/webhook/webhook.go`,
`cmd/dockops/main.go`, `cmd/dockops/main_test.go`, `README.md`, `docs/specs/…`. Keine
`PHASE*_TASK.md`, keine Test-Fixtures.

- [ ] **Step 3: Livetest auf devbox**

Nach `docker build -t gitea.naimo84.dev/loki/dockops:dev /tmp/deploy/dockops` (Prozedur Skill
`dockops-dev`) und einem `-once`-Run gegen das Test-Host-Repo, mit `DOCKOPS_LOGS_TOKEN=devtoken`
im Container:

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/logs/containers            # 401
curl -sH 'Authorization: Bearer devtoken' http://127.0.0.1:8080/logs/containers | head -c 300
curl -sH 'Authorization: Bearer devtoken' 'http://127.0.0.1:8080/logs?container=<test>&since=10m&grep=<marker>'
curl -sN -H 'Authorization: Bearer devtoken' 'http://127.0.0.1:8080/logs/stream?container=<test>'
```
Expected: 401 ohne Token, Inventar nicht-leer, Snapshot enthält den Marker, Stream liefert
`event: log`. Danach Container killen und prüfen: `event: end` mit `"reason":"docker_exit"`.

- [ ] **Step 4: Verwaiste Prozesse prüfen**

Nach dem Stream-Abbruch (Ctrl-C): `docker top dockops` bzw. `docker exec dockops ps` — kein
`docker logs --follow`-Prozess übrig. Das ist der Nachweis für Review Focus #4 auf dem echten Host.

- [ ] **Step 5: PR stellen**

```bash
git push origin feat/log-api
git push github feat/log-api
python3 - <<'PY'   # GitHub-API, Token aus der github-Remote-URL (nie ausgeben)
import json, re, subprocess, urllib.request
url = subprocess.check_output(["git", "remote", "get-url", "github"], text=True).strip()
token = re.match(r"https://([^:]+):([^@]+)@github\.com/", url).group(2)
body = {"title": "feat(logs): log API (inventory, snapshot, SSE stream)",
        "head": "feat/log-api", "base": "main",
        "body": "Implementiert das Design aus PR #8 (docs/specs/2026-10-25-dockops-log-api-design.md):\n\n"
                "- GET /logs/containers, GET /logs, GET /logs/stream (SSE)\n"
                "- Bearer-Token Pflicht, fail-closed (503 ohne DOCKOPS_LOGS_TOKEN)\n"
                "- Limits: tail 5000, since 168h, 2 MiB Snapshot, 10 MiB/15 min Stream, 8 Streams\n"
                "- docker-CLI statt SDK, kein neues Dependency\n\n"
                "Deploy-Hinweis: Router-Rule im infrastructure-Repo muss /logs ergaenzen, sonst nur loopback."}
req = urllib.request.Request("https://api.github.com/repos/naimo84/dockops/pulls",
    data=json.dumps(body).encode(), method="POST",
    headers={"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json",
             "Content-Type": "application/json", "User-Agent": "loki"})
print(json.loads(urllib.request.urlopen(req, timeout=30).read())["html_url"])
PY
```

- [ ] **Step 6: Nächste Phasen anlegen (nicht in diesem Plan umsetzen)**

- Phase C (Infrastruktur): Router-Rule um `PathPrefix('/logs')` erweitern, `DOCKOPS_LOGS_TOKEN`
  in `docker/shared/dockops#LOGS_TOKEN` + Template-Zeile je Host, Deploy, Verifikation pro Host.
- Phase D: Dozzle-Abbau.
Beide gehören in eigene PRs; sie brauchen kein weiteres Design, nur die Checkliste aus Spec §8.
