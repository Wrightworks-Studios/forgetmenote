package catalog

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/workspace"
)

func TestGroupsResolveAndFenceRevokedWork(t *testing.T) {
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
	must(c.Member(Bootstrap, "default", "alice", "read"))
	account, err := c.Account("alice")
	must(err)
	g, err := c.SetGroup(Bootstrap, Group{ID: "engineering", Name: "Engineering", Members: []string{"alice"}})
	must(err)
	must(c.SetProjectGroup(Bootstrap, "default", g.ID, "edit"))
	if _, err = c.Access("alice", "default", "edit"); err != nil {
		t.Fatal(err)
	}
	list := c.List("alice")
	if len(list) != 1 || list[0].Role != "edit" || !list[0].Capabilities.Edit || len(list[0].GroupRoles) > 0 {
		t.Fatalf("effective list: %+v", list)
	}
	_, projects, err := c.Administration(Bootstrap)
	must(err)
	if projects[0].EffectiveMembers["alice"].Direct != "read" || len(projects[0].EffectiveMembers["alice"].Groups) != 1 {
		t.Fatalf("missing access provenance: %+v", projects)
	}
	epoch := c.ProjectAccessEpoch("alice", "default")
	g.Members = nil
	g, err = c.SetGroup(Bootstrap, g)
	must(err)
	if _, err = c.Access("alice", "default", "read"); err != nil {
		t.Fatal("lost direct grant", err)
	}
	if _, err = c.Access("alice", "default", "edit"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("group not revoked", err)
	}
	g.Members = []string{"alice"}
	_, err = c.SetGroup(Bootstrap, g)
	must(err)
	err = c.WithProjectAuthority("alice", "default", account.ID, "edit", c.Epoch("alice"), epoch, func(*workspace.Store) error { t.Fatal("old grant revived"); return nil })
	if !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal(err)
	}
	inbox, err := c.EnsureInbox("alice")
	must(err)
	if err = c.SetProjectGroup(Bootstrap, inbox.ID, g.ID, "read"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("shared Inbox", err)
	}
	if err = c.SetProjectGroup(Bootstrap, "default", g.ID, "owner"); err == nil {
		t.Fatal("group owns notebook")
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	must(err)
	if _, err = c.Access("alice", "default", "edit"); err != nil {
		t.Fatal("group not restored", err)
	}
	var stored catalogEnvelope
	if err = json.Unmarshal([]byte(storedCatalog(t, c)), &stored); err != nil || stored.Version != catalogStorageVersion {
		t.Fatal("old binary not fenced", stored.Version, err)
	}
}

