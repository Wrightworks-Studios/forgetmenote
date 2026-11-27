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
	"forgetmenote/internal/workspace"
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
	_, err = app.automations.Queue.RunNext(context.Background(), app.automationrun.RunAutomation)
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
	err := app.automationrun.RunAutomation(context.Background(), &run, func() error { b, _ := json.Marshal(run); json.Unmarshal(b, &checkpoint); return nil })
	if err != nil || !strings.Contains(run.Result, "Generated note") {
		t.Fatal(err, run.Result)
	}
	checkpoint.Publication.Content = ""
	if err = app.automationrun.RunAutomation(context.Background(), &checkpoint, func() error { return nil }); err == nil {
		t.Fatal("ambiguous generation was repeated")
	}
}

// A preview shows what its run would publish only while the run may still
// publish it. One whose actor loses its session after the answer is written,
// as the answer's checkpoint is saved, fails its final check and keeps
// nothing of the answer.
func TestAutomationPreviewRefusedAtItsFinalCheckKeepsNoResult(t *testing.T) {
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "# Generated note"}, "finish_reason": "stop"}}})
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	run := automation.Run{ID: strings.Repeat("b", 64), DryRun: true, Spec: automation.Spec{ID: "task", Kind: "task", Actor: "alice", Project: "default", AIConnection: "local", ExternalProcessing: true, Instruction: "Write a short note", Output: "task.md"}}
	save := func() error {
		if run.Publication != nil && run.Publication.Content != "" {
			return app.catalog.Revoke(catalog.Bootstrap, "alice")
		}
		return nil
	}
	if err := app.automationrun.RunAutomation(context.Background(), &run, save); err == nil || run.Result != "" {
		t.Fatalf("refused preview: %v, kept %q", err, run.Result)
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
	// The robot may edit the reference notebook, but this job only reads it.
	for project, role := range map[string]string{"default": "read", "reference": "edit"} {
		if err := app.catalog.Member(catalog.Bootstrap, project, "robot", role); err != nil {
			t.Fatal(err)
		}
	}
	r := automation.Run{Spec: automation.Spec{ID: "curator", Kind: "task", Actor: "robot", Project: "default", ReadProjects: []string{"reference"}, ExternalProcessing: true}}
	ctx, err := app.automationContext(t.Context(), r.Spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = app.automationrun.SourceContext(ctx, &r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.chatrun.DocumentScope(ctx, "reference", "notes", "", "", "facts.md"); err != nil {
		t.Fatal("configured input notebook is outside model corpus", err)
	}
	if _, err = app.clientAccess(ctx, "robot", "reference", "edit"); err == nil {
		t.Fatal("job may write to a notebook it only reads")
	}
	if err = app.catalog.Member(catalog.Bootstrap, "reference", "robot", ""); err != nil {
		t.Fatal(err)
	}
	if err = app.checkAutomationEvidence(ctx, &r, true); err == nil {
		t.Fatal("revoked input still available for publication")
	}
}

// moveNotebookAuthority completes a handoff of project's authority to a
// repository, which starts the notebook's next authority generation.
func moveNotebookAuthority(ctx context.Context, app *Server, project string) error {
	store, err := app.catalog.Access(catalog.Bootstrap, project, "edit")
	if err != nil {
		return err
	}
	saved, err := store.Save(ctx, "handoff.md", []byte("# Handoff\n"), "", "fixture")
	if err != nil {
		return err
	}
	p, err := app.catalog.PreviewAuthority(catalog.Bootstrap, project, catalog.AuthorityTransfer{To: "github", Revision: saved.Revision})
	if err != nil {
		return err
	}
	if p, _, err = app.catalog.BeginAuthority(catalog.Bootstrap, project, p.ID); err != nil {
		return err
	}
	if _, err = store.PrepareAuthority(ctx, workspace.AuthorityRequest{ID: p.ID, Generation: p.Generation, From: p.From, To: p.To, Revision: p.Revision}); err != nil {
		return err
	}
	if p, err = app.catalog.PublishAuthority(catalog.Bootstrap, project, p.ID); err != nil {
		return err
	}
	if _, err = store.FinishAuthority(ctx, p.ID, true); err != nil {
		return err
	}
	return app.catalog.CompleteAuthority(p.Project, p.ID, p.Revision)
}

// A run's authority is the actor's session, its notebook access and the
// notebook's authority generation as the run started. A preview whose live
// read finds any of them changed stops there and keeps no result.
func TestAutomationPreviewStopsWhenItsActorsAuthorityChanges(t *testing.T) {
	for _, change := range []string{"session revoked", "access lowered", "authority moved"} {
		t.Run(change, func(t *testing.T) {
			app, _ := fixture(t)
			if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "robot"}); err != nil {
				t.Fatal(err)
			}
			if err := app.catalog.Member(catalog.Bootstrap, "default", "robot", "edit"); err != nil {
				t.Fatal(err)
			}
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var err error
				switch change {
				case "session revoked":
					err = app.catalog.Revoke(catalog.Bootstrap, "robot")
				case "access lowered":
					err = app.catalog.Member(catalog.Bootstrap, "default", "robot", "read")
				case "authority moved":
					err = moveNotebookAuthority(r.Context(), app, "default")
				}
				if err != nil {
					t.Error(err)
				}
				w.Write([]byte(`{"count":7}`))
			}))
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
			if _, err = app.automations.Queue.RunNext(t.Context(), app.automationrun.RunAutomation); err != nil {
				t.Fatal(err)
			}
			run, err := app.automations.Queue.Get(queued.ID)
			if err != nil || run.State != "failed" || run.Result != "" || calls != 1 {
				t.Fatalf("preview %s (%s) kept %q after %d reads: %v", run.State, run.Error, run.Result, calls, err)
			}
		})
	}
}
