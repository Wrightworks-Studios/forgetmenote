package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

// An agent prepares a purge plan and gets a browser URL for a person; it
// never gets what runs the plan, no tool runs one, and its credential cannot
// reach the route that does. The person opens the plan by its URL and runs
// it.
func TestPreparePurgeCannotExecute(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "keeper", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "writer", "edit"); err != nil {
		t.Fatal(err)
	}
	secret := webPurgeSecret()
	saved := savePurgeNote(t, store, "notes/keys.md", "# Keys\n\n"+secret+"\n", "")
	const scope = "notes:read notes:write notes:manage"
	sessions := purgeAgents(t, app, scope, "keeper", "writer")
	session, editor := sessions[0], sessions[1]

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if strings.Contains(tool.Name, "purge") {
			if tool.Name != "prepare_purge" {
				t.Fatalf("an MCP tool other than prepare_purge purges: %s", tool.Name)
			}
			found = true
		}
	}
	for name := range app.actions {
		if strings.Contains(name, "purge") && name != "prepare_purge" {
			t.Fatalf("an action other than prepare_purge purges: %s", name)
		}
	}
	if !found {
		t.Fatal("prepare_purge is not offered")
	}

	// An editor's agent cannot even prepare one.
	actionCall(t, editor, "prepare_purge", map[string]any{"project": "default", "kind": "text", "text": secret}, true)

	raw := actionCall(t, session, "prepare_purge", map[string]any{"project": "default", "kind": "text", "text": secret}, false)
	var out preparePurgeOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Plan.Confirmation != "" || out.Plan.Scope.Text != "" || strings.Contains(string(raw), secret) {
		t.Fatalf("the agent holds what runs the plan, or the text: %s", raw)
	}
	if len(out.Plan.Notes) != 1 || out.Plan.Notes[0].Path != "notes/keys.md" || !strings.Contains(out.Message, "rotate") {
		t.Fatalf("plan: %s", raw)
	}
	link, err := url.Parse(out.ConfirmURL)
	if err != nil || link.Query().Get("project") != "default" || link.Query().Get("purge") == "" || !strings.HasPrefix(out.ConfirmURL, app.origin+"/?") {
		t.Fatalf("confirm URL: %q", out.ConfirmURL)
	}
	token := link.Query().Get("purge")

	// The agent's credential cannot reach the routes that run or confirm a
	// purge, whatever it sends.
	agent := "actions-" + digest("keeper"+scope)
	for _, path := range []string{"/p/default/api/purge/execute", "/p/default/api/purge/prepare"} {
		r := httptest.NewRequest("POST", app.origin+path, strings.NewReader(`{"token":"`+token+`","scope":{"kind":"text","text":"`+secret+`"},"revision":"`+saved.Revision+`","confirmation":"x","reason":"agent"}`))
		r.Header.Set("Authorization", "Bearer "+agent)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code == 200 {
			t.Fatalf("an agent's credential reached %s: %s", path, w.Body)
		}
	}
	w := call(app, "POST", "/api/v1/actions/execute_purge", `{"project":"default"}`, agent)
	if w.Code == 200 {
		t.Fatalf("an action runs a purge: %s", w.Body)
	}
	if doc, err := store.Read(t.Context(), "notes/keys.md", ""); err != nil || doc.Revision != saved.Revision || !strings.Contains(string(doc.Content), secret) {
		t.Fatal("preparing changed the notebook:", err)
	}

	// Another person cannot use the plan; its owner opens it in the browser
	// and runs it.
	if w := proxyCall(app, "writer", "POST", "/p/default/api/purge/prepare", `{"token":"`+token+`"}`); w.Code != http.StatusForbidden {
		t.Fatalf("an editor opens the plan: HTTP %d %s", w.Code, w.Body)
	}
	w = proxyCall(app, "keeper", "POST", "/p/default/api/purge/prepare", `{"token":"`+token+`"}`)
	var plan webPurgePlan
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Scope.Text != secret || plan.Confirmation == "" {
		t.Fatalf("the owner opens the plan: HTTP %d %s", w.Code, w.Body)
	}
	body, _ := json.Marshal(map[string]any{"token": token, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": "Leaked key found by an agent"})
	if w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/execute", string(body)); w.Code != 200 || !strings.Contains(w.Body.String(), `"preparedByAgent":true`) {
		t.Fatalf("the owner runs it, and its receipt says an agent prepared it: HTTP %d %s", w.Code, w.Body)
	}
	if doc, err := store.Read(t.Context(), "notes/keys.md", ""); err != nil || strings.Contains(string(doc.Content), secret) {
		t.Fatal("not purged:", err)
	}
	// The plan, which held the text, is gone with the purge.
	if w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/prepare", `{"token":"`+token+`"}`); w.Code != http.StatusNotFound {
		t.Fatalf("a used plan: HTTP %d %s", w.Code, w.Body)
	}
}

