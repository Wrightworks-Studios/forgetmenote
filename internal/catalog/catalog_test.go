package catalog

import (
	"context"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"testing"

	"forgetmenote/internal/workspace"
)

func TestProjectsIsolateContentAndEnforceRoles(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Create(Bootstrap, "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	if err = c.Create(Bootstrap, "beta", "Beta"); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"reader": "read", "reviewer": "comment", "writer": "edit"} {
		if err = c.Member(Bootstrap, "alpha", user, role); err != nil {
			t.Fatal(err)
		}
	}
	a, err := c.Access("writer", "alpha", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Save(context.Background(), "notes/a.md", []byte("private"), "", "writer"); err != nil {
		t.Fatal(err)
	}
	for _, check := range [][3]string{{"reader", "alpha", "comment"}, {"reviewer", "alpha", "edit"}, {"writer", "beta", "read"}, {"stranger", "alpha", "read"}} {
		if _, err = c.Access(check[0], check[1], check[2]); !errors.Is(err, workspace.ErrForbidden) {
			t.Errorf("allowed %v: %v", check, err)
		}
	}
	if len(c.List("writer")) != 1 {
		t.Fatal("project listing leaked")
	}
	if _, err = c.Access("reviewer", "alpha", "comment"); err != nil {
		t.Fatal(err)
	}
	b, _ := c.Access(Bootstrap, "beta", "read")
	if rows, _ := b.List(context.Background()); len(rows) != 0 {
		t.Fatal("cross-project content")
	}
}
func TestPermissionsPersistAndRevocationIsImmediate(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Member(Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	if err = c.SetUser(Bootstrap, User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Access("alice", "default", "edit"); err != nil {
		t.Fatal(err)
	}
	if err = c.SetUser("admin", User{ID: "alice", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Access("alice", "default", "read"); err == nil {
		t.Fatal("disabled user still authorized")
	}
	if err = c.Member("alice", "default", "alice", "edit"); err == nil {
		t.Fatal("user granted own access")
	}
	if err = c.Create(Bootstrap, "../outside", "Unsafe"); err == nil {
		t.Fatal("unsafe project path")
	}
}

func TestOwnersManageOnlyTheirProjects(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Create(Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	if err = c.Member(Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatalf("assign owner: %v", err)
	}
	if _, err = c.Access("alice", "default", "owner"); err != nil {
		t.Fatal(err)
	}
	if err = c.Member("alice", "default", "bob", "edit"); err != nil {
		t.Fatal("owner cannot manage members", err)
	}
	if err = c.Member("bob", "default", "bob", "owner"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("editor escalated", err)
	}
	if err = c.Member("alice", "other", "alice", "owner"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("owner crossed project boundary", err)
	}
	if err = c.SetUser("alice", User{ID: "alice", Admin: true}); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("owner became admin", err)
	}
	users, projects, err := c.Administration("alice")
	if err != nil || len(users) != 2 || len(projects) != 1 || projects[0].ID != "default" {
		t.Fatalf("owner administration leaked: %v %v %v", users, projects, err)
	}
}
