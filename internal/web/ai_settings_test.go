package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/judgments"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
)

func TestInstanceAIControlsExistingAndFutureNotebooks(t *testing.T) {
	app, store := fixture(t)
	cookie := recoverySession(t, app)
	request := func(action, body string, code int) map[string]any {
		t.Helper()
		w := localRequest(app, "/api/organization/action/"+action, body, cookie)
		if w.Code != code {
			t.Fatalf("%s: HTTP %d, want %d: %s", action, w.Code, code, w.Body.String())
		}
		var out map[string]any
		if code == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	request("ai_settings", `{}`, 200)
	if _, err := store.Save(context.Background(), "note.md", []byte("# A saved notebook passage\n"), "", "test"); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model string
			Input []string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		vectors := make([][]float32, len(in.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": in.Model, "embeddings": vectors})
	}))
	defer provider.Close()
	if err := app.ConfigureEmbeddings(provider.URL+"/api/embed", "synthetic"); err != nil {
		t.Fatal(err)
	}
	request("set_ai_settings", `{"config":{"revision":0,"embeddings":true,"model":"synthetic","dimensions":3}}`, 200)
	request("set_ai_settings", `{"config":{"revision":0}}`, 409)
	request("set_ai_settings", `{"config":{"revision":1,"embeddings":true,"model":"different","dimensions":3}}`, 400)
	if inbox, err := app.catalog.EnsureInbox(catalog.Bootstrap); err != nil {
		t.Fatal(err)
	} else {
		w := oidcRequest(app, "GET", "/p/"+inbox.ID+"/api/knowledge/status", "", cookie)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
			t.Fatalf("private inbox opted in: HTTP %d: %s", w.Code, w.Body.String())
		}
	}
	if err := app.catalog.Create(catalog.Bootstrap, "future", "Future notebook"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"default", "future"} {
		w := oidcRequest(app, "GET", "/p/"+id+"/api/knowledge/status", "", cookie)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"model":"synthetic"`) || !strings.Contains(w.Body.String(), `"enabled":true`) {
			t.Fatalf("%s did not inherit AI policy: HTTP %d: %s", id, w.Code, w.Body.String())
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := oidcRequest(app, "GET", "/p/default/api/knowledge/status", "", cookie)
		if strings.Contains(w.Body.String(), `"indexed":1`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("embedding did not complete: HTTP %d: %s", w.Code, w.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	request("set_ai_settings", `{"config":{"revision":1,"embeddings":false,"model":"synthetic","dimensions":3}}`, 200)
	w := localRequest(app, "/p/default/api/knowledge/config", `{"config":{"enabled":true,"model":"synthetic","dimensions":3,"passageRunes":1400,"splitter":"markdown-heading-v1","endpoint":true}}`, cookie)
	if w.Code != 409 {
		t.Fatalf("notebook overrode global policy: HTTP %d: %s", w.Code, w.Body.String())
	}
	if _, err := app.indexes.Endpoint().EmbedModel(context.Background(), "synthetic", 3, []string{"blocked"}, false); err == nil {
		t.Fatal("disabled global embeddings still sent content")
	}
}

func TestInstanceAIAdoptionRequiresConfirmationForChangedEmbeddingContract(t *testing.T) {
	app, store := fixture(t)
	e, releaseEngine, err := app.acquireEngine(t.Context(), store)
	if releaseEngine != nil {
		t.Cleanup(releaseEngine)
	}

	if err != nil {
		t.Fatal(err)
	}
	config := knowledge.Config{Enabled: true, Model: "embeddinggemma", Dimensions: 3, PassageRunes: 1400, Splitter: "markdown-heading-v1", Endpoint: false}
	if _, err = e.Configure(context.Background(), config, false); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("rejected adoption sent content") }))
	defer provider.Close()
	if err = app.ConfigureEmbeddings(provider.URL+"/api/embed", ""); err != nil {
		t.Fatal(err)
	}
	w := localRequest(app, "/api/organization/action/set_ai_settings", `{"config":{"revision":0,"embeddings":true,"model":"embeddinggemma","dimensions":3}}`, recoverySession(t, app))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "rebuild") {
		t.Fatalf("changed embedding contract did not require confirmation: HTTP %d: %s", w.Code, w.Body.String())
	}
	if _, managed := app.catalog.AISettings(); managed {
		t.Fatal("rejected adoption changed policy")
	}
	if e.Config() != config {
		t.Fatal("rejected adoption changed notebook index")
	}
	w = localRequest(app, "/api/organization/action/set_ai_settings", `{"config":{"revision":0,"embeddings":false,"model":"embeddinggemma","dimensions":3}}`, recoverySession(t, app))
	if w.Code != 200 || e.Config().ID() != config.ID() || e.Config().Enabled {
		t.Fatalf("disabling legacy embeddings changed their cache identity: HTTP %d: %s", w.Code, w.Body.String())
	}
}

func TestInstanceAIRepositoryPolicyPreservesFiltersAndBlocksNotebookOverrides(t *testing.T) {
	app, _ := fixture(t)
	configureSearchSource(t, app)
	m := app.repositories
	m.SetIndexPolicy(app.sourceIndexPolicy)
	state, err := m.Status("default")
	if err != nil {
		t.Fatal(err)
	}
	state, err = m.ConfigureIndex("default", catalog.Bootstrap, state.Generation, reposource.IndexConfig{Exclude: []string{"private/"}})
	if err != nil {
		t.Fatal(err)
	}
	config, _ := app.catalog.AISettings()
	config.MapClassification, config.Embeddings, config.Rerank = true, true, true
	if _, err = app.catalog.SetAISettings(catalog.Bootstrap, config); err != nil {
		t.Fatal(err)
	}
	state, err = m.Status("default")
	if err != nil {
		t.Fatal(err)
	}
	if !state.IndexConfig.Embeddings || !state.IndexConfig.MapJudgments || !state.IndexConfig.Judgments || state.MapActor != catalog.Bootstrap || len(state.IndexConfig.Exclude) != 1 {
		t.Fatalf("policy not inherited: %+v", state)
	}
	state, err = m.ConfigureIndex("default", catalog.Bootstrap, state.Generation, reposource.IndexConfig{Exclude: []string{"secret/"}})
	if err != nil {
		t.Fatal(err)
	}
	if !state.IndexConfig.Embeddings || state.IndexConfig.Exclude[0] != "secret/" {
		t.Fatalf("path edit overrode policy: %+v", state)
	}
	config, _ = app.catalog.AISettings()
	config.Embeddings, config.MapClassification, config.Rerank = false, false, false
	if _, err = app.catalog.SetAISettings(catalog.Bootstrap, config); err != nil {
		t.Fatal(err)
	}
	state, err = m.Status("default")
	if err != nil {
		t.Fatal(err)
	}
	if state.IndexConfig.ExternalProcessing || state.IndexConfig.Embeddings || state.IndexConfig.MapJudgments || state.MapActor != "" {
		t.Fatalf("disabled policy not applied: %+v", state)
	}
}

func TestInstanceAIDecisionsKeepFeatureAndNotebookPermissions(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	config, _ := app.catalog.AISettings()
	config.Rerank = true
	if _, err := app.catalog.SetAISettings(catalog.Bootstrap, config); err != nil {
		t.Fatal(err)
	}
	judge, _ := app.judgmentsConfigured()
	if err := app.judgmentsAuthorized(context.Background(), "alice", "default", "rerank", judge); err != nil {
		t.Fatal(err)
	}
	for _, feature := range []string{"citations", "repository-map", "consistency", "organization"} {
		if err := app.judgmentsAuthorized(context.Background(), "alice", "default", feature, judge); err == nil {
			t.Fatalf("disabled feature %s allowed", feature)
		}
	}
	if err := app.catalog.Create(catalog.Bootstrap, "private", "Private"); err != nil {
		t.Fatal(err)
	}
	if err := app.judgmentsAuthorized(context.Background(), "alice", "private", "rerank", judge); err == nil {
		t.Fatal("global policy bypassed notebook access")
	}
}

func TestInstanceAIAdministrationRequiresAdminAndManagementConsent(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"ai_settings", "set_ai_settings"} {
		body := `{}`
		if action == "set_ai_settings" {
			body = `{"config":{"revision":0}}`
		}
		w := proxyCall(app, "alice", "POST", "/api/organization/action/"+action, body)
		if w.Code != 403 {
			t.Fatalf("owner accessed %s: HTTP %d: %s", action, w.Code, w.Body.String())
		}
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	actionCall(t, client, "ai_settings", map[string]any{}, true)
	grant, err := issueCredential(app.catalog, app.oauth, catalog.Bootstrap, CredentialSpec{Name: "limited", Projects: []string{"default"}, Scopes: []string{"notes:read", "notes:manage"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if w := apiCall(app, "POST", "/api/v1/actions/ai_settings", `{}`, grant.Token); w.Code != 403 {
		t.Fatalf("restricted credential read global status: HTTP %d: %s", w.Code, w.Body.String())
	}
}

func TestInstanceAIAutomaticReviewStartsFromBaselineAndStopsWhenDisabled(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); syntheticReviewAnswer(w, r) })
	config, _ := app.catalog.AISettings()
	config.Reviews = true
	if _, err := app.catalog.SetAISettings(catalog.Bootstrap, config); err != nil {
		t.Fatal(err)
	}
	app.syncInstanceAI(context.Background())
	settings, err := storedJudgmentSettings(app, "default")
	if err != nil || !settings.AutoChecks || settings.EnabledBy != catalog.Bootstrap || len(settings.Baseline) != 2 {
		t.Fatalf("missing instance baseline: %+v: %v", settings, err)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	doc, err := store.Read(context.Background(), "a.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(context.Background(), doc.Path, []byte("# Workshop\nThe workshop opens at 10:00 on weekdays."), doc.Revision, "alice"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	app.judgments.ScanAutomatic(context.Background(), now)
	if calls.Load() != 0 {
		t.Fatal("review ignored debounce")
	}
	app.judgments.ScanAutomatic(context.Background(), now.Add(3*time.Minute))
	ids, err := app.judgments.JobIDs()
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	if job := waitReview(t, app, ids[0]); job["state"] != "completed" {
		t.Fatal(job)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected two bounded decisions, got %d", calls.Load())
	}
	config, _ = app.catalog.AISettings()
	config.Reviews = false
	if _, err = app.catalog.SetAISettings(catalog.Bootstrap, config); err != nil {
		t.Fatal(err)
	}
	app.judgments.ScanAutomatic(context.Background(), now.Add(10*time.Minute))
	if calls.Load() != 2 {
		t.Fatal("disabled reviews sent more content")
	}
}

// The instance policy reviews shared notebooks, never a person's assistant or
// memory notebooks. Each remember rewrites memory.md, and reviewing the whole
// file every time filled a production instance's judgments ledger within hours. A setting kept from
// the earlier policy cannot start one either, and every step of a paused review
// repeats that check.
func TestInstanceAIAutomaticReviewLeavesPrivateMemoryAlone(t *testing.T) {
	app, _, session := agentMemoryApp(t)
	chatConfig := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(chatConfig, []byte(`{"connections":[{"id":"local","label":"Synthetic","format":"chat-completions","baseURL":"http://127.0.0.1:9","models":["synthetic"],"local":true}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(chatConfig); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	configureSyntheticJudgments(t, app, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); syntheticReviewAnswer(w, r) })
	shared, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	for path, text := range map[string]string{"a.md": "# Workshop\nThe workshop opens at 09:00 on weekdays.", "b.md": "# Workshop\nThe workshop opens at 11:00 on weekdays."} {
		if _, err := shared.Save(t.Context(), path, []byte(text), "", catalog.Bootstrap); err != nil {
			t.Fatal(err)
		}
	}
	var core, project rememberResult
	if failure := callMemoryTool(t, session, "remember", map[string]any{"items": []map[string]any{{"text": "Staging runs kernel 6.12.", "kind": "fact"}}}, &core); failure != "" {
		t.Fatal(failure)
	}
	if failure := callMemoryTool(t, session, "remember", map[string]any{"projectKey": "billing", "items": []map[string]any{{"text": "Invoices go out on the first.", "kind": "fact"}}}, &project); failure != "" {
		t.Fatal(failure)
	}
	config, _ := app.catalog.AISettings()
	config.Reviews = true
	if _, err = app.catalog.SetAISettings(catalog.Bootstrap, config); err != nil {
		t.Fatal(err)
	}
	app.syncInstanceAI(t.Context())
	for _, notebook := range []string{core.Notebook, project.Notebook} {
		if settings, err := storedJudgmentSettings(app, notebook); err != nil || settings.AutoChecks {
			t.Fatalf("the instance policy turned on reviews of private memory %s: %+v %v", notebook, settings, err)
		}
	}

	// Both memory files and a shared note change; only the shared note is reviewed.
	doc, err := shared.Read(t.Context(), "a.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = shared.Save(t.Context(), doc.Path, []byte("# Workshop\nThe workshop opens at 10:00 on weekdays."), doc.Revision, catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "billing"} {
		if failure := callMemoryTool(t, session, "remember", map[string]any{"projectKey": key, "items": []map[string]any{{"text": "Ask before running the full gate.", "kind": "lesson"}}}, &rememberResult{}); failure != "" {
			t.Fatal(failure)
		}
	}
	now := time.Now()
	app.judgments.ScanAutomatic(t.Context(), now)
	app.judgments.ScanAutomatic(t.Context(), now.Add(3*time.Minute))
	ids, err := app.judgments.JobIDs()
	var job map[string]any
	if err == nil && len(ids) == 1 {
		job, err = app.judgments.ReadJob(ids[0])
	}
	if err != nil || len(ids) != 1 || job["project"] != "default" {
		t.Fatalf("automatic reviews %v of %q: %v", ids, job["project"], err)
	}
	for deadline := time.Now().Add(30 * time.Second); job["state"] != "completed" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if job, err = app.judgments.ReadJob(ids[0]); err != nil {
			t.Fatal(err)
		}
	}
	if job["state"] != "completed" {
		t.Fatalf("shared review: %s %s", job["state"], job["message"])
	}

	admin, err := app.catalog.Account(catalog.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeJudgmentSettings(app, core.Notebook, catalog.Bootstrap, admin.ID); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	if _, err = app.judgments.CreateJob(t.Context(), catalog.Bootstrap, core.Notebook, judgments.JobInput{Kind: "consistency", OperationID: "automatic-earlier"}, true, map[string]bool{"memory.md": true}, nil); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("a retained setting started a memory review: %v", err)
	}
	if calls.Load() != before {
		t.Fatal("memory reached the judge through a retained setting")
	}
}

