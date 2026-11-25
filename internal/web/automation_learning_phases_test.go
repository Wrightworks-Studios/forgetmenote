package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"forgetmenote/internal/automation"
)

// queuedLearningRun returns the single queued learning batch.
func queuedLearningRun(t *testing.T, app *Server) automation.Run {
	t.Helper()
	runs, err := app.automations.Queue.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.State == "queued" && run.Learning != nil {
			return run
		}
	}
	t.Fatalf("no queued learning batch: %+v", runs)
	return automation.Run{}
}

func learningRecipes(t *testing.T, app *Server) int {
	t.Helper()
	store, _ := app.catalog.Access("alice", "default", "read")
	found, err := store.Find(context.Background(), "", "recipe", "", "learned/")
	if err != nil {
		t.Fatal(err)
	}
	return len(found)
}

func TestLearningDefersCandidatesThatAreNotCompleteAndSafe(t *testing.T) {
	// A well-formed but incomplete candidate is kept with its draft; one that
	// is malformed or carries a secret is not.
	for name, tc := range map[string]struct {
		answer   string
		retained bool
	}{
		"not-json":         {"I could not find a reusable pattern.", false},
		"secret":           {`{"content":"# Use the api_key from the vault","templates":{}}`, false},
		"missing-template": {`{"content":"# Observed query workflow","templates":{}}`, true},
	} {
		answer := tc.answer
		t.Run(name, func(t *testing.T) {
			calls := 0
			app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": answer}, "finish_reason": "stop"}}})
			})
			captureLearningExamples(t, app, "count")
			run := queuedLearningRun(t, app)
			if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			b := run.Learning
			if calls != 1 || b.Deferred != 1 || b.Summary != "Deferred: no complete safe candidate was returned or retained.\n" || len(b.Published) != 0 || learningRecipes(t, app) != 0 {
				t.Fatalf("calls=%d deferred=%d summary=%q published=%v", calls, b.Deferred, b.Summary, b.Published)
			}
			if len(b.Candidates) != 1 || len(b.Processed) != 1 {
				t.Fatalf("deferral not recorded: candidates=%v processed=%v", b.Candidates, b.Processed)
			}
			for _, draft := range b.Candidates {
				if (draft.Content != nil) != tc.retained || tc.retained && string(draft.Content) != answer || draft.Revision != "" || draft.Hash != "" {
					t.Fatalf("candidate draft: %+v", draft)
				}
			}
			// The deferred pattern is settled for this batch.
			if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil || calls != 1 || run.Learning.Deferred != 1 {
				t.Fatalf("deferred pattern reanalyzed: calls=%d deferred=%d %v", calls, run.Learning.Deferred, err)
			}
		})
	}
}

func TestLearningInterruptedCandidateIsNotBoughtAgain(t *testing.T) {
	calls := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; learningAnswer(w, r) })
	captureLearningExamples(t, app, "count")
	run := queuedLearningRun(t, app)
	interrupted := errors.New("crash after pinning the candidate")
	var checkpoint []byte
	err := app.runAutomation(t.Context(), &run, func() error {
		if len(run.Learning.Candidates) > 0 {
			checkpoint, _ = json.Marshal(run)
			return interrupted
		}
		return nil
	})
	if !errors.Is(err, interrupted) || calls != 0 {
		t.Fatalf("candidate was not pinned before the model call: calls=%d %v", calls, err)
	}
	var restored automation.Run
	if err = json.Unmarshal(checkpoint, &restored); err != nil {
		t.Fatal(err)
	}
	if err = app.runAutomation(t.Context(), &restored, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || restored.Learning.Deferred != 1 || !strings.Contains(restored.Learning.Summary, "Deferred: no complete safe candidate was returned or retained.") || learningRecipes(t, app) != 0 {
		t.Fatalf("interrupted attempt bought again or published: calls=%d %+v", calls, restored.Learning)
	}
}

func TestLearningPreviewValidatesWithoutPublishing(t *testing.T) {
	calls := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; learningAnswer(w, r) })
	captureLearningExamples(t, app, "count")
	run := queuedLearningRun(t, app)
	run.DryRun = true
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	b := run.Learning
	if calls != 1 || len(b.Published) != 0 || learningRecipes(t, app) != 0 || len(b.Processed) != 1 || run.Publication != nil {
		t.Fatalf("preview published: calls=%d published=%v processed=%v", calls, b.Published, b.Processed)
	}
	target := ""
	for _, p := range b.Decisions {
		target = p
	}
	if !strings.HasPrefix(target, "learned/pattern-") || b.Summary != "Validated preview: "+target+".\n" || !strings.Contains(run.Result, "## Validated call templates") {
		t.Fatalf("preview summary=%q result=%q", b.Summary, run.Result)
	}
}

func TestLearningDefersPatternsWithoutCompleteSuccessfulCalls(t *testing.T) {
	calls := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; learningAnswer(w, r) })
	for range 2 {
		if w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"project":"default","connection":"db","operation":"query","arguments":{"sql":"select 0 as count"}}`, testToken); w.Code == 200 {
			t.Fatal("failure fixture unexpectedly succeeded")
		}
	}
	run := queuedLearningRun(t, app)
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	b := run.Learning
	if calls != 0 || b.Deferred != 1 || b.Summary != "Deferred: no complete successful reference calls.\n" || len(b.Processed) != 1 || b.Decisions != nil {
		t.Fatalf("calls=%d deferred=%d summary=%q processed=%v decisions=%v", calls, b.Deferred, b.Summary, b.Processed, b.Decisions)
	}
}

func TestLearningReanalyzesAKnownPatternOnlyWhenItRegresses(t *testing.T) {
	calls := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; learningAnswer(w, r) })
	captureLearningExamples(t, app, "count")
	readyLearningQueue(t, app)
	if _, err := app.automations.Queue.RunNext(context.Background(), app.runAutomation); err != nil || calls != 1 || learningRecipes(t, app) != 1 {
		t.Fatal("first pattern not learned", err, calls)
	}
	// Routine successful use is unchanged.
	captureLearningExamples(t, app, "count")
	run := queuedLearningRun(t, app)
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil || calls != 1 {
		t.Fatal("unchanged pattern reanalyzed", err, calls)
	}
	if run.Learning.Summary != "Unchanged: no eligible new or regressed query patterns.\n" || len(run.Learning.Processed) != 1 {
		t.Fatalf("unchanged summary=%q processed=%v", run.Learning.Summary, run.Learning.Processed)
	}
	// A failure of the known pattern is a regression worth another look.
	if w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"project":"default","connection":"db","operation":"query","arguments":{"sql":"select 0 as count"}}`, testToken); w.Code == 200 {
		t.Fatal("failure fixture unexpectedly succeeded")
	}
	run = queuedLearningRun(t, app)
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(run.Learning.Published) != 1 || !strings.HasPrefix(run.Learning.Summary, "Improved: learned/pattern-") {
		t.Fatalf("regressed pattern not reanalyzed: calls=%d summary=%q", calls, run.Learning.Summary)
	}
}
