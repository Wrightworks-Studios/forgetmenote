package web

import "testing"

// Temporary: forces a full-gate failure to check its uploaded evidence. Reverted before review.
func TestAAAForcedGateFailure(t *testing.T) {
	t.Fatal("forced failure: the gate-failure artifact must hold this line")
}
