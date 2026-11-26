package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
)

func TestChatBudgetOmissionNativeRestartAndCancel(t *testing.T) {
	for _, tc := range []struct {
		format string
		cancel bool
	}{{"responses", false}, {"messages", false}, {"chat-completions", false}, {"chat-completions", true}} {
		t.Run(fmt.Sprintf("%s_cancel=%t", tc.format, tc.cancel), func(t *testing.T) {
			var requests atomic.Int32
			var app *Server
			var c chat.Conversation
			var connection chat.Connection
			stopped := make(chan struct{})
			calls := []chat.ToolCall{{ID: "first-search", Name: "search_passages", Arguments: json.RawMessage(`{"text":"primary needle"}`)}, {ID: "allowed-search", Name: "search_passages", Arguments: json.RawMessage(`{"text":"primary needle"}`)}, {ID: "omitted-read", Name: "read_note", Arguments: json.RawMessage(`{"path":"notes/secondary.md"}`)}}
			app, c = runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "fixture", 400)
					return
				}
				n := requests.Add(1)
				if n == 1 {
					fmt.Fprint(w, nativeBudgetResponse(t, tc.format, calls[:1]))
					return
				}
				if n == 2 {
					fmt.Fprint(w, nativeBudgetResponse(t, tc.format, calls[1:]))
					return
				}
				if tc.format == "chat-completions" {
					if _, ok := body["tools"]; ok {
						t.Error("exhausted/omitted Ask still offered tools after continuation")
					}
				} else {
					expected := `"none"`
					if tc.format == "messages" {
						expected = `{"type":"none"}`
					}
					if string(body["tool_choice"]) != expected {
						t.Error("exhausted/omitted native Ask did not enter final-answer control")
					}
				}
				saved, err := app.chat.Store.Read("alice", "default", c.ID)
				if err != nil {
					t.Error(err)
					http.Error(w, "fixture", 500)
					return
				}
				run := saved.Runs[0]
				if len(run.Operations) != 3 || run.ToolCount != 2 || len(chatrun.PendingToolCalls(run.Messages)) != 0 {
					t.Error("native final request arrived without one completed and one omitted paired receipt")
				}
				request := chatrun.FinalRequest(chat.Request{Model: "synthetic", System: chatrun.System, Messages: run.Messages, Tools: app.chatrun.RunTools(run, connection), MaxOutputTokens: connection.Limits.OutputTokens})
				request, _, err = chatrun.FitRequest(run, request, connection.Limits.ContextTokens, app.chat.Snapshot().Providers["local"].EstimateRequest)
				if err != nil {
					t.Error(err)
					http.Error(w, "fixture", 500)
					return
				}
				assertBudgetNativePairs(t, tc.format, body, run.Messages, request)
				encoded, _ := json.Marshal(body)
				if strings.Contains(string(encoded), "OMITTED_SECONDARY_NOTE_CONTENT") {
					t.Error("budget-omitted note was dispatched or leaked into evidence")
				}
				for _, m := range request.Messages {
					if m.Role == "tool" && m.ToolCallID == "omitted-read" {
						if !strings.Contains(m.Content, `"executed":false`) || !strings.Contains(m.Content, `"omitted":true`) || !strings.Contains(m.Content, "Tool-call budget reached") {
							t.Error("context fitting hid the explicit non-executed budget result")
						}
					}
				}
				if n == 3 {
					if tc.cancel {
						close(stopped)
						<-r.Context().Done()
						return
					}
					http.Error(w, "synthetic provider interruption", 503)
					return
				}
				answer := "The primary needle calibrates at 73 units. The secondary note was not examined."
				switch tc.format {
				case "responses":
					json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": answer}}}}, "usage": map[string]int{"input_tokens": 100, "output_tokens": 20}})
				case "messages":
					json.NewEncoder(w).Encode(map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "text", "text": answer}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 100, "output_tokens": 20}})
				default:
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 20}})
				}
			})
			connection = app.chat.Config.Connections[0]
			declared := nativeBudgetConnection(connection.BaseURL, tc.format)
			connection.Format, connection.Capabilities = tc.format, declared.Capabilities
			connection.Limits.MaxTools = 2
			setChatConnection(t, app, connection)
			store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
			if err != nil {
				t.Fatal(err)
			}
			for path, text := range map[string]string{"notes/primary.md": "# Primary evidence\nThe primary needle calibrates at 73 units.", "notes/secondary.md": "# Secondary\nOMITTED_SECONDARY_NOTE_CONTENT"} {
				if _, err = store.Save(t.Context(), path, []byte(text), "", "operator"); err != nil {
					t.Fatal(err)
				}
			}
			response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"native-tool-budget","text":"Find primary needle evidence","options":{"mode":"ask"}}`)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body)
			}
			var started struct {
				Run string `json:"run"`
			}
			json.Unmarshal(response.Body.Bytes(), &started)
			if tc.cancel {
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					t.Fatal("final request did not start")
				}
				response = proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/runs/"+started.Run+"/cancel", `{}`)
				if response.Code != 200 {
					t.Fatal(response.Code, response.Body)
				}
			}
			done := waitChatRun(t, app, c.ID)
			app.chat.Wait()
			run := done.Runs[0]
			wantState := "failed"
			if tc.cancel {
				wantState = "cancelled"
			}
			if run.State != wantState || run.ToolCount != 2 || len(run.Operations) != 3 || len(chatrun.PendingToolCalls(run.Messages)) != 0 {
				t.Fatalf("native interruption lost paired receipts: state=%s tools=%d operations=%d pending=%d", run.State, run.ToolCount, len(run.Operations), len(chatrun.PendingToolCalls(run.Messages)))
			}
			if run.Operations[0].State != "completed" || !strings.Contains(string(run.Operations[0].Result), "73 units") || run.Operations[1].State != "completed" || run.Operations[2].State != "omitted" {
				t.Fatal("allowed read and omitted read are not distinguished honestly")
			}
			receipts := append([]chat.Operation(nil), run.Operations...)
			// Exercise the durable boundary after the omission but before another model
			// admission. Keep prior accounted usage conservatively; restart must still
			// finish the answer when Resume grants a fresh tool allowance.
			if !tc.cancel {
				if err = app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error {
					r := saved.Run(run.ID)
					r.State = "running"
					r.Rounds = 2
					r.InFlightTokens = 0
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			app.chat.Store.Close()
			reopened, err := chat.OpenStore(app.db)
			if err != nil {
				t.Fatal(err)
			}
			app.chat.Store = reopened
			response = proxyCall(app, "alice", "GET", "/p/default/api/chat/"+c.ID, "")
			var public chat.Conversation
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &public) != nil || len(public.Runs[0].Operations) != 3 || public.Runs[0].Operations[2].State != "omitted" || strings.Contains(response.Body.String(), "SYNTHETIC_OPAQUE") || strings.Contains(response.Body.String(), "SYNTHETIC_NATIVE_REASONING") {
				t.Fatal("reload lost omission or exposed native reasoning")
			}
			response = proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/runs/"+run.ID+"/resume", `{}`)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body)
			}
			done = waitChatRun(t, app, c.ID)
			app.chat.Wait()
			run = done.Runs[0]
			if run.State != "completed" || requests.Load() != 4 || run.ToolCount != 2 || !reflect.DeepEqual(run.Operations, receipts) || len(chatrun.PendingToolCalls(run.Messages)) != 0 || run.Usage.Requests != 4 {
				t.Fatal("restart/resume replayed omitted work or changed native receipts/accounting")
			}
		})
	}
}

func TestChatNonAskToolBudgetStillPausesWithDiagnostic(t *testing.T) {
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"search-%d","type":"function","function":{"name":"search_passages","arguments":"{\"text\":\"needle\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`, n)
	})
	app.chat.Config.Connections[0].Limits.MaxTools = 1
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"suggest-tool-budget","text":"Find needle","options":{"mode":"suggest"}}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	run := done.Runs[0]
	if run.State != "interrupted" || run.ToolCount != 1 || len(run.Operations) != 1 || len(chatrun.PendingToolCalls(run.Messages)) != 1 {
		t.Fatal("non-Ask tool budget no longer preserves explicit continuation")
	}
	found := false
	for _, line := range strings.Split(logs.String(), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["msg"] == "chat run terminal" && event["run"] == run.ID {
			found = event["category"] == "tool_budget" && event["tools"] == float64(1) && event["omitted_tools"] == float64(0)
		}
	}
	if !found {
		t.Fatal("actual Tool-call budget reached diagnostic remains misclassified")
	}
}

