package compose

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// scriptedDocker installs a fake docker that answers by subcommand — ps,
// inspect, image inspect, tag — and logs every invocation as one line.
func scriptedDocker(t *testing.T, ps, inspect, imageInspect string) (logFile string) {
	t.Helper()
	binDir := t.TempDir()
	logFile = filepath.Join(t.TempDir(), "docker.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DOCKER_LOG"
case "$1" in
ps) printf '%s' "$FAKE_PS" ;;
inspect) printf '%s' "$FAKE_INSPECT" ;;
image) printf '%s' "$FAKE_IMAGE_INSPECT" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_LOG", logFile)
	t.Setenv("FAKE_PS", ps)
	t.Setenv("FAKE_INSPECT", inspect)
	t.Setenv("FAKE_IMAGE_INSPECT", imageInspect)
	return logFile
}

func dockerCalls(t *testing.T, logFile string) []string {
	t.Helper()
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read fake docker log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestServiceImageIDs(t *testing.T) {
	logFile := scriptedDocker(t,
		"c1\nc2\nc3\nc4\n",
		// Two containers of app on the same image (scale 2), a worker on
		// another, and a container without a service label.
		"app sha256:aaa\napp sha256:aaa\nworker sha256:bbb\n sha256:ccc\n",
		"")

	got, err := quietExecutor().ServiceImageIDs(context.Background(), "web")
	if err != nil {
		t.Fatalf("ServiceImageIDs() error = %v", err)
	}
	want := map[string][]string{"app": {"sha256:aaa"}, "worker": {"sha256:bbb"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ServiceImageIDs() = %v, want %v", got, want)
	}
	calls := dockerCalls(t, logFile)
	wantCalls := []string{
		"ps --all --quiet --no-trunc --filter label=com.docker.compose.project=web",
		`inspect --format {{index .Config.Labels "com.docker.compose.service"}} {{.Image}} c1 c2 c3 c4`,
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("docker calls = %q, want %q", calls, wantCalls)
	}
}

func TestServiceImageIDsWithoutContainers(t *testing.T) {
	logFile := scriptedDocker(t, "", "", "")

	got, err := quietExecutor().ServiceImageIDs(context.Background(), "web")
	if err != nil || len(got) != 0 {
		t.Errorf("ServiceImageIDs() = %v, %v; want an empty map", got, err)
	}
	if calls := dockerCalls(t, logFile); len(calls) != 1 {
		t.Errorf("docker calls = %q, want only the ps (nothing to inspect)", calls)
	}
}

func TestImageRepoDigestInspectsTheImageAndMatchesTheReference(t *testing.T) {
	logFile := scriptedDocker(t, "", "", `["ghcr.io/other/x@sha256:zzz","ghcr.io/x/app@sha256:old"]`)

	got, err := quietExecutor().ImageRepoDigest(context.Background(), "sha256:img", "ghcr.io/x/app:latest")
	if err != nil || got != "sha256:old" {
		t.Errorf("ImageRepoDigest() = %q, %v; want sha256:old", got, err)
	}
	if calls := dockerCalls(t, logFile); len(calls) != 1 || calls[0] != "image inspect --format {{json .RepoDigests}} sha256:img" {
		t.Errorf("docker calls = %q, want the inspect of the image ID", calls)
	}
}

func TestTagImage(t *testing.T) {
	logFile := scriptedDocker(t, "", "", "")

	if err := quietExecutor().TagImage(context.Background(), "ghcr.io/x/app@sha256:old", "ghcr.io/x/app:latest"); err != nil {
		t.Fatalf("TagImage() error = %v", err)
	}
	if calls := dockerCalls(t, logFile); len(calls) != 1 || calls[0] != "tag ghcr.io/x/app@sha256:old ghcr.io/x/app:latest" {
		t.Errorf("docker calls = %q, want one docker tag", calls)
	}
}
