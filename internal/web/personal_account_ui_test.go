package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpToolError returns the caller-visible refusal, or "" when the call succeeded.
func mcpToolError(t *testing.T, client *mcp.ClientSession, name string, args any) string {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if !result.IsError {
		return ""
	}
	raw, _ := json.Marshal(result.Content)
	return string(raw)
}

// External personal writes and paid recovery are approvals in the signed-in
// account page. An MCP grant holding every data and note scope is not that.
func TestPersonalAccountActionsRefuseMCPGrants(t *testing.T) {
	app := chatFixture(t)
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, MaintainMemory: true, TimeZone: "UTC", Features: []string{"email"}})
	if err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	connectPersonalFixture(t, app, "alice", func(r *http.Request) string {
		switch r.URL.Path {
		case "/token":
			return `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"scope":"openid email https://www.googleapis.com/auth/gmail.modify"}`
		case "/v1/userinfo":
			return `{"sub":"google-alice","email":"alice@example.test","email_verified":true}`
		case "/gmail/v1/users/me/profile":
			return `{"emailAddress":"alice@example.test","historyId":"100"}`
		case "/gmail/v1/users/me/drafts":
			writes.Add(1)
			return `{"id":"d1","message":{"id":"md1","threadId":"t1"}}`
		}
		if r.Method != "GET" {
			writes.Add(1)
		}
		return `{}`
	})
	account, err := app.catalog.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	connections, err := app.personalConnections.List(account.ID)
	if err != nil || len(connections) != 1 {
		t.Fatalf("fixture connection missing: %+v", connections)
	}
	spec, err := app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	memory, _ := json.Marshal(personalMemoryState{Status: "started", Batch: []personalInteraction{{ID: "one", UserText: "Keep it concise"}}})
	memoryState, err := app.automations.Queue.SaveJobState(personalMemorySpec(spec), "", memory, false)
	if err != nil {
		t.Fatal(err)
	}
	drafts, _ := json.Marshal(personalState{Drafts: map[string]personalDraft{"ready": {ID: "ready", State: "ready", Path: "drafts/missing.md", Reply: "Proposed reply", Source: personalObservation{Kind: "email"}}}})
	draftState, err := app.automations.Queue.SaveJobState(spec, "", drafts, false)
	if err != nil {
		t.Fatal(err)
	}

	client := actionClient(t, app, "alice", "notes:read notes:write data:read data:write")
	submission := map[string]any{"actionID": "mcp-draft", "call": map[string]any{"provider": "google", "generation": connections[0].Generation, "operation": "email.draft-create", "arguments": map[string]any{"to": []string{"colleague@example.test"}, "subject": "Launch plan", "text": "Synthetic reply"}}}
	for _, call := range []struct {
		name string
		args any
	}{
		{"submit_personal_action", submission},
		{"prepare_assistant_draft", map[string]any{"id": "ready", "revision": draftState.Revision}},
		{"recover_assistant_work", map[string]any{"id": "memory", "revision": memoryState.Revision, "retry": true}},
	} {
		if text := mcpToolError(t, client, call.name, call.args); !strings.Contains(text, workspace.ErrForbidden.Error()) {
			t.Errorf("%s was not refused as an account-UI action: %q", call.name, text)
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("MCP grant reached the provider with %d writes", writes.Load())
	}
	if current, err := app.automations.Queue.JobState(personalMemorySpec(spec)); err != nil || current.Revision != memoryState.Revision {
		t.Fatalf("MCP grant changed interrupted memory work: %v", err)
	}

	raw, _ := json.Marshal(submission)
	if w := proxyCall(app, "alice", "POST", "/api/assistant/action/submit_personal_action", string(raw)); w.Code != 200 || writes.Load() != 1 {
		t.Fatalf("account UI submission: %d %s writes=%d", w.Code, w.Body.String(), writes.Load())
	}
	raw, _ = json.Marshal(map[string]any{"id": "memory", "revision": memoryState.Revision, "retry": true})
	if w := proxyCall(app, "alice", "POST", "/api/assistant/action/recover_assistant_work", string(raw)); w.Code != 200 {
		t.Fatalf("account UI recovery: %d %s", w.Code, w.Body.String())
	}
}

// Account-page-only actions always refuse MCP and API callers, so discovery
// does not offer them to agents.
func TestClientDiscoveryOmitsAccountUIActions(t *testing.T) {
	app := chatFixture(t)
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	client := actionClient(t, app, "alice", "notes:read notes:write notes:manage data:read data:write")
	tools, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, tool := range tools.Tools {
		listed[tool.Name] = true
	}
	out := apiCall(app, "GET", "/api/v1/actions", ``, testToken)
	var discovered []struct{ Name string }
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &discovered) != nil {
		t.Fatalf("API discovery: %d %s", out.Code, out.Body.String())
	}
	for _, action := range discovered {
		listed["api:"+action.Name] = true
	}
	if !listed["assistant_activity"] || !listed["api:assistant_activity"] {
		t.Fatal("fixture discovery omitted an ordinary assistant action")
	}
	for _, name := range []string{"submit_personal_action", "prepare_assistant_draft", "recover_assistant_work"} {
		if listed[name] || listed["api:"+name] {
			t.Errorf("discovery offers account-UI action %s", name)
		}
	}
}
