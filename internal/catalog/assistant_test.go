package catalog

import (
	"strings"
	"testing"
)

func TestPersonalAssistantSurvivesRestartWithoutSharing(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetAssistantPolicy(Bootstrap, AssistantPolicy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	in := AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "America/Denver", Features: []string{"email", "slack"}}
	settings, err := c.SaveAssistant("alice", in)
	if err != nil {
		t.Fatal(err)
	}
	account, err := c.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := c.Assistant("alice")
	if err != nil || out.Revision != settings.Revision || out.Project != settings.Project {
		t.Fatalf("lost binding: %#v %v", out, err)
	}
	if _, err = c.Access("bob", out.Project, "read"); err == nil {
		t.Fatal("another employee obtained access")
	}
	if err = c.Member("alice", out.Project, "bob", "read"); err == nil {
		t.Fatal("private assistant was shared")
	}
	if _, err = c.SaveAssistant("alice", in); err == nil {
		t.Fatal("stale settings overwrote consent")
	}
	if user, ok := c.AssistantSubject(account.ID); !ok || user != "alice" {
		t.Fatal("verified identity not resolvable")
	}
	if err = c.SetUser(Bootstrap, User{ID: "alice", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if len(c.ActiveAssistants()) != 0 {
		t.Fatal("disabled employee still scheduled")
	}
	if _, ok := c.AssistantSubject(account.ID); ok {
		t.Fatal("disabled employee still linked")
	}
}

// Memory became on by default. Migration 0015 turns on every setting saved
// before then, which stored the old default; a choice saved afterwards holds,
// including off, and survives later migrate runs.
func TestMemoryIsOnForSettingsSavedBeforeItsDefault(t *testing.T) {
	dir := t.TempDir()
	open := func() *Catalog {
		t.Helper()
		c, err := Open(dir, nil, testDB(t, dir))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	migrate := func(want int) {
		t.Helper()
		if applied, err := testDB(t, dir).Migrate(t.Context()); err != nil || applied != want {
			t.Fatalf("migrate applied %d, want %d: %v", applied, want, err)
		}
	}
	c := open()
	if _, err := c.SaveAssistant("alice", AssistantSettings{TimeZone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	// The release adds the migration to a database that already holds settings.
	if _, err := testDB(t, dir).Pool.Exec(t.Context(), "DELETE FROM schema_migrations WHERE name = '0015_memory_on_by_default.sql'"); err != nil {
		t.Fatal(err)
	}
	migrate(1)

	c = open()
	settings, err := c.Assistant("alice")
	if err != nil || !settings.MaintainMemory {
		t.Fatalf("settings saved before the default kept memory off: %+v %v", settings, err)
	}
	settings.MaintainMemory = false
	if _, err = c.SaveAssistant("alice", settings); err != nil {
		t.Fatal(err)
	}
	c.Close()
	migrate(0)
	c = open()
	defer c.Close()
	if settings, err = c.Assistant("alice"); err != nil || settings.MaintainMemory {
		t.Fatalf("a choice saved after the default did not hold: %+v %v", settings, err)
	}
}

func TestProjectMemoryNotebooksStayPrivateWithoutTheAssistant(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ProjectMemory("alice", "github.com/pleish/forgetmenote", true); err == nil {
		t.Fatal("project memory without the core memory notebook")
	}
	if _, err = c.SaveAssistant("alice", AssistantSettings{TimeZone: "UTC"}); err != nil {
		t.Fatal("memory needs no assistant policy:", err)
	}
	account, _ := c.Account("alice")
	id, err := c.ProjectMemory("alice", "github.com/pleish/forgetmenote", true)
	if err != nil || !strings.HasPrefix(id, "assistant-"+account.ID+"-") || len(id) != 59 {
		t.Fatalf("project memory %q: %v", id, err)
	}
	if again, err := c.ProjectMemory("alice", "github.com/pleish/forgetmenote", false); err != nil || again != id {
		t.Fatalf("lookup %q: %v", again, err)
	}
	if other, err := c.ProjectMemory("alice", "billing", false); err != nil || other != "" {
		t.Fatalf("lookup created a notebook %q: %v", other, err)
	}
	if err = c.Member("alice", id, "bob", "read"); err == nil {
		t.Fatal("project memory was shared")
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Access("alice", id, "owner"); err != nil {
		t.Fatal("project memory lost on restart:", err)
	}
	if _, err = c.Access("bob", id, "read"); err == nil {
		t.Fatal("another person reads project memory")
	}
	if p := c.state.Projects[id]; p.AssistantOwner != account.ID || p.Name != "Memory: github.com/pleish/forgetmenote" {
		t.Fatalf("project memory notebook: %+v", p)
	}
}
