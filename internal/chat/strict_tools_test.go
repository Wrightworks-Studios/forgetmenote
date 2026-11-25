package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

const strictNestedSchema = `{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer","default":7},"nullable":{"type":["string","null"]},"options":{"$ref":"#/$defs/options"},"rows":{"type":"array","items":{"$ref":"#/$defs/options"}}},"required":["path","nullable"],"additionalProperties":false,"$defs":{"options":{"type":"object","properties":{"enabled":{"type":"boolean"},"label":{"type":"string","enum":["short","long"]},"child":{"$ref":"#/$defs/options"}},"required":["enabled"],"additionalProperties":false}}}`
const strictNestedArguments = `{"path":"note.md","limit":null,"nullable":null,"options":{"enabled":false,"label":null,"child":null},"rows":[{"enabled":true,"label":"short","child":{"enabled":false,"label":null,"child":null}}]}`

func checkStrictContract(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if properties, ok := v["properties"].(map[string]any); ok || v["type"] == "object" {
				required, _ := v["required"].([]any)
				names := map[string]bool{}
				for _, name := range required {
					names[name.(string)] = true
				}
				if len(names) != len(properties) || v["additionalProperties"] != false {
					t.Errorf("invalid strict object: %v", v)
				}
				for name := range properties {
					if !names[name] {
						t.Errorf("optional wire property %q", name)
					}
				}
			}
			for key, child := range v {
				if key != "default" && key != "examples" && key != "enum" && key != "const" {
					walk(child)
				}
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(value)
}

func TestStrictProviderOptionalNestedNativeContracts(t *testing.T) {
	for _, format := range []string{"responses", "messages", "chat-completions"} {
		t.Run(format, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var request map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				var defs []map[string]json.RawMessage
				if err := json.Unmarshal(request["tools"], &defs); err != nil || len(defs) != 1 {
					t.Errorf("tools: %s", request["tools"])
					return
				}
				tool := defs[0]
				schemaKey := "parameters"
				if format == "chat-completions" {
					if err := json.Unmarshal(tool["function"], &tool); err != nil {
						t.Error(err)
						return
					}
				}
				if format == "messages" {
					schemaKey = "input_schema"
				}
				if string(tool["strict"]) != "true" {
					t.Error("strict mode disabled")
				}
				checkStrictContract(t, tool[schemaKey])
				var schema jsonschema.Schema
				if err := json.Unmarshal(tool[schemaKey], &schema); err != nil {
					t.Error(err)
					return
				}
				resolved, err := schema.Resolve(nil)
				if err != nil {
					t.Error(err)
					return
				}
				var args any
				json.Unmarshal([]byte(strictNestedArguments), &args)
				if err := resolved.Validate(args); err != nil {
					t.Errorf("strict optional null arguments rejected: %v", err)
				}
				if requests == 2 {
					if format == "chat-completions" {
						var messages []struct {
							ToolCalls []struct {
								Function struct{ Arguments string } `json:"function"`
							} `json:"tool_calls"`
						}
						json.Unmarshal(request["messages"], &messages)
						if len(messages) != 3 || len(messages[1].ToolCalls) != 1 || messages[1].ToolCalls[0].Function.Arguments != strictNestedArguments {
							t.Error("native completion arguments changed")
						}
					} else if format == "responses" {
						var items []nativeBlock
						json.Unmarshal(request["input"], &items)
						found := false
						for _, item := range items {
							if item.Type == "function_call" && item.CallID == "call_1" {
								found = item.Arguments == strictNestedArguments
							}
						}
						if !found {
							t.Error("native Responses arguments changed")
						}
					} else {
						var messages []struct{ Content []nativeBlock }
						json.Unmarshal(request["messages"], &messages)
						found := false
						for _, message := range messages {
							for _, block := range message.Content {
								if block.Type == "tool_use" && block.ID == "call_1" {
									var got, want any
									json.Unmarshal(block.Input, &got)
									json.Unmarshal([]byte(strictNestedArguments), &want)
									found = reflect.DeepEqual(got, want)
								}
							}
						}
						if !found {
							t.Error("native Messages arguments changed")
						}
					}
				}
				w.Header().Set("Content-Type", "application/json")
				switch format {
				case "responses":
					json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read_note", "arguments": strictNestedArguments}}})
				case "messages":
					fmt.Fprintf(w, `{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read_note","input":%s}],"stop_reason":"tool_use"}`, strictNestedArguments)
				default:
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read_note", "arguments": strictNestedArguments}}}}, "finish_reason": "tool_calls"}}})
				}
			}))
			defer srv.Close()
			c := providerTestConnection(srv.URL, format)
			c.Capabilities.StrictTools = true
			p, err := NewProvider(c)
			if err != nil {
				t.Fatal(err)
			}
			req := Request{Messages: []Message{{Role: "user", Content: "Read"}}, Tools: []Tool{{Name: "read_note", Schema: json.RawMessage(strictNestedSchema)}}}
			response, err := p.Generate(t.Context(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(req.Tools[0].Schema, json.RawMessage(strictNestedSchema)) {
				t.Fatal("mutated caller's shared schema")
			}
			if len(response.Message.ToolCalls) != 1 {
				t.Fatal("missing tool call")
			}
			var got, want any
			json.Unmarshal(response.Message.ToolCalls[0].Arguments, &got)
			json.Unmarshal([]byte(strictNestedArguments), &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatal("native response changed")
			}
			req.Messages = append(req.Messages, response.Message, Message{Role: "tool", ToolCallID: "call_1", Content: "found"})
			if _, err = p.Generate(t.Context(), req, nil); err != nil {
				t.Fatal(err)
			}
			if requests != 2 {
				t.Fatalf("provider requests=%d", requests)
			}
		})
	}
}

