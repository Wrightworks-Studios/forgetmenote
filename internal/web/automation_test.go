package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/workspace"
)

func TestAutomationStrictToolArgumentsReadAndPublish(t *testing.T) {
	const args = `{"project":"default","origin":"notes","path":"context.md","revision":null}`
	calls := 0
	sawEvidence := false
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct{ Messages []chat.Message }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		for _, message := range request.Messages {
			if message.Role == "tool" && strings.Contains(message.Content, "SYNTHETIC_AUTOMATION_EVIDENCE") {
				sawEvidence = true
			}
		}
		if sawEvidence {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"# Summary\nSYNTHETIC_AUTOMATION_EVIDENCE"},"finish_reason":"stop"}]}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "read", "type": "function", "function": map[string]string{"name": "read_document", "arguments": args}}}}, "finish_reason": "tool_calls"}}})
	})
	enableStrictChat(t, app)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	if _, err := store.Save(t.Context(), "context.md", []byte("# Context\nSYNTHETIC_AUTOMATION_EVIDENCE"), "", "alice"); err != nil {
		t.Fatal(err)
	}
	run := automation.Run{ID: strings.Repeat("a", 64), Spec: automation.Spec{ID: "strict-task", Kind: "task", Actor: "alice", Project: "default", AIConnection: "local", Output: "summary.md", Instruction: "Read context.md and summarize it", ExternalProcessing: true, MaxRounds: 2}}
	if err := app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	doc, err := store.Read(t.Context(), "summary.md", "")
	if err != nil || !sawEvidence || calls != 2 || len(run.Citations) == 0 || !strings.Contains(string(doc.Content), "SYNTHETIC_AUTOMATION_EVIDENCE") {
		t.Fatalf("strict task did not read and publish: calls=%d evidence=%t citations=%d error=%v", calls, sawEvidence, len(run.Citations), err)
	}
}

func TestAutomationDrainsExistingWorkWhenScheduleCapacityIsFull(t *testing.T) {
	app, _ := runnerFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected AI call") })
	if err := app.initAutomations(); err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "new-schedule", Kind: "digest", Actor: "alice", Project: "default", Output: "digest.md", IntervalSeconds: 60, Calls: []automation.Call{{Connection: "fixture", Operation: "read"}}}
	setTestAutomationSpec(t, app, spec)
	oldSpec := spec
	oldSpec.ID = "removed-schedule"
	now := time.Now().UTC()
	for i := 0; i < 1000; i++ {
		r := automation.Run{Version: 1, ID: automation.Hash(i), Spec: oldSpec, State: "queued", Created: now.Add(time.Duration(i) * time.Millisecond), Updated: now}
		b, _ := json.Marshal(r)
		if _, err := app.db.Pool.Exec(t.Context(), `INSERT INTO automation_records (org, name, version, body) VALUES ($1, $2, 1, $3)`, app.db.Org, r.ID+".json", b); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.automations.Queue.Schedule([]automation.Spec{spec}, now); err == nil {
		t.Fatal("fixture did not fill the queue")
	}
	app.StartAutomations(t.Context())
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("full scheduling capacity prevented existing queued work from draining")
		case <-tick.C:
			runs, err := app.automations.Queue.List()
			if err != nil {
				t.Fatal(err)
			}
			if runs[0].State == "failed" && runs[0].Error == "automation configuration changed" {
				return
			}
		}
	}
}

