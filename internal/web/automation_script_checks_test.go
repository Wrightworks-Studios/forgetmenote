package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

// A script's web search through a research model connection is paid model
// work, so it follows the instance AI policy. A package that only searches
// does not use AI by its job definition, so the policy check before each run
// and each host operation passes: the search path's own check is the only one.
func TestScriptPaidSearchFollowsInstanceAIPolicy(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"web.search"}, nil, nil)
	var searches atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{
			map[string]any{"type": "web_search_call", "status": "completed", "action": map[string]any{"type": "search", "query": "fixture", "sources": []any{map[string]string{"type": "url", "url": "https://example.org/original"}}}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "One source."}}},
		}, "usage": map[string]int{"input_tokens": 100, "output_tokens": 10}})
	}))
	defer provider.Close()
	cfg := map[string]any{"defaultConnection": "local", "connections": []any{map[string]any{"id": "local", "label": "Fixture", "format": "responses", "baseURL": provider.URL, "models": []string{"gpt-6-luna"}, "defaultModel": "gpt-6-luna", "local": true, "limits": map[string]int{"contextTokens": 65536}}}, "research": map[string]any{"enabled": true, "format": "openai", "connection": "local", "maxSearches": 2, "maxFetches": 4}}
	raw, _ := json.Marshal(cfg)
	file := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(file); err != nil {
		t.Fatal(err)
	}
	if e.run.Spec.UsesAI() {
		t.Fatal("the search-only job uses AI by its definition, so the run's own policy check would refuse it too")
	}
	policy, err := app.catalog.SetAISettings(catalog.Bootstrap, catalog.AISettings{Writing: true})
	if err != nil {
		t.Fatal(err)
	}
	search := map[string]any{"query": "fixture original", "limit": 1}
	if _, err = hostStep(t, app, e, "allowed", "web.search", search); err != nil || searches.Load() != 1 {
		t.Fatalf("search with instance AI writing on: %v after %d searches", err, searches.Load())
	}
	policy.Writing = false
	if _, err = app.catalog.SetAISettings(catalog.Bootstrap, policy); err != nil {
		t.Fatal(err)
	}
	if _, err = hostStep(t, app, e, "refused", "web.search", search); !errors.Is(err, errInstanceAI) {
		t.Fatalf("search with instance AI writing off: %v", err)
	}
	if n := searches.Load(); n != 1 {
		t.Fatalf("the search provider was contacted again with instance AI writing off: %d searches", n)
	}
	if step := e.run.Script.Steps["refused"]; step == nil || step.State != "failed" {
		t.Fatalf("refused search step: %+v", step)
	}
}

// A script holds its data grants for its whole run, not only for its
// connector calls. Once the job's actor loses its grant on a connection the
// package uses, the next host operation is refused, even one that calls no
// connector in a run that has read nothing from it yet.
func TestScriptStopsOnceItsDataGrantIsRevoked(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"connectors.read", "notebooks.publish"}, map[string][]string{"api": {"read"}}, map[string]string{"notes": "edit"})
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the connection was called")
	}))
	defer upstream.Close()
	bindScriptOperations(t, app, e, upstream.URL, scriptReadOperation)
	publish := func(step, path string) error {
		_, err := hostStep(t, app, e, step, "notebooks.publish", map[string]any{"notebook": "notes", "path": path, "content": "# Published\n", "revision": ""})
		return err
	}
	if err := publish("before", "before.md"); err != nil {
		t.Fatal(err)
	}
	grant, err := app.catalog.ConnectionAccess(catalog.Bootstrap, "fixture-api")
	if err != nil {
		t.Fatal(err)
	}
	delete(grant.Users, e.run.Spec.Actor)
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, grant); err != nil {
		t.Fatal(err)
	}
	if err = publish("after", "after.md"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("publish after the data grant was revoked: %v", err)
	}
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Read(t.Context(), "before.md", ""); err != nil {
		t.Fatal("the publish before the revocation is missing:", err)
	}
	if _, err = store.Read(t.Context(), "after.md", ""); err == nil {
		t.Fatal("the script published after its data grant was revoked")
	}
	if _, recorded := e.run.Script.Steps["after"]; recorded {
		t.Fatal("the refused step was recorded as started")
	}
}

// An automatic package update is validated as saving its definition would be.
// A job that reads another notebook its service account may no longer read
// keeps its installed definition: only validation checks the job's read
// notebooks, because the package reads check its source and bound notebooks.
func TestAutomaticPackageUpdateThatFailsValidationIsNotApplied(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "access-kept", true: "access-revoked"}[revoked], func(t *testing.T) {
			app, advance, installed := automaticPackageFixture(t, true)
			actor := installed.Spec.Actor
			if err := app.catalog.Create(catalog.Bootstrap, "reference", "Reference"); err != nil {
				t.Fatal(err)
			}
			if err := app.catalog.Member(catalog.Bootstrap, "reference", actor, "read"); err != nil {
				t.Fatal(err)
			}
			spec := installed.Spec
			spec.ReadProjects = []string{"reference"}
			if err := app.validateAutomationDefinition(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			saved, err := app.automations.Definitions.Save(spec, installed.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if revoked {
				if err = app.catalog.Member(catalog.Bootstrap, "reference", actor, ""); err != nil {
					t.Fatal(err)
				}
			}
			revision := advance(map[string]string{"package/main.py": "def main(host): return {'version': 2}"})
			err = app.automationrun.RefreshPackage(t.Context(), saved)
			current, _ := app.automations.Definitions.Get(spec.ID)
			if !revoked {
				if err != nil || current.Spec.Script.Revision != revision {
					t.Fatalf("valid update not applied: %v, revision %s", err, current.Spec.Script.Revision)
				}
				return
			}
			if !errors.Is(err, workspace.ErrForbidden) {
				t.Fatalf("update for a service account that lost a read notebook: %v", err)
			}
			if current.Revision != saved.Revision || current.Spec.Script.Revision != saved.Spec.Script.Revision || current.Spec.Script.Environment != saved.Spec.Script.Environment {
				t.Fatal("an update its validation refuses replaced the installed definition", current)
			}
		})
	}
}
