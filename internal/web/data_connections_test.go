package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
)

func TestDataConnectionsAvailableWithoutConfiguration(t *testing.T) {
	app, _ := fixture(t)
	w := apiCall(app, "POST", "/api/v1/actions/list_data_connections", `{}`, testToken)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("unconfigured connections: %d %s", w.Code, w.Body.String())
	}
}

func TestMCPDataOperationSchemasRemainJSONObjects(t *testing.T) {
	app, _ := fixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"value":7}`))
	}))
	defer upstream.Close()
	input := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	output := json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`)
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{
		ID: "metrics", Kind: "http", Endpoint: upstream.URL,
		Operations: []connectors.Operation{{Name: "read", Method: "GET", Path: "/", ReadOnly: true, InputSchema: input, OutputSchema: output}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	app.dataConnections = manager
	if err = app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Users: map[string][]string{"alice": {"read"}}}); err != nil {
		t.Fatal(err)
	}
	client := actionClient(t, app, "alice", "notes:read data:read")
	raw := actionCall(t, client, "list_data_operations", map[string]any{"connection": "metrics"}, false)
	var operations []connectors.Operation
	if err := json.Unmarshal(raw, &operations); err != nil || len(operations) != 1 {
		t.Fatalf("operation discovery: %s (%v)", raw, err)
	}
	for name, schema := range map[string]json.RawMessage{"input": operations[0].InputSchema, "output": operations[0].OutputSchema} {
		var value map[string]any
		if err := json.Unmarshal(schema, &value); err != nil || value["type"] != "object" {
			t.Fatalf("%s schema: %s (%v)", name, schema, err)
		}
	}
	raw = actionCall(t, client, "call_data_operation", map[string]any{"connection": "metrics", "operation": operations[0].Name, "arguments": map[string]any{}}, false)
	var result struct {
		Data    struct{ Value int }
		Receipt connectors.Receipt
	}
	if err := json.Unmarshal(raw, &result); err != nil || !result.Receipt.Complete || result.Data.Value != 7 {
		t.Fatalf("discovered operation: %s (%v)", raw, err)
	}
	// Model adapters own their pins, while external MCP clients retain their
	// explicit contract precondition and must be told to rediscover after drift.
	actionCall(t, client, "call_data_operation", map[string]any{"connection": "metrics", "operation": "read", "arguments": map[string]any{}, "schemaHash": "obsolete-contract"}, true)
	modelTool, ok := app.actionTool("call_data_operation")
	if !ok || strings.Contains(string(modelTool.Schema), `"schemaHash"`) || app.actions["call_data_operation"].Schema.Properties["schemaHash"] == nil {
		t.Fatal("model projection changed the public MCP contract")
	}
}

func TestDataConnectionsEnforceGrantsAndCredentialConnectionScope(t *testing.T) {
	app, _ := fixture(t)
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.Write([]byte(`{"value":7}`)) }))
	defer upstream.Close()
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: upstream.URL, Operations: []connectors.Operation{{Name: "read", Method: "GET", Path: "/", ReadOnly: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = manager
	if err = app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "read")
	grant, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Users: map[string][]string{"alice": {"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	spec := CredentialSpec{Name: "client", Projects: []string{"default"}, Connections: []string{"metrics"}, Scopes: []string{"notes:read", "data:read"}, Audiences: []string{"api"}, Days: 1}
	credential, err := issueCredential(app.catalog, app.oauth, "alice", spec)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"connection":"metrics","operation":"read","arguments":{}}`
	w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", body, credential.Token)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"value":7`) {
		t.Fatal(w.Code, w.Body.String())
	}
	spec.Connections = nil
	limited, err := issueCredential(app.catalog, app.oauth, "alice", spec)
	if err != nil {
		t.Fatal(err)
	}
	if w = apiCall(app, "POST", "/api/v1/actions/call_data_operation", body, limited.Token); w.Code != 403 {
		t.Fatal("credential gained unlisted connection", w.Code, w.Body.String())
	}
	grant.Users = map[string][]string{}
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, grant); err != nil {
		t.Fatal(err)
	}
	if w = apiCall(app, "POST", "/api/v1/actions/call_data_operation", body, credential.Token); w.Code != 403 {
		t.Fatal("revoked connection executed", w.Code, w.Body.String())
	}
	if requests != 1 {
		t.Fatal("unauthorized upstream calls", requests)
	}
	ctx := context.WithValue(context.Background(), identityKey{}, "alice")
	raw, _ := json.Marshal(app.dataList(ctx))
	if string(raw) != "[]" {
		t.Fatal("revoked connection discovered", string(raw))
	}
}

func TestDataCredentialScopesAreExplicit(t *testing.T) {
	spec := CredentialSpec{Name: "data client", Projects: []string{"default"}, Scopes: []string{"notes:read", "data:read"}, Audiences: []string{"api"}, Days: 1}
	if err := spec.Validate(); err != nil {
		t.Fatalf("data scope rejected: %v", err)
	}
}

func TestDataConnectionAccessCanBeRevokedWhileUpstreamIsOffline(t *testing.T) {
	app, _ := fixture(t)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "mcp", Endpoint: upstream.URL, Operations: []connectors.Operation{{Name: "read", ReadOnly: true}, {Name: "other", ReadOnly: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	app.dataConnections = manager
	if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	grant, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Users: map[string][]string{"alice": {"read", "other"}}})
	if err != nil {
		t.Fatal(err)
	}
	grant.Users["alice"] = []string{"other"}
	raw, _ := json.Marshal(grant)
	w := apiCall(app, "POST", "/api/v1/actions/set_data_connection_access", string(raw), testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("revoking access required a working upstream: %d %s", w.Code, w.Body.String())
	}
	if app.catalog.ConnectionAllowed("alice", "metrics", "read") || !app.catalog.ConnectionAllowed("alice", "metrics", "other") {
		t.Fatal("revocation did not preserve the remaining configured operation")
	}
	if err = json.Unmarshal(w.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	grant.Users["alice"] = []string{"unconfigured"}
	raw, _ = json.Marshal(grant)
	w = apiCall(app, "POST", "/api/v1/actions/set_data_connection_access", string(raw), testToken)
	if w.Code == http.StatusOK {
		t.Fatal("unconfigured operation was granted")
	}
	if requests.Load() != 0 {
		t.Fatal("grant administration contacted the upstream", requests.Load())
	}
}
