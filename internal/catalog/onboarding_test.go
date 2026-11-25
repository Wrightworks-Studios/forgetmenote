package catalog

import (
	"errors"
	"testing"

	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

func TestProvisionWorkNotebookIsolationIdempotenceAndFailure(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Close() }()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.SetUser(Bootstrap, User{ID: "alice"}))
	must(c.SetUser(Bootstrap, User{ID: "bob"}))
	if err = c.ProvisionWorkNotebook("alice", "alice", "work", "Work"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("employee provisioned notebook", err)
	}
	must(c.ProvisionWorkNotebook(Bootstrap, "alice", "work", "Work"))
	must(c.ProvisionWorkNotebook(Bootstrap, "alice", "work", "Work"))
	if c.IsAdmin("alice") || len(c.List("alice")) != 1 || len(c.List("bob")) != 0 {
		t.Fatal("unexpected authority")
	}
	if _, err = c.Access("alice", "work", "owner"); err != nil {
		t.Fatal(err)
	}
	if err = c.ProvisionWorkNotebook(Bootstrap, "bob", "work", "Work"); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("repurposed someone else's notebook", err)
	}
	must(c.Member(Bootstrap, "work", "bob", "read"))
	if err = c.ProvisionWorkNotebook(Bootstrap, "alice", "work", "Work"); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("accepted shared notebook as private", err)
	}
	inbox, err := c.EnsureInbox("alice")
	must(err)
	if err = c.ProvisionWorkNotebook(Bootstrap, "alice", inbox.ID, inbox.Name); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("repurposed Inbox", err)
	}
	must(c.SetUser(Bootstrap, User{ID: "bob", Disabled: true}))
	if err = c.ProvisionWorkNotebook(Bootstrap, "bob", "disabled-work", "Work"); err == nil {
		t.Fatal("provisioned disabled account")
	}
	// An interrupted catalog write exposes neither the new notebook nor new grants.
	restore := pgtest.FailWrites(t, c.db, "catalog")
	if err = c.ProvisionWorkNotebook(Bootstrap, "alice", "retry-work", "Retry work"); err == nil {
		t.Fatal("accepted failed persistence")
	}
	if _, err = c.Access("alice", "retry-work", "read"); err == nil {
		t.Fatal("failed creation became visible")
	}
	restore()
	must(c.ProvisionWorkNotebook(Bootstrap, "alice", "retry-work", "Retry work"))
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	must(err)
	must(c.ProvisionWorkNotebook(Bootstrap, "alice", "retry-work", "Retry work"))
	if c.IsAdmin("alice") {
		t.Fatal("provisioning elevated instance role")
	}
}

func TestOIDCEnrollmentPersistenceFailureIsAtomic(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.SetGroup(Bootstrap, Group{ID: "employees", Name: "Employees"}); err != nil {
		t.Fatal(err)
	}
	restore := pgtest.FailWrites(t, c.db, "catalog")
	if _, err = c.ResolveOIDC("https://issuer.example", "person", "", "person@example.com", "employees"); err == nil {
		t.Fatal("accepted interrupted enrollment")
	}
	org, err := c.Organization(Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if len(org.Groups[0].Members) != 0 {
		t.Fatal("failed enrollment granted membership")
	}
	restore()
	id, err := c.ResolveOIDC("https://issuer.example", "person", "", "person@example.com", "employees")
	if err != nil {
		t.Fatal(err)
	}
	if name, err := c.SubjectName(id); err != nil || name != "person@example.com" {
		t.Fatal("enrollment did not retain default name", name, err)
	}
	again, err := c.ResolveOIDC("https://issuer.example", "person", "", "", "employees")
	if err != nil || again != id {
		t.Fatal(again, err)
	}
	org, err = c.Organization(Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if len(org.Groups[0].Members) != 1 || org.Groups[0].Members[0] != id {
		t.Fatal("duplicate or missing enrollment", org.Groups)
	}
}
