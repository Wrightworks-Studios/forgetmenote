package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/workspace"
)

func TestDataToolSchemaLeavesArgumentCapacityToConnection(t *testing.T) {
	app := chatFixture(t)
	tool, ok := app.actionTool("call_data_operation")
	if !ok {
		t.Fatal("data tool is unavailable")
	}
	var schema struct {
		Properties map[string]struct {
			MaxLength *int `json:"maxLength"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["arguments"].MaxLength != nil {
		t.Fatal("model tool schema still imposes a fixed argument cutoff")
	}
}

func TestAssistantDiscoversRecipesOnDemand(t *testing.T) {
	calls := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input struct{ Messages []chat.Message }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if calls == 1 {
			raw, _ := json.Marshal(input.Messages)
			for _, excluded := range []string{"learned-revenue.md", "Marketplace revenue", "ARCHIVED_RECIPE", "UNAPPROVED_RECIPE", "FULL_RECIPE_BODY"} {
				if strings.Contains(string(raw), excluded) {
					t.Errorf("unsolicited recipe entered the initial request: %s", excluded)
				}
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"discover","type":"function","function":{"name":"find_recipes","arguments":"{\"project\":\"profiles\",\"query\":\"revenue\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if calls == 2 {
			raw := input.Messages[len(input.Messages)-1].Content
			for _, want := range []string{"learned-revenue.md", "Marketplace revenue"} {
				if !strings.Contains(raw, want) {
					t.Errorf("requested recipe was not discoverable: %s", want)
				}
			}
			for _, excluded := range []string{"ARCHIVED_RECIPE", "UNAPPROVED_RECIPE", "FULL_RECIPE_BODY"} {
				if strings.Contains(raw, excluded) {
					t.Errorf("recipe discovery exposed %s", excluded)
				}
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"recipe","type":"function","function":{"name":"read_document","arguments":"{\"project\":\"profiles\",\"origin\":\"notes\",\"path\":\"learned-revenue.md\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if !strings.Contains(input.Messages[len(input.Messages)-1].Content, "FULL_RECIPE_BODY") {
			t.Error("recipe original was not readable after discovery")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Used the recipe."},"finish_reason":"stop"}]}`)
	})
	for _, project := range []string{"profiles", "private"} {
		if err := app.catalog.Create(catalog.Bootstrap, project, project); err != nil {
			t.Fatal(err)
		}
		if err := app.catalog.Member(catalog.Bootstrap, project, "alice", "read"); err != nil {
			t.Fatal(err)
		}
		store, _ := app.catalog.Access(catalog.Bootstrap, project, "edit")
		body := "---\ntags: [recipe]\n---\n# UNAPPROVED_RECIPE"
		if project == "profiles" {
			body = "---\ntags: [recipe]\n---\n# Marketplace revenue\n" + strings.Repeat("Concise query guidance. ", 30) + "FULL_RECIPE_BODY"
		}
		if _, err := store.Save(t.Context(), "learned-revenue.md", []byte(body), "", "test"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Save(t.Context(), "old.md", []byte("---\ntags: [recipe]\nfmn-container: archive\n---\n# ARCHIVED_RECIPE"), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	configureSlackFixture(t, app)
	b := app.slack.config.Channels["C123"]
	b.Connections, b.ReadProjects = []string{"db"}, []string{"profiles"}
	app.slack.config.Channels["C123"] = b
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	r := automation.Run{ID: automation.Hash("recipe-index"), Spec: automation.Spec{ID: "recipe-index", Kind: "assistant", Actor: "alice", Project: "default", ReadProjects: b.ReadProjects, Connections: b.Connections, AIConnection: "local", Model: "synthetic", Instruction: "How was revenue this week?", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", Thread: "recipe-thread", User: "U123"}}}
	if err := app.runAutomation(t.Context(), &r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(transport.replies) != 1 || len(r.Citations) != 1 || r.Citations[0].Path != "learned-revenue.md" {
		t.Fatal("recipe discovery did not lead to a provenance-bound read", calls, r.Citations)
	}
	ctx, err := app.automationContext(t.Context(), r.Spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = app.automationSourceContext(ctx, &r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.dispatchAction(ctx, "default", "find_recipes", json.RawMessage(`{"project":"private"}`)); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("recipe discovery escaped the approved notebook selection", err)
	}
}

func TestModelContextPreservesWholeReadThatFits(t *testing.T) {
	content := strings.Repeat("useful evidence ", 1500) + "END_OF_ORIGINAL"
	raw, _ := json.Marshal(chatSourceContext{Kind: "note_read", Sources: []chat.Citation{{ID: "source", Path: "note.md", Text: content}}})
	req := chat.Request{MaxOutputTokens: 2048, Messages: []chat.Message{{Role: "user", Content: "Read the whole note"}, {Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "read", Name: "read_note", Arguments: json.RawMessage(`{}`)}}}, {Role: "tool", ToolCallID: "read", Content: string(raw)}}}
	fitted, _, err := fitChatRequest(chat.Run{}, req, 100000)
	if err != nil || !strings.Contains(fitted.Messages[2].Content, "END_OF_ORIGINAL") {
		t.Fatalf("original that fits was clipped: %v", err)
	}
}

func TestModelDocumentReadKeepsHealthAndRangeMetadata(t *testing.T) {
	content := strings.Repeat("original Markdown ", 1000) + "END_OF_ORIGINAL"
	doc := documentOutput{documentRef: documentRef{Project: "default", Origin: "notes", Path: "recipe.md", Revision: "revision"}, Content: content, HTML: "<p>Rendered duplicate</p>", Hash: "hash", Citation: "/?note=recipe.md", Health: &recipeCheckHealth{State: "waiting_upstream"}, StartLine: 20, EndLine: 40, Lines: 80, Cursor: "next", More: true}
	raw, _ := json.Marshal(doc)
	result := chatToolResultContext("read_document", raw, []chat.Citation{{Path: doc.Path, Text: "short excerpt"}})
	var got documentOutput
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatal(err)
	}
	if got.Content != content || got.HTML != "" || got.Health == nil || got.Health.State != "waiting_upstream" || got.Citation != doc.Citation || got.Hash != doc.Hash || got.Revision != doc.Revision {
		t.Fatal("complete original, health or provenance lost from model context")
	}
	if got.StartLine != 20 || got.EndLine != 40 || got.Lines != 80 || got.Cursor != "next" || !got.More {
		t.Fatal("read continuation lost from model context")
	}
}

func TestChatAttachmentsUseConfiguredResourceBudget(t *testing.T) {
	app := chatFixture(t)
	store, err := app.catalog.Access("alice", "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	var input []chat.Attachment
	for i := 0; i < 5; i++ {
		input = append(input, chat.Attachment{Kind: "private", Path: fmt.Sprintf("draft%d.md", i), Content: strings.Repeat("x", 20000)})
	}
	if got, err := app.chatAttachments(t.Context(), store, input); err != nil || len(got) != 5 {
		t.Fatalf("valid attachments rejected: %v", err)
	}
	t.Setenv("FMN_LIMIT_SOURCE_TEXT_BYTES", "1024")
	if _, err := app.chatAttachments(t.Context(), store, input); err == nil || !strings.Contains(err.Error(), "FMN_LIMIT_SOURCE_TEXT_BYTES") {
		t.Fatalf("attachment capacity was not enforced: %v", err)
	}
}

func TestDataContextOverflowKeepsReceiptAndOriginal(t *testing.T) {
	raw, _ := json.Marshal(dataResult{Result: connectors.Result{Receipt: connectors.Receipt{ID: "original-receipt", Complete: true}, Data: strings.Repeat("data ", 10000)}})
	req := chat.Request{MaxOutputTokens: 100, Messages: []chat.Message{{Role: "user", Content: "Analyze the result"}, {Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "read", Name: "call_data_operation", Arguments: json.RawMessage(`{}`)}}}, {Role: "tool", ToolCallID: "read", Content: string(raw)}}}
	fitted, _, err := fitChatRequest(chat.Run{}, req, 4000)
	if err != nil || len(fitted.Messages) != 3 || fitted.Messages[2].ToolCallID != "read" {
		t.Fatalf("tool pairing lost: %v", err)
	}
	var value struct {
		ContextOmitted bool               `json:"contextOmitted"`
		Receipt        connectors.Receipt `json:"receipt"`
	}
	if json.Unmarshal([]byte(fitted.Messages[2].Content), &value) != nil || !value.ContextOmitted || value.Receipt.ID != "original-receipt" || !value.Receipt.Complete {
		t.Fatal("missing honest omission or complete export receipt", fitted.Messages[2].Content)
	}
	if req.Messages[2].Content != string(raw) {
		t.Fatal("durable original was modified")
	}
}

func TestAssistantUsesAvailableContextForLargeToolResults(t *testing.T) {
	for _, count := range []int{1, 9} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			calls, results := 0, 0
			app, _ := runnerFixture(t, func(w http.ResponseWriter, req *http.Request) {
				calls++
				var body struct{ Messages []chat.Message }
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if calls == 1 {
					tools := []any{}
					for i := 0; i < count; i++ {
						tools = append(tools, map[string]any{"id": fmt.Sprint(i), "type": "function", "function": map[string]string{"name": "call_data_operation", "arguments": `{"connection":"fixture","operation":"read","arguments":{}}`}})
					}
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": tools}, "finish_reason": "tool_calls"}}})
					return
				}
				for _, m := range body.Messages {
					if m.Role == "tool" && strings.Contains(m.Content, "LAST_RESULT_VALUE") {
						results++
					}
				}
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Completed using the original results."},"finish_reason":"stop"}]}`)
			})
			connection := app.chat.Config.Connections[0]
			connection.Limits.ContextTokens = 1_000_000
			setChatConnection(t, app, connection)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"value": strings.Repeat("x", 150_000) + "LAST_RESULT_VALUE"})
			}))
			t.Cleanup(upstream.Close)
			manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "fixture", Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "read", ReadOnly: true, Method: "GET", Path: "/", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}}}}})
			if err != nil {
				t.Fatal(err)
			}
			app.dataConnections = manager
			if _, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "fixture", Users: map[string][]string{"alice": {"read"}}}); err != nil {
				t.Fatal(err)
			}
			configureSlackFixture(t, app)
			binding := app.slack.config.Channels["C123"]
			binding.Connections = []string{"fixture"}
			app.slack.config.Channels["C123"] = binding
			transport := &fixtureChatTransport{}
			app.messageClients["slack"] = transport
			run := automation.Run{ID: automation.Hash(count), Spec: automation.Spec{ID: "large-answer", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Connections: []string{"fixture"}, ExternalProcessing: true, Instruction: "Read the data.", Delivery: &automation.Delivery{Channel: "C123", Thread: "thread", User: "U123"}}}
			if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || results != count || len(run.Receipts) != count || len(transport.replies) != 1 {
				t.Fatalf("calls=%d results=%d receipts=%d replies=%d", calls, results, len(run.Receipts), len(transport.replies))
			}
		})
	}
}

