package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

// trashLinkersFixture saves F/a.md and two notes linking to it, L/free.md
// and K/locked.md, moves both to the Trash and locks K, the location of
// the second. It returns their Trash entries by original path.
func trashLinkersFixture(t *testing.T, store *workspace.Store, actor string) map[string]workspace.TrashEntry {
	t.Helper()
	ctx := t.Context()
	for path, body := range map[string]string{"F/a.md": "# A\n", "L/free.md": "# Free\n\n[A](../F/a.md)\n", "K/locked.md": "# Locked\n\n[A](../F/a.md)\n"} {
		if _, err := store.Save(ctx, path, []byte(body), "", actor); err != nil {
			t.Fatal(err)
		}
	}
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	trashed, err := store.TrashNotes(ctx, []string{"L/free.md", "K/locked.md"}, status.Revision, actor)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := store.Locks(ctx, "K")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetLock(ctx, "K", true, locks.Revision, actor); err != nil {
		t.Fatal(err)
	}
	entries := map[string]workspace.TrashEntry{}
	for _, entry := range trashed.Entries {
		entries[entry.OriginalPath] = entry
	}
	return entries
}

// An approved chat move updates the links of the notes in the Trash it may
// change and skips one trashed from a location now locked, without
// refusing. The model learns only how many; no Trash path reaches the
// model, the transcript or the approval's answer. The updated note reaches
// the new location once restored.
func TestChatMoveUpdatesTrashLinksAndReportsOnlyCounts(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	entries := trashLinkersFixture(t, store, "alice")
	free, locked := entries["L/free.md"], entries["K/locked.md"]
	status, err := store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	run := chat.Run{ID: chat.NewID(), State: "running", Rounds: 1, Epoch: app.catalog.Epoch("alice"), Options: chat.Options{Corpus: c.Options.Corpus, Connection: "local", Model: "synthetic", Mode: "edit"}}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "origin", RunID: run.ID, Role: "user", Content: "Rename the folder F to G"})
		c.Runs = append(c.Runs, run)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	auth := runAuth{User: "alice", Project: "default", Epoch: run.Epoch}
	ctx, err := app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"project": "default", "from": "F", "to": "G", "revision": status.Revision})
	if err = app.executeChatTool(ctx, auth, c.ID, run.ID, chat.ToolCall{ID: "move", Name: "move_notes", Arguments: args}, app.chat.Config.Connections[0]); err != errChatConfirmation {
		t.Fatalf("prepare: %v", err)
	}
	prepared, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	op := prepared.Runs[0].Operations[0]
	approve, _ := json.Marshal(map[string]string{"decision": "approve", "confirmation": op.Confirmation})
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/actions/"+op.ID, string(approve))
	if response.Code != http.StatusOK {
		t.Fatalf("approval: %d %s", response.Code, response.Body)
	}

	completed, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result workspace.MoveResult
	if err = json.Unmarshal(completed.Runs[0].Operations[0].Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Revision == status.Revision || result.TrashLinksUpdated != 1 || result.TrashLinksSkipped != 1 {
		t.Fatalf("the approved move: %s", completed.Runs[0].Operations[0].Result)
	}
	var model map[string]any
	for _, message := range completed.Runs[0].Messages {
		if message.Role == "tool" && message.ToolCallID == "move" {
			if err = json.Unmarshal([]byte(message.Content), &model); err != nil {
				t.Fatal(message.Content, err)
			}
		}
	}
	if model["completed"] != true || model["revision"] != result.Revision || model["trashLinksUpdated"] != float64(1) || model["trashLinksSkipped"] != float64(1) {
		t.Errorf("what the model learns of the move: %v", model)
	}
	transcript, _ := json.Marshal(completed)
	for _, text := range []string{string(transcript), response.Body.String()} {
		for _, entry := range entries {
			if strings.Contains(text, entry.Path) || strings.Contains(text, entry.ID) {
				t.Errorf("%s in the Trash reaches the chat: %s", entry.Path, text)
			}
		}
	}

	if doc, err := store.Read(t.Context(), free.Path, ""); err != nil || string(doc.Content) != "# Free\n\n[A](../G/a.md)\n" {
		t.Errorf("the note in the Trash the move may change: %q %v", doc.Content, err)
	}
	if doc, err := store.Read(t.Context(), locked.Path, ""); err != nil || string(doc.Content) != "# Locked\n\n[A](../F/a.md)\n" {
		t.Errorf("the note in the Trash from a locked location: %q %v", doc.Content, err)
	}
	status, err = store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.RestoreTrash(t.Context(), free.ID, "", status.Revision, "alice"); err != nil {
		t.Fatal(err)
	}
	if doc, err := store.Read(t.Context(), "L/free.md", ""); err != nil || string(doc.Content) != "# Free\n\n[A](../G/a.md)\n" {
		t.Errorf("the restored note: %q %v", doc.Content, err)
	}
	if _, err = store.Read(t.Context(), "G/a.md", ""); err != nil {
		t.Errorf("the restored note's link target: %v", err)
	}
}

