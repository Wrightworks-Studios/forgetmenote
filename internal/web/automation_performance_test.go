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

func TestRecipePerformanceRepairStillRequiresCorrectResults(t *testing.T) {
	for _, correct := range []bool{false, true} {
		t.Run(map[bool]string{true: "correct", false: "incorrect"}[correct], func(t *testing.T) {
			const original = "# Count\n```sql count\nSELECT slow_count()\n```\n"
			const candidate = "# Count\n```sql count\nSELECT fast_count()\n```\n"
			aiCalls := 0
			app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				aiCalls++
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": candidate}, "finish_reason": "stop"}}})
			})
			app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var args map[string]string
				json.NewDecoder(r.Body).Decode(&args)
				n := 7
				if strings.Contains(args["sql"], "slow_count") {
					time.Sleep(200 * time.Millisecond)
				} else if !correct {
					n = 8
				}
				json.NewEncoder(w).Encode(map[string]any{"rows": [][]int{{n}}})
			}))
			defer upstream.Close()
			m, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "db", Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "query", ReadOnly: true, Method: "POST", Path: "/", Body: true, InputSchema: json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string"}},"required":["sql"],"additionalProperties":false}`)}}}}})
			if err != nil {
				t.Fatal(err)
			}
			app.dataConnections = m
			if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "db", Users: map[string][]string{"alice": {"query"}}}); err != nil {
				t.Fatal(err)
			}
			store, _ := app.catalog.Access("alice", "default", "edit")
			doc, err := store.Save(t.Context(), "count.md", []byte(original), "", "test")
			if err != nil {
				t.Fatal(err)
			}
			run := automation.Run{ID: automation.Hash(correct), Spec: automation.Spec{ID: "latency", Kind: "recipe", Actor: "alice", Project: "default", Recipe: doc.Path, Connections: []string{"db"}, AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Checks: []automation.Check{{Name: "count", Block: "count", Call: automation.Call{Connection: "db", Operation: "query"}, MaxDurationMS: 100, Assertions: []automation.Assertion{{Pointer: "/rows/0/0", Equals: json.RawMessage(`7`)}}}}}}
			err = app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil })
			if (err == nil) != correct || aiCalls != 1 {
				t.Fatal("incorrect acceptance", err, aiCalls)
			}
			saved, e := store.Read(t.Context(), doc.Path, "")
			if e != nil {
				t.Fatal(e)
			}
			if correct && string(saved.Content) != candidate || !correct && saved.Revision != doc.Revision {
				t.Fatal("performance repair bypassed correctness checks")
			}
		})
	}
}
