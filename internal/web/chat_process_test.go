package web

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type crashMarker struct{ Chat, Run string }

// crashDatabase opens the database a crashing child and its parent share, as
// one instance's database outlives its process.
func crashDatabase(t *testing.T, url string) *pg.DB {
	t.Helper()
	db, err := pg.Open(context.Background(), url, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

// This subprocess stops at the actual shared handler boundary. The parent sends
// SIGKILL before/after a real Git mutation, then opens a fresh catalog/server.
func TestChatCrashBoundaryHelper(t *testing.T) {
	root := os.Getenv("FMN_CHAT_CRASH_DIR")
	if root == "" {
		return
	}
	phase := os.Getenv("FMN_CHAT_CRASH_PHASE")
	db := crashDatabase(t, os.Getenv("FMN_CHAT_CRASH_DATABASE"))
	projects, err := catalog.Open(root, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	if err = projects.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	app, err := New(projects, db, root, "http://localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", History: false}, Connection: "local", Model: "synthetic", Mode: "edit", Actions: []string{"notes"}})
	if err != nil {
		t.Fatal(err)
	}
	runID := chat.NewID()
	call := chat.ToolCall{ID: "durable-call", Name: "submit_change_group", Arguments: json.RawMessage(`{"mode":"apply","group":{"id":"model-id","summary":"Explicit crash test mutation","edits":[{"path":"notes/crash.md","content":"One durable note","revision":"","hash":""}]}}`)}
	err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "user", RunID: runID, Role: "user", Content: "Create the crash test note"})
		c.Runs = append(c.Runs, chat.Run{ID: runID, SubmissionID: "crash", RequestHash: "hash", State: "running", CreatedAt: chat.Now(), Options: c.Options, Rounds: 1, Messages: []chat.Message{{Role: "user", Content: "Create the crash test note"}, {Role: "assistant", ToolCalls: []chat.ToolCall{call}}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	action := app.actions[call.Name]
	original := action.Call
	signal := func() {
		raw, _ := json.Marshal(crashMarker{Chat: c.ID, Run: runID})
		if e := os.WriteFile(filepath.Join(root, "crash-marker.json"), raw, 0600); e != nil {
			t.Fatal(e)
		}
		select {}
	}
	action.Call = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		if phase == "before" {
			signal()
		}
		result, e := original(ctx, raw)
		if e != nil {
			return result, e
		}
		if phase == "after" {
			signal()
		}
		return result, e
	}
	app.actions[call.Name] = action
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: projects.Epoch("alice")}
	ctx, err := app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.chatrun.ExecuteNotebookTool(ctx, auth, c.ID, runID, call, chat.Connection{}); err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash boundary did not block")
}
func TestChatProcessKillBeforeAndAfterGitMutation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			database := pgtest.URL(t, true)
			command := exec.Command(executable, "-test.run=^TestChatCrashBoundaryHelper$")
			command.Env = append(os.Environ(), "FMN_CHAT_CRASH_DIR="+root, "FMN_CHAT_CRASH_PHASE="+phase, "FMN_CHAT_CRASH_DATABASE="+database)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { command.Process.Kill(); command.Wait() }()
			var marker crashMarker
			until := time.Now().Add(10 * time.Second)
			for time.Now().Before(until) {
				raw, e := os.ReadFile(filepath.Join(root, "crash-marker.json"))
				if e == nil && json.Unmarshal(raw, &marker) == nil && marker.Chat != "" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if marker.Chat == "" {
				t.Fatal("crash helper did not reach actual handler boundary")
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			command.Wait()
			db := crashDatabase(t, database)
			projects, err := catalog.Open(root, nil, db)
			if err != nil {
				t.Fatal(err)
			}
			defer projects.Close()
			app, err := New(projects, db, root, "http://localhost", testToken)
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body any
				json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Recovered original operation receipt."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
			}))
			defer provider.Close()
			config := filepath.Join(t.TempDir(), "chat.json")
			raw, _ := json.Marshal(map[string]any{"connections": []any{map[string]any{"id": "local", "label": "Synthetic", "format": "chat-completions", "baseURL": provider.URL, "local": true, "models": []string{"synthetic"}}}})
			os.WriteFile(config, raw, 0600)
			if err = app.ConfigureChat(config); err != nil {
				t.Fatal(err)
			}
			recovered, err := app.chat.Store.Read("alice", "default", marker.Chat)
			if err != nil || recovered.Runs[0].State != "interrupted" || len(recovered.Runs[0].Operations) != 1 {
				t.Fatalf("fresh process did not recover journal: %+v %v", recovered.Runs, err)
			}
			store, _ := projects.Access("alice", "default", "read")
			before, _ := store.Status(t.Context())
			_, beforeErr := store.Read(t.Context(), "notes/crash.md", "")
			if phase == "after" && beforeErr != nil || phase == "before" && beforeErr == nil {
				t.Fatal("wrong actual Git crash boundary")
			}
			response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+marker.Chat+"/runs/"+marker.Run+"/resume", `{}`)
			if response.Code != 200 {
				t.Fatal(response.Body)
			}
			done := waitChatRun(t, app, marker.Chat)
			if done.Runs[0].State != "completed" {
				t.Fatalf("resume failed: %+v", done.Runs[0])
			}
			after, _ := store.Status(t.Context())
			note, err := store.Read(t.Context(), "notes/crash.md", "")
			if err != nil || string(note.Content) != "One durable note" {
				t.Fatal("recovered mutation missing")
			}
			if phase == "after" && after.Revision != before.Revision {
				t.Fatal("lost acknowledgement duplicated Git commit")
			}
			if phase == "before" && after.Revision == before.Revision {
				t.Fatal("pending mutation was lost")
			}
		})
	}
}
