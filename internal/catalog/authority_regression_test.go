package catalog

import (
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestAdminDemotionRevokesExistingAuthorityAcrossRestore(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Member(Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	admin := true
	if err = c.UpdateUser(Bootstrap, UserUpdate{ID: "alice", Admin: &admin}); err != nil {
		t.Fatal(err)
	}
	account, err := c.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	epoch, projectEpoch := c.Epoch("alice"), c.ProjectAccessEpoch("alice", "default")
	admin = false
	if err = c.UpdateUser(Bootstrap, UserUpdate{ID: "alice", Admin: &admin}); err != nil {
		t.Fatal(err)
	}
	if c.Epoch("alice") == epoch {
		t.Error("admin demotion did not revoke earlier authority epoch")
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	admin = true
	if err = c.UpdateUser(Bootstrap, UserUpdate{ID: "alice", Admin: &admin}); err != nil {
		t.Fatal(err)
	}
	called := false
	err = c.WithProjectAuthority("alice", "default", account.ID, "owner", epoch, projectEpoch, func(*workspace.Store) error { called = true; return nil })
	if !errors.Is(err, workspace.ErrForbidden) || called {
		t.Fatal("restored admin revived old transaction grant", err, called)
	}
	if err = c.WithProjectAuthority("alice", "default", account.ID, "owner", c.Epoch("alice"), projectEpoch, func(*workspace.Store) error { return nil }); err != nil {
		t.Fatal("new admin authority unusable", err)
	}
}

func TestFailedAdminDemotionPreservesAuthorityEpoch(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.SetUser(Bootstrap, User{ID: "alice", Admin: true}); err != nil {
		t.Fatal(err)
	}
	epoch := c.Epoch("alice")
	// The catalog owner must roll its in-memory authority back if publication fails.
	pgtest.FailWrites(t, c.db, "catalog")
	if err = c.SetUser(Bootstrap, User{ID: "alice", Admin: false}); err == nil {
		t.Fatal("synthetic catalog write unexpectedly succeeded")
	}
	if !c.IsAdmin("alice") || c.Epoch("alice") != epoch {
		t.Fatal("failed demotion changed live authority")
	}
}
