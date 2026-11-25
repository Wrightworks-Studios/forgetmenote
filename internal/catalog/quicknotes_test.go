package catalog

import (
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"sync"
	"testing"
)

func TestProtectedInboxProvisioningAndPrivacy(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Account("alice"); err != nil {
		t.Fatal(err)
	}
	if c.CanCreateProjects("alice") {
		t.Fatal("fixture grants ordinary project creation")
	}
	p, err := c.EnsureInbox("alice")
	if err != nil {
		t.Fatalf("active account cannot provision protected Inbox: %v", err)
	}
	if c.CanCreateProjects("alice") {
		t.Fatal("Inbox conferred ordinary creation authority")
	}
	if _, err = c.Access("alice", p.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"alice", Bootstrap} {
		if err = c.Member(actor, p.ID, "bob", "read"); !errors.Is(err, workspace.ErrForbidden) {
			t.Fatalf("%s shared protected Inbox: %v", actor, err)
		}
		if err = c.Member(actor, p.ID, "alice", ""); !errors.Is(err, workspace.ErrForbidden) {
			t.Fatalf("%s removed protected owner: %v", actor, err)
		}
	}
	if _, err = c.Access(Bootstrap, p.ID, "read"); err != nil {
		t.Fatal("Admin authority changed", err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	restored, err := c.EnsureInbox("alice")
	if err != nil || restored.ID != p.ID {
		t.Fatalf("binding not restored: %+v %v", restored, err)
	}
	if _, err = c.Access("bob", p.ID, "read"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("private contents accessible", err)
	}
	if err = c.SetUser(Bootstrap, User{ID: "alice", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.EnsureInbox("alice"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("disabled account provisioned", err)
	}
}

func TestProtectedInboxConcurrentProvisioning(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var wg sync.WaitGroup
	ids := make(chan string, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := c.EnsureInbox("alice")
			if e != nil {
				t.Error(e)
				return
			}
			ids <- p.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if first != id {
			t.Fatal("multiple Inbox projects")
		}
	}
	if first == "" {
		t.Fatal("no Inbox")
	}
}
