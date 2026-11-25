package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/chat"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestChatReportedUsageDoesNotLimitWork(t *testing.T) {
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		n := requests.Add(1)
		if n == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"list","type":"function","function":{"name":"search_passages","arguments":"{\"text\":\"synthetic\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2000000,"completion_tokens":1000000}}`)
			return
		}
		if n == 2 && len(body.Tools) == 0 {
			t.Error("reported usage forced an early final answer")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Completed with recorded usage."},"finish_reason":"stop"}],"usage":{"prompt_tokens":2000000,"completion_tokens":1000000}}`)
	})
	path := "/p/default/api/chat/" + c.ID
	for _, submission := range []string{"first", "after-high-usage"} {
		w := proxyCall(app, "alice", "POST", path+"/turns", fmt.Sprintf(`{"submissionId":%q,"text":"Read the notebook list","options":{"mode":"ask"}}`, submission))
		if w.Code != 200 {
			t.Fatalf("new work rejected after high usage: %d %s", w.Code, w.Body)
		}
		done := waitChatRun(t, app, c.ID)
		app.chat.Wait()
		if run := done.Runs[len(done.Runs)-1]; run.State != "completed" {
			t.Fatalf("high usage stopped work: %s %s", run.State, run.Error)
		}
	}
	saved, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Runs[0].Usage.Tokens() != 6000000 || saved.Runs[0].Usage.Requests != 2 || requests.Load() != 3 {
		t.Fatalf("provider usage was lost: %+v requests=%d", saved.Runs, requests.Load())
	}
	runID := saved.Runs[1].ID
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		run := c.Run(runID)
		run.State = "interrupted"
		run.Options.BudgetTokens = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "POST", path+"/runs/"+runID+"/resume", `{"budgetTokens":1}`)
	if w.Code != 200 {
		t.Fatalf("legacy continuation budget blocked work: %d %s", w.Code, w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	if run := done.Run(runID); run.State != "completed" || run.Usage.Tokens() != 6000000 || run.Usage.Requests != 2 || requests.Load() != 4 {
		t.Fatalf("continuation lost usage or stopped: %+v requests=%d", run, requests.Load())
	}
}

func TestChatTokenUsageNeverBlocksAnyNativeProvider(t *testing.T) {
	for _, format := range []string{"responses", "messages", "chat-completions"} {
		t.Run(format, func(t *testing.T) {
			var requests atomic.Int32
			app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var request any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				requests.Add(1)
				switch format {
				case "responses":
					fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}],"usage":{"input_tokens":2000000,"output_tokens":1000000}}`)
				case "messages":
					fmt.Fprint(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn","usage":{"input_tokens":2000000,"output_tokens":1000000}}`)
				default:
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":2000000,"completion_tokens":1000000}}`)
				}
			})
			connection := app.chat.Config.Connections[0]
			connection.Format = format
			setChatConnection(t, app, connection)
			path := "/p/default/api/chat/" + c.ID
			for i := 0; i < 2; i++ {
				w := proxyCall(app, "alice", "POST", path+"/turns", fmt.Sprintf(`{"submissionId":"native-%d","text":"Answer briefly.","options":{"mode":"ask","budgetTokens":1}}`, i))
				if w.Code != 200 {
					t.Fatalf("legacy quota rejected %s: %d %s", format, w.Code, w.Body)
				}
				done := waitChatRun(t, app, c.ID)
				app.chat.Wait()
				if run := done.Runs[len(done.Runs)-1]; run.State != "completed" || run.Usage.Tokens() != 3000000 {
					t.Fatalf("native provider stopped or lost reported usage: %+v", run)
				}
			}
			saved, err := app.chat.Store.Read("alice", "default", c.ID)
			if err != nil {
				t.Fatal(err)
			}
			runID := saved.Runs[1].ID
			if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error { c.Run(runID).State = "interrupted"; return nil }); err != nil {
				t.Fatal(err)
			}
			w := proxyCall(app, "alice", "POST", path+"/runs/"+runID+"/resume", `{}`)
			if w.Code != 200 {
				t.Fatalf("native continuation rejected: %d %s", w.Code, w.Body)
			}
			done := waitChatRun(t, app, c.ID)
			app.chat.Wait()
			if run := done.Run(runID); run.State != "completed" || run.Usage.Tokens() != 6000000 || run.Usage.Requests != 2 || requests.Load() != 3 {
				t.Fatalf("native continuation stopped or lost usage: %+v", run)
			}
		})
	}
}
