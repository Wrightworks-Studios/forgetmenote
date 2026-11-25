package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
)

// A transcript retains passages and citations from every notebook its runs
// used. Losing read access to any of them withholds the whole conversation,
// while deletion stays available.
func TestChatConversationRequiresAccessToEveryNotebookItUsed(t *testing.T) {
	var requests atomic.Int32
	app, conversation := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			args, _ := json.Marshal(documentRequest{documentRef: documentRef{Project: "other", Origin: "notes", Path: "answer.md"}})
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant", "tool_calls": []any{map[string]any{"id": "1", "type": "function", "function": map[string]string{"name": "read_document", "arguments": string(args)}}},
			}, "finish_reason": "tool_calls"}}})
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The other notebook says OTHER_SECRET."},"finish_reason":"stop"}]}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "other", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	other, _ := app.catalog.Access(catalog.Bootstrap, "other", "edit")
	if _, err := other.Save(t.Context(), "answer.md", []byte("# Answer\nOTHER_SECRET"), "", catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	base := "/p/default/api/chat/" + conversation.ID
	if w := proxyCall(app, "alice", "POST", base+"/turns", `{"submissionId":"other-read","text":"Read answer.md in the other notebook.","options":{"mode":"ask","corpus":{"version":1,"archives":"exclude","drafts":"exclude","history":false,"noteProjects":["other"]}}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	app.chat.Wait()
	done := waitChatRun(t, app, conversation.ID)
	answer := done.Turns[len(done.Turns)-1]
	if run := done.Runs[0]; run.State != "completed" || len(run.Operations) != 1 || len(run.Operations[0].Citations) != 1 || run.Operations[0].Citations[0].Project != "other" {
		t.Fatalf("cross-notebook read fixture: state=%s error=%q operations=%+v", run.State, run.Error, run.Operations)
	}
	if w := proxyCall(app, "alice", "GET", base, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "OTHER_SECRET") {
		t.Fatal("conversation unavailable before revocation", w.Code)
	}

	if err := app.catalog.Member(catalog.Bootstrap, "other", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "alice", "GET", base, ""); w.Code != 403 || strings.Contains(w.Body.String(), "OTHER_SECRET") {
		t.Errorf("transcript disclosed after losing the other notebook: %d", w.Code)
	}
	// A stream that is not refused would block, so bound the request.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, "GET", app.origin+base+"/events?after=0", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Remote-User", "alice")
	r.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	r.Header.Set("Origin", app.origin)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 403 || strings.Contains(w.Body.String(), "OTHER_SECRET") {
		t.Errorf("event stream disclosed after losing the other notebook: %d", w.Code)
	}
	save := proxyCall(app, "alice", "POST", base+"/save-note", `{"submissionId":"save-revoked","path":"notes/leak.md","revision":"","turnIds":["`+answer.ID+`"]}`)
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "read")
	if _, err := store.Read(t.Context(), "notes/leak.md", ""); err == nil || save.Code != 403 {
		t.Errorf("saved another notebook's evidence after losing it: %d %s", save.Code, save.Body)
	}
	if w := proxyCall(app, "alice", "DELETE", base, ""); w.Code != 200 {
		t.Errorf("deletion unavailable after losing the other notebook: %d %s", w.Code, w.Body)
	}
}
