package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/workspace"
	"forgetmenote/plugins"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type recalledMemory struct {
	Notebook   string `json:"notebook"`
	Path       string `json:"path"`
	Revision   string `json:"revision"`
	Restricted bool   `json:"restricted"`
	Items      []struct {
		ID       string `json:"id"`
		Text     string `json:"text"`
		Kind     string `json:"kind"`
		Topic    string `json:"topic"`
		From     string `json:"from"`
		Anchor   string `json:"anchor"`
		Checked  string `json:"checked"`
		Recheck  string `json:"recheck"`
		Pinned   bool   `json:"pinned"`
		Stale    bool   `json:"stale"`
		Disputed bool   `json:"disputed"`
		Rank     int    `json:"rank"`
	} `json:"items"`
	Changed []struct {
		Old    string `json:"old"`
		New    string `json:"new"`
		ID     string `json:"id"`
		Reason string `json:"reason"`
	} `json:"changed"`
}

func (m recalledMemory) byText() map[string]int {
	out := map[string]int{}
	for i, item := range m.Items {
		out[item.Text] = i
	}
	return out
}

type rememberResult struct {
	Notebook string `json:"notebook"`
	Path     string `json:"path"`
	Revision string `json:"revision"`
	Items    []struct {
		ID        string `json:"id"`
		Duplicate bool   `json:"duplicate"`
		Replaced  string `json:"replaced"`
		Disputes  string `json:"disputes"`
		Similar   string `json:"similar"`
	} `json:"items"`
	Review       string `json:"review"`
	ReviewReason string `json:"reviewReason"`
}

type confirmResult struct {
	Outcome string `json:"outcome"`
	ID      string `json:"id"`
}

func recallCore(t *testing.T, session *mcp.ClientSession) recalledMemory {
	t.Helper()
	var out struct {
		Core recalledMemory `json:"core"`
	}
	if failure := callMemoryTool(t, session, "recall_memory", map[string]any{}, &out); failure != "" {
		t.Fatal("recall:", failure)
	}
	return out.Core
}

// An MCP client connected as one employee whose assistant notebook exists.
func agentMemoryFixture(t *testing.T, maintain bool) (*Server, *mcp.ClientSession, *workspace.Store) {
	t.Helper()
	app, id, session := agentMemoryApp(t)
	if _, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings, err := app.catalog.SaveAssistant(id, catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"email"}, MaintainMemory: maintain})
	if err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access(id, settings.Project, "owner")
	if err != nil {
		t.Fatal(err)
	}
	return app, session, store
}

