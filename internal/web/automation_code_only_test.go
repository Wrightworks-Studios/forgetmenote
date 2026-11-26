package web

import (
	"encoding/json"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/connectors"
)

func TestScriptCodeOnlyConnectorDoesNotBecomeAIOrChatEvidence(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"connectors.read", "ai.decisions"}, map[string][]string{"api": {"read"}}, nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"explicit":true}`))
	}))
	defer upstream.Close()
	bindScriptConnector(t, app, e, upstream.URL)
	for _, enabled := range []bool{false, true} {
		raw, _ := json.Marshal(map[string]any{"version": 1, "connections": []any{map[string]any{"id": "fixture-api", "kind": "http", "endpoint": upstream.URL, "codeOnly": enabled, "operations": []any{map[string]any{"name": "read", "readOnly": true, "path": "/case"}}}}})
		file := filepath.Join(t.TempDir(), "connections.json")
		os.WriteFile(file, raw, 0600)
		manager, err := connectors.Load(file)
		if err != nil {
			t.Fatal(err)
		}
		app.dataConnections = manager
		e.run.Script.Contract, err = app.scriptContract(e.run.Spec)
		if err != nil {
			t.Fatal(err)
		}
		_, err = hostStep(t, app, e, map[bool]string{false: "denied", true: "catalog"}[enabled], "connectors.read", map[string]any{"connection": "api", "operation": "read", "arguments": map[string]any{}})
		if (err == nil) != enabled {
			t.Fatalf("code-only permission=%v: %v", enabled, err)
		}
		if app.dataAllowed(chat.WithScope(e.ctx, &chat.RunScope{}), "fixture-api", "read") {
			t.Fatal("code-only data disclosed to chat")
		}
		actor, _ := automation.IdentityFrom(e.ctx)
		actor.Spec.Script, actor.Spec.Kind, actor.Spec.AIConnection = nil, "task", "provider"
		if app.dataAllowed(automation.WithIdentity(e.ctx, actor), "fixture-api", "read") {
			t.Fatal("code-only data disclosed to a non-script AI job")
		}
		manager.Close()
	}
}

func TestMCPCodeOnlyCatalogCannotBecomeAssistantEvidence(t *testing.T) {
	app, _ := fixture(t)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"explicit":true}`))
	}))
	defer upstream.Close()
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "catalog", Kind: "http", Endpoint: upstream.URL, CodeOnly: true, Operations: []connectors.Operation{{Name: "read", Path: "/", ReadOnly: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	app.dataConnections = manager
	if err = app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "catalog", Users: map[string][]string{"alice": {"read"}}}); err != nil {
		t.Fatal(err)
	}
	client := actionClient(t, app, "alice", "notes:read data:read")
	actionCall(t, client, "call_data_operation", map[string]any{"connection": "catalog", "operation": "read", "arguments": map[string]any{}}, true)
	if requests.Load() != 0 {
		t.Fatal("MCP assistant contacted code-only catalog")
	}
}

func TestAutomationRunnerKeepsCodeOnlyCatalogOutsideChat(t *testing.T) {
	app, e := scriptFixture(t, `def main(host):
    result = host.call('catalog', 'connectors.read', connection='api', operation='read', arguments={})
    return {'explicit': result['data']['explicit']}
`, []string{"connectors.read", "ai.decisions"}, map[string][]string{"api": {"read"}}, nil)
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Write([]byte(`{"explicit":true}`))
	}))
	defer upstream.Close()
	bindScriptConnector(t, app, e, upstream.URL)
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "fixture-api", Kind: "http", Endpoint: upstream.URL, CodeOnly: true, Operations: []connectors.Operation{{Name: "read", Path: "/case", ReadOnly: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	app.dataConnections = manager
	e.run.Script = nil
	if err = app.automationrun.RunAutomation(t.Context(), e.run, e.save); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || !strings.Contains(e.run.Result, `"explicit":true`) {
		t.Fatal(requests, e.run.Result)
	}
}
