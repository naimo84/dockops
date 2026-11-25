package stackhash

import (
	"testing"

	"github.com/local/dockops/internal/config"
)

func restartFileStack() config.Stack {
	return config.Stack{
		Name:         "traefik",
		Enabled:      true,
		ComposeFile:  "/srv/stacks/traefik/compose.yaml",
		RestartFiles: []string{"config.yaml"},
	}
}

// A restart file is invisible to compose but part of the deployment: an
// edit must change the hash.
func TestRestartFileContentsAffectHash(t *testing.T) {
	stack := restartFileStack()
	a, err := ComputeWithInputs(stack, Inputs{RestartFiles: [][]byte{[]byte("a: 1\n")}}, []byte(baseCompose))
	if err != nil {
		t.Fatalf("ComputeWithInputs() error = %v", err)
	}
	b, err := ComputeWithInputs(stack, Inputs{RestartFiles: [][]byte{[]byte("a: 2\n")}}, []byte(baseCompose))
	if err != nil {
		t.Fatalf("ComputeWithInputs() error = %v", err)
	}
	if a == b {
		t.Error("hash unchanged although the restart file content changed")
	}
}

// Stacks without restart_files keep their hash: the fields are omitted when
// empty, so the upgrade does not redeploy the fleet.
func TestRestartFilesAbsentKeepHash(t *testing.T) {
	stack := envFileStack()
	plain, err := Compute(stack, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	none, err := ComputeWithInputs(stack, Inputs{RestartFiles: nil}, []byte(baseCompose))
	if err != nil {
		t.Fatalf("ComputeWithInputs() error = %v", err)
	}
	if plain != none {
		t.Errorf("hash without restart files = %s, want %s", none, plain)
	}
}

func TestRestartFileContentsCountMismatch(t *testing.T) {
	_, err := ComputeWithInputs(restartFileStack(), Inputs{RestartFiles: [][]byte{[]byte("a"), []byte("b")}}, []byte(baseCompose))
	if err == nil {
		t.Error("ComputeWithInputs() with two contents for one restart file error = nil, want a mismatch error")
	}
}

func TestRestartFilesDigest(t *testing.T) {
	if got := RestartFilesDigest(nil, nil); got != "" {
		t.Errorf("RestartFilesDigest(nil) = %q, want empty", got)
	}
	paths := []string{"config.yaml"}
	a := RestartFilesDigest(paths, [][]byte{[]byte("a: 1\n")})
	if a == "" || a != RestartFilesDigest(paths, [][]byte{[]byte("a: 1\n")}) {
		t.Fatalf("RestartFilesDigest not deterministic: %q", a)
	}
	if a == RestartFilesDigest(paths, [][]byte{[]byte("a: 2\n")}) {
		t.Error("digest unchanged although the content changed")
	}
	if a == RestartFilesDigest([]string{"other.yaml"}, [][]byte{[]byte("a: 1\n")}) {
		t.Error("digest unchanged although the path changed")
	}
}