// An MCP client connected over OAuth as one employee who never set up the assistant.
func agentMemoryApp(t *testing.T) (*Server, string, *mcp.ClientSession) {
	t.Helper()
	app, _ := fixture(t)
	id := addLocal(t, app, recoverySession(t, app), "memory-owner")
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	app.origin = ts.URL
	app.oauth.Access[digest("memory-token")] = withOAuthFamily(accessGrant{Subject: id, Client: "memory-fixture", Scope: "notes:read notes:write", Resource: ts.URL + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	return app, id, memorySession(t, app, "memory-token")
}

func memorySession(t *testing.T, app *Server, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "memory-fixture", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: app.origin + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func callMemoryTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if result.IsError {
		text := ""
		for _, c := range result.Content {
			if v, ok := c.(*mcp.TextContent); ok {
				text += v.Text
			}
		}
		return text
	}
	raw, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s result: %v %s", name, err, raw)
	}
	return ""
}

// Connected agents learn from the server's instructions whether to keep memory
// without being asked. Memory is on unless the account turned it off, and each
// connection reads its own account's setting under either protocol.
func TestConnectedAgentsAreToldWhetherToKeepMemory(t *testing.T) {
	app, id, _ := agentMemoryApp(t)
	other := addLocal(t, app, recoverySession(t, app), "memory-colleague")
	app.oauth.Access[digest("colleague-token")] = withOAuthFamily(accessGrant{Subject: other, Client: "memory-fixture", Scope: "notes:read notes:write", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	instructions := func(token string, legacy bool) string {
		t.Helper()
		client := mcp.NewClient(&mcp.Implementation{Name: "memory-fixture", Version: "1"}, nil)
		if legacy {
			client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
					if method == "server/discover" {
						return nil, &jsonrpc.Error{Code: -32601, Message: "legacy client fixture"}
					}
					return next(ctx, method, r)
				}
			})
		}
		session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: app.origin + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: token}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		init := session.InitializeResult()
		if legacy != (init.ProtocolVersion < "2026-07-28") {
			t.Fatal("wrong protocol fixture", init.ProtocolVersion)
		}
		if !strings.Contains(init.Instructions, plugins.Instructions()) {
			t.Fatal("the canonical workflow is missing")
		}
		return init.Instructions
	}
	const on, off = "Memory is on for this account", "Memory is off for this account"
	for _, legacy := range []bool{true, false} {
		if got := instructions("memory-token", legacy); !strings.Contains(got, on) || !strings.Contains(got, "recall_memory") || strings.Contains(got, off) {
			t.Fatalf("legacy=%v: an account that never chose was not told memory is on:\n%s", legacy, got)
		}
		// Agents search memory before work instead of loading all of it, and keep
		// how the user retrieves and analyzes data without asking each time.
		if got := instructions("memory-token", legacy); !strings.Contains(got, "`query`") || !strings.Contains(got, "how they like data retrieved and analyzed") || strings.Contains(got, "ask once whether") || !strings.Contains(got, "Tidy as you go") || !strings.Contains(got, "`similar`") {
			t.Fatalf("legacy=%v: memory instructions do not ask agents to search and keep usage preferences:\n%s", legacy, got)
		}
	}

	settings, err := app.catalog.Assistant(id)
	if err != nil {
		t.Fatal(err)
	}
	settings.MaintainMemory = false
	if _, err = app.catalog.SaveAssistant(id, settings); err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []bool{true, false} {
		if got := instructions("memory-token", legacy); !strings.Contains(got, off) || !strings.Contains(got, app.origin+"/account/assistant") || strings.Contains(got, on) {
			t.Fatalf("legacy=%v: an account that turned memory off was told to keep it:\n%s", legacy, got)
		}
		if got := instructions("colleague-token", legacy); !strings.Contains(got, on) {
			t.Fatalf("legacy=%v: one account's setting reached another:\n%s", legacy, got)
		}
	}

	// Discovery results are cacheable; a per-account result must not be shared.
	request, err := http.NewRequestWithContext(t.Context(), "POST", app.origin+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"memory-fixture","version":"1"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "server/discover")
	request.Header.Set("Authorization", "Bearer memory-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var discovered struct {
		Result struct {
			Instructions string `json:"instructions"`
			CacheScope   string `json:"cacheScope"`
		} `json:"result"`
	}
	if err = json.NewDecoder(response.Body).Decode(&discovered); err != nil || !strings.Contains(discovered.Result.Instructions, off) || discovered.Result.CacheScope != "private" {
		t.Fatalf("discovery %d: %+v %v", response.StatusCode, discovered.Result.CacheScope, err)
	}
}

// Recall returns every current item: a large memory keeps all of its sections.
func TestAgentMemoryRecallReturnsEveryItem(t *testing.T) {
	_, session, _ := agentMemoryFixture(t, true)
	for _, kind := range []string{"fact", "decision", "lesson"} {
		items := []map[string]any{}
		for i := range 20 {
			items = append(items, map[string]any{"kind": kind, "text": fmt.Sprintf("Synthetic %s %02d: %s", kind, i, strings.Repeat("detail ", 50))})
		}
		var out rememberResult
		if failure := callMemoryTool(t, session, "remember", map[string]any{"items": items}, &out); failure != "" {
			t.Fatal(kind, failure)
		}
	}
	core := recallCore(t, session)
	kinds := map[string]int{}
	for _, item := range core.Items {
		kinds[item.Kind]++
	}
	if len(core.Items) != 60 || kinds["fact"] != 20 || kinds["decision"] != 20 || kinds["lesson"] != 20 {
		t.Fatalf("recall returned %d items: %v", len(core.Items), kinds)
	}
}

