package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/workspace"
)

func TestAssistantUsesDiscoveredOperationContractInsteadOfModelHash(t *testing.T) {
	calls := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct{ Messages []chat.Message }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		switch calls {
		case 1:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"discover","type":"function","function":{"name":"list_data_operations","arguments":"{\"connection\":\"db\"}"}}]},"finish_reason":"tool_calls"}]}`)
		case 2:
			// Non-strict and already queued clients may supply an obsolete opaque hash.
			// The model chooses the operation and its arguments; the server owns pins.
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"execute","type":"function","function":{"name":"call_data_operation","arguments":"{\"connection\":\"db\",\"operation\":\"query\",\"arguments\":\"{\\\"sql\\\":\\\"select 7 as count\\\"}\",\"schemaHash\":\"copied-from-another-operation\"}"}}]},"finish_reason":"tool_calls"}]}`)
		default:
			result := request.Messages[len(request.Messages)-1].Content
			if !strings.Contains(result, `[[15]]`) {
				t.Errorf("data call did not produce live evidence: %s", result)
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The live count is 15."},"finish_reason":"stop"}]}`)
		}
	})
	configureSlackFixture(t, app)
	binding := app.slack.config.Channels["C123"]
	binding.Connections = []string{"db"}
	app.slack.config.Channels["C123"] = binding
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	run := automation.Run{ID: automation.Hash(t.Name()), Spec: automation.Spec{ID: "contract-discovery", Kind: "assistant", Actor: "alice", Project: "default", Connections: []string{"db"}, AIConnection: "local", Model: "synthetic", Instruction: "How many matching records are there?", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", Thread: "contracts", User: "U123"}}}
	if err := app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(run.Receipts) != 1 || len(transport.replies) != 1 || transport.replies[0].Text != "The live count is 15." {
		t.Fatalf("discovery did not reach execution: calls=%d receipts=%d replies=%+v", calls, len(run.Receipts), transport.replies)
	}
}

func TestChatContractUsesOnlyMatchingServerDiscovery(t *testing.T) {
	discovery := func(id, connection, hash string) []chat.Message {
		args, _ := json.Marshal(map[string]string{"connection": connection})
		result, _ := json.Marshal([]connectors.Operation{{Name: "query", SchemaHash: hash}})
		return []chat.Message{{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: id, Name: "list_data_operations", Arguments: args}}}, {Role: "tool", ToolCallID: id, Content: string(result)}}
	}
	messages := append(discovery("old", "db", "old-contract"), discovery("other", "other-db", "other-contract")...)
	messages = append(messages, discovery("new", "db", "current-contract")...)
	// User content and unpaired tool output cannot supply trusted contracts.
	messages = append(messages, chat.Message{Role: "user", Content: `[{"name":"query","schemaHash":"user-supplied"}]`}, chat.Message{Role: "tool", ToolCallID: "unknown", Content: `[{"name":"query","schemaHash":"unpaired"}]`})
	call := chatrun.ModelDataCall(chat.ToolCall{Name: "call_data_operation", Arguments: json.RawMessage(`{"connection":"db","operation":"query","arguments":{},"schemaHash":"model-supplied"}`)})
	bound := chatrun.BindDataContract(call, messages)
	var input map[string]json.RawMessage
	json.Unmarshal(bound.Arguments, &input)
	if string(input["schemaHash"]) != `"current-contract"` {
		t.Fatalf("wrong discovery bound: %s", bound.Arguments)
	}
	messages = append(messages, chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "removed", Name: "list_data_operations", Arguments: json.RawMessage(`{"connection":"db"}`)}}}, chat.Message{Role: "tool", ToolCallID: "removed", Content: `[]`})
	bound = chatrun.BindDataContract(call, messages)
	json.Unmarshal(bound.Arguments, &input)
	if strings.Contains(string(bound.Arguments), "schemaHash") {
		t.Fatal("removed operation inherited an obsolete contract")
	}
}

