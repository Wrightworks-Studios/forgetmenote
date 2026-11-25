package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

// Notebooks in the notes store have no directory, so they never keep the
// operator's runtime configuration from loading; the working tree a notebook
// had before the cutover still holds its files, and stays refused.
func TestIntegrationConfigurationStaysOutsideNotebookDirectories(t *testing.T) {
	dir := t.TempDir()
	projects, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(projects.Close)
	if err = projects.Create(catalog.Bootstrap, "localnotes", "Local notes"); err != nil {
		t.Fatal(err)
	}
	app, err := New(projects, pgtest.Org(t), dir, "http://localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	content := []byte(`{"version":1,"connections":[]}`)
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "connections.json")
	if err = os.WriteFile(outside, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err = app.ConfigureDataConnections(outside); err != nil {
		t.Fatalf("notebooks without a directory prevented loading private runtime configuration: %v", err)
	}
	local, err := projects.Access(catalog.Bootstrap, "localnotes", "read")
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(previousReleaseTree(t, local), "connections.json")
	if err = os.WriteFile(inside, content, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(outsideDir, "linked.json")
	if err = os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{inside, link} {
		if err = app.ConfigureDataConnections(path); err == nil || !strings.Contains(err.Error(), "outside notebook repositories") {
			t.Fatalf("notebook-owned configuration was not rejected at the ownership boundary: %s: %v", path, err)
		}
	}
}

// Two notebooks of one instance stay apart for browser and MCP callers, and
// revoking a member ends their browser and MCP access to that notebook only.
func TestTwoNotebooksBrowserMCPIsolationAndRevocation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	projects, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(projects.Close)
	stores := map[string]*workspace.Store{}
	for _, row := range []struct{ id, user string }{{"alpha", "alice"}, {"beta", "bob"}} {
		if err = projects.Create(catalog.Bootstrap, row.id, row.id); err != nil {
			t.Fatal(err)
		}
		if err = projects.Member(catalog.Bootstrap, row.id, row.user, "edit"); err != nil {
			t.Fatal(err)
		}
		if stores[row.id], err = projects.Access(catalog.Bootstrap, row.id, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	app, err := New(projects, pgtest.Org(t), dir, "http://localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	if err = app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, user, text string }{{"alpha", "alice", "# Alpha"}, {"beta", "bob", "# Beta"}} {
		raw, _ := json.Marshal(map[string]string{"path": "same.md", "content": row.text})
		response := proxyCall(app, row.user, "POST", "/p/"+row.id+"/api/note", string(raw))
		if response.Code != 200 {
			t.Fatalf("browser %s: %d %s", row.id, response.Code, response.Body)
		}
		note, err := stores[row.id].Read(ctx, "same.md", "")
		if err != nil || string(note.Content) != row.text {
			t.Fatalf("notebook isolation %s: %q %v", row.id, note.Content, err)
		}
		identity, err := projects.Account(row.user)
		if err != nil {
			t.Fatal(err)
		}
		history, err := stores[row.id].History(ctx, "same.md")
		if err != nil || history[0].AuthorEmail != identity.Identity().Email || history[0].Source != "browser" {
			t.Fatalf("browser attribution %s: %+v %v", row.id, history, err)
		}
	}
	if response := proxyCall(app, "alice", "GET", "/p/beta/api/note?path=same.md", ""); response.Code != 403 {
		t.Fatalf("browser isolation: %d", response.Code)
	}
	client := actionClient(t, app, "alice", "notes:read notes:write")
	actionCall(t, client, "save_note", map[string]any{"project": "alpha", "path": "mcp.md", "content": "MCP notebook write", "revision": ""}, false)
	actionCall(t, client, "save_note", map[string]any{"project": "beta", "path": "mcp.md", "content": "forbidden", "revision": ""}, true)
	if _, err = stores["beta"].Read(ctx, "mcp.md", ""); err == nil {
		t.Fatal("a refused MCP save reached the other notebook")
	}
	account, _ := projects.Account("alice")
	history, err := stores["alpha"].History(ctx, "mcp.md")
	if err != nil || history[0].AuthorEmail != account.Identity().Email || history[0].Source != "mcp" || history[0].Client != "actions-client" {
		t.Fatalf("MCP attribution: %+v %v", history, err)
	}
	// Live state follows the same notebook boundary as ordinary saves.
	raw := actionCall(t, client, "read_live_note", map[string]any{"project": "alpha", "path": "mcp.md"}, false)
	var live workspace.LiveDocument
	if json.Unmarshal(raw, &live) != nil || live.LiveRevision == "" {
		t.Fatalf("live read: %s", raw)
	}
	actionCall(t, client, "patch_live_note", map[string]any{"project": "alpha", "path": "mcp.md", "base": live.LiveRevision, "operationId": "notebook-patch-1", "edits": []workspace.TextEdit{{Start: 0, End: 3, Old: "MCP", Text: "Live"}}}, false)
	if response := proxyCall(app, "alice", "GET", "/p/alpha/api/live?path=mcp.md", ""); response.Code != 200 || !strings.Contains(response.Body.String(), "Live notebook write") {
		t.Fatalf("browser live read: %d %s", response.Code, response.Body)
	}
	if response := proxyCall(app, "bob", "GET", "/p/alpha/api/live?path=mcp.md", ""); response.Code != 403 {
		t.Fatalf("live isolation: %d", response.Code)
	}
	if _, err = stores["alpha"].LiveCheckpoint(ctx, "mcp.md"); err != nil {
		t.Fatal(err)
	}
	state, _ := stores["alpha"].Status(ctx)
	if _, err = stores["alpha"].SetLock(ctx, "same.md", true, state.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	actionCall(t, client, "save_note", map[string]any{"project": "alpha", "path": "same.md", "content": "locked", "revision": state.Revision}, true)
	historyResponse := proxyCall(app, "alice", "POST", "/p/alpha/api/knowledge/hybrid-search", `{"text":"Alpha","scope":"history","mode":"text"}`)
	if historyResponse.Code != 200 || !strings.Contains(historyResponse.Body.String(), "same.md") {
		t.Fatalf("notebook history: %d %s", historyResponse.Code, historyResponse.Body)
	}
	if err = projects.Member(catalog.Bootstrap, "alpha", "alice", ""); err != nil {
		t.Fatal(err)
	}
	actionCall(t, client, "read_note", map[string]any{"project": "alpha", "path": "same.md"}, true)
	if response := proxyCall(app, "alice", "GET", "/p/alpha/api/note?path=same.md", ""); response.Code != 403 {
		t.Fatalf("browser revocation: %d", response.Code)
	}
	if response := proxyCall(app, "bob", "GET", "/p/beta/api/note?path=same.md", ""); response.Code != 200 || !strings.Contains(response.Body.String(), "# Beta") {
		t.Fatalf("revoking one notebook's member affected another: %d %s", response.Code, response.Body)
	}
}