func TestAgentMemoryRememberedItemsComeBackWithIDAndSource(t *testing.T) {
	app, session, store := agentMemoryFixture(t, false)
	var saved rememberResult
	if failure := callMemoryTool(t, session, "remember", map[string]any{"userDirected": true, "items": []map[string]any{{"text": "Deploy production with --compatibility compatible when no stored format changes.", "kind": "decision", "topic": "Deployment", "anchor": "deploy/production/deploy_instance.py@87efcb4"}}}, &saved); failure != "" {
		t.Fatal("remember:", failure)
	}
	if len(saved.Items) != 1 || !regexp.MustCompile(`^m[0-9a-f]{7}$`).MatchString(saved.Items[0].ID) || saved.Path != "memory.md" || saved.Revision == "" {
		t.Fatalf("remember result: %+v", saved)
	}
	id := saved.Items[0].ID
	var recalled struct {
		Core recalledMemory `json:"core"`
	}
	if failure := callMemoryTool(t, session, "recall_memory", map[string]any{}, &recalled); failure != "" {
		t.Fatal("recall:", failure)
	}
	if recalled.Core.Path != "memory.md" || recalled.Core.Revision != saved.Revision || len(recalled.Core.Items) != 1 {
		t.Fatalf("recall: %+v", recalled)
	}
	item := recalled.Core.Items[0]
	if item.ID != id || item.Kind != "decision" || item.Topic != "Deployment" || item.From != "you" || !item.Pinned || item.Anchor != "deploy/production/deploy_instance.py@87efcb4" || !strings.HasPrefix(item.Text, "Deploy production") {
		t.Fatalf("recalled item: %+v", item)
	}
	doc, err := store.Read(t.Context(), "memory.md", "")
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC()
	meta := fmt.Sprintf("  `%s · decision · from: you, %s · checked: %[2]s · recheck: %s · anchor: deploy/production/deploy_instance.py@87efcb4`\n", id, today.Format(time.DateOnly), today.AddDate(0, 6, 0).Format("2006-01"))
	if !strings.Contains(string(doc.Content), "## Deployment\n- Deploy production with --compatibility compatible when no stored format changes.\n"+meta) || doc.Revision != saved.Revision {
		t.Fatalf("committed memory:\n%s", doc.Content)
	}
	if got := gitIdentity(t, app, store); !strings.Contains(got[4], "Gitnotes-Source: mcp") {
		t.Fatal("memory commit lost MCP attribution", got)
	}

	var again rememberResult
	callMemoryTool(t, session, "remember", map[string]any{"userDirected": true, "items": []map[string]any{{"text": "Deploy production with --compatibility compatible when no stored format changes.", "kind": "decision"}}}, &again)
	if len(again.Items) != 1 || again.Items[0].ID != id || !again.Items[0].Duplicate || again.Revision != saved.Revision {
		t.Fatalf("duplicate remember: %+v", again)
	}

	var project rememberResult
	if failure := callMemoryTool(t, session, "remember", map[string]any{"userDirected": true, "projectKey": "git@github.com:Pleish/ForgetMeNote.git", "items": []map[string]any{{"text": "Run npm run build after every UI edit.", "kind": "lesson"}}}, &project); failure != "" {
		t.Fatal("project remember:", failure)
	}
	var both struct {
		Core    recalledMemory  `json:"core"`
		Project *recalledMemory `json:"project"`
	}
	callMemoryTool(t, session, "recall_memory", map[string]any{"projectKey": "https://github.com/pleish/forgetmenote"}, &both)
	if project.Path != "memory.md" || !strings.HasPrefix(project.Notebook, saved.Notebook+"-") || both.Project == nil || both.Project.Notebook != project.Notebook || len(both.Project.Items) != 1 || both.Project.Items[0].Text != "Run npm run build after every UI edit." || len(both.Core.Items) != 1 || both.Core.Notebook != saved.Notebook {
		t.Fatalf("project memory: %+v %+v", project, both)
	}
}

