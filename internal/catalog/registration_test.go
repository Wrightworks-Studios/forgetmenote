package catalog

import (
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"reflect"
	"testing"
	"time"
)

func TestLegacyAccountDirectoryRepairPreservesIdentity(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	original := Account{ID: "0123456789abcdef0123456789abcdef", Generation: 3,
		GitHub: &GitHubAccount{ID: 12, Login: "linked-user", Created: time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)}}
	c.state.Accounts["chayla"] = original
	c.state.AccessEpoch["chayla"] = 7
	if err := c.persist(); err != nil {
		t.Fatal(err)
	}
	got, err := c.Account("chayla")
	if err != nil || !reflect.DeepEqual(got, original) {
		t.Fatalf("legacy identity changed: %+v %v", got, err)
	}
	if c.state.Users["chayla"] != (User{ID: "chayla"}) {
		t.Fatal("directory repair missing or granted authority")
	}
	if c.IsAdmin("chayla") || c.CanCreateProjects("chayla") || len(c.List("chayla")) != 0 || c.Epoch("chayla") != 7 {
		t.Fatal("repair changed permissions or session epoch")
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	got, err = c.Account("chayla")
	if err != nil || !reflect.DeepEqual(got, original) || c.state.Users["chayla"].ID != "chayla" {
		t.Fatal("repair did not survive restart")
	}
}

func TestAccountRegistrationRollsBackOnPersistenceFailure(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "new account", true: "legacy account"}[legacy], func(t *testing.T) {
			c, err := Open(t.TempDir(), nil, pgtest.Org(t))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if legacy {
				// Stored, so the reload after the failed write still holds it.
				c.state.Accounts["person"] = Account{ID: "0123456789abcdef0123456789abcdef", Generation: 4}
				if err := c.persist(); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := json.Marshal(c.state)
			restore := pgtest.FailWrites(t, c.db, "catalog")
			if _, err := c.Account("person"); err == nil {
				t.Fatal("expected registration persistence failure")
			}
			restore()
			after, _ := json.Marshal(c.state)
			if string(before) != string(after) {
				t.Fatal("failed registration changed catalog state")
			}
			if _, err := c.Account("person"); err != nil {
				t.Fatal("registration retry failed", err)
			}
			if c.state.Users["person"] != (User{ID: "person"}) {
				t.Fatal("retry missing from directory")
			}
		})
	}
}

func TestAccountRegistrationPreservesExistingUserFlags(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	original := User{ID: "person", DisplayName: "Existing Person", Admin: true, CanCreateProjects: true}
	if err := c.SetUser(Bootstrap, original); err != nil {
		t.Fatal(err)
	}
	account, err := c.Account("person")
	if err != nil || c.state.Users["person"] != original {
		t.Fatal("registration overwrote existing grants", err)
	}
	restore := pgtest.FailWrites(t, c.db, "catalog")
	again, err := c.Account("person")
	restore()
	if err != nil || again.ID != account.ID {
		t.Fatal("repeat login changed identity or rewrote catalog", err)
	}
	original.Disabled = true
	if err := c.SetUser(Bootstrap, original); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Account("person"); err == nil || c.state.Users["person"] != original {
		t.Fatal("disabled user reenabled")
	}
	if _, err := c.Account(Bootstrap); err != nil {
		t.Fatal(err)
	}
	if _, exists := c.state.Users[Bootstrap]; exists {
		t.Fatal("recovery account added to ordinary directory")
	}
}
