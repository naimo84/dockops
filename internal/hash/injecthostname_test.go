package stackhash

import (
	"testing"
)

// Only opted-in stacks carry the host name in their hash; for them a new
// host name is a deployment change.
func TestTargetHostnameHash(t *testing.T) {
	stack := envFileStack()
	plain, err := Compute(stack, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}

	// TargetHostname without the flag must not matter (the loader never
	// produces it, but the hash must not depend on that).
	stray := stack
	stray.TargetHostname = "hvaler"
	if h, _ := Compute(stray, []byte(baseCompose)); h != plain {
		t.Error("hash changed by TargetHostname without inject_hostname")
	}

	on := stack
	on.InjectHostname = true
	on.TargetHostname = "hvaler"
	a, err := Compute(on, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	if a == plain {
		t.Error("hash unchanged by inject_hostname")
	}
	on.TargetHostname = "internal"
	if b, _ := Compute(on, []byte(baseCompose)); b == a {
		t.Error("hash unchanged although the host name changed")
	}
}