func TestAgentMemoryWorksWithoutTheAssistant(t *testing.T) {
	app, id, session := agentMemoryApp(t)
	type recall struct {
		Core    recalledMemory  `json:"core"`
		Project *recalledMemory `json:"project"`
	}
	var empty recall
	if failure := callMemoryTool(t, session, "recall_memory", map[string]any{"projectKey": "billing"}, &empty); failure != "" || empty.Core.Notebook != "" || len(empty.Core.Items) != 0 || empty.Project == nil || len(empty.Project.Items) != 0 {
		t.Fatalf("recall before any memory: %+v %s", empty, failure)
	}
	// Memory is on until the account turns it off, so an observation is kept.
	var out rememberResult
	if failure := callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": "Staging runs kernel 6.12.", "kind": "fact"}}}, &out); failure != "" {
		t.Fatal("remember by default:", failure)
	}
	if failure := callMemoryTool(t, session, "remember", map[string]any{"userDirected": true, "items": []map[string]any{{"text": "Invoices go out on the first business day.", "kind": "fact"}}}, &out); failure != "" {
		t.Fatal("remember:", failure)
	}
	account, _ := app.catalog.Account(id)
	settings, _ := app.catalog.Assistant(id)
	if out.Notebook != "assistant-"+account.ID || settings.Project != out.Notebook || !settings.MaintainMemory || settings.Enabled || settings.ExternalProcessing || app.catalog.AssistantPolicy().Enabled {
		t.Fatalf("core memory %q enabled the assistant or lost the memory default: %+v", out.Notebook, settings)
	}
	var activity struct {
		Drafts []any `json:"drafts"`
	}
	if failure := callMemoryTool(t, session, "assistant_activity", map[string]any{}, &activity); failure != "" || activity.Drafts == nil {
		t.Fatalf("the account page cannot load memory without the assistant: %+v %s", activity, failure)
	}
	var project rememberResult
	if failure := callMemoryTool(t, session, "remember", map[string]any{"userDirected": true, "projectKey": "git@github.com:Pleish/ForgetMeNote.git", "items": []map[string]any{{"text": "Run npm run build after every UI edit.", "kind": "lesson"}}}, &project); failure != "" || !strings.HasPrefix(project.Notebook, out.Notebook+"-") {
		t.Fatalf("project remember %+v: %s", project, failure)
	}

	issue := func(notebook string) *mcp.ClientSession {
		grant, err := issueCredential(app.catalog, app.oauth, id, CredentialSpec{Name: "memory", Projects: []string{notebook}, Scopes: []string{"notes:read", "notes:write"}, Audiences: []string{"mcp"}, Days: 1})
		if err != nil {
			t.Fatal(err)
		}
		return memorySession(t, app, grant.Token)
	}
	coreOnly := issue(out.Notebook)
	if failure := callMemoryTool(t, coreOnly, "remember", map[string]any{"userDirected": true, "projectKey": "billing", "items": []map[string]any{{"text": "Billing runs on the first.", "kind": "fact"}}}, &out); failure == "" {
		t.Fatal("a notebook-limited credential created project memory")
	}
	if notebook, _ := app.catalog.ProjectMemory(id, "billing", false); notebook != "" {
		t.Fatal("refused write provisioned", notebook)
	}
	projectOnly := issue(project.Notebook)
	var limited recall
	if failure := callMemoryTool(t, projectOnly, "recall_memory", map[string]any{"projectKey": "https://github.com/pleish/forgetmenote"}, &limited); failure != "" || !limited.Core.Restricted || len(limited.Core.Items) != 0 || limited.Project == nil || limited.Project.Restricted || len(limited.Project.Items) != 1 {
		t.Fatalf("project-limited recall %+v: %s", limited, failure)
	}
	if failure := callMemoryTool(t, projectOnly, "remember", map[string]any{"userDirected": true, "items": []map[string]any{{"text": "Core item.", "kind": "fact"}}}, &out); failure == "" {
		t.Fatal("a project-limited credential wrote core memory")
	}
}

