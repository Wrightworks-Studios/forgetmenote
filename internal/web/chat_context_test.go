package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/chat"
)

// Context-only tests explicitly bind the current user turn, just as Start does.
func initialChatTestMessages(t *testing.T, app *Server, c chat.Conversation, connection chat.Connection) ([]chat.Message, []chat.Citation, chat.Memory, error) {
	t.Helper()
	if len(c.Turns) == 0 || c.Turns[len(c.Turns)-1].Role != "user" {
		t.Fatal("context fixture needs a current user message")
	}
	c.Turns = append([]chat.Turn(nil), c.Turns...)
	run := chat.Run{ID: chat.NewID(), Options: c.Options}
	c.Turns[len(c.Turns)-1].RunID = run.ID
	return app.chatrun.InitialMessages(t.Context(), c, run, connection)
}

func TestChatResumeKeepsOriginalTaskHistoryAndAttachments(t *testing.T) {
	captured := make(chan string, 1)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		captured <- string(body)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Original task resumed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
	})
	oldID, newID := chat.NewID(), chat.NewID()
	if err := app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Runs = append(c.Runs, chat.Run{ID: oldID, State: "failed", Options: c.Options, Epoch: app.catalog.Epoch("alice")}, chat.Run{ID: newID, State: "completed", Options: c.Options})
		c.Turns = append(c.Turns, chat.Turn{ID: "before", Role: "user", Content: "PRECEDING_HISTORY"}, chat.Turn{ID: "old-user", RunID: oldID, Role: "user", Content: "ORIGINAL_USER_TASK", Attachments: []chat.Attachment{{Kind: "private", Path: "notes/original.md", Content: "ORIGINAL_PRIVATE_ATTACHMENT"}}}, chat.Turn{ID: "new-user", RunID: newID, Role: "user", Content: "LATER_USER_TASK", Attachments: []chat.Attachment{{Kind: "private", Path: "notes/newer.md", Content: "LATER_PRIVATE_ATTACHMENT"}}}, chat.Turn{ID: "new-answer", RunID: newID, Role: "assistant", Content: "LATER_ASSISTANT_RESPONSE"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/runs/"+oldID+"/resume", `{}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	app.chat.Wait()
	var body string
	select {
	case body = <-captured:
	default:
		t.Fatal("resumed original request never reached provider")
	}
	for _, required := range []string{"ORIGINAL_USER_TASK", "ORIGINAL_PRIVATE_ATTACHMENT", "PRECEDING_HISTORY"} {
		if !strings.Contains(body, required) {
			t.Errorf("missing original input %s", required)
		}
	}
	for _, forbidden := range []string{"LATER_USER_TASK", "LATER_PRIVATE_ATTACHMENT", "LATER_ASSISTANT_RESPONSE"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("later input became old run request: %s", forbidden)
		}
	}
	got, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil || got.Runs[0].State != "completed" {
		t.Fatalf("original run failed: %v %+v", err, got.Runs)
	}
	_, _, _, err = app.chatrun.InitialMessages(t.Context(), got, chat.Run{ID: "missing-origin"}, app.chat.Config.Connections[0])
	if err == nil {
		t.Fatal("missing original user message silently fell back to newer input")
	}
}

func TestChatUncitedConversationPreservesVerifiedContext(t *testing.T) {
	prompts := make(chan string, 2)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		prompts <- string(body)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"CONVERSATION_GREETING_MARKER"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
	})
	for _, id := range []string{"hello", "followup"} {
		w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", fmt.Sprintf(`{"submissionId":%q,"text":"Hello again","options":{"mode":"ask"}}`, id))
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		done := waitChatRun(t, app, c.ID)
		if done.Runs[len(done.Runs)-1].State != "completed" {
			t.Fatal(done.Runs)
		}
	}
	<-prompts
	if body := <-prompts; !strings.Contains(body, "CONVERSATION_GREETING_MARKER") {
		t.Fatal("unchanged ordinary conversation lost its previous answer")
	}
}

func TestChatUntrackedToolEvidenceDoesNotReuseUnrelatedCitations(t *testing.T) {
	prompts := make(chan string, 3)
	var count atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		prompts <- string(body)
		if count.Add(1) == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"status","type":"function","function":{"name":"project_status","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"UNTRACKED_STATUS_CLAIM_MARKER"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
	})
	saved := call(app, "POST", "/api/note", `{"path":"notes/budget.md","content":"# Budget\nThe budget is 50.","revision":""}`, testToken)
	if saved.Code != 200 {
		t.Fatal(saved.Body)
	}
	for _, id := range []string{"first", "second"} {
		w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", fmt.Sprintf(`{"submissionId":%q,"text":"Check budget status","options":{"mode":"ask"}}`, id))
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		done := waitChatRun(t, app, c.ID)
		if done.Runs[len(done.Runs)-1].State != "completed" {
			t.Fatal(done.Runs)
		}
		if id == "first" && len(done.Runs[0].Citations) == 0 {
			t.Fatal("fixture needs unrelated committed citations")
		}
	}
	<-prompts
	<-prompts
	if body := <-prompts; strings.Contains(body, "UNTRACKED_STATUS_CLAIM_MARKER") {
		t.Fatal("untracked tool claim borrowed unrelated citation provenance")
	}
}
