package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
)

func TestAutomationPreviewExecutesReadWithoutPublishing(t *testing.T) {
	app, store := fixture(t)
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "robot"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "robot", "edit"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(`{"count":7}`)) }))
	defer upstream.Close()
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: upstream.URL, Operations: []connectors.Operation{{Name: "read", ReadOnly: true, Method: "GET", Path: "/"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = manager
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Users: map[string][]string{"robot": {"read"}}}); err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "preview", Kind: "digest", Actor: "robot", Project: "default", Disabled: true, Connections: []string{"metrics"}, Calls: []automation.Call{{Connection: "metrics", Operation: "read"}}, Output: "output.md"}
	queued, err := app.automations.Queue.EnqueuePreview(spec, "one")
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.automations.Queue.RunNext(context.Background(), app.runAutomation)
	if err != nil {
		t.Fatal(err)
	}
	run, err := app.automations.Queue.Get(queued.ID)
	if err != nil || run.State != "completed" || !strings.Contains(run.Result, `"count": 7`) || calls != 1 {
		t.Fatal("preview", run.State, run.Error, run.Result, calls, err)
	}
	if _, err = store.Read(context.Background(), "output.md", ""); !os.IsNotExist(err) {
		t.Fatal("preview published a note", err)
	}
}

func TestAutomationTaskPinsRevisionBeforeModel(t *testing.T) {
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "# Generated note"}, "finish_reason": "stop"}}})
	})
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit")
	run := automation.Run{ID: strings.Repeat("a", 64), DryRun: true, Spec: automation.Spec{ID: "task", Kind: "task", Actor: "alice", Project: "default", AIConnection: "local", ExternalProcessing: true, Instruction: "Write a short note", Output: "task.md"}}
	var checkpoint automation.Run
	err := app.runAutomation(context.Background(), &run, func() error { b, _ := json.Marshal(run); json.Unmarshal(b, &checkpoint); return nil })
	if err != nil || !strings.Contains(run.Result, "Generated note") {
		t.Fatal(err, run.Result)
	}
	checkpoint.Publication.Content = ""
	if err = app.runAutomation(context.Background(), &checkpoint, func() error { return nil }); err == nil {
		t.Fatal("ambiguous generation was repeated")
	}
}

func TestAutomationInputNotebooksAreReadableAndRevocationApplies(t *testing.T) {
	app, _ := fixture(t)
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "robot"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Create(catalog.Bootstrap, "reference", "Reference"); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"default", "reference"} {
		if err := app.catalog.Member(catalog.Bootstrap, project, "robot", "read"); err != nil {
			t.Fatal(err)
		}
	}
	r := automation.Run{Spec: automation.Spec{ID: "curator", Kind: "task", Actor: "robot", Project: "default", ReadProjects: []string{"reference"}, ExternalProcessing: true}}
	ctx, err := app.automationContext(t.Context(), r.Spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = app.automationSourceContext(ctx, &r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.documentChatScope(ctx, "reference", "notes", "", "", "facts.md"); err != nil {
		t.Fatal("configured input notebook is outside model corpus", err)
	}
	if _, err = app.clientAccess(ctx, "robot", "reference", "edit"); err == nil {
		t.Fatal("read input writable")
	}
	if err = app.catalog.Member(catalog.Bootstrap, "reference", "robot", ""); err != nil {
		t.Fatal(err)
	}
	if err = app.checkAutomationEvidence(ctx, &r, true); err == nil {
		t.Fatal("revoked input still available for publication")
	}
}
