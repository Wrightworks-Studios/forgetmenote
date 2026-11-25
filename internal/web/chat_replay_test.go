package web

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http"
	"testing"
	"time"
)

func TestChatGuardedEditAndInterruptedReceiptReplay(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		tool := false
		for _, m := range in.Messages {
			tool = tool || m.Role == "tool"
		}
		w.Header().Set("Content-Type", "application/json")
		if tool {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Saved with a durable receipt."},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`)
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"submit_change_group","arguments":"{\"mode\":\"apply\",\"group\":{\"id\":\"model-id\",\"summary\":\"Create requested note\",\"edits\":[{\"path\":\"notes/guarded.md\",\"content\":\"Authorized note\",\"revision\":\"\",\"hash\":\"\"}]}}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":25}}`)
		}
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	path := "/p/default/api/chat/" + c.ID
	w := proxyCall(app, "alice", "POST", path+"/turns", `{"submissionId":"edit-once","text":"Create a note called guarded","options":{"mode":"edit","actions":["notes"]}}`)
	if w.Code != 200 {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	run := done.Runs[0]
	if run.State != "completed" || len(run.Operations) != 1 {
		t.Fatalf("edit did not finish: %+v", run)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	before, _ := store.Status(context.Background())
	note, err := store.Read(context.Background(), "notes/guarded.md", "")
	if err != nil || string(note.Content) != "Authorized note" {
		t.Fatalf("guarded edit %s %v", note.Content, err)
	}
	// Recreate the durable state at a lost acknowledgement: Git has the receipt,
	// but the chat has not recorded the tool result or issued its final model call.
	err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		r := c.Run(run.ID)
		r.State = "interrupted"
		r.Rounds = 1
		r.Messages = r.Messages[:2]
		r.Operations[0].State = "running"
		r.Operations[0].Result = nil
		r.Usage.Reserved = 0
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	w = proxyCall(app, "alice", "POST", path+"/runs/"+run.ID+"/resume", `{}`)
	if w.Code != 200 {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	resumed := waitChatRun(t, app, c.ID)
	if resumed.Runs[0].State != "completed" {
		t.Fatalf("receipt recovery failed: %+v", resumed.Runs[0])
	}
	after, _ := store.Status(context.Background())
	if before.Revision != after.Revision {
		t.Fatal("recovery duplicated the Git mutation")
	}
}
