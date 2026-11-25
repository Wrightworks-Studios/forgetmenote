package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFinalAReadNoteEvidenceInvalidation(t *testing.T) {
	var calls atomic.Int32
	prompts := make(chan string, 4)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		raw, _ := json.Marshal(body["messages"])
		prompts <- string(raw)
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"read-old","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"notes/archive.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":10}}`)
		case 2:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The archive value is ZEBRA_OLD_FACT."},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":10}}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Follow-up."},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":10}}`)
		}
	})
	saved := call(app, "POST", "/api/note", `{"path":"notes/archive.md","content":"# Archive\nZEBRA_OLD_FACT","revision":""}`, testToken)
	if saved.Code != 200 {
		t.Fatal(saved.Code, saved.Body)
	}
	path := "/p/default/api/chat/" + c.ID + "/turns"
	sent := proxyCall(app, "alice", "POST", path, `{"submissionId":"first","text":"Hello","options":{"mode":"ask"}}`)
	if sent.Code != 200 {
		t.Fatal(sent.Code, sent.Body)
	}
	first := waitChatRun(t, app, c.ID)
	if first.Runs[0].State != "completed" || calls.Load() != 2 {
		t.Fatalf("fixture run failed: %+v", first.Runs)
	}
	if len(first.Runs[0].Citations) == 0 {
		t.Error("read_note returned evidence without durable citations")
	}
	initialPrompt := <-prompts
	if strings.Contains(initialPrompt, "ZEBRA_OLD_FACT") {
		t.Fatal("fixture unexpectedly retrieved original note before tool call")
	}
	toolPrompt := <-prompts
	if !strings.Contains(toolPrompt, "ZEBRA_OLD_FACT") {
		t.Fatal("fixture did not read original evidence")
	}
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.Read(t.Context(), "notes/archive.md", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"path": "notes/archive.md", "content": "# Archive\nHORSE_NEW_FACT", "revision": old.Revision})
	changed := call(app, "POST", "/api/note", string(raw), testToken)
	if changed.Code != 200 {
		t.Fatal(changed.Code, changed.Body)
	}
	sent = proxyCall(app, "alice", "POST", path, `{"submissionId":"next","text":"Thank you","options":{"mode":"ask"}}`)
	if sent.Code != 200 {
		t.Fatal(sent.Code, sent.Body)
	}
	second := waitChatRun(t, app, c.ID)
	if len(second.Runs) != 2 || second.Runs[1].State != "completed" {
		t.Fatalf("followup failed: %+v", second.Runs)
	}
	nextPrompt := <-prompts
	if strings.Contains(nextPrompt, "ZEBRA_OLD_FACT") {
		t.Fatalf("Changed read_note evidence survived in follow-up model context: %s", nextPrompt)
	}
}

func TestFinalAUnchangedPassageAnswerRemainsReusable(t *testing.T) {
	prompts := make(chan string, 3)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		raw, _ := json.Marshal(body["messages"])
		prompts <- string(raw)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"UNCHANGED_ANSWER_MARKER"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":10}}`)
	})
	saved := call(app, "POST", "/api/note", `{"path":"notes/archive.md","content":"# Archive\nUseful archive facts.\n\n## Second\nIndependent section.\n","revision":""}`, testToken)
	if saved.Code != 200 {
		t.Fatal(saved.Code, saved.Body)
	}
	path := "/p/default/api/chat/" + c.ID + "/turns"
	sent := proxyCall(app, "alice", "POST", path, `{"submissionId":"first","text":"Archive facts","options":{"mode":"ask"}}`)
	if sent.Code != 200 {
		t.Fatal(sent.Code, sent.Body)
	}
	first := waitChatRun(t, app, c.ID)
	if first.Runs[0].State != "completed" || len(first.Runs[0].Citations) == 0 {
		t.Fatalf("fixture has no successful retrieved answer: %+v", first.Runs)
	}
	<-prompts
	sent = proxyCall(app, "alice", "POST", path, `{"submissionId":"next","text":"Repeat your previous answer","options":{"mode":"ask"}}`)
	if sent.Code != 200 {
		t.Fatal(sent.Code, sent.Body)
	}
	second := waitChatRun(t, app, c.ID)
	if len(second.Runs) != 2 || second.Runs[1].State != "completed" {
		t.Fatalf("followup failed: %+v", second.Runs)
	}
	nextPrompt := <-prompts
	if !strings.Contains(nextPrompt, "UNCHANGED_ANSWER_MARKER") {
		t.Fatalf("Unchanged answer incorrectly omitted; citations=%+v; next prompt=%s", first.Runs[0].Citations, nextPrompt)
	}
}
