package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestKnowledgePermissionBoundariesAndInstructions(t *testing.T) {
	app, _ := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	for user, role := range map[string]string{"reader": "read", "commenter": "comment", "editor": "edit", "owner": "owner"} {
		app.catalog.Member("@local-admin", "default", user, role)
	}
	w := proxyCall(app, "reader", "GET", "/p/default/api/knowledge/status", "")
	if w.Code != 200 {
		t.Fatalf("knowledge status: %d %s", w.Code, w.Body)
	}
	config, _ := json.Marshal(map[string]any{"config": knowledge.Config{Enabled: true, Model: "fixture-v1", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}})
	for _, user := range []string{"reader", "commenter", "editor"} {
		if w := proxyCall(app, user, "POST", "/p/default/api/knowledge/config", string(config)); w.Code != 403 {
			t.Fatalf("%s configured shared index: %d", user, w.Code)
		}
	}
	if w := proxyCall(app, "owner", "POST", "/p/default/api/knowledge/config", string(config)); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	for _, user := range []string{"reader", "commenter", "editor"} {
		if w := proxyCall(app, user, "POST", "/p/default/api/knowledge/instructions", `{"content":"Preserve human wording.","revision":""}`); w.Code != 403 {
			t.Fatalf("%s changed instructions: %d", user, w.Code)
		}
	}
	if w := proxyCall(app, "owner", "POST", "/p/default/api/knowledge/instructions", `{"content":"Preserve human wording.","revision":""}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := proxyCall(app, "editor", "POST", "/p/default/api/note", `{"path":"AGENTS.md","content":"Replace authority","revision":""}`); w.Code != 403 {
		t.Fatal("generic instruction bypass", w.Code)
	}
	if w := proxyCall(app, "reader", "POST", "/p/default/api/knowledge/embeddings", `{}`); w.Code != 403 {
		t.Fatal("reader index write", w.Code)
	}
	if w := proxyCall(app, "nobody", "GET", "/p/default/api/knowledge/status", ""); w.Code != 403 {
		t.Fatal("project existence leaked", w.Code)
	}
}

func TestMCPKnowledgeVectorsProposalsAndPermissionChanges(t *testing.T) {
	app, store := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	doc, _ := store.Save(ctx, "proof.md", []byte("# Proof\nEvidence from an experiment."), "", "")
	app.catalog.Member("@local-admin", "default", "alice", "edit")
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	app.oauth.Access[digest("alice-access")] = withOAuthFamily(accessGrant{Subject: "alice", Client: "registered-client", Scope: "notes:read notes:write", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	client := mcp.NewClient(&mcp.Implementation{Name: "untrusted label", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: "alice-access"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tool := func(name string, args map[string]any, wantError bool) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError != wantError {
			t.Fatalf("%s: %+v %v", name, result, err)
		}
		return result
	}
	tool("knowledge_status", map[string]any{"project": "default"}, false)
	tool("knowledge_status", map[string]any{"project": "secret"}, true)
	e, releaseEngine, _ := app.acquireEngine(t.Context(), store)
	if releaseEngine != nil {
		t.Cleanup(releaseEngine)
	}

	status, err := e.Configure(ctx, knowledge.Config{Enabled: true, Model: "fixture-v1", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	tool("get_indexing_passages", map[string]any{"project": "default"}, false)
	pending, _ := e.Pending(ctx, 32)
	p := pending.Passages[0]
	submission := knowledge.Submission{Revision: status.Revision, ConfigID: status.ConfigID, Vectors: []knowledge.VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}}
	tool("submit_embeddings", map[string]any{"project": "default", "submission": submission}, false)
	tool("semantic_search", map[string]any{"project": "default", "query": knowledge.SearchQuery{Vector: []float32{1, 0, 0}, ConfigID: status.ConfigID}}, false)
	group := workspace.EditGroup{ID: "test-proposal", Summary: "Evidence correction", Edits: []workspace.NoteEdit{{Path: doc.Path, Content: "# Proof\nRevised evidence.", Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content)}}}
	tool("submit_change_group", map[string]any{"project": "default", "mode": "proposal", "group": group}, false)
	proposals, _ := e.Proposals(ctx)
	if len(proposals) != 1 || proposals[0].Provenance.Client != "registered-client" {
		t.Fatal("missing proposal attribution", proposals)
	}
	app.catalog.Member("@local-admin", "default", "alice", "comment")
	tool("review_decision", map[string]any{"project": "default", "kind": "proposal", "id": proposals[0].ID, "decision": "accept"}, true)
	tool("submit_embeddings", map[string]any{"project": "default", "submission": submission}, true)
	app.catalog.Member("@local-admin", "default", "alice", "edit")
	head, _ := store.Status(ctx)
	store.SetLock(ctx, doc.Path, true, head.Revision, "")
	tool("review_decision", map[string]any{"project": "default", "kind": "proposal", "id": proposals[0].ID, "decision": "accept"}, true)
	head, _ = store.Status(ctx)
	store.SetLock(ctx, doc.Path, false, head.Revision, "")
	tool("review_decision", map[string]any{"project": "default", "kind": "proposal", "id": proposals[0].ID, "decision": "accept"}, false)
	tool("review_decision", map[string]any{"project": "default", "kind": "proposal", "id": proposals[0].ID, "decision": "accept"}, false)
	history, _ := store.History(ctx, doc.Path)
	if history[0].Source != "mcp" || history[0].Client != "registered-client" {
		t.Fatal("wrong applied attribution", history[0])
	}
}

func TestMCPSemanticSetupUsesOwnerAuthorityAndPreservesConfiguration(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	app.catalog.Member("@local-admin", "default", "alice", "edit")
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	grant := withOAuthFamily(accessGrant{Subject: "alice", Client: "test-agent", Scope: "notes:read notes:write", Resource: ts.URL + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	app.oauth.Access[digest("setup-token")] = grant
	client := mcp.NewClient(&mcp.Implementation{Name: "setup-agent", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: "setup-token"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(args map[string]any, wantError bool) {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "configure_semantic_search", Arguments: args})
		if err != nil || result.IsError != wantError {
			t.Fatalf("setup: %+v %v", result, err)
		}
	}
	args := map[string]any{"project": "default", "model": "fixture-v1", "dimensions": 3}
	// Merely connecting with Edit authority does not grant configuration authority.
	for _, role := range []string{"read", "comment", "edit"} {
		app.catalog.Member("@local-admin", "default", "alice", role)
		call(args, true)
	}
	app.catalog.Member("@local-admin", "default", "alice", "owner")
	call(args, false)
	e, releaseEngine, _ := app.acquireEngine(t.Context(), store)
	if releaseEngine != nil {
		t.Cleanup(releaseEngine)
	}

	pending, _ := e.Pending(ctx, 1)
	if !pending.Status.Config.Enabled || pending.Status.Config.PassageRunes != 1400 || pending.Status.Config.Endpoint {
		t.Fatal(pending.Status)
	}
	call(args, false)
	// Preserve manual passage size and provider choice when reusing a model.
	manual := pending.Status.Config
	manual.PassageRunes = 256
	manual.Endpoint = true
	if _, err := e.Configure(ctx, manual, true); err != nil {
		t.Fatal(err)
	}
	call(args, false)
	pending, _ = e.Pending(ctx, 1)
	if pending.Status.Config != manual {
		t.Fatal("agent replaced manual settings", pending.Status.Config)
	}
	args["model"] = "different-model"
	call(args, true)
	args["rebuild"] = true
	call(args, false)
	args["dimensions"] = 0
	call(args, true)
	args["dimensions"] = 3
	grant.Subject = "@local-admin"
	app.oauth.Mu.Lock()
	app.oauth.Access[digest("setup-token")] = grant
	app.oauth.Mu.Unlock()
	call(args, false)
	grant.Scope = "notes:read"
	app.oauth.Mu.Lock()
	app.oauth.Access[digest("setup-token")] = grant
	app.oauth.Mu.Unlock()
	call(args, true)
}

func TestHybridSearchReadBoundary(t *testing.T) {
	app, store := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	if _, err := store.Save(context.Background(), "flowers.md", []byte("# Flowers\nForget-me-nots are blue."), "", ""); err != nil {
		t.Fatal(err)
	}
	app.catalog.Member("@local-admin", "default", "reader", "read")
	for _, mode := range []string{"all", "text"} {
		w := proxyCall(app, "reader", "POST", "/p/default/api/knowledge/hybrid-search", `{"text":"Forget-me-nots","mode":"`+mode+`"}`)
		if w.Code != 200 {
			t.Fatalf("reader search: %d %s", w.Code, w.Body)
		}
		var result struct {
			Matches []struct{ Path string }
			Mode    string
			Message string
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Matches) != 1 || result.Matches[0].Path != "flowers.md" || result.Mode != "text" {
			t.Fatalf("unexpected matches: %+v", result)
		}
		if mode == "all" && result.Message == "" {
			t.Fatal("missing fallback explanation")
		}
	}
	if w := proxyCall(app, "nobody", "POST", "/p/default/api/knowledge/hybrid-search", `{"text":"flowers"}`); w.Code != 403 {
		t.Fatal("search bypassed permissions", w.Code)
	}
}

func TestMCPHybridSearchReadScopeAndRevocation(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	if _, err := store.Save(ctx, "flowers.md", []byte("# Flowers\nBlue petals."), "", ""); err != nil {
		t.Fatal(err)
	}
	app.catalog.Member("@local-admin", "default", "reader", "read")
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	app.oauth.Access[digest("hybrid-token")] = withOAuthFamily(accessGrant{Subject: "reader", Client: "hybrid-agent", Scope: "notes:read", Resource: ts.URL + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	client := mcp.NewClient(&mcp.Implementation{Name: "hybrid-agent", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: "hybrid-token"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(project string, wantError bool) {
		t.Helper()
		v, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "hybrid_search", Arguments: map[string]any{"project": project, "query": map[string]any{"text": "Flowers"}}})
		if err != nil || v.IsError != wantError {
			raw, _ := json.Marshal(v)
			t.Fatalf("%s %v", raw, err)
		}
		if !wantError {
			raw, _ := json.Marshal(v.StructuredContent)
			var result knowledge.HybridResult
			if err := json.Unmarshal(raw, &result); err != nil || len(result.Matches) != 1 || result.Matches[0].Path != "flowers.md" {
				t.Fatalf("%s %v", raw, err)
			}
		}
	}
	call("default", false)
	call("secret", true)
	if err := app.catalog.Member("@local-admin", "default", "reader", ""); err != nil {
		t.Fatal(err)
	}
	call("default", true)
}
