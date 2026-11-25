package auth

import (
	"strconv"
	"testing"
	"time"
)

// Credentials stored before issuing families were recorded keep loading and are
// unaffected by OAuth family revocation; recorded families survive a restart.
func TestCredentialFamilyIsBackwardCompatibleAndDurable(t *testing.T) {
	now := time.Now().Unix()
	legacy := `{"clients":{},"access":{},"refresh":{},"credentials":{"legacy-hash":{"name":"old","projects":["default"],"scopes":["notes:read"],"audiences":["api"],"days":1,"id":"old","subject":"@local-admin","epoch":0,"created":` + strconv.FormatInt(now, 10) + `,"expires":` + strconv.FormatInt(now+86400, 10) + `}}}`
	state, err := storedState(t, legacy)
	if err != nil {
		t.Fatal("legacy credential state rejected:", err)
	}
	minted := state.Credentials["legacy-hash"]
	minted.ID, minted.FamilyDigest = "minted", Digest("family")
	state.Credentials["minted-hash"] = minted
	state.RevokeFamily("")
	if _, ok := state.Credentials["legacy-hash"]; !ok {
		t.Fatal("revoking an empty family removed an unbound credential")
	}
	if err := state.Persist(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenState(state.db)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Credentials["minted-hash"].FamilyDigest != Digest("family") {
		t.Fatal("issuing family was not persisted")
	}
	reopened.RevokeFamily("family")
	if _, ok := reopened.Credentials["minted-hash"]; ok {
		t.Fatal("family revocation kept its minted credential")
	}
	if _, ok := reopened.Credentials["legacy-hash"]; !ok {
		t.Fatal("family revocation removed an unrelated credential")
	}
}
