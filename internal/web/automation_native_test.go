package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/reposource"
)

func TestAutomationIndependentCheckSuiteRejectsChangedResult(t *testing.T) {
	app, _ := fixture(t)
	different := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := 7
		if r.URL.Path == "/candidate" && different {
			v = 8
		}
		json.NewEncoder(w).Encode(map[string]any{"rows": []any{[]int{v}}})
	}))
	defer server.Close()
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: server.URL, Operations: []connectors.Operation{{Name: "candidate", Path: "/candidate", ReadOnly: true}, {Name: "reference", Path: "/reference", ReadOnly: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = manager
	spec := automation.Spec{ID: "contracts", Kind: "checks", Actor: catalog.Bootstrap, Project: "default", Connections: []string{"metrics"}, Checks: []automation.Check{{Name: "same-data", Call: automation.Call{Connection: "metrics", Operation: "candidate"}, Reference: &automation.Call{Connection: "metrics", Operation: "reference"}}}}
	if err = spec.Validate(); err != nil {
		t.Fatal(err)
	}
	run := automation.Run{ID: automation.Hash("first"), Spec: spec, DryRun: true}
	if err = app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil || !strings.Contains(run.Result, "same-data") {
		t.Fatal(err, run.Result)
	}
	different = true
	run = automation.Run{ID: automation.Hash("second"), Spec: spec, DryRun: true}
	if err = app.runAutomation(t.Context(), &run, func() error { return nil }); err == nil {
		t.Fatal("different result passed")
	}
}

func TestCheckReportPreservesConcurrentEdit(t *testing.T) {
	app, store := fixture(t)
	first, err := store.Save(t.Context(), "report.md", []byte("before"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, e := store.Save(context.Background(), first.Path, []byte("human edit"), first.Revision, "human")
		if e != nil {
			t.Error(e)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	app.dataConnections, err = connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: server.URL, Operations: []connectors.Operation{{Name: "read", ReadOnly: true, Path: "/"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	r := automation.Run{ID: automation.Hash("concurrent"), Spec: automation.Spec{ID: "check", Kind: "checks", Actor: catalog.Bootstrap, Project: "default", Output: "report.md", Connections: []string{"metrics"}, Checks: []automation.Check{{Name: "healthy", Call: automation.Call{Connection: "metrics", Operation: "read"}, Assertions: []automation.Assertion{{Pointer: "/ok", Equals: json.RawMessage(`true`)}}}}}}
	if err = app.runAutomation(t.Context(), &r, func() error { return nil }); err == nil {
		t.Fatal("report replaced concurrent edit")
	}
	doc, err := store.Read(t.Context(), first.Path, "")
	if err != nil || string(doc.Content) != "human edit" {
		t.Fatal(err, string(doc.Content))
	}
}

func TestAutomationNativeHealthRunsWithoutModel(t *testing.T) {
	app, _ := fixture(t)
	run := automation.Run{ID: automation.Hash("health"), DryRun: true, Spec: automation.Spec{ID: "health", Kind: "native", Actor: catalog.Bootstrap, Project: "default", Native: &automation.NativeTask{Task: "notebook-health"}}}
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil || !strings.Contains(run.Result, "revision") {
		t.Fatal(err, run.Result)
	}
}

// Without repository sources a refresh automation fails as the unavailable
// service, as a browser request for repository sources does.
func TestAutomationRepositoryRefreshWithoutSourcesIsUnavailable(t *testing.T) {
	app, _ := fixture(t)
	run := automation.Run{ID: automation.Hash("refresh"), DryRun: true, Spec: automation.Spec{ID: "refresh", Kind: "native", Actor: catalog.Bootstrap, Project: "default", Native: &automation.NativeTask{Task: "repository-refresh"}}}
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); !errors.Is(err, reposource.ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestAutomationHealthDoesNotHideFailureBehindNextQueuedRun(t *testing.T) {
	app, _ := fixture(t)
	spec := automation.Spec{ID: "digest", Kind: "digest", Actor: catalog.Bootstrap, Project: "default", Output: "digest.md", Calls: []automation.Call{{Connection: "source", Operation: "read"}}}
	if _, err := app.automations.Queue.Enqueue(spec, "failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.automations.Queue.RunNext(t.Context(), func(context.Context, *automation.Run, func() error) error { return errors.New("upstream unavailable") }); err != nil {
		t.Fatal(err)
	}
	if _, err := app.automations.Queue.Enqueue(spec, "next"); err != nil {
		t.Fatal(err)
	}
	r := automation.Run{ID: automation.Hash("health"), DryRun: true, Spec: automation.Spec{ID: "health", Kind: "native", Actor: catalog.Bootstrap, Project: "default", Native: &automation.NativeTask{Task: "automation-health"}}}
	if err := app.runAutomation(t.Context(), &r, func() error { return nil }); err != nil || r.Alert == nil || !r.Alert.Incident.Active {
		t.Fatal("failure masked by next scheduled run", r.Alert, err)
	}
}