// An agent asked about memory finds it apart from the personal assistant, whose
// switches are both off here. Memory is on before anything is remembered, the
// first item names its notebook, and the assistant's own tools, whose files the
// assistant calls memory, point to the memory tools.
func TestAssistantStatusReportsMemoryApartFromTheAssistant(t *testing.T) {
	_, _, session := agentMemoryApp(t)
	type status struct {
		Available bool `json:"available"`
		Settings  struct {
			Enabled bool `json:"enabled"`
		} `json:"settings"`
		Memory *struct {
			On       bool   `json:"on"`
			Notebook string `json:"notebook"`
		} `json:"memory"`
	}
	var before status
	if failure := callMemoryTool(t, session, "assistant_status", map[string]any{}, &before); failure != "" || before.Available || before.Settings.Enabled || before.Memory == nil || !before.Memory.On || before.Memory.Notebook != "" {
		t.Fatalf("status before any memory: %+v %s", before, failure)
	}
	var capabilities struct {
		Memory struct {
			RecallTool   string `json:"recallTool"`
			RememberTool string `json:"rememberTool"`
		} `json:"memory"`
	}
	if failure := callMemoryTool(t, session, "capabilities", map[string]any{}, &capabilities); failure != "" || capabilities.Memory.RecallTool != "recall_memory" || capabilities.Memory.RememberTool != "remember" {
		t.Fatalf("capabilities do not point to memory: %+v %s", capabilities, failure)
	}
	var saved rememberResult
	if failure := callMemoryTool(t, session, "remember", map[string]any{"userDirected": true, "items": []map[string]any{{"text": "Revenue comparisons use complete calendar months.", "kind": "preference"}}}, &saved); failure != "" {
		t.Fatal("remember:", failure)
	}
	var after status
	if failure := callMemoryTool(t, session, "assistant_status", map[string]any{}, &after); failure != "" || after.Available || after.Settings.Enabled || after.Memory == nil || !after.Memory.On || after.Memory.Notebook != saved.Notebook {
		t.Fatalf("status after remembering in %q: %+v %s", saved.Notebook, after, failure)
	}
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pointed := 0
	for _, tool := range tools.Tools {
		switch tool.Name {
		case "assistant_status", "assistant_context", "update_assistant_memory":
			if !strings.Contains(tool.Description, "recall_memory") || !strings.Contains(tool.Description, "remember") {
				t.Errorf("%s does not point to the memory tools: %s", tool.Name, tool.Description)
			}
			pointed++
		}
	}
	if pointed != 3 {
		t.Fatalf("listed %d of the assistant's three tools", pointed)
	}
}

