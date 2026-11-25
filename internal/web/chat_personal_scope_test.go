package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
)

// personalWriteFixture scripts Edit-mode chat that writes after optional reads
// of the person's connected mailbox. Only provider HTTP endpoints are synthetic.
func personalWriteFixture(t *testing.T, calls ...string) *Server {
	t.Helper()
	var requests atomic.Int32
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := int(requests.Add(1))
		if n > len(calls) {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			return
		}
		name, args, _ := strings.Cut(calls[n-1], " ")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": fmt.Sprint(n), "type": "function", "function": map[string]string{"name": name, "arguments": args}}}}, "finish_reason": "tool_calls"}}})
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	if _, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"email"}}); err != nil {
		t.Fatal(err)
	}
	connectPersonalFixture(t, app, "alice", func(r *http.Request) string {
		switch r.URL.Path {
		case "/token":
			return `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"scope":"openid email https://www.googleapis.com/auth/gmail.modify"}`
		case "/v1/userinfo":
			return `{"sub":"google-alice","email":"alice@example.test","email_verified":true}`
		case "/gmail/v1/users/me/profile":
			return `{"emailAddress":"alice@example.test","historyId":"100"}`
		case "/gmail/v1/users/me/messages":
			return `{"messages":[{"id":"m1","threadId":"t1"}]}`
		case "/gmail/v1/users/me/threads/t1":
			raw, _ := json.Marshal(map[string]any{"id": "t1", "historyId": "101", "messages": []any{map[string]any{"id": "m1", "threadId": "t1", "historyId": "101", "internalDate": fmt.Sprint(time.Now().UnixMilli()), "payload": map[string]any{"mimeType": "text/plain", "headers": []any{map[string]string{"name": "From", "value": "colleague@example.test"}, map[string]string{"name": "Subject", "value": "Launch plan"}}, "body": map[string]string{"data": base64.RawURLEncoding.EncodeToString([]byte("PRIVATE_EMAIL_BODY"))}}}}})
			return string(raw)
		}
		return `{}`
	})
	return app
}

const personalLeakWrite = `submit_change_group {"mode":"apply","group":{"id":"x","summary":"Summary","edits":[{"path":"leak.md","content":"# Leak\nPRIVATE_EMAIL_BODY\n","revision":"","hash":""}]}}`

func runPersonalEdit(t *testing.T, app *Server, project string) (string, chat.Run) {
	t.Helper()
	w := proxyCall(app, "alice", "POST", "/p/"+project+"/api/chat", `{"connection":"local","model":"synthetic","mode":"edit"}`)
	var c chat.Conversation
	if json.Unmarshal(w.Body.Bytes(), &c) != nil || c.ID == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = proxyCall(app, "alice", "POST", "/p/"+project+"/api/chat/"+c.ID+"/turns", `{"submissionId":"personal-edit","text":"Summarize my email into a note.","options":{"mode":"edit","actions":["notes"]}}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		got, err := app.chat.Store.Read("alice", project, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Runs) > 0 && (!chat.Active(got.Runs[0].State) || got.Runs[0].State == "awaiting_confirmation") {
			app.chat.Wait()
			return c.ID, got.Runs[0]
		}
	}
	t.Fatal("personal edit run did not settle or pause")
	return "", chat.Run{}
}

// Personal memory and connected-account content reach a shared notebook only
// through a change the person reviews: the run pauses for approval of the exact
// change instead of writing it, and the approved change is committed.
func TestChatPersonalContextWritesToSharedNotebookNeedApproval(t *testing.T) {
	for name, calls := range map[string][]string{
		"personal read":   {`read_personal_source {"provider":"google","generation":1,"operation":"email.thread","arguments":"{\"id\":\"t1\"}"}`, personalLeakWrite},
		"injected memory": {personalLeakWrite},
	} {
		t.Run(name, func(t *testing.T) {
			app := personalWriteFixture(t, calls...)
			id, run := runPersonalEdit(t, app, "default")
			store, _ := app.catalog.Access(catalog.Bootstrap, "default", "read")
			if doc, err := store.Read(t.Context(), "leak.md", ""); err == nil {
				t.Fatalf("personal content committed to a shared notebook without approval: %q", doc.Content)
			}
			var op chat.Operation
			for _, candidate := range run.Operations {
				if candidate.Name == "submit_change_group" {
					op = candidate
				}
			}
			if run.PersonalConsent == "" || run.State != "awaiting_confirmation" || op.State != "awaiting_confirmation" || op.Confirmation == "" || !strings.Contains(string(op.Arguments), "PRIVATE_EMAIL_BODY") {
				t.Fatalf("run state=%s error=%q operation=%+v", run.State, run.Error, op)
			}
			if w := proxyCall(app, "bob", "GET", "/p/default/api/note?path=leak.md", ""); w.Code == 200 {
				t.Fatal("another reader can see the unapproved personal content")
			}
			body, _ := json.Marshal(map[string]string{"decision": "approve", "confirmation": op.Confirmation})
			if w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+id+"/actions/"+op.ID, string(body)); w.Code != 200 {
				t.Fatalf("approve personal change: %d %s", w.Code, w.Body.String())
			}
			if doc, err := store.Read(t.Context(), "leak.md", ""); err != nil || !strings.Contains(string(doc.Content), "PRIVATE_EMAIL_BODY") {
				t.Fatalf("approved personal change not committed: %v", err)
			}
		})
	}
}

// The person's own protected assistant notebook is already where sourced
// suggestions are committed; personal context may be saved there.
func TestChatPersonalContextCanWriteOwnAssistantNotebook(t *testing.T) {
	app := personalWriteFixture(t, `read_personal_source {"provider":"google","generation":1,"operation":"email.thread","arguments":"{\"id\":\"t1\"}"}`, personalLeakWrite)
	settings, err := app.catalog.Assistant("alice")
	if err != nil || settings.Project == "" {
		t.Fatal(settings, err)
	}
	_, run := runPersonalEdit(t, app, settings.Project)
	store, _ := app.catalog.Access("alice", settings.Project, "read")
	if doc, err := store.Read(t.Context(), "leak.md", ""); err != nil || !strings.Contains(string(doc.Content), "PRIVATE_EMAIL_BODY") {
		t.Fatalf("own assistant notebook write: run=%s %q err=%v", run.State, run.Error, err)
	}
}