func TestRecipeMaintenanceRepairsChecksAndCommits(t *testing.T) {
	const candidate = "# Count records\n\nUse the supported count query.\n\n```sql count\nselect 2 as count\n```\n"
	aiCalls := 0
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		aiCalls++
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": candidate}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 50, "completion_tokens": 20}})
	})
	enableStrictChat(t, app)
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args map[string]any
		json.NewDecoder(r.Body).Decode(&args)
		n := 1
		if strings.Contains(fmt.Sprint(args["sql"]), "select 2") {
			n = 2
		}
		json.NewEncoder(w).Encode(map[string]any{"rows": [][]int{{n}}})
	}))
	defer upstream.Close()
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "fixture", Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "query", ReadOnly: true, Method: "POST", Path: "/query", Body: true, InputSchema: json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string"}},"required":["sql"],"additionalProperties":false}`)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = manager
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "fixture", Users: map[string][]string{"alice": {"query"}}}); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	doc, err := store.Save(context.Background(), "recipe.md", []byte("# Count records\n\n```sql count\nselect 1 as count\n```\n"), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run := automation.Run{ID: strings.Repeat("1", 64), Spec: automation.Spec{ID: "maintain", Kind: "recipe", Actor: "alice", Project: "default", Recipe: "recipe.md", Connections: []string{"fixture"}, AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Checks: []automation.Check{{Name: "count", Block: "count", Call: automation.Call{Connection: "fixture", Operation: "query", Arguments: map[string]any{}}, Assertions: []automation.Assertion{{Pointer: "/rows/0/0", Equals: json.RawMessage(`2`)}}}}}}
	if err = app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Read(context.Background(), "recipe.md", "")
	if err != nil || !strings.Contains(string(saved.Content), "select 2") || saved.Revision == doc.Revision || aiCalls != 1 {
		t.Fatal(err, string(saved.Content), aiCalls)
	}
	if err = app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil }); err != nil {
		t.Fatal("committed publication failed replay", err)
	}
	replayed, _ := store.Read(context.Background(), "recipe.md", "")
	if replayed.Revision != saved.Revision {
		t.Fatal("replay made another commit")
	}
	run.Publication = nil
	if err = app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil }); err != nil || aiCalls != 1 {
		t.Fatal("passing checks should need no AI", err, aiCalls)
	}
	// A pending candidate cannot overwrite a newer edit after restart.
	run.Publication = &automation.Publication{Path: "recipe.md", Content: candidate, Revision: doc.Revision}
	newer, err := store.Save(context.Background(), "recipe.md", []byte("# Human's newer change\n"), saved.Revision, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil }); err == nil {
		t.Fatal("stale publication accepted")
	}
	actual, _ := store.Read(context.Background(), "recipe.md", "")
	if actual.Revision != newer.Revision {
		t.Fatal("overwrote newer note")
	}

	// Rechecking current content is safe; another paid repair of the same
	// interrupted run is not, even if the current content still fails checks.
	run.Publication = nil
	if err = app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil }); err == nil || aiCalls != 1 {
		t.Fatal("ambiguous generation purchased another repair", err, aiCalls)
	}
}

func TestAutomationRespectsRepositoryAIConsent(t *testing.T) {
	calls := 0
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "answer"}, "finish_reason": "stop"}}})
	})
	configureSearchSource(t, app)
	state, err := app.repositories.Status("default")
	if err != nil {
		t.Fatal(err)
	}
	config := state.IndexConfig
	config.ExternalProcessing = false
	if _, err = app.repositories.ConfigureIndex("default", catalog.Bootstrap, state.Generation, config); err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "consent", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true}
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.automationrun.Answer(ctx, &automation.Run{Spec: spec}, "Inspect the repository", true); err == nil || calls != 0 {
		t.Fatal("automation bypassed repository AI consent", err, calls)
	}
}

func TestRecipeFailuresPreserveOriginalAndSkipAIOnOutage(t *testing.T) {
	const original = "# Recipe\n\n```sql count\nselect 1\n```\n"
	aiCalls := 0
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		aiCalls++
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": original}, "finish_reason": "stop"}}})
	})
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit")
	outage := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if outage {
			http.Error(w, "private provider failure", 503)
			return
		}
		w.Write([]byte(`{"rows":[[1]]}`))
	}))
	defer upstream.Close()
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "fixture", Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "query", ReadOnly: true, Path: "/", InputSchema: json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string"}},"required":["sql"],"additionalProperties":false}`)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = manager
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "fixture", Users: map[string][]string{"alice": {"query"}}}); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	doc, err := store.Save(t.Context(), "recipe.md", []byte(original), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "failure", Kind: "recipe", Actor: "alice", Project: "default", Recipe: "recipe.md", Connections: []string{"fixture"}, AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Checks: []automation.Check{{Name: "count", Block: "count", Call: automation.Call{Connection: "fixture", Operation: "query"}, Assertions: []automation.Assertion{{Pointer: "/rows/0/0", Equals: json.RawMessage(`2`)}}}}}
	for _, unavailable := range []bool{true, false} {
		outage = unavailable
		run := automation.Run{ID: strings.Repeat("2", 64), Spec: spec}
		err = app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil })
		if err == nil || run.Publication != nil {
			t.Fatal("failed check was accepted", err)
		}
		if unavailable && aiCalls != 0 {
			t.Fatal("outage consumed model calls", aiCalls)
		}
		if !unavailable && aiCalls != 1 {
			t.Fatal("bad candidate was not checked", aiCalls)
		}
		actual, e := store.Read(t.Context(), "recipe.md", "")
		if e != nil || actual.Revision != doc.Revision || string(actual.Content) != original {
			t.Fatal("failed repair changed note", e)
		}
	}
	if _, err = store.Save(t.Context(), "recipe.md", []byte("---\nfmn-container: archive\n---\n"+original), doc.Revision, "alice"); err != nil {
		t.Fatal(err)
	}
	run := automation.Run{ID: strings.Repeat("3", 64), Spec: spec}
	if err = app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err == nil || aiCalls != 1 {
		t.Fatal("archived recipe was sent for AI repair", err, aiCalls)
	}
}

