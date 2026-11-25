package web

import (
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

// Shared directory writes need note-write consent in addition to management.
func TestAssistantResponsibilitiesRequireNoteWriteConsent(t *testing.T) {
	app, _ := fixture(t)
	if err := app.catalog.Create(catalog.Bootstrap, "directory", "Directory"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "directory", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true, DirectoryProject: "directory"}); err != nil {
		t.Fatal(err)
	}
	account, err := app.catalog.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"user": "alice", "content": "# Alice\n\nOwns the synthetic launch.\n", "revision": ""}
	manageOnly := actionClient(t, app, "alice", "notes:read notes:manage")
	if text := mcpToolError(t, manageOnly, "set_assistant_responsibilities", args); !strings.Contains(text, workspace.ErrForbidden.Error()) {
		t.Fatalf("unexpected refusal: %s", text)
	}
	store, err := app.catalog.Access("alice", "directory", "read")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Read(t.Context(), "people/"+account.ID+".md", ""); err == nil {
		t.Fatal("responsibilities written without notes:write")
	}
	writer := actionClient(t, app, "alice", "notes:read notes:write notes:manage")
	actionCall(t, writer, "set_assistant_responsibilities", args, false)
	if doc, err := store.Read(t.Context(), "people/"+account.ID+".md", ""); err != nil || !strings.Contains(string(doc.Content), "synthetic launch") {
		t.Fatalf("consented responsibilities write: %v", err)
	}
}
