package catalog

import (
	"errors"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestChatClientPolicyPersistsAndRejectsStaleOrFailedSaves(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	in := ChatClients{Clients: map[string]messaging.Client{"slack": {Workspace: "T123", Disabled: true}}}
	if _, err = c.SetChatClients("outsider", in); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal(err)
	}
	saved, err := c.SetChatClients(Bootstrap, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetChatClients(Bootstrap, in); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal(err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := c.ChatClients()
	if got.Revision != saved.Revision || !got.Clients["slack"].Disabled {
		t.Fatalf("lost chat policy: %+v", got)
	}
	delete(got.Clients, "slack")
	if !c.ChatClients().Clients["slack"].Disabled {
		t.Fatal("mutable policy escaped catalog")
	}
	pgtest.FailWrites(t, c.db, "catalog")
	in = c.ChatClients()
	in.Clients = map[string]messaging.Client{}
	if _, err = c.SetChatClients(Bootstrap, in); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	if !c.ChatClients().Clients["slack"].Disabled {
		t.Fatal("failed save changed live policy")
	}
}
