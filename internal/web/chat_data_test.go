package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
)

func TestChatDataCaptureIsDatedAndRevocable(t *testing.T) {
	for _, tc := range []struct {
		name, format  string
		rounds, tools int
		execute       bool
	}{
		{"discovery", "chat-completions", 4, 8, true},
		{"native-discovery", "responses", 4, 8, true},
		{"round-limit", "responses", 3, 8, false},
		{"tool-limit", "responses", 4, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests, executions atomic.Int32
			calls := []chat.ToolCall{
				{ID: "connections", Name: "list_data_connections", Arguments: json.RawMessage(`{}`)},
				{ID: "operations", Name: "list_data_operations", Arguments: json.RawMessage(`{"connection":"counts"}`)},
				{ID: "query-1", Name: "call_data_operation", Arguments: json.RawMessage(`{"connection":"counts","operation":"count","arguments":"{}","schemaHash":"model-copied-wrong-contract"}`)},
			}
			app, conversation := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Tools      []json.RawMessage `json:"tools"`
					ToolChoice string            `json:"tool_choice"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "invalid fixture request", 400)
					return
				}
				n := int(requests.Add(1))
				if n <= len(calls) && len(body.Tools) > 0 && body.ToolChoice != "none" {
					if tc.format == "responses" {
						fmt.Fprint(w, nativeBudgetResponse(t, tc.format, calls[n-1:n]))
					} else {
						call := calls[n-1]
						message := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": call.ID, "type": "function", "function": map[string]string{"name": call.Name, "arguments": string(call.Arguments)}}}}
						json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 20}})
					}
					return
				}
				answer := "The data operation was not executed within the allowance."
				if executions.Load() > 0 {
					answer = "The observed count is seven."
				}
				if tc.format == "responses" {
					json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": answer}}}}, "usage": map[string]int{"input_tokens": 100, "output_tokens": 20}})
				} else {
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 20}})
				}
			})
			connection := enableStrictChat(t, app)
			connection.Format = tc.format
			connection.Limits.MaxRounds, connection.Limits.MaxTools = tc.rounds, tc.tools
			setChatConnection(t, app, connection)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				executions.Add(1)
				w.Write([]byte(`{"count":7}`))
			}))
			defer upstream.Close()
			manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "counts", Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "count", ReadOnly: true, Method: "GET", Path: "/count"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			app.dataConnections = manager
			grant, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "counts", Users: map[string][]string{"alice": {"count"}}})
			if err != nil {
				t.Fatal(err)
			}
			if w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+conversation.ID+"/turns", `{"submissionId":"data-1","text":"What is the current count?","options":{"mode":"ask"}}`); w.Code != 202 && w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			got := waitChatRun(t, app, conversation.ID)
			run := got.Runs[len(got.Runs)-1]
			if run.State != "completed" {
				t.Fatal(run.State, run.Error)
			}
			wantExecutions, wantRequests := int32(0), int32(3)
			if tc.execute {
				wantExecutions, wantRequests = 1, 4
			}
			if executions.Load() != wantExecutions || requests.Load() != wantRequests {
				t.Fatalf("discovery/execution/final-answer sequence: executions=%d want=%d requests=%d want=%d", executions.Load(), wantExecutions, requests.Load(), wantRequests)
			}
			found := false
			for _, c := range run.Citations {
				if c.Kind == "data" {
					found = c.CapturedAt != "" && c.Source == "counts" && c.References == "count"
				}
			}
			if found != tc.execute || tc.execute && run.Provenance.Complete {
				t.Fatal("live result missing, invented, or reusable as fresh evidence", run.Citations)
			}
			if tc.execute {
				grant.Users = nil
				if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, grant); err != nil {
					t.Fatal(err)
				}
				if w := proxyCall(app, "alice", "GET", "/p/default/api/chat/"+conversation.ID, ""); w.Code != 403 {
					t.Fatal("revoked live capture accessible", w.Code)
				}
			}
		})
	}
}
