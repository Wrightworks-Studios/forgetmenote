package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/chat"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCorrectnessAskFinalAtToolLimit(t *testing.T) {
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools    []json.RawMessage `json:"tools"`
			Messages []chat.Message    `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		n := requests.Add(1)
		if len(body.Tools) > 0 {
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"Searching","tool_calls":[{"id":"call-%d","type":"function","function":{"name":"search_passages","arguments":"{\"text\":\"needle\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`, n)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Final supported answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
	})
	app.chat.Config.Connections[0].Limits.MaxTools = 1
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"tool-limit-answer","text":"Find needle","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	run := done.Runs[0]
	if run.State != "completed" || requests.Load() != 2 || run.ToolCount != 1 || len(run.Operations) != 1 || len(pendingToolCalls(run.Messages)) != 0 || done.Turns[len(done.Turns)-1].Content != "Final supported answer" {
		t.Fatalf("No final answer at exhausted tool budget: state=%s error=%q requests=%d tools=%d", run.State, run.Error, requests.Load(), run.ToolCount)
	}
}
func TestCorrectnessAskFinalWhenSiblingCallsExceedRemainingTools(t *testing.T) {
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		n := requests.Add(1)
		if len(body.Tools) > 0 {
			count := 1
			if n > 1 {
				count = 2
			}
			calls := []any{}
			for i := 0; i < count; i++ {
				calls = append(calls, map[string]any{"id": fmt.Sprintf("call-%d-%d", n, i), "type": "function", "function": map[string]string{"name": "search_passages", "arguments": `{"text":"needle"}`}})
			}
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "Searching", "tool_calls": calls}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 10}})
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Final supported answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
	})
	app.chat.Config.Connections[0].Limits.MaxTools = 2
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"sibling-tool-limit-answer","text":"Find needle","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	run := done.Runs[0]
	if run.State != "completed" || requests.Load() != 3 || run.ToolCount != 2 || len(run.Operations) != 3 || len(pendingToolCalls(run.Messages)) != 0 || done.Turns[len(done.Turns)-1].Content != "Final supported answer" {
		t.Fatalf("No final answer when sibling batch exceeds remainder: state=%s error=%q requests=%d tools=%d pending=%d", run.State, run.Error, requests.Load(), run.ToolCount, len(pendingToolCalls(run.Messages)))
	}
	if run.Operations[2].State != "omitted" || !strings.Contains(string(run.Operations[2].Result), `"executed":false`) {
		t.Fatal("excess sibling was not explicitly recorded as unexecuted")
	}
}
