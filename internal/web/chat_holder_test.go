package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The gateway running a chat is an internal recovery detail: a reader of the
// conversation never sees which process holds a run.
func TestChatConversationHidesRunHolder(t *testing.T) {
	app, conversation := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}]}`)
	})
	base := "/p/default/api/chat/" + conversation.ID
	if w := proxyCall(app, "alice", "POST", base+"/turns", `{"submissionId":"first","text":"Hello."}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	app.chat.Wait()
	waitChatRun(t, app, conversation.ID)
	stored, err := app.chat.Store.Read("alice", "default", conversation.ID)
	if err != nil || len(stored.Runs) != 1 || stored.Runs[0].Holder == "" {
		t.Fatalf("the fixture's run records no holder: %+v %v", stored.Runs, err)
	}
	w := proxyCall(app, "alice", "GET", base, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), stored.Runs[0].Holder) || strings.Contains(w.Body.String(), `"holder":`) {
		t.Fatalf("the conversation names its gateway: %d %s", w.Code, w.Body)
	}
}
