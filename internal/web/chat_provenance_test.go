package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/chat"
	"net/http"
	"strings"
	"testing"
)

func TestChatInheritedProvenanceThroughMemoryAndSnapshots(t *testing.T) {
	for _, snapshotBound := range []bool{false, true} {
		label := "compact source history"
		if snapshotBound {
			label = "uncited snapshot history"
		}
		t.Run(label, func(t *testing.T) {
			app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
			save := func(path, content, revision string) {
				t.Helper()
				raw, _ := json.Marshal(map[string]string{"path": path, "content": content, "revision": revision})
				w := call(app, "POST", "/api/note", string(raw), testToken)
				if w.Code != 200 {
					t.Fatal(w.Body)
				}
			}
			save("notes/alphalaunch.md", "# Alphalaunch\nThe original claim.", "")
			save("notes/bananareport.md", "# Bananareport\nAn independent report.", "")
			query := "alphalaunch"
			if snapshotBound {
				query = "ordinary greeting"
			}
			connection := app.chat.Config.Connections[0]
			c.Turns = []chat.Turn{{ID: chat.NewID(), Role: "user", Content: query}}
			_, citations, first, err := initialChatTestMessages(t, app, c, connection)
			if err != nil {
				t.Fatal(err)
			}
			if snapshotBound && len(citations) != 0 {
				t.Fatalf("snapshot fixture unexpectedly cited evidence: %+v", citations)
			}
			c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), Role: "assistant", Content: "ORIGINAL_INHERITED_CLAIM", Citations: citations, Provenance: first.Provenance})
			if !snapshotBound {
				for i := 0; i < 12; i++ {
					c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), Role: "user", Content: "Intermediate conversation"})
				}
			}
			c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), Role: "user", Content: "bananareport"})
			messages, ownCitations, inherited, err := initialChatTestMessages(t, app, c, connection)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(messages)
			if !strings.Contains(string(raw), "ORIGINAL_INHERITED_CLAIM") {
				t.Fatal("fixture did not preserve unchanged original input")
			}
			if !snapshotBound && !strings.Contains(inherited.Text, "ORIGINAL_INHERITED_CLAIM") {
				t.Fatal("fixture did not use compact memory")
			}
			if len(ownCitations) != 1 || ownCitations[0].Path != "notes/bananareport.md" {
				t.Fatalf("fixture direct citations: %+v", ownCitations)
			}
			descendant := chat.Turn{ID: chat.NewID(), Role: "assistant", Content: "DESCENDANT_INHERITED_CLAIM", Citations: ownCitations, Provenance: inherited.Provenance}
			if err = app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error {
				saved.Turns = []chat.Turn{descendant, {ID: chat.NewID(), Role: "user", Content: "bananareport"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			reloaded, err := app.chat.Store.Read("alice", "default", c.ID)
			if err != nil {
				t.Fatal(err)
			}
			messages, _, _, err = initialChatTestMessages(t, app, reloaded, connection)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(messages)
			if !strings.Contains(string(raw), "DESCENDANT_INHERITED_CLAIM") {
				t.Fatal("unchanged inherited context was discarded after reload")
			}
			if snapshotBound {
				save("notes/unrelated.md", "Changes the inherited uncited snapshot.", "")
			} else {
				store, _ := app.catalog.Access("alice", "default", "read")
				old, err := store.Read(t.Context(), "notes/alphalaunch.md", "")
				if err != nil {
					t.Fatal(err)
				}
				save(old.Path, "# Alphalaunch\nThe original claim has changed.", old.Revision)
			}
			messages, _, _, err = initialChatTestMessages(t, app, reloaded, connection)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(messages)
			if strings.Contains(string(raw), "DESCENDANT_INHERITED_CLAIM") {
				t.Fatalf("changed inherited dependency survived: %s", raw)
			}
		})
	}
}
