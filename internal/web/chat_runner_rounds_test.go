package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// A run persists its rounds in a fixed order: the initial evidence, then for
// each round its start, the streamed text, the assistant turn and its usage,
// then each tool call's result, and finally the terminal state, whose
// diagnostic reports the last request's fit.
func TestChatRunPersistsEachRoundInOrder(t *testing.T) {
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	var requests atomic.Int32
	var secondRoles atomic.Value
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid provider request")
		}
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Searching first.","tool_calls":[{"id":"call-1","type":"function","function":{"name":"search_passages","arguments":"{\"text\":\"synthetic\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":40,"completion_tokens":7}}`)
			return
		}
		roles := []string{}
		for _, m := range body.Messages {
			roles = append(roles, m.Role)
		}
		secondRoles.Store(roles)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The final answer."},"finish_reason":"stop"}],"usage":{"prompt_tokens":60,"completion_tokens":5}}`)
	})
	if w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"rounds-1","text":"What does the notebook say?","options":{"mode":"ask"}}`); w.Code != 200 {
		t.Fatalf("send failed: %d %s", w.Code, w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	run := done.Runs[0]
	events := []string{}
	for _, event := range done.Events {
		if event.Run != run.ID {
			continue
		}
		label := event.Type
		var data map[string]any
		_ = json.Unmarshal(event.Data, &data)
		switch event.Type {
		case "progress":
			label += ":" + fmt.Sprint(data["message"])
		case "state":
			label += ":" + fmt.Sprint(data["state"])
		case "assistant":
			label += fmt.Sprintf(":intermediate=%v", data["intermediate"])
		}
		events = append(events, label)
	}
	want := []string{
		"user", "state:queued",
		"progress:Retrieving committed notebook evidence", "citations",
		"progress:Generating response", "text_delta", "assistant:intermediate=true", "usage",
		"tool_result",
		"progress:Generating response", "text_delta", "assistant:intermediate=false", "usage",
		"state:completed",
	}
	if !slices.Equal(events, want) {
		t.Fatalf("events\n got %q\nwant %q", events, want)
	}
	roles := []string{}
	for _, m := range run.Messages {
		roles = append(roles, m.Role)
	}
	turns := []string{}
	for _, turn := range done.Turns {
		turns = append(turns, turn.Role+":"+turn.Content)
	}
	if run.State != "completed" || run.Rounds != 2 || run.ToolCount != 1 || len(run.Operations) != 1 || run.Operations[0].State != "completed" || run.InFlightTokens != 0 || run.Usage.Requests != 2 || run.Usage.InputTokens != 100 || run.Usage.OutputTokens != 12 || run.Usage.Reserved != 0 {
		t.Fatalf("run %+v", run)
	}
	if !slices.Equal(roles, []string{"user", "assistant", "tool", "assistant"}) || !slices.Equal(turns, []string{"user:What does the notebook say?", "assistant:Searching first.", "assistant:The final answer."}) {
		t.Fatalf("messages %v turns %q", roles, turns)
	}
	if got, _ := secondRoles.Load().([]string); !slices.Equal(got, []string{"system", "user", "assistant", "tool"}) {
		t.Fatalf("second request carried %v", got)
	}
	var requested, terminal []map[string]any
	for _, line := range strings.Split(logs.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["chat"] != c.ID {
			continue
		}
		switch record["msg"] {
		case "chat run request":
			requested = append(requested, record)
		case "chat run terminal":
			terminal = append(terminal, record)
		}
	}
	if len(requested) != 2 || len(terminal) != 1 || terminal[0]["category"] != "completed" || terminal[0]["input_tokens"] != requested[1]["input_tokens"] || requested[1]["rounds"] != float64(2) {
		t.Fatalf("diagnostics: requests %v terminal %v", requested, terminal)
	}
}