func TestAgentMemoryRefusesUnconsentedAndSecretWrites(t *testing.T) {
	_, session, store := agentMemoryFixture(t, false)
	var out rememberResult
	if failure := callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": "Staging runs kernel 6.12.", "kind": "fact"}}}, &out); failure == "" {
		t.Fatal("agent wrote memory without maintenance consent")
	}

	_, session, store = agentMemoryFixture(t, true)
	for name, args := range map[string]map[string]any{
		"credential":        {"userDirected": true, "items": []map[string]any{{"text": "Use token ghp_" + strings.Repeat("a1B2", 9) + " for the API.", "kind": "fact"}}},
		"credential anchor": {"userDirected": true, "items": []map[string]any{{"text": "The deploy key lives in the vault.", "kind": "fact", "anchor": "-----BEGIN OPENSSH PRIVATE KEY-----"}}},
		"multi-line item":   {"userDirected": true, "items": []map[string]any{{"text": "First line\n## Injected heading", "kind": "fact"}}},
		"unknown kind":      {"userDirected": true, "items": []map[string]any{{"text": "Something.", "kind": "instruction"}}},
	} {
		if failure := callMemoryTool(t, session, "remember", args, &out); failure == "" {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := store.Read(t.Context(), "memory.md", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused writes reached the notebook: %v", err)
	}
}

func TestAgentMemoryKeepsUserEditsAndConcurrentItems(t *testing.T) {
	// Admit all eight concurrent writes on any host; admission tests own the busy reply.
	t.Setenv("FMN_LIMIT_DECODE_REQUESTS", "8")
	t.Setenv("FMN_LIMIT_WRITE_REQUESTS", "8")
	_, session, store := agentMemoryFixture(t, true)
	var first rememberResult
	if failure := callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": "Staging runs kernel 6.12.", "kind": "fact"}}}, &first); failure != "" {
		t.Fatal("remember:", failure)
	}
	doc, err := store.Read(t.Context(), "memory.md", "")
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(doc.Content), "- Staging runs kernel 6.12.", "- Staging runs kernel 6.13.", 1) + "- Prefer squash merges.\n"
	if _, err = store.Save(t.Context(), "memory.md", []byte(edited), doc.Revision, "memory-owner"); err != nil {
		t.Fatal(err)
	}

	var stale rememberResult
	callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": "Staging runs kernel 6.12.", "kind": "fact"}}}, &stale)
	if len(stale.Items) != 1 || stale.Items[0].ID != first.Items[0].ID || !stale.Items[0].Duplicate {
		t.Fatalf("an edited item was added back: %+v", stale)
	}

	var wg sync.WaitGroup
	failures := make(chan string, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out rememberResult
			if failure := callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": fmt.Sprintf("Concurrent fact %d.", i), "kind": "fact"}}}, &out); failure != "" {
				failures <- failure
			}
		}()
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error("concurrent remember:", failure)
	}

	var recalled struct {
		Core recalledMemory `json:"core"`
	}
	callMemoryTool(t, session, "recall_memory", map[string]any{}, &recalled)
	byText := map[string]int{}
	for i, item := range recalled.Core.Items {
		byText[item.Text] = i
	}
	if i, ok := byText["Staging runs kernel 6.13."]; !ok || recalled.Core.Items[i].ID != first.Items[0].ID || recalled.Core.Items[i].From != "agent" || !recalled.Core.Items[i].Pinned {
		t.Fatalf("user edit not kept as pinned: %+v", recalled.Core.Items)
	}
	if i, ok := byText["Prefer squash merges."]; !ok || recalled.Core.Items[i].From != "you" || !recalled.Core.Items[i].Pinned || recalled.Core.Items[i].ID == "" {
		t.Fatalf("user bullet not recalled as theirs: %+v", recalled.Core.Items)
	}
	for i := range 8 {
		if _, ok := byText[fmt.Sprintf("Concurrent fact %d.", i)]; !ok {
			t.Errorf("concurrent fact %d lost", i)
		}
	}
	if _, ok := byText["Staging runs kernel 6.12."]; ok {
		t.Fatal("superseded user edit came back")
	}
	final, err := store.Read(t.Context(), "memory.md", "")
	if err != nil || !strings.HasPrefix(string(final.Content), edited) {
		t.Fatalf("user edit rewritten:\n%s", final.Content)
	}
}

