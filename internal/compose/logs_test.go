package compose

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// fakeDockerSpewing writes a docker binary that emits one line every 10 ms
// until it is killed, so a context cancel can be observed as process death.
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
