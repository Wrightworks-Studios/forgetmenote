package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"forgetmenote/internal/chat"
)

func TestShipAAttachedCommittedSourceInvalidatesRetainedAnswer(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	save := func(path, content, revision string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"path": path, "content": content, "revision": revision})
		w := call(app, "POST", "/api/note", string(raw), testToken)
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
	}
	save("notes/alphalaunch.md", "# Alphalaunch\nAn independent launch report.", "")
	save("notes/attached.md", "# Contract\nThe deadline is Tuesday.", "")
	store, _ := app.catalog.Access("alice", "default", "read")
	attachments, err := app.chatrun.Attachments(t.Context(), store, []chat.Attachment{{Kind: "committed", Path: "notes/attached.md"}})
	if err != nil {
		t.Fatal(err)
	}
	connection := app.chat.Config.Connections[0]
	c.Turns = []chat.Turn{{ID: chat.NewID(), Role: "user", Content: "alphalaunch", Attachments: attachments}}
	messages, citations, memory, err := initialChatTestMessages(t, app, c, connection)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(messages)
	if !strings.Contains(string(raw), "The deadline is Tuesday.") {
		t.Fatal("attachment not passed to model")
	}
	if len(citations) != 1 || citations[0].Path != "notes/alphalaunch.md" {
		t.Fatalf("unexpected retrieval %+v", citations)
	}
	c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), Role: "assistant", Content: "ATTACHED_DEADLINE_TUESDAY", Citations: citations, Provenance: memory.Provenance})
	unchanged := c
	unchanged.Turns = append(append([]chat.Turn(nil), c.Turns...), chat.Turn{ID: chat.NewID(), Role: "user", Content: "alphalaunch"})
	unchangedMessages, _, _, err := initialChatTestMessages(t, app, unchanged, connection)
	if err != nil {
		t.Fatal(err)
	}
	unchangedRaw, _ := json.Marshal(unchangedMessages)
	if strings.Contains(string(unchangedRaw), "ATTACHED_DEADLINE_TUESDAY") {
		t.Fatal("prior committed attachment exception reused without selection on the new turn")
	}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error { saved.Turns = c.Turns; return nil }); err != nil {
		t.Fatal(err)
	}
	old, err := store.Read(t.Context(), "notes/attached.md", "")
	if err != nil {
		t.Fatal(err)
	}
	save(old.Path, "# Contract\nThe deadline is Friday.", old.Revision)
	c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), Role: "user", Content: "alphalaunch"})
	messages, _, _, err = initialChatTestMessages(t, app, c, connection)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(messages)
	if strings.Contains(string(raw), "ATTACHED_DEADLINE_TUESDAY") {
		t.Fatalf("retained obsolete attachment-derived answer: %s", raw)
	}
	stored, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil || stored.Turns[1].Content != "ATTACHED_DEADLINE_TUESDAY" || stored.Turns[0].Attachments[0].Content != attachments[0].Content {
		t.Fatal("context invalidation changed the original transcript or attachment")
	}
}