func TestAgentMemoryConfirmRenewsReplacesAndRetires(t *testing.T) {
	_, session, store := agentMemoryFixture(t, true)
	var saved rememberResult
	if failure := callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": "Staging runs kernel 6.12.", "kind": "fact"}, {"text": "CI uses Go 1.27.2.", "kind": "fact"}}}, &saved); failure != "" || len(saved.Items) != 2 {
		t.Fatal("remember:", failure, saved)
	}
	kernel, golang := saved.Items[0].ID, saved.Items[1].ID
	doc, err := store.Read(t.Context(), "memory.md", "")
	if err != nil {
		t.Fatal(err)
	}
	// An overdue recheck month; the text, and so the item's ID, is unchanged.
	overdue := regexp.MustCompile("("+kernel+" .*recheck: )[0-9-]+").ReplaceAllString(string(doc.Content), "${1}2020-01")
	if _, err = store.Save(t.Context(), "memory.md", []byte(overdue), doc.Revision, "memory-owner"); err != nil {
		t.Fatal(err)
	}
	core := recallCore(t, session)
	if i, ok := core.byText()["Staging runs kernel 6.12."]; !ok || !core.Items[i].Stale || core.Items[i].Pinned {
		t.Fatalf("overdue item: %+v", core.Items)
	}

	var confirmed confirmResult
	callMemoryTool(t, session, "confirm_memory", map[string]any{"id": kernel, "result": "holds"}, &confirmed)
	core = recallCore(t, session)
	if i := core.byText()["Staging runs kernel 6.12."]; confirmed.Outcome != "renewed" || core.Items[i].Stale || core.Items[i].Recheck != time.Now().UTC().AddDate(0, 3, 0).Format("2006-01") {
		t.Fatalf("renewal: %+v %+v", confirmed, core.Items[i])
	}

	callMemoryTool(t, session, "confirm_memory", map[string]any{"id": golang, "result": "changed", "replacement": "CI uses Go 1.27.3.", "reason": "go.mod pins 1.27.3"}, &confirmed)
	core = recallCore(t, session)
	_, old := core.byText()["CI uses Go 1.27.2."]
	i, current := core.byText()["CI uses Go 1.27.3."]
	if confirmed.Outcome != "superseded" || old || !current || core.Items[i].ID != confirmed.ID || len(core.Changed) != 1 || core.Changed[0].Old != "CI uses Go 1.27.2." || core.Changed[0].New != "CI uses Go 1.27.3." || core.Changed[0].Reason != "go.mod pins 1.27.3" {
		t.Fatalf("supersession: %+v %+v", confirmed, core)
	}
	var again rememberResult
	callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": "CI uses Go 1.27.2.", "kind": "fact"}}}, &again)
	if len(again.Items) != 1 || !again.Items[0].Duplicate {
		t.Fatalf("a retired item came back: %+v", again)
	}

	callMemoryTool(t, session, "confirm_memory", map[string]any{"id": kernel, "result": "wrong", "reason": "staging moved to kernel 6.13"}, &confirmed)
	if _, ok := recallCore(t, session).byText()["Staging runs kernel 6.12."]; confirmed.Outcome != "retired" || ok {
		t.Fatalf("a wrong item stayed current: %+v", confirmed)
	}

	// The owner's own words are never rewritten: a change becomes a disputed item.
	if doc, err = store.Read(t.Context(), "memory.md", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "memory.md", append(doc.Content, "\n## Workflow\n- Prefer squash merges.\n"...), doc.Revision, "memory-owner"); err != nil {
		t.Fatal(err)
	}
	core = recallCore(t, session)
	mine := core.Items[core.byText()["Prefer squash merges."]].ID
	callMemoryTool(t, session, "confirm_memory", map[string]any{"id": mine, "result": "changed", "replacement": "Prefer rebase merges."}, &confirmed)
	core = recallCore(t, session)
	squash, ok := core.byText()["Prefer squash merges."]
	rebase, added := core.byText()["Prefer rebase merges."]
	if confirmed.Outcome != "disputed" || !ok || !added || !core.Items[squash].Disputed || !core.Items[rebase].Disputed {
		t.Fatalf("pinned change: %+v %+v", confirmed, core.Items)
	}
	callMemoryTool(t, session, "confirm_memory", map[string]any{"id": mine, "result": "wrong"}, &confirmed)
	final, err := store.Read(t.Context(), "memory.md", "")
	if err != nil || confirmed.Outcome != "pinned" || !strings.Contains(string(final.Content), "## Workflow\n- Prefer squash merges.\n") {
		t.Fatalf("pinned item rewritten: %+v\n%s", confirmed, final.Content)
	}
}