// A chat action that is not a move still may not change a note in the
// Trash: a change group editing one is refused before it is offered.
func TestChatChangeGroupCannotEditTrashNotes(t *testing.T) {
	app, store := fixture(t)
	entries := trashLinkersFixture(t, store, "test")
	free := entries["L/free.md"]
	status, err := store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap)
	ctx = context.WithValue(ctx, chatScopeKey{}, &chatRunScope{Project: "default", Run: chat.Run{Options: chat.Options{Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude"}}}, Content: workspace.ContentScope{Policy: workspace.ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}})
	args, _ := json.Marshal(map[string]any{"project": "default", "mode": "apply", "group": workspace.EditGroup{ID: "trash-edit", Summary: "Edit a note in the Trash", Edits: []workspace.NoteEdit{{Path: free.Path, Content: "# Free\n\n[A](../G/a.md)\n", Revision: status.Revision}}}})
	if _, err = app.dispatchAction(ctx, "default", "submit_change_group", args); err == nil {
		t.Fatal("a chat change group edited a note in the Trash")
	}
	if doc, err := store.Read(t.Context(), free.Path, ""); err != nil || string(doc.Content) != "# Free\n\n[A](../F/a.md)\n" {
		t.Errorf("the note in the Trash: %q %v", doc.Content, err)
	}
}

// move_notes over MCP reports how many notes in the Trash a move updated;
// an MCP client's move, like the browser's, still refuses one it may not
// change and names it.
func TestMCPMoveNotesCountsTrashLinks(t *testing.T) {
	app, store := fixture(t)
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	entries := trashLinkersFixture(t, store, "test")
	status, err := store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	move := map[string]any{"project": "default", "from": "F", "to": "G", "revision": status.Revision}
	refused, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "move_notes", Arguments: move})
	if err != nil || !refused.IsError {
		t.Fatalf("a move reaching a note in the Trash from a locked location: %+v %v", refused, err)
	}
	if raw, _ := json.Marshal(refused.Content); !strings.Contains(string(raw), "now in the Trash as "+entries["K/locked.md"].Path) {
		t.Errorf("the refusal: %s", raw)
	}

	locks, err := store.Locks(t.Context(), "K")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetLock(t.Context(), "K", false, locks.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	status, err = store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	move["revision"] = status.Revision
	moved, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "move_notes", Arguments: move})
	if err != nil || moved.IsError {
		t.Fatalf("the move: %+v %v", moved, err)
	}
	raw, _ := json.Marshal(moved.StructuredContent)
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result["updatedLinks"] != float64(2) || result["trashLinksUpdated"] != float64(2) {
		t.Errorf("the move's result: %s", raw)
	}
	if _, skipped := result["trashLinksSkipped"]; skipped {
		t.Errorf("a move that skipped nothing reports skipped notes: %s", raw)
	}
}
