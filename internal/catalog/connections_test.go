package catalog

import (
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestConnectionGrantsPersistRevokeAndFenceOldBinaries(t *testing.T) {
	dir := t.TempDir()
	c, e := Open(dir, nil, testDB(t, dir))
	if e != nil {
		t.Fatal(e)
	}
	defer func() { c.Close() }()
	if e = c.Member(Bootstrap, "default", "alice", "read"); e != nil {
		t.Fatal(e)
	}
	group, e := c.SetGroup(Bootstrap, Group{ID: "engineering", Name: "Engineering", Members: []string{"alice"}})
	if e != nil {
		t.Fatal(e)
	}
	grant, e := c.SetConnectionAccess(Bootstrap, ConnectionGrant{Connection: "data", Groups: map[string][]string{group.ID: {"query"}}})
	if e != nil {
		t.Fatal(e)
	}
	if !c.ConnectionAllowed("alice", "data", "query") || c.ConnectionAllowed("alice", "data", "write") {
		t.Fatal("operation grant mismatch")
	}
	if _, e = c.SetConnectionAccess(Bootstrap, ConnectionGrant{Connection: "data"}); !errors.Is(e, workspace.ErrConflict) {
		t.Fatal("stale grant replaced", e)
	}
	c.Close()
	c, e = Open(dir, nil, testDB(t, dir))
	if e != nil {
		t.Fatal(e)
	}
	if !c.ConnectionAllowed("alice", "data", "query") {
		t.Fatal("grant lost")
	}
	if e = c.DeleteGroup(Bootstrap, group.ID, group.Revision); e != nil {
		t.Fatal(e)
	}
	if _, e = c.SetGroup(Bootstrap, Group{ID: group.ID, Name: group.Name, Members: []string{"alice"}}); e != nil {
		t.Fatal(e)
	}
	if c.ConnectionAllowed("alice", "data", "query") {
		t.Fatal("recreated group inherited deleted connection grant")
	}
	grant, e = c.ConnectionAccess(Bootstrap, "data")
	if e != nil {
		t.Fatal(e)
	}
	grant.Groups = nil
	if _, e = c.SetConnectionAccess(Bootstrap, grant); e != nil {
		t.Fatal(e)
	}
	if c.ConnectionAllowed("alice", "data", "query") {
		t.Fatal("revoked grant retained")
	}
	if _, e = c.SetConnectionAccess("alice", grant); !errors.Is(e, workspace.ErrForbidden) {
		t.Fatal("reader managed grants", e)
	}
}

func TestConnectionGrantResultDoesNotAliasCatalog(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Member(Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	group, err := c.SetGroup(Bootstrap, Group{ID: "engineering", Name: "Engineering", Members: []string{"alice"}})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := c.SetConnectionAccess(Bootstrap, ConnectionGrant{Connection: "data", Users: map[string][]string{"alice": {"query"}}, Groups: map[string][]string{group.ID: {"query"}}})
	if err != nil {
		t.Fatal(err)
	}
	grant.Users["alice"][0] = "write"
	grant.Groups[group.ID][0] = "write"
	if !c.ConnectionAllowed("alice", "data", "query") || c.ConnectionAllowed("alice", "data", "write") {
		t.Fatal("mutating the returned grant changed stored permissions")
	}
}