// An agent's plan says nothing of the records outside the notebook, such as
// other members' private chats. Those records are matched against what the
// notebook holds, and whoever can write to the notebook decides that: an
// agent that wrote candidates into a note could otherwise read a chat by
// probing with patterns over them and comparing counts. Neither an Owner's
// nor an Admin's agent gets those counts; the person who opens the plan in
// the browser does.
func TestPreparePurgeGivesAgentsNoCountsOfRecordsOutsideTheNotebook(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"keeper": "owner", "reader": "read"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	conversation, err := app.chat.Store.Create("reader", "default", chat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.chat.Store.Update("reader", "default", conversation.ID, func(c *chat.Conversation) error {
		c.AddEvent("run-1", "user", map[string]any{"text": "remember that my bank PIN is 7391, my card ends 4242"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.jevState.records.Write("jobs/"+strings.Repeat("d", 64)+".json", map[string]any{"version": 2, "project": "default", "message": "remember that my bank PIN is 7391, my card ends 4242"}); err != nil {
		t.Fatal(err)
	}
	sessions := purgeAgents(t, app, "notes:read notes:write notes:manage", "keeper", catalog.Bootstrap)
	var dictionary strings.Builder
	dictionary.WriteString("# Scratch\n\n")
	for i := 7300; i < 7400; i++ {
		fmt.Fprintf(&dictionary, "bank PIN is %04d\nremember that my bank PIN is %04d, my card ends 4242\n", i, i)
	}
	actionCall(t, sessions[0], "save_note", map[string]any{"project": "default", "path": "notes/scratch.md", "content": dictionary.String(), "revision": ""}, false)
	probes := []map[string]any{
		{"kind": "text", "pattern": `bank PIN is 739[0-4]`, "paths": []string{"notes/scratch.md"}},
		{"kind": "text", "pattern": `bank PIN is 739[5-9]`, "paths": []string{"notes/scratch.md"}},
		{"kind": "text", "text": "bank PIN is 7391"},
		{"kind": "note", "path": "notes/scratch.md"},
	}
	var token string
	for i, session := range sessions {
		for _, probe := range probes {
			probe["project"] = "default"
			raw := actionCall(t, session, "prepare_purge", probe, false)
			var out struct {
				Plan       map[string]json.RawMessage `json:"plan"`
				ConfirmURL string                     `json:"confirmUrl"`
				Message    string                     `json:"message"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			if derived, ok := out.Plan["derived"]; ok {
				t.Errorf("agent %d's plan for %v counts records outside the notebook: %s", i, probe, derived)
			}
			if string(out.Plan["recordsCountedOnReview"]) != "true" || !strings.Contains(out.Message, "counted only when") {
				t.Errorf("agent %d's plan for %v does not say the records are counted on review: %s", i, probe, raw)
			}
			if i == 0 && probe["text"] != nil {
				link, _ := url.Parse(out.ConfirmURL)
				token = link.Query().Get("purge")
			}
		}
	}
	// The Owner who opens the plan is told that other people's records are
	// redacted too, and an Admin how many.
	w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/prepare", `{"token":"`+token+`"}`)
	var plan webPurgePlan
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Derived != (purgeDerived{Own: true, Others: purgeOthersRedacted}) {
		t.Fatalf("the owner reviews the agent's plan: HTTP %d %s", w.Code, w.Body)
	}
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	w, plan = proxyCall(app, "admin", "POST", "/p/default/api/purge/prepare", `{"token":"`+token+`"}`), webPurgePlan{}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Derived.Chats != 1 || plan.Derived.JevRecords != 1 || plan.Derived.Own {
		t.Fatalf("the Admin reviews the agent's plan: HTTP %d %s", w.Code, w.Body)
	}
}

// purgeAgents connects an MCP client for each of users, with scope, to one
// test server.
func purgeAgents(t *testing.T, app *Server, scope string, users ...string) []*mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	app.origin = ts.URL
	var sessions []*mcp.ClientSession
	for _, user := range users {
		token := "actions-" + digest(user+scope)
		app.oauth.Mu.Lock()
		app.oauth.Access[digest(token)] = withOAuthFamily(accessGrant{Subject: user, Client: "actions-client", Scope: scope, Resource: ts.URL + "/mcp", Expires: time.Now().Add(time.Hour).Unix(), Epoch: app.catalog.Epoch(user)})
		app.oauth.Mu.Unlock()
		client := mcp.NewClient(&mcp.Implementation{Name: "purge-agent", Version: "1"}, nil)
		session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: token}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Close() })
		sessions = append(sessions, session)
	}
	return sessions
}

// Agents holding revisions from before a purge keep working: a read-only
// tool pinned to an old revision reads the revision standing for it, and a
// pin to a removed version is refused as purged.
func TestMCPReadsMapRevisionsAPurgeRewrote(t *testing.T) {
	app, store := fixture(t)
	secret := webPurgeSecret()
	first := savePurgeNote(t, store, "notes/keys.md", "# Keys\n\n"+secret+"\n", "")
	gone := savePurgeNote(t, store, "notes/gone.md", "# Gone\n\nPrivate data.\n", "")
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	if code, _, body := executePurgeHTTP(app, plan, "Leaked key"); code != 200 {
		t.Fatal(body)
	}
	plan = preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeNote, Path: "notes/gone.md"})
	if code, _, body := executePurgeHTTP(app, plan, "Private data"); code != 200 {
		t.Fatal(body)
	}
	session := actionClient(t, app, catalog.Bootstrap, "notes:read")
	raw := actionCall(t, session, "read_note", map[string]any{"project": "default", "path": "notes/keys.md", "revision": first.Revision}, false)
	var doc noteOutput
	if json.Unmarshal(raw, &doc) != nil || doc.Revision == first.Revision || !strings.Contains(doc.Content, workspace.PurgedText) || strings.Contains(string(raw), secret) {
		t.Fatalf("read_note at an old revision: %s", raw)
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_note", Arguments: map[string]any{"project": "default", "path": "notes/gone.md", "revision": gone.Revision}})
	if err != nil || !result.IsError {
		t.Fatalf("read_note of a purged version: %+v %v", result, err)
	}
	text, _ := json.Marshal(result.Content)
	if !strings.Contains(string(text), "purge") || strings.Contains(string(text), "Private data") {
		t.Fatalf("a pin to a purged version: %s", text)
	}
	raw = actionCall(t, session, "read_project_changes", map[string]any{"project": "default", "since": first.Revision}, false)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("changes since an old revision: %s", raw)
	}
}
