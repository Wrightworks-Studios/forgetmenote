package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Saving a data watch checks its arguments against the operation's input
// schema, as its run would, so a bad argument is refused when it is saved
// rather than when the watch first runs. The refusal names the watch and the
// argument, through the browser and through MCP alike, and calls nothing.
func TestSavingADataWatchChecksItsArguments(t *testing.T) {
	app, _ := fixture(t)
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	app.origin = ts.URL
	admin := recoverySession(t, app)
	alice := addLocal(t, app, admin, "watch-arguments")
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"watch-arguments"}, "password": {"test-password-long"}}.Encode())
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	cookie := login.Result().Cookies()[0]
	if w := localRequest(app, "/api/organization/action/set_assistant_policy", `{"revision":0,"enabled":true}`, admin); w.Code != 200 {
		t.Fatalf("assistant policy: %d %s", w.Code, w.Body)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("saving a watch called its operation")
	}))
	t.Cleanup(upstream.Close)
	schema := `{"type":"object","properties":{"region":{"type":"string"},"limit":{"type":"integer","maximum":100}},"required":["region"],"additionalProperties":false}`
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Label: "Metrics", Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true, Operations: []connectors.Operation{
		{Name: "open", ReadOnly: true, Method: "GET", Path: "/open"},
		{Name: "orders", ReadOnly: true, Method: "GET", Path: "/orders", InputSchema: json.RawMessage(schema), Query: map[string]string{"region": "region", "limit": "limit"}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = manager
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Users: map[string][]string{alice: {"open", "orders"}}}); err != nil {
		t.Fatal(err)
	}
	settings := catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"operations"}}
	// The settings as a tool's arguments, with these watches.
	withWatches := func(watches string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(settings)
		var fields map[string]any
		json.Unmarshal(body, &fields)
		var list []any
		if err := json.Unmarshal([]byte(watches), &list); err != nil {
			t.Fatal(err)
		}
		fields["watches"] = list
		return fields
	}
	save := func(watches string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(withWatches(watches))
		return localRequest(app, "/api/assistant/action/save_assistant_settings", string(body), cookie)
	}
	w := save(`[]`)
	if w.Code != 200 {
		t.Fatalf("assistant setup: %d %s", w.Code, w.Body)
	}
	if err = json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct{ name, watches, argument string }{
		{"too-large", `[{"connection":"metrics","operation":"open","arguments":{}},{"connection":"metrics","operation":"orders","arguments":{"region":"west","limit":500}}]`, "limit"},
		{"missing", `[{"connection":"metrics","operation":"orders","arguments":{"limit":5}}]`, "region"},
		{"unknown", `[{"connection":"metrics","operation":"orders","arguments":{"region":"west","colour":"blue"}}]`, "colour"},
	} {
		w := save(bad.watches)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "orders") || !strings.Contains(w.Body.String(), bad.argument) {
			t.Errorf("%s: a watch with a bad argument was not refused by name: %d %s", bad.name, w.Code, w.Body)
		}
	}
	session := contractClient(t, app, ts.URL, alice, "notes:read notes:manage")
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "save_assistant_settings", Arguments: withWatches(`[{"connection":"metrics","operation":"orders","arguments":{"region":"west","limit":500}}]`)})
	if err != nil || !result.IsError || !strings.Contains(contractText(result), "orders") || !strings.Contains(contractText(result), "limit") {
		t.Fatalf("MCP saved a watch with a bad argument: %v %s", err, contractText(result))
	}
	stored, err := app.catalog.Assistant(alice)
	if err != nil || stored.Revision != settings.Revision || len(stored.Watches) != 0 {
		t.Fatalf("a refused save changed the settings: %+v %v", stored, err)
	}
	if w := save(`[{"connection":"metrics","operation":"orders","arguments":{"region":"west","limit":100}}]`); w.Code != 200 {
		t.Fatalf("a watch with good arguments: %d %s", w.Code, w.Body)
	}
	if stored, err = app.catalog.Assistant(alice); err != nil || len(stored.Watches) != 1 || stored.Watches[0].Operation != "orders" {
		t.Fatalf("saved watches: %+v %v", stored.Watches, err)
	}
}
