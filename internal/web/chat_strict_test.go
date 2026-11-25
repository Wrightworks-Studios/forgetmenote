package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"github.com/google/jsonschema-go/jsonschema"
)

func enableStrictChat(t *testing.T, app *Server) chat.Connection {
	t.Helper()
	connection := app.chat.Config.Connections[0]
	connection.Capabilities.StrictTools = true
	setChatConnection(t, app, connection)
	return connection
}

func TestStrictChatOptionalActionRoundTrip(t *testing.T) {
	var requests atomic.Int32
	var sawRead, sawBrowse atomic.Bool
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role       string
				Content    string
				ToolCallID string `json:"tool_call_id"`
				ToolCalls  []struct {
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			}
			Tools []struct {
				Function struct {
					Name       string
					Strict     bool
					Parameters json.RawMessage
				}
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		for _, tool := range body.Tools {
			if !tool.Function.Strict {
				t.Error("strict disabled")
			}
			var schema jsonschema.Schema
			if json.Unmarshal(tool.Function.Parameters, &schema) != nil {
				t.Error("invalid schema")
				return
			}
			if _, err := schema.Resolve(nil); err != nil {
				t.Error(err)
			}
		}
		for _, m := range body.Messages {
			if m.Role == "tool" && m.ToolCallID == "read" {
				sawRead.Store(strings.Contains(m.Content, "SYNTHETIC_STRICT_NOTE") && !strings.Contains(m.Content, `"error"`))
			}
			if m.Role == "tool" && m.ToolCallID == "browse" {
				sawBrowse.Store(strings.Contains(m.Content, "notes") && !strings.Contains(m.Content, `"error"`))
			}
			for _, call := range m.ToolCalls {
				if call.Function.Name == "read_note" && !strings.Contains(call.Function.Arguments, `"revision":null`) {
					t.Error("native read continuation changed")
				}
				if call.Function.Name == "browse_folder" && call.Function.Arguments != `{"path":null}` {
					t.Error("native discovered continuation changed")
				}
			}
		}
		var calls []any
		call := func(id, name, args string) {
			calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}})
		}
		switch requests.Add(1) {
		case 1:
			call("read", "read_note", `{"path":"notes/strict.md","revision":null}`)
			call("discover", "discover_actions", `{"names":["browse_folder"]}`)
		case 2:
			found := false
			for _, tool := range body.Tools {
				found = found || tool.Function.Name == "browse_folder"
			}
			if !found {
				t.Error("discovered tool missing")
			}
			call("browse", "browse_folder", `{"path":null}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Strict tools completed."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":4}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 4}})
	})
	enableStrictChat(t, app)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/note", `{"path":"notes/strict.md","content":"SYNTHETIC_STRICT_NOTE","revision":""}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	w = proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"strict-round-trip","text":"Read the note and browse the notebook.","options":{"mode":"ask"}}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	run := done.Runs[0]
	if run.State != "completed" || !sawRead.Load() || !sawBrowse.Load() || requests.Load() != 3 {
		t.Fatalf("strict round trip: state=%s error=%s read=%t browse=%t requests=%d", run.State, run.Error, sawRead.Load(), sawBrowse.Load(), requests.Load())
	}
	for _, op := range run.Operations {
		if op.Name == "read_note" || op.Name == "browse_folder" {
			var args map[string]json.RawMessage
			json.Unmarshal(op.Arguments, &args)
			if _, ok := args["revision"]; ok {
				t.Error("optional revision was not omitted")
			}
			if op.Name == "browse_folder" {
				if _, ok := args["path"]; ok {
					t.Error("optional path was not omitted")
				}
			}
		}
	}
}

func TestStrictChatAllExposedToolSchemas(t *testing.T) {
	var requests atomic.Int32
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Function struct {
					Name       string
					Strict     bool
					Parameters json.RawMessage
				}
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		for _, tool := range body.Tools {
			if !tool.Function.Strict {
				t.Errorf("%s strict disabled", tool.Function.Name)
			}
			var schema any
			json.Unmarshal(tool.Function.Parameters, &schema)
			var check func(any)
			check = func(v any) {
				switch value := v.(type) {
				case map[string]any:
					if props, ok := value["properties"].(map[string]any); ok || value["type"] == "object" {
						required, _ := value["required"].([]any)
						names := map[string]bool{}
						for _, name := range required {
							names[name.(string)] = true
						}
						if len(required) != len(props) || value["additionalProperties"] != false {
							t.Errorf("%s invalid strict object", tool.Function.Name)
						}
						for name := range props {
							if !names[name] {
								t.Errorf("%s optional property %s", tool.Function.Name, name)
							}
						}
					}
					for _, child := range value {
						check(child)
					}
				case []any:
					for _, child := range value {
						check(child)
					}
				}
			}
			check(schema)
		}
		requests.Add(1)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})
	connection := enableStrictChat(t, app)
	tools := app.runTools(chat.Run{Options: chat.Options{Mode: "edit", Actions: []string{"notes", "comments", "live", "review"}, Research: true}}, connection)
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	for name := range app.actions {
		p := policy(name)
		if !p.Read && !p.Proposal && !p.Edit && !p.Control {
			continue
		}
		if !names[name] {
			if tool, ok := app.actionTool(name); ok {
				tools = append(tools, tool)
				names[name] = true
			}
		}
	}
	for _, tool := range tools {
		t.Run(tool.Name, func(t *testing.T) {
			before := string(tool.Schema)
			_, err := app.chat.Snapshot().Providers[connection.ID].Generate(t.Context(), chat.Request{Messages: []chat.Message{{Role: "user", Content: "Check exposed schema"}}, Tools: []chat.Tool{tool}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if string(tool.Schema) != before {
				t.Fatal("shared schema mutated")
			}
		})
	}
	if requests.Load() != int32(len(tools)) {
		t.Fatalf("reached provider for %d/%d tools", requests.Load(), len(tools))
	}
}
