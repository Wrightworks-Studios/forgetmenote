package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"forgetmenote/internal/chat"
)

func TestReleaseBTransitiveHistoryProvenance(t *testing.T) {
	requests := make(chan string, 3)
	count := 0
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
		}
		requests <- string(raw)
		count++
		answer := "ALPHA_ORIGINAL_CLAIM"
		if count == 2 {
			answer = "ALPHA_COPIED_IN_FOLLOWUP"
		}
		if count == 3 {
			answer = "Final answer"
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`, answer)
	})
	save := func(path, content, revision string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"path": path, "content": content, "revision": revision})
		w := call(app, "POST", "/api/note", string(raw), testToken)
		if w.Code != 200 {
			t.Fatalf("save: %d %s", w.Code, w.Body)
		}
	}
	save("notes/alphalaunch.md", "# Alphalaunch\nLaunch value is alpha.", "")
	save("notes/bananareport.md", "# Bananareport\nIndependent fruit inventory.", "")
	store, err := app.catalog.Access("alice", "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.Read(t.Context(), "notes/alphalaunch.md", "")
	if err != nil {
		t.Fatal(err)
	}
	send := func(id, text string) chat.Conversation {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"submissionId": id, "text": text, "options": map[string]string{"mode": "ask"}})
		w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", string(raw))
		if w.Code != 200 {
			t.Fatalf("send: %d %s", w.Code, w.Body)
		}
		done := waitChatRun(t, app, c.ID)
		if done.Runs[len(done.Runs)-1].State != "completed" {
			t.Fatal(done.Runs[len(done.Runs)-1].Error)
		}
		return done
	}
	first := send("one", "alphalaunch")
	<-requests
	if len(first.Turns[1].Citations) != 1 || first.Turns[1].Citations[0].Path != "notes/alphalaunch.md" {
		t.Fatalf("first fixture citations: %+v", first.Turns[1].Citations)
	}
	second := send("two", "bananareport")
	body2 := <-requests
	if !strings.Contains(body2, "ALPHA_ORIGINAL_CLAIM") {
		t.Fatal("fixture did not expose prior answer to model")
	}
	if len(second.Turns[3].Citations) != 1 || second.Turns[3].Citations[0].Path != "notes/bananareport.md" {
		t.Fatalf("second fixture citations: %+v", second.Turns[3].Citations)
	}
	save("notes/alphalaunch.md", "# Alphalaunch\nLaunch value changed to beta.", original.Revision)
	send("three", "bananareport")
	body3 := <-requests
	if strings.Contains(body3, "ALPHA_ORIGINAL_CLAIM") {
		t.Fatal("original answer itself was not invalidated")
	}
	if strings.Contains(body3, "ALPHA_COPIED_IN_FOLLOWUP") {
		t.Fatalf("stale claim reused through a descendant answer whose stored provenance only covers unrelated current retrieval: %s", body3)
	}
}