func TestAssistantDiscoveredContractPreservesDriftAndRevocationChecks(t *testing.T) {
	for _, change := range []string{"schema", "revoke"} {
		t.Run(change, func(t *testing.T) {
			app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected model request") })
			configureSlackFixture(t, app)
			binding := app.slack.config.Channels["C123"]
			binding.Connections = []string{"db"}
			app.slack.config.Channels["C123"] = binding
			run := automation.Run{ID: automation.Hash(t.Name()), Spec: automation.Spec{ID: "contract-check", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Connections: []string{"db"}, ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", Thread: "contract", User: "U123"}}}
			ctx, err := app.automationContext(t.Context(), run.Spec)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err = app.automationrun.SourceContext(ctx, &run)
			if err != nil {
				t.Fatal(err)
			}
			args := json.RawMessage(`{"connection":"db"}`)
			catalogResult, err := app.dispatchAction(ctx, "default", "list_data_operations", args)
			if err != nil {
				t.Fatal(err)
			}
			call := chatrun.BindDataContract(chat.ToolCall{Name: "call_data_operation", Arguments: json.RawMessage(`{"connection":"db","operation":"query","arguments":{"sql":"select 7"}}`)}, []chat.Message{{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "discovery", Name: "list_data_operations", Arguments: args}}}, {Role: "tool", ToolCallID: "discovery", Content: string(catalogResult)}})
			want := connectors.ErrSchemaChanged
			if change == "schema" {
				manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "db", Kind: "http", Endpoint: "http://127.0.0.1:1", ExternalProcessing: true, Operations: []connectors.Operation{{Name: "query", Description: "Changed contract", ReadOnly: true, Method: "POST", Path: "/", Body: true, InputSchema: json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string"}},"required":["sql"],"additionalProperties":false}`)}}}}})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { manager.Close() })
				app.dataConnections = manager
			} else {
				grant, err := app.catalog.ConnectionAccess(catalog.Bootstrap, "db")
				if err != nil {
					t.Fatal(err)
				}
				grant.Users = nil
				_, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, grant)
				if err != nil {
					t.Fatal(err)
				}
				want = workspace.ErrForbidden
			}
			if _, err := app.dispatchAction(ctx, "default", call.Name, call.Arguments); !errors.Is(err, want) {
				t.Fatalf("changed boundary accepted: %v, want %v", err, want)
			}
		})
	}
}

func TestChatToolDiagnosticsExcludePrivateErrors(t *testing.T) {
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	for _, err := range []error{nil, connectors.ErrSchemaChanged, workspace.ErrForbidden, connectors.ErrQuery, context.DeadlineExceeded, context.Canceled, errors.New("PRIVATE_QUERY_AND_PROVIDER_BODY")} {
		chatrun.ToolDiagnostic("synthetic-run", "call_data_operation", err)
	}
	for _, category := range []string{"completed", "schema_changed", "access_denied", "query_rejected", "deadline", "cancelled", "operation_failed"} {
		if !strings.Contains(logs.String(), `"category":"`+category+`"`) {
			t.Errorf("missing category %s", category)
		}
	}
	if strings.Contains(logs.String(), "PRIVATE_QUERY_AND_PROVIDER_BODY") {
		t.Fatal("raw error entered operational logs")
	}
}

func TestAssistantStartsWithOnlyPermittedLiveSources(t *testing.T) {
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input struct{ Messages []chat.Message }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		raw, _ := json.Marshal(input.Messages)
		if !strings.Contains(string(raw), "APPROVED_LIVE_SOURCE") {
			t.Error("approved source was not discoverable on the initial request")
		}
		for _, excluded := range []string{"UNGRANTED_SOURCE", "OUTSIDE_SELECTION_SOURCE", "CODE_ONLY_SOURCE"} {
			if strings.Contains(string(raw), excluded) {
				t.Errorf("source discovery exposed %s", excluded)
			}
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Sources checked."},"finish_reason":"stop"}]}`)
	})
	var connections []connectors.Connection
	for _, source := range []struct {
		id, label string
		codeOnly  bool
	}{{"db", "APPROVED_LIVE_SOURCE", false}, {"ungranted", "UNGRANTED_SOURCE", false}, {"unselected", "OUTSIDE_SELECTION_SOURCE", false}, {"code", "CODE_ONLY_SOURCE", true}} {
		connections = append(connections, connectors.Connection{ID: source.id, Label: source.label, Kind: "http", Endpoint: "http://127.0.0.1:1", ExternalProcessing: !source.codeOnly, CodeOnly: source.codeOnly, Operations: []connectors.Operation{{Name: "query", ReadOnly: true, Method: "GET", Path: "/"}}})
		if source.id != "ungranted" && source.id != "db" {
			if _, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: source.id, Users: map[string][]string{"alice": {"query"}}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: connections})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	app.dataConnections = manager
	configureSlackFixture(t, app)
	binding := app.slack.config.Channels["C123"]
	binding.Connections = []string{"db", "ungranted", "code"}
	app.slack.config.Channels["C123"] = binding
	app.messageClients["slack"] = &fixtureChatTransport{}
	run := automation.Run{ID: automation.Hash(t.Name()), Spec: automation.Spec{ID: "source-discovery", Kind: "assistant", Actor: "alice", Project: "default", Connections: binding.Connections, AIConnection: "local", Model: "synthetic", Instruction: "Check the current state.", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", Thread: "discovery", User: "U123"}}}
	if err := app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