func assertBudgetNativePairs(t *testing.T, format string, body map[string]json.RawMessage, original []chat.Message, fitted chat.Request) {
	t.Helper()
	field := "messages"
	if format == "responses" {
		field = "input"
	}
	var wire []map[string]json.RawMessage
	if json.Unmarshal(body[field], &wire) != nil {
		t.Error("invalid native continuation")
		return
	}
	expectedAssistants := []chat.Message{}
	expectedItems := []json.RawMessage{}
	for _, m := range original {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			expectedAssistants = append(expectedAssistants, m)
			if format == "responses" {
				var items []json.RawMessage
				json.Unmarshal(m.Continuation, &items)
				expectedItems = append(expectedItems, items...)
			}
		}
	}
	actualItems := []json.RawMessage{}
	results := map[string]string{}
	assistantIndex := 0
	for _, m := range wire {
		var kind, role string
		json.Unmarshal(m["type"], &kind)
		json.Unmarshal(m["role"], &role)
		if format == "responses" {
			if kind == "function_call_output" {
				var id, text string
				json.Unmarshal(m["call_id"], &id)
				json.Unmarshal(m["output"], &text)
				if _, ok := results[id]; ok {
					t.Error("duplicate native result")
				}
				results[id] = text
			} else if kind != "" {
				raw, _ := json.Marshal(m)
				actualItems = append(actualItems, raw)
			}
		} else if role == "assistant" {
			if assistantIndex >= len(expectedAssistants) {
				t.Error("extra native assistant")
				continue
			}
			expected := expectedAssistants[assistantIndex]
			assistantIndex++
			if format == "messages" {
				var want, got any
				json.Unmarshal(expected.Continuation, &want)
				json.Unmarshal(m["content"], &got)
				if !reflect.DeepEqual(want, got) {
					t.Error("thinking/signatures/tool-use blocks changed")
				}
			} else {
				var continuation map[string]string
				json.Unmarshal(expected.Continuation, &continuation)
				var reasoning string
				json.Unmarshal(m["reasoning_content"], &reasoning)
				if reasoning != continuation["reasoning_content"] {
					t.Error("native reasoning_content changed")
				}
				var calls []struct {
					ID       string                           `json:"id"`
					Function struct{ Name, Arguments string } `json:"function"`
				}
				json.Unmarshal(m["tool_calls"], &calls)
				if len(calls) != len(expected.ToolCalls) {
					t.Error("native assistant lost calls")
				} else {
					for i, call := range calls {
						want := expected.ToolCalls[i]
						if call.ID != want.ID || call.Function.Name != want.Name || call.Function.Arguments != string(want.Arguments) {
							t.Error("native call identity or arguments changed")
						}
					}
				}
			}
		} else if format == "messages" && role == "user" {
			var blocks []map[string]json.RawMessage
			json.Unmarshal(m["content"], &blocks)
			for _, block := range blocks {
				if string(block["type"]) == `"tool_result"` {
					var id, text string
					json.Unmarshal(block["tool_use_id"], &id)
					json.Unmarshal(block["content"], &text)
					if _, ok := results[id]; ok {
						t.Error("duplicate native result")
					}
					results[id] = text
				}
			}
		} else if role == "tool" {
			var id, text string
			json.Unmarshal(m["tool_call_id"], &id)
			json.Unmarshal(m["content"], &text)
			if _, ok := results[id]; ok {
				t.Error("duplicate native result")
			}
			results[id] = text
		}
	}
	if format == "responses" {
		var want, got any
		raw, _ := json.Marshal(expectedItems)
		json.Unmarshal(raw, &want)
		raw, _ = json.Marshal(actualItems)
		json.Unmarshal(raw, &got)
		if !reflect.DeepEqual(want, got) {
			t.Error("opaque Responses items changed")
		}
	} else if assistantIndex != len(expectedAssistants) {
		t.Error("native assistant turn disappeared")
	}
	count := 0
	for _, m := range fitted.Messages {
		if m.Role == "tool" {
			count++
			text, ok := results[m.ToolCallID]
			if !ok || text != m.Content {
				t.Error("native result content or identity changed")
			}
		}
	}
	if count != len(results) {
		t.Error("orphaned native call/result pairing")
	}
}
