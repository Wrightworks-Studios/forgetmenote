package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
)

func TestChatApprovedLockHonorsFixedCorpus(t *testing.T) {
	for _, target := range []string{"notes/target.md", "notes"} {
		for _, excluded := range []bool{false, true} {
			for _, locked := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/excluded=%v/locked=%v", target, excluded, locked), func(t *testing.T) {
					app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
					if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
						t.Fatal(err)
					}
					store, err := app.catalog.Access("alice", "default", "owner")
					if err != nil {
						t.Fatal(err)
					}
					body := "# Target\n"
					if excluded {
						body = "---\nfmn-container: archive\n---\n" + body
					}
					if _, err = store.Save(t.Context(), "notes/target.md", []byte(body), "", "alice"); err != nil {
						t.Fatal(err)
					}
					if _, err = store.Save(t.Context(), "notes/active.md", []byte("# Active sibling\n"), "", "alice"); err != nil {
						t.Fatal(err)
					}
					before, err := store.Locks(t.Context(), target)
					if err != nil {
						t.Fatal(err)
					}
					if !locked {
						before, err = store.SetLock(t.Context(), target, true, before.Revision, "alice")
						if err != nil {
							t.Fatal(err)
						}
					}
					run := chat.Run{ID: chat.NewID(), State: "running", Rounds: 1, Epoch: app.catalog.Epoch("alice"), Options: chat.Options{Corpus: c.Options.Corpus, Connection: "local", Model: "synthetic", Mode: "edit"}}
					if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
						c.Turns = append(c.Turns, chat.Turn{ID: "origin", RunID: run.ID, Role: "user", Content: "Change the requested lock within the selected corpus"})
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
					args, _ := json.Marshal(map[string]any{"path": target, "locked": locked, "revision": before.Revision})
					if err = app.executeChatTool(ctx, auth, c.ID, run.ID, chat.ToolCall{ID: "lock", Name: "set_note_lock", Arguments: args}, app.chat.Config.Connections[0]); err != chatrun.ErrConfirmation {
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
					after, err := store.Locks(t.Context(), target)
					if err != nil {
						t.Fatal(err)
					}
					completed, err := app.chat.Store.Read("alice", "default", c.ID)
					if err != nil {
						t.Fatal(err)
					}
					var result struct {
						Error string `json:"error"`
					}
					if err = json.Unmarshal(completed.Runs[0].Operations[0].Result, &result); err != nil {
						t.Fatal(err)
					}
					if excluded {
						if result.Error == "" || after.Revision != before.Revision || len(after.Rules) != len(before.Rules) {
							t.Fatalf("excluded approval changed lock policy/history: before=%+v after=%+v response=%s", before, after, response.Body)
						}
					} else if result.Error != "" || after.Revision == before.Revision || (len(after.Rules) == 1) != locked {
						t.Fatalf("eligible approval failed: after=%+v response=%s", after, response.Body)
					}
				})
			}
		}
	}
}
