package catalog

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestWorkspaceCreatorIsolationAndOwnerSafety(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Close() }()
	var creator User
	if err := json.Unmarshal([]byte(`{"id":"alice","canCreateProjects":true,"displayName":"Alice"}`), &creator); err != nil {
		t.Fatal(err)
	}
	if err := c.SetUser(Bootstrap, creator); err != nil {
		t.Fatal(err)
	}
	if err := c.Create("alice", "private", "Private"); err != nil {
		t.Fatalf("workspace creator rejected: %v", err)
	}
	if _, err := c.Access("alice", "private", "owner"); err != nil {
		t.Fatal("creator is not Owner", err)
	}
	if len(c.List("stranger")) != 0 || len(c.List("alice")) != 1 {
		t.Fatal("workspace isolation failed")
	}
	if err := c.Member("alice", "private", "bob", "edit"); err != nil {
		t.Fatal(err)
	}
	if err := c.Create("bob", "other", "Other"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("editor created workspace", err)
	}
	if err := c.Member("alice", "private", "alice", "edit"); err == nil {
		t.Fatal("removed last Owner")
	}
	if err := c.Member(Bootstrap, "private", "alice", ""); err == nil {
		t.Fatal("Admin removed last Owner")
	}
	if err := c.Member("alice", "private", "bob", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.Member("alice", "private", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.SetUser("bob", creator); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("Owner edited instance user", err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create("alice", "second", "Second"); err != nil {
		t.Fatal("creation grant not persisted", err)
	}
	creator.Disabled = true
	if err := c.SetUser(Bootstrap, creator); err != nil {
		t.Fatal(err)
	}
	if err := c.Create("alice", "third", "Third"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("disabled creator accepted", err)
	}
}

func TestPeopleSearchIsScopedAndDoesNotExposeAccountDetails(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Member(Bootstrap, "default", "owner", "owner")
	c.Member(Bootstrap, "default", "editor", "edit")
	for _, u := range []User{{ID: "a", DisplayName: "Same Name", Admin: true}, {ID: "b", DisplayName: "Same Name"}, {ID: "hidden", DisplayName: "Same Name", Disabled: true}} {
		if err := c.SetUser(Bootstrap, u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.FindPeople("editor", "default", "Same"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("editor searched directory", err)
	}
	if _, err := c.FindPeople("owner", "missing", "Same"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("unowned workspace searched", err)
	}
	people, err := c.FindPeople("owner", "default", "")
	if err != nil || len(people) != 0 {
		t.Fatal("empty query enumerated directory", people, err)
	}
	people, err = c.FindPeople("owner", "default", "same")
	if err != nil || len(people) != 2 || people[0].ID == people[1].ID {
		t.Fatal("ambiguous names lost stable identities", people, err)
	}
	raw, _ := json.Marshal(people)
	if string(raw) != `[{"id":"a","name":"Same Name"},{"id":"b","name":"Same Name"}]` {
		t.Fatalf("unexpected person details: %s", raw)
	}
}
