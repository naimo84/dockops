package stackhash

import (
	"testing"

	"github.com/local/dockops/internal/config"
)

// TestApplyPolicyAndHealthTimeoutNotHashed pins the phase-11 decision: both
// new fields tune the apply step, they change no deployment, so editing them
// must not change the stack hash (and therefore must not redeploy anything).
func TestApplyPolicyAndHealthTimeoutNotHashed(t *testing.T) {
	base := baseStack()

	notify := base
	notify.UpdatePolicy = config.UpdatePolicyNotify

	apply := base
	apply.UpdatePolicy = config.UpdatePolicyApply
	apply.HealthTimeout = config.Duration(30_000_000_000) // 30s

	applyDefaultTimeout := base
	applyDefaultTimeout.UpdatePolicy = config.UpdatePolicyApply

	hashes := make([]string, 0, 3)
	for _, stack := range []config.Stack{notify, apply, applyDefaultTimeout} {
		h, err := Compute(stack, []byte(baseCompose))
		if err != nil {
			t.Fatalf("Compute(%s) error = %v", stack.UpdatePolicy, err)
		}
		hashes = append(hashes, h)
	}
	for i := 1; i < len(hashes); i++ {
		if hashes[i] != hashes[0] {
			t.Errorf("hashes differ (%v), want update_policy and health_timeout ignored by the hash", hashes)
		}
	}
}
