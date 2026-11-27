package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
)

// An approved chat move that a locked note outside the chat's corpus links
// into is refused, and the note's path reaches nothing the chat can read:
// not the model, the transcript or the approval's answer.
func TestChatMoveRefusalNamesNoNoteOutsideTheCorpus(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	const hidden = "Archive/hidden-ledger.md"
	for path, body := range map[string]string{"F/a.md": "# A\n", hidden: "---\nfmn-container: archive\n---\n# Ledger\n\n[A](../F/a.md)\n"} {
		if _, err := store.Save(t.Context(), path, []byte(body), "", "alice"); err != nil {
			t.Fatal(err)
		}
	}
	locks, err := store.Locks(t.Context(), "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetLock(t.Context(), "Archive", true, locks.Revision, "alice"); err != nil {
		t.Fatal(err)
	}
	status, err := store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	corpus := *c.Options.Corpus
	corpus.Archives = "exclude"
	run := chat.Run{ID: chat.NewID(), State: "running", Rounds: 1, Epoch: app.catalog.Epoch("alice"), Options: chat.Options{Corpus: &corpus, Connection: "local", Model: "synthetic", Mode: "edit"}}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "origin", RunID: run.ID, Role: "user", Content: "Rename the folder F to G"})
		c.Runs = append(c.Runs, run)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: run.Epoch}
	ctx, err := app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"project": "default", "from": "F", "to": "G", "revision": status.Revision})
	if err = app.chatrun.ExecuteChatTool(ctx, auth, c.ID, run.ID, chat.ToolCall{ID: "move", Name: "move_notes", Arguments: args}, app.chat.Config.Connections[0]); err != chatrun.ErrConfirmation {
		t.Fatalf("prepare: %v", err)
	}
	prepared, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	op := prepared.Runs[0].Operations[0]
	approve, _ := json.Marshal(map[string]string{"decision": "approve", "confirmation": op.Confirmation})
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/actions/"+op.ID, string(approve))

	if now, err := store.Status(t.Context()); err != nil || now.Revision != status.Revision {
		t.Fatalf("the refused move changed the notebook: %v %v", now.Revision, err)
	}
	completed, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript, _ := json.Marshal(completed)
	if !strings.Contains(string(transcript), "not accessible") {
		t.Errorf("the transcript does not say the move was refused: %s", transcript)
	}
	for _, text := range []string{string(transcript), response.Body.String()} {
		if strings.Contains(text, hidden) || strings.Contains(text, "hidden-ledger") || strings.Contains(text, "Archive/") {
			t.Errorf("the note outside the corpus reaches the chat: %s", text)
		}
	}
}
