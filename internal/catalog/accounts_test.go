package catalog

import (
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestAccountsSurviveRestartPermissionsAndGitHubUsernameChanges(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	alice, err := c.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := c.Account("bob")
	if err != nil {
		t.Fatal(err)
	}
	if alice.ID == bob.ID {
		t.Fatal("shared internal identity")
	}
	old := GitHubAccount{ID: 12, Login: "old-login", Created: time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err = c.LinkGitHub("alice", old, alice.Generation); err != nil {
		t.Fatal(err)
	}
	if err = c.GitHubAttribution("alice", "old-login@users.noreply.github.com", true); err != nil {
		t.Fatal("legacy noreply", err)
	}
	if err = c.LinkGitHub("bob", old, bob.Generation); err == nil {
		t.Fatal("one GitHub account linked to two local users")
	}
	c.SetUser(Bootstrap, User{ID: "alice", Admin: true})
	linked, _ := c.Account("alice")
	if linked.ID != alice.ID || linked.GitHub == nil {
		t.Fatal("permission update replaced identity")
	}
	old.Login = "new-login"
	if err = c.LinkGitHub("alice", old, linked.Generation); err != nil {
		t.Fatal(err)
	}
	if err = c.GitHubAttribution("alice", "12+new-login@users.noreply.github.com", true); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	persisted, err := c.Account("alice")
	if err != nil || persisted.ID != alice.ID || persisted.Identity().Email != "12+new-login@users.noreply.github.com" {
		t.Fatal("identity changed after restart", persisted, err)
	}
	var state catalogEnvelope
	if json.Unmarshal([]byte(storedCatalog(t, c)), &state) != nil || state.Version != catalogStorageVersion || state.Data.Accounts["alice"].ID != alice.ID {
		t.Fatal("identity not persisted")
	}
	if err = c.DisconnectGitHub("alice"); err != nil {
		t.Fatal(err)
	}
	local, _ := c.Account("alice")
	if local.ID != alice.ID || local.Identity() != alice.LocalIdentity() {
		t.Fatal("disconnect changed local identity")
	}
	c.SetUser(Bootstrap, User{ID: "alice", Disabled: true})
	if _, err = c.Account("alice"); err == nil {
		t.Fatal("disabled user got identity")
	}
	if err = c.LinkGitHub("alice", old, local.Generation); err == nil {
		t.Fatal("disabled user linked GitHub")
	}
}
func TestAccountPersistenceFailureDoesNotSwitchAttribution(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	original, err := c.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	restore := pgtest.FailWrites(t, c.db, "catalog")
	err = c.LinkGitHub("alice", GitHubAccount{ID: 12, Login: "alice", Created: time.Now()}, original.Generation)
	restore()
	if err == nil {
		t.Fatal("expected persistence failure")
	}
	current, _ := c.Account("alice")
	if current.ID != original.ID || current.GitHub != nil {
		t.Fatal("failed persistence changed attribution")
	}
}
