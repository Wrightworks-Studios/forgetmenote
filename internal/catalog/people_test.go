package catalog

import (
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestParticipantNamesFenceNotebookAndIdentityNamespaces(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Create(Bootstrap, "people", "People"); err != nil {
		t.Fatal(err)
	}
	for _, person := range []User{{ID: "alice", DisplayName: "Alice Reader"}, {ID: "bob", DisplayName: "Bob Reader"}, {ID: "unrelated", DisplayName: "Private unrelated name"}} {
		if err = c.SetUser(Bootstrap, person); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"alice", "bob"} {
		if err = c.Member(Bootstrap, "people", id, "read"); err != nil {
			t.Fatal(err)
		}
	}
	account, err := c.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SetUser(Bootstrap, User{ID: account.ID, DisplayName: "Separate login namespace"}); err != nil {
		t.Fatal(err)
	}
	if err = c.Member(Bootstrap, "people", account.ID, "read"); err != nil {
		t.Fatal(err)
	}
	identities := []ParticipantIdentity{{Subject: account.ID}, {Account: account.ID}, {Subject: "unrelated"}, {Account: "missing"}, {Subject: Bootstrap}}
	names, err := c.ParticipantNames("bob", "people", identities)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Separate login namespace", "Alice Reader", "Former contributor", "Former contributor", "Recovery administrator"}
	for i, name := range names {
		if name != want[i] {
			t.Fatalf("identity %d: %q != %q", i, name, want[i])
		}
	}
	names[1] = "changed"
	names, err = c.ParticipantNames("bob", "people", identities)
	if err != nil || names[1] != "Alice Reader" {
		t.Fatal("name projection escaped its owner", names, err)
	}
	if _, err = c.ParticipantNames("unrelated", "people", identities); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("unrelated reader obtained names", err)
	}
	if err = c.Member(Bootstrap, "people", "bob", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ParticipantNames("bob", "people", identities); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("revoked reader obtained names", err)
	}
}
