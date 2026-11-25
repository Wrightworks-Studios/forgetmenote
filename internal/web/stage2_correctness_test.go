package web

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
	"net/http"
	"strings"
	"testing"
)

func TestStage2CapacityFailurePreservesProviderUsage(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": strings.Repeat("answer", 1500)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 30, "completion_tokens": 1500}})
	})
	app.chat.Store.SetLimits(chat.Limits{MaxRecordBytes: 18000}, nil)
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"capacity","text":"Answer briefly","options":{"mode":"ask"}}`)
	if w.Code != 200 {
		t.Fatalf("submit %d %s", w.Code, w.Body)
	}
	got := waitChatRun(t, app, c.ID)
	r := got.Runs[0]
	if r.Usage.Tokens() == 0 && r.InFlightTokens > 0 && r.Usage.Reserved == 0 {
		t.Fatalf("provider returned 1530 spent tokens, but capacity failure released all durable accounting: state=%s usage=%+v inFlight=%d error=%s", r.State, r.Usage, r.InFlightTokens, r.Error)
	}
}

func TestStage2SubmissionRetryAfterDefaultsChange(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	path := "/p/default/api/chat/" + c.ID + "/turns"
	first := `{"submissionId":"first","text":"Read the note","options":{}}`
	w := proxyCall(app, "alice", "POST", path, first)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	waitChatRun(t, app, c.ID)
	w = proxyCall(app, "alice", "POST", path, `{"submissionId":"second","text":"Edit the note","options":{"mode":"edit","actions":["notes"]}}`)
	if w.Code != 200 {
		t.Fatalf("second %d %s", w.Code, w.Body)
	}
	waitChatRun(t, app, c.ID)
	w = proxyCall(app, "alice", "POST", path, first)
	if w.Code != 200 {
		t.Fatalf("identical first submission changed meaning because conversation defaults changed: %d %s", w.Code, w.Body)
	}
}

func TestStage2LostOwnerAcknowledgementRemainsPending(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		for _, m := range in.Messages {
			if m.Role == "tool" {
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"action failed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`)
				return
			}
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"submit_change_group","arguments":"{\"mode\":\"apply\",\"group\":{\"id\":\"model-id\",\"summary\":\"Create requested note\",\"edits\":[{\"path\":\"notes/owner.md\",\"content\":\"Committed before acknowledgement loss\",\"revision\":\"\",\"hash\":\"\"}]}}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":25}}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	registered := app.actions["submit_change_group"]
	actual := registered.Call
	registered.Call = func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		_, err := actual(ctx, args)
		if err != nil {
			return nil, err
		}
		return nil, workspace.ErrOwnerUnavailable
	}
	app.actions["submit_change_group"] = registered
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"owner-failure","text":"Create owner note","options":{"mode":"edit","actions":["notes"]}}`)
	if w.Code != 200 {
		t.Fatalf("send %d %s", w.Code, w.Body)
	}
	got := waitChatRun(t, app, c.ID)
	store, err := app.catalog.Access("alice", "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	note, err := store.Read(context.Background(), "notes/owner.md", "")
	if err != nil || string(note.Content) != "Committed before acknowledgement loss" {
		t.Fatalf("fixture mutation failed: %v", err)
	}
	r := got.Runs[0]
	if r.Operations[0].State == "completed" {
		t.Fatalf("lost owner acknowledgement sealed receipt-backed mutation as completed error and continued model: run=%s operation=%+v", r.State, r.Operations[0])
	}
}

func TestStage2CheckpointReplayDoesNotCommitNewLiveWork(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		for _, m := range in.Messages {
			if m.Role == "tool" {
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Checkpoint saved"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`)
				return
			}
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"checkpoint-1","type":"function","function":{"name":"checkpoint_live_note","arguments":"{\"path\":\"notes/live.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":25}}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(context.Background(), "notes/live.md", []byte("zero"), "", ""); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveRead(context.Background(), "notes/live.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.LivePatch(context.Background(), workspace.TargetedPatch{Path: "notes/live.md", Base: live.LiveRevision, OperationID: "first-live", Edits: []workspace.TextEdit{{Start: 0, End: 4, Old: "zero", Text: "one"}}}); err != nil {
		t.Fatal(err)
	}
	path := "/p/default/api/chat/" + c.ID
	w := proxyCall(app, "alice", "POST", path+"/turns", `{"submissionId":"checkpoint","text":"Save accepted live edits","options":{"mode":"edit","actions":["live"]}}`)
	if w.Code != 200 {
		t.Fatalf("send %d %s", w.Code, w.Body)
	}
	got := waitChatRun(t, app, c.ID)
	run := got.Runs[0]
	if run.State != "completed" || len(run.Operations) != 1 {
		t.Fatalf("fixture run %+v", run)
	}
	checkpointed, err := store.Read(context.Background(), "notes/live.md", "")
	if err != nil || string(checkpointed.Content) != "one" {
		t.Fatalf("first checkpoint failed %v", err)
	}
	// Simulate death after the actual checkpoint, before chat saved its result.
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
	live, err = store.LiveRead(context.Background(), "notes/live.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.LivePatch(context.Background(), workspace.TargetedPatch{Path: "notes/live.md", Base: live.LiveRevision, OperationID: "second-live", Edits: []workspace.TextEdit{{Start: 0, End: 3, Old: "one", Text: "other user's newer pending work"}}}); err != nil {
		t.Fatal(err)
	}
	w = proxyCall(app, "alice", "POST", path+"/runs/"+run.ID+"/resume", `{}`)
	if w.Code != 200 {
		t.Fatalf("resume %d %s", w.Code, w.Body)
	}
	waitChatRun(t, app, c.ID)
	after, err := store.Read(context.Background(), "notes/live.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != checkpointed.Revision {
		t.Fatalf("resuming original checkpoint committed later live work under a fresh receipt: before=%s after=%s content=%q", checkpointed.Revision, after.Revision, after.Content)
	}
}
