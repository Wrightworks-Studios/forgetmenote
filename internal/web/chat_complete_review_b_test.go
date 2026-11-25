package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http"
	"strings"
	"testing"
)

func TestReviewerBStaleAnswerCannotReviveAfterRefresh(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/note", `{"path":"notes/source.md","content":"Original source value alpha","revision":""}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	original, err := store.Read(t.Context(), "notes/source.md", "")
	if err != nil {
		t.Fatal(err)
	}
	c.Turns = []chat.Turn{{ID: "old", Role: "assistant", Content: "OBSOLETE_ALPHA_CLAIM", Citations: []chat.Citation{{Path: "notes/source.md", Revision: original.Revision}}}, {ID: "current", Role: "user", Content: "What does the source say?"}}
	_, _, originalMemory, err := initialChatTestMessages(t, app, c, app.chat.Config.Connections[0])
	if err != nil {
		t.Fatal(err)
	}
	c.Memory = originalMemory
	raw, _ := json.Marshal(map[string]string{"path": "notes/source.md", "content": "Changed source value beta", "revision": original.Revision})
	w = proxyCall(app, "alice", "POST", "/p/default/api/note", string(raw))
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	first, _, refreshed, err := initialChatTestMessages(t, app, c, app.chat.Config.Connections[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range first {
		if strings.Contains(m.Content, "OBSOLETE_ALPHA_CLAIM") {
			t.Fatal("stale answer was not initially invalidated")
		}
	}
	c.Memory = refreshed
	c.Turns = append(c.Turns, chat.Turn{ID: "newer", Role: "assistant", Content: "Beta is current"}, chat.Turn{ID: "followup", Role: "user", Content: "What is the source value?"})
	second, _, _, err := initialChatTestMessages(t, app, c, app.chat.Config.Connections[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range second {
		if strings.Contains(m.Content, "OBSOLETE_ALPHA_CLAIM") {
			t.Fatal("obsolete answer revived when the shared memory fingerprint refreshed on a subsequent turn")
		}
	}
}

func TestReviewerBConfiguredStrictToolsWorksWithChat(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Strict answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":4}}`)
	})
	connection := app.chat.Config.Connections[0]
	connection.Capabilities.StrictTools = true
	setChatConnection(t, app, connection)
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"strict","text":"Read notebook evidence","options":{"mode":"ask"}}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	got := waitChatRun(t, app, c.ID)
	if got.Runs[0].State != "completed" {
		t.Fatalf("accepted strict-tools configuration cannot run application tools: %s (%s)", got.Runs[0].State, got.Runs[0].Error)
	}
}

func TestReviewerBStaleAnswerRevivesThroughHTTP(t *testing.T) {
	var requests []string
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests = append(requests, string(body))
		answer := "Current value beta"
		if len(requests) == 1 {
			answer = "OBSOLETE_ALPHA_CLAIM"
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": answer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 5}})
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/note", `{"path":"notes/source.md","content":"Original source value alpha","revision":""}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	original, err := store.Read(t.Context(), "notes/source.md", "")
	if err != nil {
		t.Fatal(err)
	}
	send := func(id string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"submissionId": id, "text": "What is the source value?", "options": map[string]string{"mode": "ask"}})
		w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", string(raw))
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		got := waitChatRun(t, app, c.ID)
		if got.Runs[len(got.Runs)-1].State != "completed" {
			t.Fatal(got.Runs[len(got.Runs)-1].Error)
		}
	}
	send("one")
	raw, _ := json.Marshal(map[string]string{"path": "notes/source.md", "content": "Changed source value beta", "revision": original.Revision})
	w = proxyCall(app, "alice", "POST", "/p/default/api/note", string(raw))
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	send("two")
	if strings.Contains(requests[1], "OBSOLETE_ALPHA_CLAIM") {
		t.Fatal("original claim was not initially invalidated")
	}
	send("three")
	if strings.Contains(requests[2], "OBSOLETE_ALPHA_CLAIM") {
		t.Fatal("third provider request resends the old alpha answer after two requests against the changed beta source")
	}
}
