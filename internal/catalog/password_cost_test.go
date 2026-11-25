package catalog

import (
	"strings"
	"testing"
)

// The other tests hash at the cheap test cost; this one pays the real cost.
func TestStoredPasswordsUseProductionCostAndRejectCheaperHashes(t *testing.T) {
	cheap := passwordIterations
	t.Cleanup(func() { passwordIterations = cheap })
	if cheap == productionPasswordIterations {
		t.Fatal("test binary still hashes at the production cost")
	}
	weak, err := passwordHash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	passwordIterations = productionPasswordIterations
	hash, err := passwordHash("correct horse battery")
	if err != nil || !strings.HasPrefix(hash, "pbkdf2-sha256:600000:") {
		t.Fatalf("stored hash does not record the production cost: %q %v", hash, err)
	}
	if !passwordMatches(hash, "correct horse battery") || passwordMatches(hash, "wrong horse battery") {
		t.Fatal("production hash does not verify exactly its password")
	}
	// A server never lowers its cost, so a cheaper stored hash must not verify.
	if passwordMatches(weak, "correct horse battery") {
		t.Fatal("a hash below the production cost verified")
	}
}
