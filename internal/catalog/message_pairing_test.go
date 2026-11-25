package catalog

import (
	"slices"
	"testing"

	"forgetmenote/internal/messaging"
)

func pairingAccount(t *testing.T, c *Catalog, sub, email, domain string) string {
	t.Helper()
	id, err := c.ResolveOIDC("https://accounts.example", sub, "", email)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.ObserveOIDCEmail("https://accounts.example", sub, email, domain); err != nil {
		t.Fatal(err)
	}
	return id
}

func pairingConflictUsers(p MessagePairing) []string {
	out := []string{}
	for _, conflict := range p.Conflicts {
		out = append(out, conflict.User)
	}
	slices.Sort(out)
	return out
}

func TestEmailPairingLinksOnlyUnambiguousWorkspaceAccounts(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	alice := pairingAccount(t, c, "alice", "alice@example.com", "example.com")
	personal := pairingAccount(t, c, "bob-personal", "bob@example.com", "")
	twinA := pairingAccount(t, c, "twin-a", "twin@example.com", "example.com")
	twinB := pairingAccount(t, c, "twin-b", "twin@example.com", "example.com")
	mapped := pairingAccount(t, c, "mapped", "mapped@example.com", "example.com")
	members := []messaging.Member{
		{User: "UALICE", Email: "Alice@Example.com"},
		{User: "UBOB", Email: "bob@example.com"},
		{User: "UTWIN", Email: "twin@example.com"},
		{User: "UMAPPED", Email: "mapped@example.com"},
		{User: "UNOBODY", Email: "nobody@example.com"},
	}
	preview, err := c.PairMessageUsersByEmail(Bootstrap, "slack", "T1", []string{"Example.com"}, members, map[string]bool{"UMAPPED": true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Paired) != 1 || c.MessageLink("slack", "T1", "UALICE") != "" {
		t.Fatalf("a dry run changed links or found the wrong pairs: %+v", preview)
	}
	got, err := c.PairMessageUsersByEmail(Bootstrap, "slack", "T1", []string{"example.com"}, members, map[string]bool{"UMAPPED": true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.MessageLink("slack", "T1", "UALICE") != alice || len(got.Paired) != 1 || got.Paired[0].Source != "email" {
		t.Fatalf("the unambiguous member was not paired: %+v", got)
	}
	for _, user := range []string{"UBOB", "UTWIN", "UMAPPED", "UNOBODY"} {
		if c.MessageLink("slack", "T1", user) != "" {
			t.Fatalf("%s was paired without a single verified workspace match", user)
		}
	}
	if users := pairingConflictUsers(got); !slices.Equal(users, []string{"UTWIN"}) || !slices.Equal(got.Conflicts[0].Accounts, []string{min(twinA, twinB), max(twinA, twinB)}) {
		t.Fatalf("conflicts = %+v", got.Conflicts)
	}
	_, _ = personal, mapped
	c.Close()
	if c, err = Open(dir, nil, testDB(t, dir)); err != nil {
		t.Fatal(err)
	}
	if c.MessageLink("slack", "T1", "UALICE") != alice {
		t.Fatal("pairing did not persist")
	}
	// Linking a paired member yourself makes the link yours.
	if err = c.LinkMessageUser(alice, "slack", "T1", "UALICE"); err != nil {
		t.Fatal(err)
	}
	members[0].Email = "alice.new@example.com"
	if _, err = c.PairMessageUsersByEmail(Bootstrap, "slack", "T1", []string{"example.com"}, members, nil, false); err != nil || c.MessageLink("slack", "T1", "UALICE") != alice {
		t.Fatal("pairing removed a link the person confirmed", err)
	}
}

func TestEmailPairingNeverOverridesPeopleAndFollowsChanges(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	alice := pairingAccount(t, c, "alice", "alice@example.com", "example.com")
	carol := pairingAccount(t, c, "carol", "carol@example.com", "example.com")
	dave := pairingAccount(t, c, "dave", "dave@example.com", "example.com")
	if err = c.LinkMessageUser(carol, "slack", "T1", "UCAROL-OLD"); err != nil {
		t.Fatal(err)
	}
	if err = c.LinkMessageUser(dave, "slack", "T1", "UTAKEN"); err != nil {
		t.Fatal(err)
	}
	members := []messaging.Member{{User: "UALICE", Email: "alice@example.com"}, {User: "UCAROL", Email: "carol@example.com"}, {User: "UTAKEN", Email: "alice-2@example.com"}}
	pair := func(members []messaging.Member) MessagePairing {
		t.Helper()
		got, err := c.PairMessageUsersByEmail(Bootstrap, "slack", "T1", []string{"example.com"}, members, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := pair(members)
	if c.MessageLink("slack", "T1", "UALICE") != alice || c.MessageLink("slack", "T1", "UCAROL") != "" || c.MessageLink("slack", "T1", "UTAKEN") != dave {
		t.Fatalf("pairing overrode a link someone made: %+v", got)
	}
	if users := pairingConflictUsers(got); !slices.Equal(users, []string{"UCAROL"}) {
		t.Fatalf("conflicts = %+v", got.Conflicts)
	}
	if c.MessagePairingAffected("slack", "T1", []string{"example.com"}, messaging.Member{User: "UTAKEN"}, false) {
		t.Fatal("a member event would rerun pairing for a link someone made")
	}

	// Unlinking an automatic pairing is remembered until the person links again.
	if err = c.UnlinkMessageUser(alice, "slack", "T1", "UALICE"); err != nil {
		t.Fatal(err)
	}
	if got = pair(members); c.MessageLink("slack", "T1", "UALICE") != "" || got.Declined != 1 {
		t.Fatalf("an unlinked pairing came back: %+v", got)
	}
	if err = c.LinkMessageUser(alice, "slack", "T1", "UALICE"); err != nil {
		t.Fatal(err)
	}
	if pair(members); c.MessageLink("slack", "T1", "UALICE") != alice {
		t.Fatal("pairing removed a link the person made")
	}
	if err = c.UnlinkMessageUser(alice, "slack", "T1", "UALICE"); err != nil {
		t.Fatal(err)
	}
	if pair(members); c.MessageLink("slack", "T1", "UALICE") != "" {
		t.Fatal("unlinking a link the person made did not stop automatic pairing")
	}

	// A changed address or a disabled account removes only automatic pairings.
	bob := pairingAccount(t, c, "bob", "bob@example.com", "example.com")
	members = append(members, messaging.Member{User: "UBOB", Email: "bob@example.com"})
	if pair(members); c.MessageLink("slack", "T1", "UBOB") != bob {
		t.Fatal("bob was not paired")
	}
	members[3].Email = "bob.new@example.com"
	if got = pair(members); c.MessageLink("slack", "T1", "UBOB") != "" || len(got.Unpaired) != 1 {
		t.Fatalf("a changed address kept the pairing: %+v", got)
	}
	members[3].Email = "bob@example.com"
	pair(members)
	disabled := true
	if err = c.UpdateUser(Bootstrap, UserUpdate{ID: bob, Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if pair(members); c.MessageLink("slack", "T1", "UBOB") != "" || c.MessageLink("slack", "T1", "UTAKEN") != dave {
		t.Fatal("a disabled account stayed paired, or a manual link was removed")
	}
	if _, err = c.PairMessageUsersByEmail(Bootstrap, "slack", "T1", []string{"example.com"}, []messaging.Member{{User: "UALICE"}}, nil, false); err == nil {
		t.Fatal("a member list without addresses was accepted, which would unpair everyone")
	}
}
