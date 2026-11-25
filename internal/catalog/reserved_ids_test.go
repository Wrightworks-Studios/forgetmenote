package catalog

import (
	"bytes"
	"forgetmenote/internal/pg/pgtest"
	"os"
	"path/filepath"
	"testing"
)

// Protected Inbox and assistant IDs derive from stable account IDs. Ordinary
// creation, provisioning and import cannot pre-claim one; the internal
// provisioning paths still work.
func TestNotebookCreationCannotClaimPersonalNotebookIDs(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.SetAssistantPolicy(Bootstrap, AssistantPolicy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err = c.SetUser(Bootstrap, User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	alice, err := c.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	source, err := c.Access(Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "default.bundle")
	if err = source.Backup(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"inbox-" + alice.ID, "assistant-" + alice.ID, "inbox-planning", "assistant-notes"} {
		if err := c.Create(Bootstrap, id, "Squatted"); err == nil {
			t.Errorf("created reserved notebook ID %s", id)
		}
		if err := c.ProvisionWorkNotebook("admin", "alice", id, "Squatted"); err == nil {
			t.Errorf("provisioned reserved notebook ID %s", id)
		}
		if err := c.Import(t.Context(), Bootstrap, id, "Squatted", bytes.NewReader(raw)); err == nil {
			t.Errorf("imported a bundle as reserved notebook ID %s", id)
		}
	}
	if err = c.Create(Bootstrap, "inboxes", "Ordinary"); err != nil {
		t.Fatal("ordinary ID sharing a prefix word was refused", err)
	}
	inbox, err := c.EnsureInbox("alice")
	if err != nil || inbox.ID != "inbox-"+alice.ID {
		t.Fatalf("protected Inbox provisioning: %+v %v", inbox, err)
	}
	settings, err := c.SaveAssistant("alice", AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC"})
	if err != nil || settings.Project != "assistant-"+alice.ID {
		t.Fatalf("assistant provisioning: %+v %v", settings, err)
	}
}
