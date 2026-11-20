package stackhash

import (
	"testing"

	"github.com/local/dockops/internal/config"
)

func envFileStack() config.Stack {
	return config.Stack{
		Name:        "web",
		Enabled:     true,
		ComposeFile: "/srv/stacks/web/compose.yaml",
		EnvFiles:    []string{"config.env"},
	}
}

// The text of an env_files entry is deployment recipe: changing it must
// change the hash.
func TestEnvFileContentsAffectHash(t *testing.T) {
	stack := envFileStack()
	a, err := ComputeWithInputs(stack, Inputs{EnvFiles: [][]byte{[]byte("TAG=1.0\n")}}, []byte(baseCompose))
	if err != nil {
		t.Fatalf("ComputeWithInputs() error = %v", err)
	}
	b, err := ComputeWithInputs(stack, Inputs{EnvFiles: [][]byte{[]byte("TAG=1.1\n")}}, []byte(baseCompose))
	if err != nil {
		t.Fatalf("ComputeWithInputs() error = %v", err)
	}
	if a == b {
		t.Error("hash unchanged although the env file content changed")
	}
}

// Without contents the canonical form is the old one: stacks without
// env_files keep their hash across the upgrade (no fleet-wide redeploy).
func TestEnvFileContentsAbsentKeepHash(t *testing.T) {
	stack := envFileStack()
	plain, err := Compute(stack, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	none, err := ComputeWithInputs(stack, Inputs{}, []byte(baseCompose))
	if err != nil {
		t.Fatalf("ComputeWithInputs() error = %v", err)
	}
	if plain != none {
		t.Errorf("hash without contents = %s, want the Compute hash %s", none, plain)
	}
}

func TestEnvFileContentsCountMismatch(t *testing.T) {
	_, err := ComputeWithInputs(envFileStack(), Inputs{EnvFiles: [][]byte{[]byte("A=1"), []byte("B=2")}}, []byte(baseCompose))
	if err == nil {
		t.Error("ComputeWithInputs() with two contents for one env file error = nil, want a mismatch error")
	}
}
