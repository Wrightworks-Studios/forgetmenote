package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
)

func TestScheduledMonitorDeliversOnlyMatchingResultsToAuthorizedRecipient(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	config, _ := json.Marshal(app.slack.config)
	var raw map[string]any
	json.Unmarshal(config, &raw)
	raw["directMessages"] = map[string]any{"U123": map[string]any{"project": "default", "aiConnection": "local", "model": "synthetic", "externalProcessing": true, "connections": []string{"health"}}}
	config, _ = json.Marshal(raw)
	file := filepath.Join(t.TempDir(), "slack.json")
	os.WriteFile(file, config, 0600)
	if err := app.ConfigureSlack(file); err != nil {
		t.Fatal(err)
	}
	count := 0
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			json.NewEncoder(w).Encode(map[string]int{"errors": count})
			return
		}
		posts++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if !strings.HasSuffix(r.URL.Path, "chat.postMessage") || body["channel"] != "U123" || body["text"] != "Alert: errors" {
			t.Error("monitor did not use private delivery", body)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	app.slack.endpoint = server.URL + "/chat.postEphemeral"
	m, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "health", Kind: "http", Endpoint: server.URL, Operations: []connectors.Operation{{Name: "read", ReadOnly: true, Path: "/health"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = m
	grant, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "health", Users: map[string][]string{"alice": {"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	var spec automation.Spec
	json.Unmarshal([]byte(`{"id":"errors","kind":"monitor","actor":"alice","project":"default","connections":["health"],"calls":[{"connection":"health","operation":"read"}],"notifySlackUser":"U123","condition":{"pointer":"/errors","greaterThan":0}}`), &spec)
	if err = spec.Validate(); err != nil {
		t.Fatal(err)
	}
	run := automation.Run{ID: automation.Hash("quiet"), Spec: spec}
	if err = app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil || posts != 0 {
		t.Fatal("quiet monitor sent or failed", err, posts)
	}
	count = 2
	run = automation.Run{ID: automation.Hash("alert"), Spec: spec}
	if err = app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil || posts != 1 {
		t.Fatal("alert missing", err, posts)
	}
	if len(run.Receipts) != 1 || run.Receipts[0].Connection != "health" {
		t.Fatal("monitor lost private execution evidence", run.Receipts)
	}
	if err = app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil || posts != 1 {
		t.Fatal("alert replayed", err, posts)
	}
	grant.Users = nil
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, grant); err != nil {
		t.Fatal(err)
	}
	if err = app.runAutomation(t.Context(), &run, func() error { return nil }); err == nil || posts != 1 {
		t.Fatal("revocation ignored", err, posts)
	}
}
