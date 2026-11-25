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

func TestReviewBUnchangedMultiSectionAnswerStaysInFollowup(t *testing.T) {
	var calls atomic.Int32
	next := make(chan string, 1)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if calls.Add(1) == 2 {
			next <- string(body)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"UNCHANGED_EVIDENCE_ANSWER_MARKER: the launch budget is 50."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":20}}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	if _, err := store.Save(t.Context(), "notes/launch.md", []byte("# Launch budget\nThe launch budget is 50.\n\n## Schedule\nLaunch is in May.\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	endpoint := "/p/default/api/chat/" + c.ID + "/turns"
	first := proxyCall(app, "alice", "POST", endpoint, `{"submissionId":"first","text":"What is the launch budget?","options":{"mode":"ask"}}`)
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body)
	}
	done := waitChatRun(t, app, c.ID)
	if done.Runs[0].State != "completed" || len(done.Turns) < 2 {
		t.Fatalf("first run failed: %+v", done.Runs)
	}
	t.Logf("original citations: %+v", done.Turns[1].Citations)
	followup := proxyCall(app, "alice", "POST", endpoint, `{"submissionId":"second","text":"Can you expand on your launch budget answer?","options":{"mode":"ask"}}`)
	if followup.Code != 200 {
		t.Fatal(followup.Code, followup.Body)
	}
	done = waitChatRun(t, app, c.ID)
	if done.Runs[1].State != "completed" {
		t.Fatalf("second run failed: %+v", done.Runs[1])
	}
	body := <-next
	if !strings.Contains(body, "UNCHANGED_EVIDENCE_ANSWER_MARKER") {
		t.Fatalf("unchanged cited assistant answer disappeared from followup model input: %s", body)
	}
}

func TestReviewBResumeUninitializedOlderRunKeepsOriginalTask(t *testing.T) {
	var calls atomic.Int32
	captured := make(chan string, 4)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		calls.Add(1)
		captured <- string(body)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"NEWER_ASSISTANT_ANSWER_SHOULD_NOT_BECOME_OLD_USER_TASK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":20}}`)
	})
	endpoint := "/p/default/api/chat/" + c.ID
	rejected := proxyCall(app, "alice", "POST", endpoint+"/turns", `{"submissionId":"old","text":"✅","options":{"mode":"ask"}}`)
	if rejected.Code != 200 {
		t.Fatal(rejected.Code, rejected.Body)
	}
	first := waitChatRun(t, app, c.ID)
	if first.Runs[0].State != "failed" || len(first.Runs[0].Messages) != 0 || calls.Load() != 0 {
		t.Fatalf("failed early-run fixture: %+v", first.Runs[0])
	}
	accepted := proxyCall(app, "alice", "POST", endpoint+"/turns", `{"submissionId":"newer","text":"Tell me about notebook content","options":{"mode":"ask"}}`)
	if accepted.Code != 200 {
		t.Fatal(accepted.Code, accepted.Body)
	}
	done := waitChatRun(t, app, c.ID)
	if done.Runs[1].State != "completed" || calls.Load() != 1 {
		t.Fatalf("newer run fixture failed: %+v", done.Runs)
	}
	<-captured
	resumed := proxyCall(app, "alice", "POST", endpoint+"/runs/"+first.Runs[0].ID+"/resume", `{}`)
	if resumed.Code != 200 {
		t.Fatal(resumed.Code, resumed.Body)
	}
	app.chat.Wait()
	original, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("resume replaced original emoji user request with newer assistant response (old run state %s): %s", original.Runs[0].State, <-captured)
	}
}
