package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"forgetmenote/internal/purgehttp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

// Each notebook bounds the plans agents prepare for it on its own: an agent
// preparing many plans for one notebook, even a person's own Inbox, never
// ends a plan another person's agent prepared for theirs.
func TestPurgePlansOfOneNotebookNeverEndAnothersPlans(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "keeper", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Create(catalog.Bootstrap, "mine", "Mine"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "mine", "mallory", "owner"); err != nil {
		t.Fatal(err)
	}
	inbox, err := app.catalog.EnsureInbox("mallory")
	if err != nil {
		t.Fatal(err)
	}
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\n"+secret+"\n", "")
	sessions := purgeAgents(t, app, "notes:read notes:write notes:manage", "keeper", "mallory")
	raw := actionCall(t, sessions[0], "prepare_purge", map[string]any{"project": "default", "kind": "text", "text": secret}, false)
	var out purgehttp.AgentPlan
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	link, _ := url.Parse(out.ConfirmURL)
	token := link.Query().Get("purge")
	for _, project := range []string{"mine", inbox.ID} {
		for i := range 65 {
			actionCall(t, sessions[1], "prepare_purge", map[string]any{"project": project, "kind": "text", "text": fmt.Sprintf("guess %d", i)}, false)
		}
	}
	if w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/prepare", `{"token":"`+token+`"}`); w.Code != 200 {
		t.Fatalf("keeper opens the plan after another notebook's plans: HTTP %d %s", w.Code, w.Body)
	}
	var plans int
	if err := app.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM pending_flows WHERE org = $1 AND kind = $2`, app.db.Org, purgehttp.FlowKind("mine")).Scan(&plans); err != nil || plans != 64 {
		t.Fatalf("one notebook keeps %d plans (%v), not its bound", plans, err)
	}
}

// A purge's text or pattern is at most 64 KiB, and a text purge names at
// most 1,000 notes: larger scopes are refused as too large on every
// transport, and an agent's is never stored as a plan.
func TestPurgeScopesAreBounded(t *testing.T) {
	app, store := fixture(t)
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nA key.\n", "")
	paths := make([]string, purgehttp.MaxPaths+1)
	for i := range paths {
		paths[i] = fmt.Sprintf("notes/%d.md", i)
	}
	versions := make([]string, purgehttp.MaxPaths+1)
	for i := range versions {
		versions[i] = strings.Repeat("a", 40)
	}
	large := strings.Repeat("x", purgehttp.MaxText+1)
	scopes := []workspace.PurgeScope{
		{Kind: workspace.PurgeText, Text: large},
		{Kind: workspace.PurgeText, Pattern: large},
		{Kind: workspace.PurgeText, Text: "key", Paths: paths},
		{Kind: workspace.PurgeVersions, Path: "notes/keys.md", Versions: versions},
	}
	for _, scope := range scopes {
		body, _ := json.Marshal(map[string]any{"scope": scope})
		if w := call(app, "POST", "/api/purge/prepare", string(body), testToken); w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), `"too_large"`) {
			t.Errorf("a browser plan of %d bytes of text, %d of pattern, %d paths and %d versions: HTTP %d %.200s", len(scope.Text), len(scope.Pattern), len(scope.Paths), len(scope.Versions), w.Code, w.Body)
		}
		body, _ = json.Marshal(map[string]any{"scope": scope, "revision": "x", "confirmation": "x", "reason": "x"})
		if w := call(app, "POST", "/api/purge/execute", string(body), testToken); w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("running a purge of %d bytes of text, %d of pattern, %d paths and %d versions: HTTP %d %.200s", len(scope.Text), len(scope.Pattern), len(scope.Paths), len(scope.Versions), w.Code, w.Body)
		}
	}
	session := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write notes:manage")
	for _, scope := range scopes {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "prepare_purge", Arguments: map[string]any{"project": "default", "kind": scope.Kind, "path": scope.Path, "versions": scope.Versions, "text": scope.Text, "pattern": scope.Pattern, "paths": scope.Paths}})
		if err != nil {
			t.Fatal(err)
		}
		typed, _ := json.Marshal(result.Meta[mcpErrorMeta])
		if !result.IsError || string(typed) != `{"code":"too_large","status":413}` {
			t.Errorf("an agent's plan of %d bytes of text, %d of pattern, %d paths and %d versions: %s", len(scope.Text), len(scope.Pattern), len(scope.Paths), len(scope.Versions), typed)
		}
	}
	var plans int
	if err := app.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM pending_flows WHERE org = $1`, app.db.Org).Scan(&plans); err != nil || plans != 0 {
		t.Fatalf("%d plans stored (%v)", plans, err)
	}
	// At the bound, a scope is planned.
	actionCall(t, session, "prepare_purge", map[string]any{"project": "default", "kind": "text", "text": strings.Repeat("x", purgehttp.MaxText)}, false)
}

// A plan an agent prepared holds the text to erase; once it expires, the
// gateway's periodic sweep removes it, even when no other plan begins.
func TestExpiredPurgePlansAreSwept(t *testing.T) {
	app, store := fixture(t)
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\n"+secret+"\n", "")
	session := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write notes:manage")
	actionCall(t, session, "prepare_purge", map[string]any{"project": "default", "kind": "text", "text": secret}, false)
	ctx := context.Background()
	if _, err := app.db.Pool.Exec(ctx, `UPDATE pending_flows SET expires = now() - interval '1 second' WHERE org = $1`, app.db.Org); err != nil {
		t.Fatal(err)
	}
	app.sweepFlows()
	var holding int
	if err := app.db.Pool.QueryRow(ctx, `SELECT count(*) FROM pending_flows WHERE position(convert_to($1, 'UTF8') IN body) > 0`, secret).Scan(&holding); err != nil || holding != 0 {
		t.Fatalf("%d expired plans still hold the text (%v)", holding, err)
	}
	app.exports.mu.Lock()
	armed := app.exports.sweep != nil
	app.exports.mu.Unlock()
	if !armed {
		t.Fatal("the gateway does not sweep pending flows again")
	}
}
