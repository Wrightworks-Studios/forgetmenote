package web

import (
	"encoding/json"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaging"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessagingRecoversNewArtifactReceiptFromCommittedConversation(t *testing.T) {
	app, _ := learningFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("recovery purchased a second answer") })
	configureSlackFixture(t, app)
	b := app.slack.config.Channels["C123"]
	b.Connections = []string{"db"}
	app.slack.config.DirectMessages = map[string]SlackChannel{"U123": b}
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "D123", User: "U123", Direct: true}
	r := automation.Run{ID: automation.Hash("artifact-crash"), GenerationStarted: true, ChatPrepared: true, Spec: automation.Spec{ID: "artifact-crash", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Connections: []string{"db"}, Instruction: "Query fresh data and export it", ExternalProcessing: true, Delivery: &d}}
	old := connectors.Result{Receipt: connectors.Receipt{ID: "old", Connection: "db", Operation: "query", Complete: true}}
	r.ChatResults = []connectors.Result{old} // Last queue checkpoint, before the query.
	fresh := connectors.Result{Receipt: connectors.Receipt{ID: "fresh", Connection: "db", Operation: "query", Complete: true}, Data: map[string]any{"columns": []string{"count"}, "rows": [][]int{{15}}}}
	a, err := messaging.RenderArtifact(fresh.Data, messaging.ArtifactRequest{Receipt: "fresh", Format: "csv"})
	if err != nil {
		t.Fatal(err)
	}
	resultRaw, _ := json.Marshal(fresh)
	artifactRaw, _ := json.Marshal(a)
	owner, binding := messageConversation(app.slack.policy(), b, d, "alice")
	c, err := app.chat.Store.OpenBound(owner, "default", binding, chat.Options{Mode: "ask", Connection: "local", Model: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	// The conversation commit succeeded just before the queue checkpoint failed.
	if err := app.chat.Store.Update(owner, "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), RunID: r.ID, Role: "assistant", Content: "Fresh result attached."})
		c.Runs = append(c.Runs, chat.Run{ID: r.ID, State: "completed", Operations: []chat.Operation{{Name: "chat_result", Result: resultRaw}, {Name: "chat_artifact", Result: artifactRaw}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.runAutomation(t.Context(), &r, func() error { return nil }); err != nil {
		t.Fatal("committed export did not recover", err)
	}
	if len(transport.replies) != 1 || len(transport.replies[0].Artifacts) != 1 || transport.replies[0].Artifacts[0].Receipt != "fresh" {
		t.Fatal("lost source-bound export", transport.replies)
	}
}

func TestMessagingExportsSourceResultsAndReusesThemOnFollowup(t *testing.T) {
	step, queries := 0, 0
	receipt := ""
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		name, args := "", map[string]any{}
		switch step {
		case 0:
			name, args = "call_data_operation", map[string]any{"connection": "db", "operation": "query", "arguments": map[string]any{}, "project": "default"}
		case 1:
			var result dataResult
			if err := json.Unmarshal([]byte(input.Messages[len(input.Messages)-1].Content), &result); err != nil || result.Receipt.ID == "" {
				t.Errorf("no query receipt: %+v %v", input, err)
			}
			receipt = result.Receipt.ID
			name, args = "create_result_artifact", map[string]any{"receipt": receipt, "format": "csv"}
		case 3:
			if !strings.Contains(input.Messages[len(input.Messages)-1].Content, receipt) {
				t.Error("follow-up lost retained receipt")
			}
			name, args = "create_result_artifact", map[string]any{"receipt": receipt, "format": "xlsx"}
		}
		step++
		message := map[string]any{"role": "assistant", "content": "Attached the requested export."}
		if name != "" {
			raw, _ := json.Marshal(args)
			message = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call", "type": "function", "function": map[string]string{"name": name, "arguments": string(raw)}}}}
		} else if strings.Contains(input.Messages[len(input.Messages)-1].Content, `"error"`) {
			t.Error(input.Messages[len(input.Messages)-1].Content)
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": "stop"}}})
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries++
		w.Write([]byte(`{"columns":["region","count"],"rows":[["East",7],["West",9]]}`))
	}))
	defer upstream.Close()
	m, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "db", Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "query", ReadOnly: true, Method: "GET", Path: "/", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = m
	if _, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "db", Users: map[string][]string{"alice": {"query"}}}); err != nil {
		t.Fatal(err)
	}
	transport := &fixtureChatTransport{}
	app.messageClients = map[string]messaging.Transport{"fixture": transport}
	// No per-destination model override: use the configured instance default.
	binding := messaging.Binding{Project: "default", Connections: []string{"db"}, ExternalProcessing: true}
	if _, err := app.catalog.SetChatClients(catalog.Bootstrap, catalog.ChatClients{Clients: map[string]messaging.Client{"fixture": {Workspace: "team", Users: map[string]string{"user": "alice"}, DirectMessages: map[string]messaging.Binding{"user": binding}}}}); err != nil {
		t.Fatal(err)
	}
	if err := app.initAutomations(); err != nil {
		t.Fatal(err)
	}
	var first automation.Run
	for i, prompt := range []string{"Export the query as CSV.", "Now export the same result as a workbook."} {
		event := messaging.Event{ID: prompt, Text: prompt, Address: messaging.Address{Client: "fixture", Workspace: "team", Channel: "dm", User: "user", Direct: true}}
		if err := app.enqueueMessage(t.Context(), event); err != nil {
			t.Fatal(err)
		}
		if _, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil {
			t.Fatal(err)
		}
		if len(transport.replies) != i+1 || len(transport.replies[i].Artifacts) != 1 {
			t.Fatalf("export delivery: %+v", transport.replies)
		}
		if i == 0 {
			runs, _ := app.automations.Queue.List()
			first = runs[0]
		}
	}
	if queries != 1 || step != 5 {
		t.Fatalf("follow-up reran data query: %d queries, %d model calls", queries, step)
	}
	if got := string(transport.replies[0].Artifacts[0].Data); got != "region,count\nEast,7\nWest,9\n" {
		t.Fatalf("CSV differs from source: %q", got)
	}
	if a := transport.replies[1].Artifacts[0]; a.Name != "result.xlsx" || !strings.HasPrefix(string(a.Data), "PK") || a.Receipt != receipt {
		t.Fatal("missing workbook/receipt", a.Name, a.Receipt)
	}
	ctx, err := app.automationContext(t.Context(), first.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"receipt":"forged","format":"csv"}`, `{"receipt":"` + receipt + `","format":"csv","rows":[[999]]}`} {
		if _, err := app.messageArtifact(ctx, &first, json.RawMessage(raw)); err == nil {
			t.Fatal("accepted fabricated export", raw)
		}
	}
	first.ChatResults[0].Receipt.Complete = false
	if _, err := app.messageArtifact(ctx, &first, json.RawMessage(`{"receipt":"`+receipt+`","format":"csv"}`)); err == nil {
		t.Fatal("exported incomplete result")
	}
	first.ChatResults[0].Receipt.Complete = true
	grant, err := app.catalog.ConnectionAccess(catalog.Bootstrap, "db")
	if err != nil {
		t.Fatal(err)
	}
	grant.Users = map[string][]string{}
	if _, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, grant); err != nil {
		t.Fatal(err)
	}
	if _, err := app.messageArtifact(ctx, &first, json.RawMessage(`{"receipt":"`+receipt+`","format":"csv"}`)); err == nil {
		t.Fatal("export survived source permission revocation")
	}
}

func TestMessageAssistantOffersResultBoundArtifacts(t *testing.T) {
	found := false
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct{ Function struct{ Name string } }
		}
		json.NewDecoder(r.Body).Decode(&body)
		for _, tool := range body.Tools {
			if tool.Function.Name == "create_result_artifact" {
				found = true
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Ready"},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	app.slack.config.DirectMessages = map[string]SlackChannel{"U123": app.slack.config.Channels["C123"]}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true}`)) }))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL + "/chat.postEphemeral"
	run := automation.Run{ID: automation.Hash("artifact-tools"), Spec: automation.Spec{ID: "artifact-tools", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Instruction: "Make a chart from the result", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "D123", SlackUser: "U123", Direct: true}}}
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("chat assistant cannot create artifacts from query receipts")
	}
}
