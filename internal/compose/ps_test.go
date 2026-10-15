package compose

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// setDockerStdout lets the fake docker binary print content on stdout
// instead of its default noise (see fakeDocker).
func setDockerStdout(t *testing.T, content string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "stdout.txt")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatalf("write stdout fixture: %v", err)
	}
	t.Setenv("DOCKER_STDOUT_FILE", file)
}

func TestPsProjectArguments(t *testing.T) {
	argsFile := fakeDocker(t)
	setDockerStdout(t, "[]")

	if _, err := quietExecutor().PsProject(context.Background(), "immich"); err != nil {
		t.Fatalf("PsProject() error = %v", err)
	}
	want := []string{"compose", "-p", "immich", "ps", "--all", "--format", "json"}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("PsProject() args = %v, want %v", got, want)
	}
}

func TestPsProjectParsesJSONArray(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, `[
		{"Name":"immich-server","State":"running","Health":"healthy"},
		{"Name":"immich-db","State":"exited","Health":""}
	]`)

	containers, err := quietExecutor().PsProject(context.Background(), "immich")
	if err != nil {
		t.Fatalf("PsProject() error = %v", err)
	}
	want := []Container{
		{Name: "immich-server", State: "running", Health: "healthy"},
		{Name: "immich-db", State: "exited", Health: ""},
	}
	if !reflect.DeepEqual(containers, want) {
		t.Errorf("PsProject() = %+v, want %+v (JSON array form, modern compose)", containers, want)
	}
}

func TestPsProjectParsesJSONLines(t *testing.T) {
	fakeDocker(t)
	// Older compose versions emit one JSON object per line.
	setDockerStdout(t, "{\"Name\":\"a-1\",\"State\":\"running\"}\n{\"Name\":\"a-2\",\"State\":\"exited\"}\n")

	containers, err := quietExecutor().PsProject(context.Background(), "a")
	if err != nil {
		t.Fatalf("PsProject() error = %v", err)
	}
	want := []Container{
		{Name: "a-1", State: "running"},
		{Name: "a-2", State: "exited"},
	}
	if !reflect.DeepEqual(containers, want) {
		t.Errorf("PsProject() = %+v, want %+v (JSON lines form)", containers, want)
	}
}

func TestPsProjectParsesSingleObjectAndNamesAlias(t *testing.T) {
	fakeDocker(t)
	// A lone object (not wrapped in an array) and the "Names" key some
	// versions emit instead of "Name" — both tolerated.
	setDockerStdout(t, `{"Names":"solo-1","State":"running","Health":"healthy"}`)

	containers, err := quietExecutor().PsProject(context.Background(), "solo")
	if err != nil {
		t.Fatalf("PsProject() error = %v", err)
	}
	want := []Container{{Name: "solo-1", State: "running", Health: "healthy"}}
	if !reflect.DeepEqual(containers, want) {
		t.Errorf("PsProject() = %+v, want %+v", containers, want)
	}
}

func TestPsProjectEmptyOutput(t *testing.T) {
	for name, out := range map[string]string{
		"empty":            "",
		"null":             "null",
		"empty array":      "[]",
		"whitespace alone": "\n \n",
	} {
		fakeDocker(t)
		setDockerStdout(t, out)
		containers, err := quietExecutor().PsProject(context.Background(), "ghost")
		if err != nil {
			t.Fatalf("%s: PsProject() error = %v", name, err)
		}
		if len(containers) != 0 {
			t.Errorf("%s: PsProject() = %+v, want no containers", name, containers)
		}
	}
}

func TestPsProjectInvalidOutput(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, "definitely not { json")

	_, err := quietExecutor().PsProject(context.Background(), "broken")
	if err == nil || !strings.Contains(err.Error(), "ps --all --format json") {
		t.Errorf("PsProject() error = %v, want error naming the command", err)
	}
}

func TestPsProjectCommandFailure(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, "[]")
	t.Setenv("DOCKER_EXIT", "1")

	_, err := quietExecutor().PsProject(context.Background(), "broken")
	if err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("PsProject() error = %v, want exit status 1", err)
	}
}

func TestPsAllProjectsArguments(t *testing.T) {
	argsFile := fakeDocker(t)
	setDockerStdout(t, "")

	if _, err := quietExecutor().PsAllProjects(context.Background()); err != nil {
		t.Fatalf("PsAllProjects() error = %v", err)
	}
	want := []string{"ps", "--format", "{{json .}}"}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("PsAllProjects() args = %v, want %v", got, want)
	}
}

func TestPsAllProjectsLabelForms(t *testing.T) {
	fakeDocker(t)
	// docker ps emits one JSON object per row; the Labels field varies by
	// version: a comma-joined "k=v" string, an array of "k=v" entries or a
	// JSON object. Rows without the compose label (buildkit and friends) are
	// noise and must not surface.
	setDockerStdout(t, strings.Join([]string{
		`{"Names":"legacy-1","Labels":"com.docker.compose.project=legacy,com.docker.compose.service=app","State":"running"}`,
		`{"Names":"immich-1","Labels":"com.docker.compose.project=immich,com.docker.compose.service=server","State":"running"}`,
		`{"Names":"buildx-1","Labels":"","State":"running"}`,
		`{"Names":"arr-1","Labels":["com.docker.compose.project=arr","docker.volume=anonymous"],"State":"running"}`,
		`{"Names":"map-1","Labels":{"com.docker.compose.project":"map","com.docker.compose.service":"web"},"State":"running"}`,
		`{"Names":"legacy-2","Labels":"com.docker.compose.project=legacy,com.docker.compose.service=db","State":"running"}`,
	}, "\n"))

	refs, err := quietExecutor().PsAllProjects(context.Background())
	if err != nil {
		t.Fatalf("PsAllProjects() error = %v", err)
	}
	want := []ProjectRef{
		{Project: "arr", Containers: []string{"arr-1"}},
		{Project: "immich", Containers: []string{"immich-1"}},
		{Project: "legacy", Containers: []string{"legacy-1", "legacy-2"}},
		{Project: "map", Containers: []string{"map-1"}},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("PsAllProjects() = %+v, want %+v (sorted, unlabeled rows skipped)", refs, want)
	}
}

func TestPsAllProjectsNoOutput(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, "")

	refs, err := quietExecutor().PsAllProjects(context.Background())
	if err != nil {
		t.Fatalf("PsAllProjects() error = %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("PsAllProjects() = %+v, want no projects", refs)
	}
}

func TestPsAllProjectsInvalidOutput(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, "{{not json")

	_, err := quietExecutor().PsAllProjects(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ps --format") {
		t.Errorf("PsAllProjects() error = %v, want error naming the command", err)
	}
}