func TestStrictArgumentsRestoreOriginalSemantics(t *testing.T) {
	tests := []struct {
		name, schema, arguments, want string
		fail                          bool
	}{
		{"nested recursive references", strictNestedSchema, strictNestedArguments, `{"path":"note.md","nullable":null,"options":{"enabled":false},"rows":[{"enabled":true,"label":"short","child":{"enabled":false}}]}`, false},
		{"optional nullable preserved", `{"type":"object","properties":{"value":{"type":["string","null"]}},"additionalProperties":false}`, `{"value":null}`, `{"value":null}`, false},
		{"zeros and exact integers", `{"type":"object","properties":{"number":{"type":"integer"},"false":{"type":"boolean"},"empty":{"type":"string"},"rows":{"type":"array","items":{"type":"integer"}}},"additionalProperties":false}`, `{"number":9007199254740993,"false":false,"empty":"","rows":[]}`, `{"number":9007199254740993,"false":false,"empty":"","rows":[]}`, false},
		{"required null rejected", strictNestedSchema, `{"path":null,"nullable":null}`, "", true},
		{"missing required rejected", strictNestedSchema, `{"nullable":null}`, "", true},
		{"unknown null rejected", strictNestedSchema, `{"path":"note.md","nullable":null,"unknown":null}`, "", true},
		{"unknown nested null rejected", strictNestedSchema, `{"path":"note.md","nullable":null,"options":{"enabled":false,"unknown":null}}`, "", true},
		{"array null not omitted", strictNestedSchema, `{"path":"note.md","nullable":null,"rows":[null]}`, "", true},
		{"anyOf object", `{"type":"object","properties":{"value":{"anyOf":[{"type":"string"},{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}},"required":["path"],"additionalProperties":false}]}},"required":["value"],"additionalProperties":false}`, `{"value":{"path":"note.md","limit":null}}`, `{"value":{"path":"note.md"}}`, false},
		{"root recursive reference", `{"type":"object","properties":{"name":{"type":"string"},"child":{"$ref":"#"}},"required":["name"],"additionalProperties":false}`, `{"name":"root","child":{"name":"child","child":null}}`, `{"name":"root","child":{"name":"child"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeStrictToolArguments(json.RawMessage(tt.schema), json.RawMessage(tt.arguments))
			if tt.fail {
				if err == nil {
					t.Fatalf("accepted invalid original arguments: %s", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var want any
			decoder := json.NewDecoder(strings.NewReader(tt.want))
			decoder.UseNumber()
			decoder.Decode(&want)
			canonical, _ := json.Marshal(want)
			if string(got) != string(canonical) {
				t.Fatalf("got %s want %s", got, canonical)
			}
		})
	}
}

func TestStrictSchemasRejectUnrepresentableObjects(t *testing.T) {
	for _, additional := range []string{`true`, `{"type":"string"}`} {
		schema := json.RawMessage(`{"type":"object","properties":{"labels":{"type":"object","additionalProperties":` + additional + `}},"required":["labels"],"additionalProperties":false}`)
		_, err := StrictToolDefinitions([]Tool{{Name: "labels", Schema: schema}})
		if err == nil || !strings.Contains(err.Error(), "labels") || !strings.Contains(err.Error(), "unknown properties") {
			t.Fatalf("unrepresentable map silently changed: %v", err)
		}
	}
}
