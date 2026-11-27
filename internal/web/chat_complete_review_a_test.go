package web

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCompleteReviewAStaleAnswerRevivesAfterOneTurn(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.Save(t.Context(), "notes/budget.md", []byte("# Budget\nThe project budget is 100 dollars."), "", "")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := app.chat.Config.Resolve("local", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	c := chat.Conversation{Owner: "alice", Project: "default", Options: chat.Options{Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", History: false}}, Turns: []chat.Turn{{Role: "user", Content: "What is the budget?"}}}
	_, citations, memory, err := initialChatTestMessages(t, app, c, connection)
	if err != nil {
		t.Fatal(err)
	}
	if len(citations) == 0 {
		t.Fatal("fixture no citations")
	}
	c.Memory = memory
	c.Turns = append(c.Turns, chat.Turn{Role: "assistant", Content: "OLD_ANSWER_MARKER: The budget is 100 dollars.", Provenance: memory.Provenance, Citations: citations})
	if _, err = store.Save(t.Context(), "notes/budget.md", []byte("# Budget\nThe project budget is 200 dollars."), old.Revision, ""); err != nil {
		t.Fatal(err)
	}
	c.Turns = append(c.Turns, chat.Turn{Role: "user", Content: "Check the budget again."})
	msgs, _, memory, err := initialChatTestMessages(t, app, c, connection)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "OLD_ANSWER_MARKER") {
			t.Fatal("fixture old answer not initially retired")
		}
	}
	c.Memory = memory
	c.Turns = append(c.Turns, chat.Turn{Role: "assistant", Content: "The updated budget is 200 dollars."}, chat.Turn{Role: "user", Content: "Summarize the budget."})
	msgs, _, _, err = initialChatTestMessages(t, app, c, connection)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "OLD_ANSWER_MARKER") {
			t.Fatalf("stale assistant answer restored into provider context after one later retrieval: %q", m.Content)
		}
	}
}

func TestCompleteReviewARevokedPendingApprovalBlocksOtherNotebook(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		for _, m := range req.Messages {
			if m.Role == "tool" {
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"control","type":"function","function":{"name":"set_note_lock","arguments":"{\"path\":\"notes/test.md\",\"locked\":true,\"revision\":\"unused\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
				return
			}
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"discover","type":"function","function":{"name":"discover_actions","arguments":"{\"names\":[\"set_note_lock\"]}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Create(catalog.Bootstrap, "second", "Second"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "second", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"prepare-lock","text":"Lock this note","options":{"mode":"edit","actions":["notes"]}}`)
	if w.Code != 200 {
		t.Fatalf("send %d %s", w.Code, w.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		got, e := app.chat.Store.Read("alice", "default", c.ID)
		if e != nil {
			t.Fatal(e)
		}
		app.chat.Mu.Lock()
		active := len(app.chat.Active)
		app.chat.Mu.Unlock()
		if len(got.Runs) > 0 && got.Runs[0].State == "awaiting_confirmation" && active == 0 {
			paused = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !paused {
		t.Fatal("fixture did not reach idle pending approval")
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	w = proxyCall(app, "alice", "POST", "/p/second/api/chat", `{"connection":"local","model":"synthetic","mode":"ask"}`)
	var second chat.Conversation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &second) != nil {
		t.Fatalf("create second %d %s", w.Code, w.Body)
	}
	w = proxyCall(app, "alice", "POST", "/p/second/api/chat/"+second.ID+"/turns", `{"submissionId":"new-notebook","text":"Hello","options":{"mode":"ask"}}`)
	if w.Code != 200 {
		t.Fatalf("revoked inaccessible conversation permanently blocks user's permitted notebook: %d %s", w.Code, w.Body)
	}
}

func TestCompleteReviewAKnownMutationLostAfterRevocation(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "notes/comment.md", []byte("# Comment target"), "", ""); err != nil {
		t.Fatal(err)
	}
	discussion, err := store.Comments(t.Context(), "notes/comment.md")
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"path": "notes/comment.md", "text": "SUCCESSFUL_COMMENT_MARKER", "revision": discussion.Revision})
	opts := chat.Options{Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", History: false}, Mode: "edit", Connection: "local", Model: "synthetic", Actions: []string{"comments"}}
	c, err := app.chat.Store.Create("alice", "default", opts)
	if err != nil {
		t.Fatal(err)
	}
	runID := chat.NewID()
	call := chat.ToolCall{ID: "comment-call", Name: "add_comment", Arguments: args}
	err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "comment-task", RunID: runID, Role: "user", Content: "Add the requested comment"})
		c.Runs = append(c.Runs, chat.Run{ID: runID, Options: opts, State: "running", Rounds: 1, Messages: []chat.Message{{Role: "assistant", ToolCalls: []chat.ToolCall{call}}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	action := app.actions["add_comment"]
	actual := action.Call
	var acknowledged json.RawMessage
	action.Call = func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		result, e := actual(ctx, args)
		if e != nil {
			return nil, e
		}
		acknowledged = result
		if e = app.catalog.Member(catalog.Bootstrap, "default", "alice", ""); e != nil {
			return nil, e
		}
		return result, nil
	}
	app.actions["add_comment"] = action
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
	ctx, err := app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := app.chat.Config.Resolve("local", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	err = app.chatrun.ExecuteChatTool(ctx, auth, c.ID, runID, call, connection)
	if err == nil {
		t.Fatal("fixture expected post-call revocation to stop model execution")
	}
	if !strings.Contains(string(acknowledged), "SUCCESSFUL_COMMENT_MARKER") {
		t.Fatal("fixture did not receive acknowledged mutation")
	}
	discussion, err = store.Comments(t.Context(), "notes/comment.md")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(discussion)
	if !strings.Contains(string(raw), "SUCCESSFUL_COMMENT_MARKER") {
		t.Fatal("actual mutation missing")
	}
	got, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	op := got.Runs[0].Operations[0]
	if op.State != "completed" || !strings.Contains(string(op.Result), "SUCCESSFUL_COMMENT_MARKER") {
		t.Fatalf("successful acknowledged Git comment discarded after revocation; operation=%+v", op)
	}
	for _, message := range got.Runs[0].Messages {
		if message.Role == "tool" {
			t.Fatal("revoked action result entered model continuation")
		}
	}
	for _, suffix := range []string{"", "/events?after=0"} {
		if w := proxyCall(app, "alice", "GET", "/p/default/api/chat/"+c.ID+suffix, ""); w.Code != 403 {
			t.Fatalf("revoked receipt disclosure: %d", w.Code)
		}
	}
	app.chat.Store.Close()
	reopened, err := chat.OpenStore(app.db)
	if err != nil {
		t.Fatal(err)
	}
	app.chat.Store = reopened
	recovered, err := reopened.Read("alice", "default", c.ID)
	if err != nil || recovered.Runs[0].Operations[0].State != "completed" {
		t.Fatalf("known receipt lost on recovery: %v", err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	before, err := store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.chatrun.ExecuteChatTool(ctx, auth, c.ID, runID, call, connection); err != nil {
		t.Fatal(err)
	}
	after, err := store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if before.Revision != after.Revision {
		t.Fatal("recovered known comment was replayed")
	}
}
