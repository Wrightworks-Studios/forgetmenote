package catalog

import (
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestOIDCProvisionLinkAndDisable(t *testing.T) {
	dir := t.TempDir()
	c, e := Open(dir, nil, testDB(t, dir))
	if e != nil {
		t.Fatal(e)
	}
	defer func() { c.Close() }()
	id, e := c.ResolveOIDC("https://id.example", "alice", "", "")
	if e != nil {
		t.Fatal(e)
	}
	again, e := c.ResolveOIDC("https://id.example", "alice", "", "")
	if e != nil || again != id {
		t.Fatal(again, e)
	}
	other, e := c.ResolveOIDC("https://other.example", "alice", "", "")
	if e != nil || other == id {
		t.Fatal(other, e)
	}
	if c.IsAdmin(id) || len(c.List(id)) != 0 {
		t.Fatal("login granted access")
	}
	a, _ := c.Account("legacy")
	c.Member(Bootstrap, "default", "legacy", "owner")
	linked, e := c.ResolveOIDC("https://id.example", "legacy-sub", "legacy", "")
	if e != nil || linked != "legacy" {
		t.Fatal(linked, e)
	}
	b, _ := c.Account(linked)
	if a.ID != b.ID {
		t.Fatal("lost attribution")
	}
	if _, e = c.ResolveOIDC("https://id.example", "legacy-sub", id, ""); e == nil {
		t.Fatal("stole link")
	}
	c.SetUser(Bootstrap, User{ID: id, Disabled: true})
	if _, e = c.ResolveOIDC("https://id.example", "alice", "", ""); !errors.Is(e, workspace.ErrForbidden) {
		t.Fatal("reactivated", e)
	}
	c.Close()
	c, e = Open(dir, nil, testDB(t, dir))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.ResolveOIDC("https://id.example", "alice", "", ""); e == nil {
		t.Fatal("disabled lost on restart")
	}
	if linked, e = c.ResolveOIDC("https://id.example", "legacy-sub", "", ""); e != nil || linked != "legacy" {
		t.Fatal(linked, e)
	}
}

func TestOIDCEmailDisplayNamePersistsAndPreservesLinkedName(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	id, err := c.ResolveOIDC("https://id.example", "person", "", "person@example.com")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if name, err := c.SubjectName(id); err != nil || name != "person@example.com" {
		t.Fatal("default name did not persist", name, err)
	}
	if err = c.SetUser(Bootstrap, User{ID: "legacy", DisplayName: "Existing Name"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Account("legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ResolveOIDC("https://id.example", "linked", "legacy", "other@example.com"); err != nil {
		t.Fatal(err)
	}
	if name, err := c.SubjectName("legacy"); err != nil || name != "Existing Name" {
		t.Fatal("link overwrote existing name", name, err)
	}
}

func TestAdminSeedPreservesLaterDecisions(t *testing.T) {
	c, e := Open(t.TempDir(), nil, pgtest.Org(t))
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = c.SeedAdmins([]string{"alice"}); e != nil {
		t.Fatal(e)
	}
	c.SetUser(Bootstrap, User{ID: "alice", Disabled: true})
	if e = c.SeedAdmins([]string{"alice"}); e != nil {
		t.Fatal(e)
	}
	if c.Enabled("alice") || c.IsAdmin("alice") {
		t.Fatal("startup reenabled user")
	}
}

func TestOIDCKeepsVerifiedEmailForMessagePairing(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	id, err := c.ResolveOIDC("https://id.example", "person", "", "Person@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.ObserveOIDCEmail("https://id.example", "person", "Person@Example.com", "Example.com"); err != nil {
		t.Fatal(err)
	}
	sequence := c.state.ActivitySequence
	if err = c.ObserveOIDCEmail("https://id.example", "person", "person@example.com", "example.com"); err != nil || c.state.ActivitySequence != sequence {
		t.Fatal("an unchanged sign-in rewrote the catalog", err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ExternalIdentities(id); len(got) != 1 || got[0].Email != "person@example.com" || got[0].HostedDomain != "example.com" {
		t.Fatalf("verified email did not persist: %+v", got)
	}
	if err = c.ObserveOIDCEmail("https://id.example", "person", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := c.ExternalIdentities(id); got[0].Email != "" || got[0].HostedDomain != "" {
		t.Fatalf("an unverified sign-in kept the old address: %+v", got)
	}
	if err = c.ObserveOIDCEmail("https://id.example", "stranger", "x@example.com", "example.com"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("an unknown identity recorded an address", err)
	}
}
