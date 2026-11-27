package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/automationrun"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/decision"
)

func TestLearningConsolidatesRelatedVariantsAcrossBatches(t *testing.T) {
	for _, related := range []bool{true, false} {
		t.Run(map[bool]string{true: "same-task", false: "unrelated-task"}[related], func(t *testing.T) {
			modelCalls := 0
			app, spec := learningFixture(t, func(w http.ResponseWriter, r *http.Request) { modelCalls++; learningAnswer(w, r) })
			spec.Learning.Jev = true
			setTestAutomationSpec(t, app, spec)
			configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Questions map[string]decision.Question `json:"questions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				answers := map[string]any{}
				for id, q := range req.Questions {
					probabilities := map[string]int{}
					for option := range q.Criteria.(map[string]any) {
						probabilities[option] = 0
					}
					probabilities["new"] = 1
					answers[id] = map[string]any{"type": "choice", "choice": "new", "confidence": 1, "probabilities": probabilities}
				}
				json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
			})
			client := actionClient(t, app, "alice", "notes:read data:read")
			capture := func(sql, intent string) {
				actionCall(t, client, "call_data_operation", map[string]any{"project": "default", "connection": "db", "operation": "query", "arguments": map[string]any{"sql": sql}, "learningContext": map[string]string{"conversation": "task-123", "intent": intent}}, false)
			}
			capture("select 7 as count", "Count matching examples")
			readyLearningQueue(t, app)
			if ran, err := app.automations.Queue.RunNext(t.Context(), app.automationrun.RunAutomation); err != nil || !ran {
				t.Fatal(ran, err)
			}
			runs, _ := app.automations.Queue.List()
			if len(runs) != 1 || modelCalls != 0 || runStatus(runs[0]).Learning.State != "deferred" || !strings.Contains(runStatus(runs[0]).Learning.Summary, "need 2") {
				t.Fatal("singleton evidence should wait with a reason", runs, modelCalls)
			}
			intent := "Count matching examples"
			if !related {
				intent = "Measure unrelated account balances"
			}
			capture("select 8 as total", intent)
			runs, _ = app.automations.Queue.List()
			for _, run := range runs {
				if run.State != "queued" {
					continue
				}
				if err := app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				want := 0
				if related {
					want = 1
				}
				if len(run.Learning.Published) != want || modelCalls != want {
					t.Fatalf("published=%d model calls=%d want=%d: %s", len(run.Learning.Published), modelCalls, want, run.Result)
				}
			}
		})
	}
}

// Keep real captured events but move their queue admission time into the past,
// exercising durable worker/restart behavior without sleeping for debounce.
func readyLearningQueue(t *testing.T, app *Server) string {
	t.Helper()
	runs, err := app.automations.Queue.List()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	q, err := automation.Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.State != "queued" {
			continue
		}
		for _, event := range run.Learning.Events {
			_, err = q.ObserveLearning(run.Spec, event, time.Now().Add(-time.Duration(run.Spec.Learning.DebounceSeconds+10)*time.Second))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	app.automations.Queue = q
	return dir
}

func captureLearningExamples(t *testing.T, app *Server, suffix string) {
	t.Helper()
	for _, n := range []string{"7", "8"} {
		w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"project":"default","connection":"db","operation":"query","arguments":{"sql":"select `+n+` as `+suffix+`"}}`, testToken)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
}

func TestLearningRestartDoesNotOverwriteConcurrentEdit(t *testing.T) {
	modelCalls := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) { modelCalls++; learningAnswer(w, r) })
	captureLearningExamples(t, app, "count")
	dir := readyLearningQueue(t, app)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	ran, err := app.automations.Queue.RunNext(ctx, func(ctx context.Context, run *automation.Run, save func() error) error {
		return app.automationrun.RunAutomation(ctx, run, func() error {
			if err := save(); err != nil {
				return err
			}
			raw, _ := json.Marshal(run.Learning.Candidates)
			if run.Publication == nil && strings.Contains(string(raw), `"templates"`) {
				stop() // crash after persisting the draft, before validating/publishing it
				return context.Canceled
			}
			return nil
		})
	})
	if err != nil || !ran || modelCalls != 1 {
		t.Fatal(ran, err, modelCalls)
	}
	runs, _ := app.automations.Queue.List()
	if len(runs) != 1 || runs[0].State != "queued" || len(runs[0].Learning.Candidates) != 1 {
		t.Fatalf("missing restart checkpoint: %+v", runs)
	}
	target := ""
	for _, p := range runs[0].Learning.Decisions {
		target = p
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	newer, err := store.Save(context.Background(), target, []byte("# Newer human instructions\n"), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	app.automations.Queue, err = automation.Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.automations.Queue.RunNext(context.Background(), app.automationrun.RunAutomation); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Read(context.Background(), target, "")
	if after.Revision != newer.Revision || string(after.Content) != string(newer.Content) {
		t.Fatal("restarted learning overwrote newer content")
	}
	runs, _ = app.automations.Queue.List()
	if runs[0].State != "failed" || len(runs[0].Learning.Candidates) != 1 || modelCalls != 1 {
		t.Fatal("conflicting draft was lost or model was rerun")
	}
}

func TestLearningUpdatePreservesPriorTemplates(t *testing.T) {
	modelCalls := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) { modelCalls++; learningAnswer(w, r) })
	captureLearningExamples(t, app, "count")
	readyLearningQueue(t, app)
	if _, err := app.automations.Queue.RunNext(context.Background(), app.automationrun.RunAutomation); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	notes, err := store.Find(context.Background(), "", "recipe", "", "learned/")
	if err != nil || len(notes) != 1 {
		t.Fatal(notes, err)
	}
	captureLearningExamples(t, app, "total")
	readyLearningQueue(t, app)
	_, err = app.automations.Queue.RunNext(context.Background(), func(ctx context.Context, run *automation.Run, save func() error) error {
		// The same route Jev supplies when a new pattern fits an existing recipe.
		run.Learning.Decisions = map[string]string{}
		for _, e := range run.Learning.Events {
			run.Learning.Decisions[automationrun.LearningGroup(e)] = notes[0].Path
		}
		return app.automationrun.RunAutomation(ctx, run, save)
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Read(context.Background(), notes[0].Path, "")
	if err != nil || len(automationrun.ProfileLearningRecord(after.Content).Shapes) != 2 || strings.Count(string(after.Content), "{{/arguments/sql}}") != 2 || modelCalls != 2 {
		t.Fatalf("old validated template disappeared after update: %s (%v, calls=%d)", after.Content, err, modelCalls)
	}
	// Routine successful use of the now-known pattern must not buy another turn.
	captureLearningExamples(t, app, "total")
	readyLearningQueue(t, app)
	if _, err = app.automations.Queue.RunNext(context.Background(), app.automationrun.RunAutomation); err != nil || modelCalls != 2 {
		t.Fatal("unchanged pattern was reanalyzed", err, modelCalls)
	}
}

func TestLearningPublicationCheckpointReplaysWithoutAnotherCommit(t *testing.T) {
	modelCalls := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) { modelCalls++; learningAnswer(w, r) })
	captureLearningExamples(t, app, "count")
	readyLearningQueue(t, app)
	var checkpoint []byte
	_, err := app.automations.Queue.RunNext(context.Background(), func(ctx context.Context, run *automation.Run, save func() error) error {
		return app.automationrun.RunAutomation(ctx, run, func() error {
			if run.Publication != nil {
				checkpoint, _ = json.Marshal(run)
			}
			return save()
		})
	})
	if err != nil || len(checkpoint) == 0 {
		t.Fatal("publication checkpoint missing", err)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	before, _ := store.Status(context.Background())
	var restored automation.Run
	if err = json.Unmarshal(checkpoint, &restored); err != nil {
		t.Fatal(err)
	}
	if err = app.automationrun.RunAutomation(context.Background(), &restored, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Status(context.Background())
	if before.Revision != after.Revision || modelCalls != 1 || len(restored.Learning.Published) != 1 {
		t.Fatal("publication replay generated or committed twice")
	}
}

func TestLearningDoesNotReanalyzeAnAlreadyHandledFailure(t *testing.T) {
	modelCalls := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) { modelCalls++; learningAnswer(w, r) })
	w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"project":"default","connection":"db","operation":"query","arguments":{"sql":"select 0 as count"}}`, testToken)
	if w.Code == 200 {
		t.Fatal("failure fixture unexpectedly succeeded")
	}
	captureLearningExamples(t, app, "count")
	readyLearningQueue(t, app)
	if _, err := app.automations.Queue.RunNext(context.Background(), app.automationrun.RunAutomation); err != nil || modelCalls != 1 {
		t.Fatal(err, modelCalls)
	}
	// Keep the completed batch, including its failed call, in retained evidence.
	captureLearningExamples(t, app, "count")
	runs, _ := app.automations.Queue.List()
	for _, run := range runs {
		if run.State == "queued" {
			if err := app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if modelCalls != 1 {
		t.Fatalf("retained old failure bought another investigation: calls=%d", modelCalls)
	}
}