func TestAutomationHonorsSharedModelCapacity(t *testing.T) {
	calls := 0
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "answer"}, "finish_reason": "stop"}}})
	})
	app.chat.Config.Limits.MaxConcurrent = 1
	app.chat.Active["existing-chat"] = func() {}
	spec := automation.Spec{ID: "capacity", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true}
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	limited, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err = app.automationrun.Answer(limited, &automation.Run{Spec: spec}, "Summarize", false); err == nil || calls != 0 {
		t.Fatal("automation bypassed model concurrency", err, calls)
	}
	delete(app.chat.Active, "existing-chat")
	if _, err = app.automationrun.Answer(ctx, &automation.Run{Spec: spec}, "Summarize", false); err != nil || calls != 1 || len(app.chat.Active) != 0 || app.chat.Connections["local"] != 0 {
		t.Fatal("model slot was not released", err, calls)
	}
}

func TestAutomationRejectsChangedRepositoryExclusions(t *testing.T) {
	var app *Server
	calls := 0
	app, _ = runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		message := map[string]any{"role": "assistant", "content": "The source is healthy."}
		finish := "stop"
		if calls == 1 {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": "source-1", "type": "function", "function": map[string]string{"name": "read_document", "arguments": `{"project":"default","origin":"repository","path":"health.go"}`}}}
			finish = "tool_calls"
		} else {
			state, err := app.repositories.Status("default")
			if err != nil {
				t.Error(err)
			}
			config := state.IndexConfig
			config.Exclude = []string{"health.go"}
			if _, err = app.repositories.ConfigureIndex("default", catalog.Bootstrap, state.Generation, config); err != nil {
				t.Error(err)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
	})
	configureSearchSource(t, app)
	spec := automation.Spec{ID: "source", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true}
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	run := automation.Run{Spec: spec}
	answer, err := app.automationrun.Answer(ctx, &run, "Read health.go", true)
	if err == nil || answer != "" || calls != 2 || len(run.Citations) != 1 {
		t.Fatal("changed source was published", answer, err, calls, run.Citations)
	}
}

func TestRecipeDiscoveryRespectsChatCorpus(t *testing.T) {
	app := chatFixture(t)
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	for _, kind := range []string{"active", "archive", "draft"} {
		metadata := "---\ntags: [recipe]\n"
		if kind != "active" {
			metadata += "fmn-container: " + kind + "\n"
		}
		if _, err := store.Save(t.Context(), kind+".md", []byte(metadata+"---\n# "+kind+" recipe\n"), "", "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	run := automation.Run{Spec: automation.Spec{ID: "recipes", Actor: "alice", Project: "default", AIConnection: "local", ExternalProcessing: true}}
	ctx, err := app.automationContext(t.Context(), run.Spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = app.automationrun.SourceContext(ctx, &run)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := app.dispatchAction(ctx, "default", "find_recipes", json.RawMessage(`{"project":"default"}`))
	if err != nil {
		t.Fatal(err)
	}
	var found []workspace.Entry
	if json.Unmarshal(raw, &found) != nil || len(found) != 1 || found[0].Path != "active.md" {
		t.Fatal("excluded recipe disclosed", string(raw))
	}
}

func setTestAutomationSpec(t *testing.T, app *Server, spec automation.Spec) {
	t.Helper()
	specs := app.automations.Definitions.Specs()
	found := false
	for i := range specs {
		if specs[i].ID == spec.ID {
			specs[i] = spec
			found = true
		}
	}
	if !found {
		specs = append(specs, spec)
	}
	if err := app.automations.Definitions.SetFiles(specs); err != nil {
		t.Fatal(err)
	}
}
