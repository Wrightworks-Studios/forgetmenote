package catalog

import (
	"forgetmenote/internal/pg/pgtest"
	"strings"
	"testing"
)

func TestLocalAccountsPersistWithoutSharingIdentities(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	alice, err := c.CreateLocalUser(Bootstrap, "Alice", "same-test-password")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := c.CreateLocalUser(Bootstrap, "bob", "same-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if alice == bob || alice == "alice" {
		t.Fatal("login name used as identity")
	}
	a, _ := c.Account(alice)
	b, _ := c.Account(bob)
	if a.ID == b.ID {
		t.Fatal("shared attribution")
	}
	if _, err = c.CreateLocalUser(Bootstrap, "ALICE", "different-test-password"); err == nil {
		t.Fatal("duplicate login accepted")
	}
	if _, err = c.CreateLocalUser(alice, "mallory", "long-test-password"); err == nil {
		t.Fatal("nonadmin provisioned account")
	}
	if err = c.Member(Bootstrap, "default", alice, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ResolveOIDC("https://issuer.example", "external-subject", alice, ""); err != nil {
		t.Fatal(err)
	}
	original := c.state.Passwords["alice"]
	if original.Hash == c.state.Passwords["bob"].Hash {
		t.Fatal("missing per-user salt")
	}
	_, epoch, err := c.AuthenticateLocal("ALICE", "same-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SetLocalPassword(alice, bob, "same-test-password", "new-test-password"); err == nil {
		t.Fatal("impersonation")
	}
	if err = c.SetLocalPassword(alice, alice, "wrong", "new-test-password"); err == nil {
		t.Fatal("missing old password check")
	}
	if err = c.SetLocalPassword(alice, alice, "same-test-password", "new-test-password"); err != nil {
		t.Fatal(err)
	}
	if c.ValidSession(alice, epoch) {
		t.Fatal("password change did not revoke")
	}
	if _, _, err = c.AuthenticateLocal("alice", "same-test-password"); err == nil {
		t.Fatal("old password valid")
	}
	if _, _, err = c.AuthenticateLocal("alice", "new-test-password"); err != nil {
		t.Fatal(err)
	}
	if err = c.SetUser(Bootstrap, User{ID: alice, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err = c.AuthenticateLocal("alice", "new-test-password"); err == nil {
		t.Fatal("disabled account signed in")
	}
	if _, err = c.CreateLocalUser(Bootstrap, "alice", "new-test-password"); err == nil {
		t.Fatal("disabled user recreated")
	}
	if c.LocalUsername(alice) != "alice" || len(c.ExternalIdentities(alice)) != 1 {
		t.Fatal("identity mapping lost")
	}
	if c.state.Accounts[alice].ID != a.ID {
		t.Fatal("attribution changed")
	}
	raw := storedCatalog(t, c)
	if strings.Contains(raw, "same-test-password") || strings.Contains(raw, "new-test-password") {
		t.Fatal("plaintext persisted")
	}
	if _, _, err = c.AuthenticateLocal("bob", "same-test-password"); err != nil {
		t.Fatal("other user affected", err)
	}
}

func TestInitialAdministratorIsAtomicAndDoesNotReopen(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.CreateInitialAdmin("stranger", "admin", "initial-test-password"); err == nil {
		t.Fatal("unauthorized setup")
	}
	if _, err = c.CreateInitialAdmin(Bootstrap, "admin", "short"); err == nil || !c.InitialSetupAvailable() {
		t.Fatal("invalid setup changed catalog")
	}
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 2)
	for _, name := range []string{"alice", "bob"} {
		go func() {
			id, err := c.CreateInitialAdmin(Bootstrap, name, "initial-test-password")
			results <- result{id, err}
		}()
	}
	successes := 0
	var admin string
	for range 2 {
		r := <-results
		if r.err == nil {
			successes++
			admin = r.id
		}
	}
	if successes != 1 || !c.IsAdmin(admin) {
		t.Fatal("initial setup was not exclusive", successes)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsAdmin(admin) || c.InitialSetupAvailable() {
		t.Fatal("setup did not persist")
	}
	if err = c.SetUser(Bootstrap, User{ID: admin, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.InitialSetupAvailable() || c.IsAdmin(admin) {
		t.Fatal("disabled account reopened setup")
	}
	if _, err = c.CreateInitialAdmin(Bootstrap, "replacement", "initial-test-password"); err == nil {
		t.Fatal("setup recreated administrator")
	}
}

func TestInitialAdministratorPersistenceFailureRollsBack(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pgtest.FailWrites(t, c.db, "catalog")
	if _, err = c.CreateInitialAdmin(Bootstrap, "admin", "initial-test-password"); err == nil {
		t.Fatal("expected persistence failure")
	}
	if !c.InitialSetupAvailable() || len(c.state.Passwords) != 0 {
		t.Fatal("partial administrator persisted in memory")
	}
}
