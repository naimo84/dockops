package compose

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
)

func TestImagesParsesResolvedConfig(t *testing.T) {
	argsFile := fakeDocker(t)
	setDockerStdout(t, `{
	  "name": "web",
	  "services": {
	    "app": {"image": "ghcr.io/x/app:latest"},
	    "worker": {"image": "ghcr.io/x/worker:2"},
	    "builder": {"build": {"context": "."}}
	  }
	}`)

	stackDir := t.TempDir()
	req := RequestFor(config.Stack{Name: "web", ComposeFile: "compose.yaml"}, stackDir)
	images, err := quietExecutor().Images(context.Background(), req)
	if err != nil {
		t.Fatalf("Images() error = %v", err)
	}
	want := map[string]string{
		"app":    "ghcr.io/x/app:latest",
		"worker": "ghcr.io/x/worker:2",
	}
	if !reflect.DeepEqual(images, want) {
		t.Errorf("Images() = %v, want %v (build-only service omitted)", images, want)
	}

	wantArgs := []string{
		"compose", "-p", "web",
		"-f", stackDir + "/compose.yaml",
		"config", "--format", "json",
	}
	if got := recordedArgs(t, argsFile); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("Images() args = %v, want %v", got, wantArgs)
	}
}

func TestImagesMultiFileAndEmptyResult(t *testing.T) {
	argsFile := fakeDocker(t)
	setDockerStdout(t, `{"services": {"builder": {"build": {"context": "."}}}}`)

	stackDir := t.TempDir()
	req := RequestFor(config.Stack{
		Name:         "web",
		ComposeFiles: []string{"base.yaml", "override.yaml"},
	}, stackDir)
	images, err := quietExecutor().Images(context.Background(), req)
	if err != nil {
		t.Fatalf("Images() error = %v", err)
	}
	if len(images) != 0 {
		t.Errorf("Images() = %v, want an empty map for a build-only stack", images)
	}
	got := recordedArgs(t, argsFile)
	wantFiles := []string{stackDir + "/base.yaml", stackDir + "/override.yaml"}
	if !reflect.DeepEqual(got[3:7], []string{"-f", wantFiles[0], "-f", wantFiles[1]}) {
		t.Errorf("Images() -f arguments = %v, want %v in order", got[3:7], wantFiles)
	}
}

func TestImagesCommandFailure(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, "compose config boom")
	t.Setenv("DOCKER_EXIT", "1")

	req := RequestFor(config.Stack{Name: "web", ComposeFile: "compose.yaml"}, t.TempDir())
	_, err := quietExecutor().Images(context.Background(), req)
	if err == nil {
		t.Fatal("Images() error = nil, want the docker failure")
	}
	if !strings.Contains(err.Error(), "compose config boom") {
		t.Errorf("Images() error = %v, want the output excerpt", err)
	}
}

func TestImagesInvalidJSON(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, "not json")

	req := RequestFor(config.Stack{Name: "web", ComposeFile: "compose.yaml"}, t.TempDir())
	_, err := quietExecutor().Images(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "parse json") {
		t.Errorf("Images() error = %v, want a parse error", err)
	}
}

func TestImagesRequiresProject(t *testing.T) {
	fakeDocker(t)
	if _, err := quietExecutor().Images(context.Background(), Request{}); err == nil {
		t.Error("Images() without a project error = nil, want error")
	}
}

func TestImageDigestMatchesRepository(t *testing.T) {
	argsFile := fakeDocker(t)
	want := "sha256:" + strings.Repeat("a", 64)
	// The list holds another repository's entry too: only the matching one
	// counts.
	setDockerStdout(t, `["ghcr.io/x/other@sha256:`+strings.Repeat("b", 64)+`","ghcr.io/x/app@`+want+`"]`)

	got, err := quietExecutor().ImageDigest(context.Background(), "ghcr.io/x/app:1.0")
	if err != nil {
		t.Fatalf("ImageDigest() error = %v", err)
	}
	if got != want {
		t.Errorf("ImageDigest() = %q, want %q", got, want)
	}
	wantArgs := []string{"image", "inspect", "--format", "{{json .RepoDigests}}", "ghcr.io/x/app:1.0"}
	if args := recordedArgs(t, argsFile); !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("ImageDigest() args = %v, want %v", args, wantArgs)
	}
}

func TestImageDigestDockerHubNormalization(t *testing.T) {
	fakeDocker(t)
	want := "sha256:" + strings.Repeat("c", 64)
	// docker stores Hub images as "nginx@…", the compose file may say
	// "nginx:latest" or "docker.io/library/nginx".
	setDockerStdout(t, `["nginx@`+want+`"]`)
	for _, ref := range []string{"nginx:latest", "library/nginx", "docker.io/library/nginx:1.25"} {
		got, err := quietExecutor().ImageDigest(context.Background(), ref)
		if err != nil {
			t.Fatalf("ImageDigest(%q) error = %v", ref, err)
		}
		if got != want {
			t.Errorf("ImageDigest(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestImageDigestLocallyBuilt(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, `[]`)
	got, err := quietExecutor().ImageDigest(context.Background(), "myapp:dev")
	if err != nil {
		t.Fatalf("ImageDigest() error = %v", err)
	}
	if got != "" {
		t.Errorf("ImageDigest() = %q, want empty for an image without RepoDigest", got)
	}
}

func TestImageDigestCommandFailure(t *testing.T) {
	fakeDocker(t)
	setDockerStdout(t, "no such image")
	t.Setenv("DOCKER_EXIT", "1")
	if _, err := quietExecutor().ImageDigest(context.Background(), "missing:latest"); err == nil {
		t.Error("ImageDigest() error = nil, want the docker failure")
	}
}

func TestRequestFor(t *testing.T) {
	stack := config.Stack{Name: "web", ComposeFiles: []string{"/abs/a.yaml", "b.yaml"}}
	req := RequestFor(stack, "/stacks/web")
	if req.Project != "web" || req.StackDir != "/stacks/web" {
		t.Errorf("RequestFor() = %+v, want project and stack dir", req)
	}
	if want := []string{"/abs/a.yaml", "b.yaml"}; !reflect.DeepEqual(req.ComposeFiles, want) {
		t.Errorf("ComposeFiles = %v, want %v", req.ComposeFiles, want)
	}
	if got := req.fileArgs(); !reflect.DeepEqual(got, []string{"-f", "/abs/a.yaml", "-f", "/stacks/web/b.yaml"}) {
		t.Errorf("fileArgs() = %v, want resolved files in order", got)
	}
}
