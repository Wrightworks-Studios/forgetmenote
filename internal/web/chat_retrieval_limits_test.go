package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/chat"
	"net/http"
	"strings"
	"testing"
)

func TestChatRetrievalHonorsPerNoteAllowance(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	var body strings.Builder
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&body, "## Saffron section %d\nSaffron evidence item %d is distinct.\n", i, i)
	}
	raw, _ := json.Marshal(map[string]string{"path": "notes/saffron.md", "content": body.String(), "revision": ""})
	if w := call(app, "POST", "/api/note", string(raw), testToken); w.Code != 200 {
		t.Fatal(w.Body)
	}
	connection := app.chat.Config.Connections[0]
	connection.Limits.PerNotePassages = 1
	c.Turns = []chat.Turn{{ID: chat.NewID(), Role: "user", Content: "saffron"}}
	_, citations, _, err := initialChatTestMessages(t, app, c, connection)
	if err != nil {
		t.Fatal(err)
	}
	if len(citations) != 1 {
		t.Errorf("automatic context ignored configured per-note allowance: %d citations", len(citations))
	}
	run := chat.Run{ID: chat.NewID(), State: "running", Epoch: app.catalog.Epoch("alice"), Options: c.Options}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error {
		origin := c.Turns[0]
		origin.RunID = run.ID
		saved.Turns = append(saved.Turns, origin)
		saved.Runs = append(saved.Runs, run)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: run.Epoch}
	ctx, err := app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.executeChatTool(ctx, auth, c.ID, run.ID, chat.ToolCall{ID: "search", Name: "search_passages", Arguments: json.RawMessage(`{"text":"saffron"}`)}, connection); err != nil {
		t.Fatal(err)
	}
	saved, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Run(run.ID).Citations) != 1 {
		t.Errorf("tool search ignored configured per-note allowance: %d citations", len(saved.Run(run.ID).Citations))
	}
}
