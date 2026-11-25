package automation

import (
	"context"
	"forgetmenote/internal/pg/pgtest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLearningConfigurationDoesNotRequireKnownRecipes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "automations.json")
	data := `{"version":1,"jobs":[{"id":"learn-data","kind":"learning","actor":"worker","project":"profiles","connections":["warehouse"],"aiConnection":"model","externalProcessing":true,"learning":{"captureInputs":true,"folder":"learned","debounceSeconds":120,"maxWaitSeconds":900,"minExamples":2,"retentionHours":24}}]}`
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(file)
	if err != nil {
		t.Fatalf("usage learning should accept unknown query patterns without a predeclared recipe: %v", err)
	}
	if len(c.Jobs) != 1 || c.Jobs[0].Kind != "learning" {
		t.Fatal(c)
	}
}

func learningSpec() Spec {
	return Spec{ID: "learn", Kind: "learning", Actor: "worker", Project: "profiles", Connections: []string{"db"}, AIConnection: "model", ExternalProcessing: true, Learning: &LearningPolicy{CaptureInputs: true, Folder: "learned", DebounceSeconds: 120, MaxWaitSeconds: 300, MinExamples: 2, RetentionHours: 24}}
}

func TestLearningCannotBeManuallyQueuedWithoutEvidence(t *testing.T) {
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.Enqueue(learningSpec(), "manual"); err == nil {
		t.Fatal("learning admitted a run with no observed activity")
	}
}

func TestLearningEvidenceIsFrozenAndConfigurationIsolated(t *testing.T) {
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	spec := learningSpec()
	first, err := q.ObserveLearning(spec, LearningEvent{ID: "one", At: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.ObserveLearning(spec, LearningEvent{ID: "two", At: now.Add(time.Second)}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	events, err := q.LearningEvidence(spec, now, now.Add(time.Second))
	if err != nil || len(events) != 1 || events[0].ID != "one" {
		t.Fatal("frozen batch pulled a newer event", events, err)
	}
	spec.Instruction = "Changed operator guidance"
	changed, err := q.ObserveLearning(spec, LearningEvent{ID: "one", At: now}, now)
	if err != nil || changed.ID == first.ID {
		t.Fatal("changed configuration replaced prior evidence", err)
	}
	runs, err := q.List()
	if err != nil || len(runs) != 2 {
		t.Fatal("prior run was overwritten", err)
	}
}

func TestLearningExampleConfiguration(t *testing.T) {
	if _, err := Load("../../deploy/integrations/learning.example.json"); err != nil {
		t.Fatal(err)
	}
}

func TestLearningDebounceRecoveryAndSuccessor(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	spec := learningSpec()
	first, err := q.ObserveLearning(spec, LearningEvent{ID: "one"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := q.RunNext(context.Background(), func(context.Context, *Run, func() error) error { t.Error("ran during debounce"); return nil }); err != nil || ran {
		t.Fatalf("premature run %v %v", ran, err)
	}
	q, err = Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.ObserveLearning(spec, LearningEvent{ID: "two"}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || len(second.Learning.Events) != 2 || !second.Learning.ReadyAt.Equal(now.Add(3*time.Minute)) {
		t.Fatal("lost debounce/observations after restart", second)
	}
	duplicate, err := q.ObserveLearning(spec, LearningEvent{ID: "two"}, now.Add(2*time.Minute))
	if err != nil || len(duplicate.Learning.Events) != 2 {
		t.Fatal("retry duplicated evidence", err)
	}
	third, err := q.ObserveLearning(spec, LearningEvent{ID: "three"}, now.Add(170*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := q.ObserveLearning(spec, LearningEvent{ID: "four"}, now.Add(280*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != fourth.ID || !fourth.Learning.ReadyAt.Equal(now.Add(300*time.Second)) {
		t.Fatal("continuous traffic starved bounded debounce")
	}
	fourth.Learning.ReadyAt = now.Add(-time.Second)
	if err = q.write(&fourth); err != nil {
		t.Fatal(err)
	}
	_, err = q.RunNext(context.Background(), func(_ context.Context, r *Run, save func() error) error {
		next, err := q.ObserveLearning(spec, LearningEvent{ID: "five"}, now.Add(time.Second))
		if err != nil {
			return err
		}
		if next.ID == r.ID || len(r.Learning.Events) != 4 {
			t.Error("new arrival changed running evidence")
		}
		return save()
	})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := q.List()
	if err != nil || len(runs) != 2 {
		t.Fatal(runs, err)
	}
}
