package stackhash

import (
	"testing"

	"github.com/local/dockops/internal/config"
)

// TestUpdatePolicyNotHashed pins the phase-10 decision: update_policy changes
// no deployment, so editing it must not change the stack hash (and therefore
// must not trigger a redeploy when the notifier policy is switched).
func TestUpdatePolicyNotHashed(t *testing.T) {
	notify := baseStack()
	notify.UpdatePolicy = config.UpdatePolicyNotify
	pinned := baseStack()
	pinned.UpdatePolicy = config.UpdatePolicyPinned
	empty := baseStack()
	empty.UpdatePolicy = ""

	hNotify, err := Compute(notify, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute(notify) error = %v", err)
	}
	hPinned, err := Compute(pinned, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute(pinned) error = %v", err)
	}
	hEmpty, err := Compute(empty, []byte(baseCompose))
	if err != nil {
		t.Fatalf("Compute(empty) error = %v", err)
	}
	if hNotify != hPinned || hNotify != hEmpty {
		t.Errorf("hashes differ (notify=%s pinned=%s empty=%s), want update_policy ignored", hNotify, hPinned, hEmpty)
	}
}