func TestGroupConflictAndFailedWritePreservePermissionsAndAudit(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Member(Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	g, err := c.SetGroup(Bootstrap, Group{ID: "team", Name: "Team", Members: []string{"alice"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetGroup(Bootstrap, Group{ID: "team", Name: "Overwritten", Revision: 0}); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("lost update", err)
	}
	before, err := c.Organization(Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.FailWrites(t, c.db, "catalog")
	g.Members = nil
	if _, err = c.SetGroup(Bootstrap, g); err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	after, err := c.Organization(Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Groups[0].Members) != 1 || len(after.Activity) != len(before.Activity) {
		t.Fatal("failed write changed live state")
	}
}

func TestGroupDeletionCannotReviveInvitationsOrStaleEdits(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	g, err := c.SetGroup(Bootstrap, Group{ID: "team", Name: "Original team"})
	if err != nil {
		t.Fatal(err)
	}
	invite, err := c.CreateInvitation(Bootstrap, InvitationSpec{Name: "Colleague", Groups: []string{g.ID}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteGroup(Bootstrap, g.ID, g.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetGroup(Bootstrap, Group{ID: g.ID, Name: "Different team"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.InspectInvitation(invite.Token); !errors.Is(err, workspace.ErrForbidden) {
		t.Error("deleted group invitation revived", err)
	}
	if _, err = c.SetGroup(Bootstrap, g); !errors.Is(err, workspace.ErrConflict) {
		t.Error("stale editor overwrote replacement group", err)
	}
}

func TestOrganizationRecordsExternalAccountsAndOperatorGrants(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id, err := c.ResolveOIDC("https://identity.example", "subject", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.GrantAdmin(id); err != nil {
		t.Fatal(err)
	}
	if err = c.SeedAdmins([]string{"proxy-admin"}); err != nil {
		t.Fatal(err)
	}
	org, err := c.Organization(Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"account.external", "account.grant_admin", "account.seed_admin"} {
		found := false
		for _, event := range org.Activity {
			found = found || event.Action == action && event.Outcome == "succeeded"
		}
		if !found {
			t.Errorf("missing %s receipt", action)
		}
	}
}

func TestInvitationConcurrentAcceptanceExpiryAndReset(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	spec := InvitationSpec{Name: "Alice", Projects: map[string]string{"default": "edit"}}
	issued, err := c.CreateInvitation(Bootstrap, spec, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := c.AcceptInvitation(issued.Token, "", "alice", "a-long-password", true)
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("acceptances=%d", success)
	}
	id, epoch, err := c.AuthenticateLocal("alice", "a-long-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Access(id, "default", "edit"); err != nil {
		t.Fatal(err)
	}
	reset, err := c.CreateInvitation(Bootstrap, InvitationSpec{}, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.AcceptInvitation(reset.Token, "", "", "replacement-password", false); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("external mode reset local password", err)
	}
	if _, err = c.AcceptInvitation(reset.Token, "", "", "replacement-password", true); err != nil {
		t.Fatal(err)
	}
	if c.ValidSession(id, epoch) {
		t.Fatal("reset retained sessions")
	}
	exp, err := c.CreateInvitation(Bootstrap, spec, "")
	if err != nil {
		t.Fatal(err)
	}
	v := c.state.Invitations[exp.ID]
	v.Expires = time.Now().Add(-time.Second)
	c.state.Invitations[v.ID] = v
	if _, err = c.AcceptInvitation(exp.Token, "", "other", "a-long-password", true); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("accepted expired invite", err)
	}
	raw := storedCatalog(t, c)
	if strings.Contains(raw, issued.Token) || strings.Contains(raw, reset.Token) || strings.Contains(raw, "replacement-password") {
		t.Fatal("catalog contains invitation/password secret")
	}
}

func TestServiceAndOffboardingOwnershipBoundaries(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, u := range []User{{ID: "alice", Admin: true}, {ID: "bob"}} {
		if err = c.SetUser(Bootstrap, u); err != nil {
			t.Fatal(err)
		}
	}
	bot, err := c.CreateService("alice", "Build bot", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if c.Interactive(bot.ID) {
		t.Fatal("bot can sign in")
	}
	if _, err = c.EnsureInbox(bot.ID); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("bot has Inbox", err)
	}
	if err = c.Member("alice", "default", bot.ID, "owner"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("bot owns notebook", err)
	}
	if err = c.Member("alice", "default", bot.ID, "edit"); err != nil {
		t.Fatal(err)
	}
	if err = c.Create(bot.ID, "bot-notes", "Bot"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("bot created notebook", err)
	}
	disabled := true
	if err = c.UpdateUser("alice", UserUpdate{ID: "alice", Disabled: &disabled}); !errors.Is(err, ErrLastAdmin) {
		t.Fatal("disabled final human Admin", err)
	}
	if err = c.Member(Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	if err = c.UpdateUser(Bootstrap, UserUpdate{ID: "alice", Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Access(bot.ID, "default", "edit"); err != nil {
		t.Fatal("creator offboarding disabled independent bot", err)
	}
	report, err := c.Offboarding(Bootstrap, "alice")
	if err != nil || len(report.Services) != 1 || len(report.Owned) != 1 {
		t.Fatal("incomplete offboarding", report, err)
	}
	if err = c.ReassignOwnership(Bootstrap, "alice", "bob", report.Owned); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Access("bob", "default", "owner"); err != nil {
		t.Fatal(err)
	}
	if err = c.SetServiceSteward(Bootstrap, bot.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if err = c.UpdateUser(Bootstrap, UserUpdate{ID: bot.ID, Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Access(bot.ID, "default", "read"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("disabled bot retained access", err)
	}
}