func TestChatKeepsOriginalDataUntilContextFitting(t *testing.T) {
	raw, _ := json.Marshal(dataResult{Result: connectors.Result{Data: map[string]string{"value": strings.Repeat("x", 15000) + "LAST_RESULT_VALUE"}}})
	result := chatToolResultContext("call_data_operation", raw, nil)
	if !strings.Contains(result, "LAST_RESULT_VALUE") || !json.Valid([]byte(result)) {
		t.Fatal("valid tool data discarded before model context fitting")
	}
}

func TestAutomationReadsOriginalMarkdownWithoutRenderedDuplicate(t *testing.T) {
	content := "# Metric definition\n\n" + strings.Repeat("Revenue preserves the requested population and time boundaries.\n", 400) + "END_OF_DEFINITION"
	calls := 0
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Messages []struct {
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if calls == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"definition","type":"function","function":{"name":"read_document","arguments":"{\"project\":\"default\",\"path\":\"definition.md\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		var document documentOutput
		last := request.Messages[len(request.Messages)-1]
		if err := json.Unmarshal([]byte(last.Content), &document); err != nil || last.ToolCallID != "definition" {
			t.Errorf("read result is not paired document evidence: %v", err)
		}
		if document.Content != content || document.HTML != "" || document.Revision == "" || document.Hash == "" || document.Citation == "" {
			t.Error("model must receive complete Markdown and provenance without duplicate HTML")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Definition read."},"finish_reason":"stop"}]}`)
	})
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if _, err := store.Save(t.Context(), "definition.md", []byte(content), "", "alice"); err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "read-definition", Kind: "task", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true}
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	run := automation.Run{Spec: spec}
	answer, err := app.automationAnswer(ctx, &run, "Read the definition.", true)
	if err != nil || answer != "Definition read." || calls != 2 || len(run.Citations) != 1 || run.Citations[0].Path != "definition.md" {
		t.Fatal("read-backed answer lost its source dependency", answer, err, calls, run.Citations)
	}

	// The public document API still serves the rendered form used by the UI.
	client := actionClient(t, app, "alice", "notes:read")
	document := actionCall(t, client, "read_document", map[string]any{"project": "default", "path": "definition.md"}, false)
	raw, _ := json.Marshal(document)
	if !strings.Contains(string(raw), `"html":`) {
		t.Fatal("model compaction changed the document API")
	}
}