func TestAgentMemoryJevRetiresTheObservationItReplaces(t *testing.T) {
	app, session, store := agentMemoryFixture(t, true)
	chatConfig := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(chatConfig, []byte(`{"connections":[{"id":"local","label":"Synthetic","format":"chat-completions","baseURL":"http://127.0.0.1:9","models":["synthetic"],"local":true,"capabilities":{"tools":true},"limits":{"runSeconds":10}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(chatConfig); err != nil {
		t.Fatal(err)
	}
	var choice atomic.Value
	var calls atomic.Int32
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var in struct {
			Questions map[string]struct {
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		want := choice.Load().(string)
		answers := map[string]any{}
		for name, question := range in.Questions {
			probabilities := map[string]float64{}
			for option := range question.Criteria {
				probabilities[option] = 0
			}
			if _, offered := probabilities[want]; !offered {
				t.Errorf("question %s does not offer %s: %v", name, want, question.Criteria)
			}
			probabilities[want] = 1
			answers[name] = map[string]any{"type": "choice", "choice": want, "confidence": 1, "probabilities": probabilities}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 40, "output_tokens": 4}})
	})
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"memory-owner"}, "password": {"test-password-long"}}.Encode())
	if w := localRequest(app, "/api/jev/settings", `{"consent":true}`, login.Result().Cookies()[0]); w.Code != 200 {
		t.Fatalf("Jev consent: %d %s", w.Code, w.Body.String())
	}

	var first, next rememberResult
	callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": "Staging runs kernel 6.12.", "kind": "fact"}}}, &first)
	choice.Store(first.Items[0].ID)
	callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": "Staging runs kernel 6.13.", "kind": "fact"}}}, &next)
	core := recallCore(t, session)
	_, old := core.byText()["Staging runs kernel 6.12."]
	_, current := core.byText()["Staging runs kernel 6.13."]
	if calls.Load() != 1 || next.Review != "checked" || len(next.Items) != 1 || next.Items[0].Replaced != first.Items[0].ID || old || !current || len(core.Changed) != 1 || core.Changed[0].New != "Staging runs kernel 6.13." {
		t.Fatalf("Jev replacement: calls=%d %+v %+v", calls.Load(), next, core)
	}

	doc, err := store.Read(t.Context(), "memory.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "memory.md", []byte(strings.Replace(string(doc.Content), "## Facts\n", "## Facts\n- Staging uses 16 GB nodes.\n", 1)), doc.Revision, "memory-owner"); err != nil {
		t.Fatal(err)
	}
	core = recallCore(t, session)
	mine := core.Items[core.byText()["Staging uses 16 GB nodes."]].ID
	choice.Store(mine)
	var disputed rememberResult
	callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": "Staging uses 32 GB nodes.", "kind": "fact"}}}, &disputed)
	core = recallCore(t, session)
	own, kept := core.byText()["Staging uses 16 GB nodes."]
	if disputed.Review != "checked" || len(disputed.Items) != 1 || disputed.Items[0].Disputes != mine || disputed.Items[0].Replaced != "" || !kept || !core.Items[own].Disputed {
		t.Fatalf("an owner item was replaced: %+v %+v", disputed, core.Items)
	}
}

// A review Jev cannot answer still saves the new item. The result says why in
// one word, and the log names the failed step for the operator, never the text.
func TestAgentMemoryFailedReviewIsLoggedAndKeepsTheItem(t *testing.T) {
	app, session, _ := agentMemoryFixture(t, true)
	chatConfig := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(chatConfig, []byte(`{"connections":[{"id":"local","label":"Synthetic","format":"chat-completions","baseURL":"http://127.0.0.1:9","models":["synthetic"],"local":true,"capabilities":{"tools":true},"limits":{"runSeconds":10}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(chatConfig); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"invalid key"}`, http.StatusUnauthorized)
	})
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"memory-owner"}, "password": {"test-password-long"}}.Encode())
	if w := localRequest(app, "/api/jev/settings", `{"consent":true}`, login.Result().Cookies()[0]); w.Code != 200 {
		t.Fatalf("Jev consent: %d %s", w.Code, w.Body.String())
	}
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	const first, second = "Payments to operators settle on Fridays.", "Payments to operators settle on Mondays."
	var out rememberResult
	callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": first, "kind": "fact", "topic": "Operator payments"}}}, &out)
	if out.Review != "" || calls.Load() != 0 {
		t.Fatalf("a first item in its topic was reviewed: %+v", out)
	}
	callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": second, "kind": "fact", "topic": "Operator payments"}}}, &out)
	core := recallCore(t, session)
	_, kept := core.byText()[first]
	_, added := core.byText()[second]
	if calls.Load() != 1 || out.Review != "failed" || out.ReviewReason != "provider" || len(out.Items) != 1 || out.Items[0].Replaced != "" || !kept || !added {
		t.Fatalf("failed review: calls=%d %+v", calls.Load(), out)
	}
	var entry map[string]any
	for line := range strings.Lines(logs.String()) {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil && e["msg"] == "memory review failed" {
			entry = e
		}
	}
	if entry == nil || entry["level"] != "WARN" || entry["feature"] != "personal-assistant" || entry["step"] != "provider" || entry["status"] != float64(401) || entry["questions"] != float64(1) || entry["existing"] != float64(1) || entry["error"] != decision.ErrUnavailable.Error() {
		t.Fatalf("review failure log: %v\n%s", entry, logs.String())
	}
	if strings.Contains(logs.String(), "Payments") || strings.Contains(logs.String(), "invalid key") {
		t.Fatalf("the log disclosed memory or provider text:\n%s", logs.String())
	}
}