// judgmentSettings are the stored judgment settings of a notebook these tests read.
type judgmentSettings struct {
	AutoChecks bool              `json:"autoChecks"`
	EnabledBy  string            `json:"enabledBy"`
	Baseline   map[string]string `json:"baseline"`
}

// storedJudgmentSettings reads a notebook's judgment settings; a notebook with none
// stored has automatic review off.
func storedJudgmentSettings(app *Server, notebook string) (judgmentSettings, error) {
	var stored map[string]any
	err := app.judgments.Records().Read(judgments.NotebookName(notebook), &stored)
	if errors.Is(err, os.ErrNotExist) {
		return judgmentSettings{}, nil
	}
	if err != nil {
		return judgmentSettings{}, err
	}
	raw, _ := json.Marshal(stored)
	var settings judgmentSettings
	return settings, json.Unmarshal(raw, &settings)
}

// writeJudgmentSettings stores settings that turn on automatic review of a notebook
// directly, as an earlier policy could have left them.
func writeJudgmentSettings(app *Server, notebook, enabledBy, accountID string) error {
	return app.judgments.Records().Write(judgments.NotebookName(notebook), map[string]any{"version": 1, "notebook": notebook, "autoChecks": true, "enabledBy": enabledBy, "accountId": accountID, "baseline": map[string]string{}})
}
