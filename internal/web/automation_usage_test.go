package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
)

func TestLiveRecipeUsageQueuesBoundedChecksWithoutPersistingInputs(t *testing.T) {
	app, _ := fixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Millisecond)
		w.Write([]byte(`{"rows":[[7]]}`))
	}))
	defer upstream.Close()
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "fixture", Kind: "http", Endpoint: upstream.URL, Operations: []connectors.Operation{{Name: "query", ReadOnly: true, Path: "/", InputSchema: json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string"}},"required":["sql"],"additionalProperties":false}`)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = manager
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	doc, err := store.Save(t.Context(), "recipe.md", []byte("# Recipe\n```sql count\nselect 7\n```\n"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = app.initAutomations(); err != nil {
		t.Fatal(err)
	}
	var spec automation.Spec
	json.Unmarshal([]byte(`{"id":"check-usage","kind":"recipe","actor":"`+catalog.Bootstrap+`","project":"default","recipe":"recipe.md","connections":["fixture"],"usage":{"slowMs":1,"errors":true,"cooldownSeconds":3600},"checks":[{"name":"count","block":"count","call":{"connection":"fixture","operation":"query"},"assertions":[{"pointer":"/rows/0/0","equals":7}]}]}`), &spec)
	setTestAutomationSpec(t, app, spec)
	body, _ := json.Marshal(dataCall{Project: "default", Connection: "fixture", Operation: "query", Arguments: map[string]any{"sql": "select 'sensitive-runtime-value'"}, ProfilePath: "recipe.md", ProfileRevision: doc.Revision})
	for i := 0; i < 2; i++ {
		w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", string(body), testToken)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 1 {
		t.Fatalf("slow recipe calls should enqueue one maintenance run: %d %v", len(runs), err)
	}
	var raw []byte
	app.db.Pool.QueryRow(t.Context(), `SELECT body FROM automation_records WHERE org = $1 AND name = $2`, app.db.Org, runs[0].ID+".json").Scan(&raw)
	if strings.Contains(string(raw), "sensitive-runtime-value") || !strings.Contains(string(raw), doc.Revision) {
		t.Fatal("observation retained arguments or lost provenance")
	}
	if _, err = app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil {
		t.Fatal(err)
	}
	q, err := automation.Open(app.db)
	if err != nil {
		t.Fatal(err)
	}
	app.automations.Queue = q
	if w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", string(body), testToken); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	runs, _ = q.List()
	if len(runs) != 1 || runs[0].State != "completed" {
		t.Fatal("cooldown lost after restart or checks recursed", runs)
	}
}
