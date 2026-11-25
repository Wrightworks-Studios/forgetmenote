package automation

import (
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestScriptRepositoryEventsPreserveComparisonAcrossDebounce(t *testing.T) {
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	spec := scriptSpec(t)
	spec.Disabled = false
	spec.ReadProjects = []string{"code", "packages"}
	spec.Trigger = &Trigger{Kind: "repository.updated", Project: "code", DebounceSeconds: 60, MaxWaitSeconds: 120}
	now := time.Now()
	observe := func(revision string, at time.Time) Run {
		t.Helper()
		r, err := q.ObserveEvent(spec, Event{ID: revision, Project: "code", Revision: revision, At: at}, at)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := observe("a", now)
	if first.Batch == nil {
		t.Fatal("missing batch")
	}
	second := observe("b", now.Add(2*time.Minute))
	third := observe("c", now.Add(2*time.Minute+time.Second))
	if second.ID != third.ID {
		t.Fatal("events did not debounce")
	}
	raw, _ := json.Marshal(third.Batch.Events[0])
	var event map[string]any
	_ = json.Unmarshal(raw, &event)
	if event["revision"] != "c" || event["previousRevision"] != "a" {
		t.Fatalf("comparison lost: %s", raw)
	}
}

func TestRepositoryEventsCoalesceAndKeepCursorAfterRetention(t *testing.T) {
	now := time.Now()
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "curation", Kind: "task", Actor: "robot", Project: "notes", ReadProjects: []string{"code"}, Output: "changes.md", Instruction: "Summarize source changes", ExternalProcessing: true, Trigger: &Trigger{Kind: "repository.updated", Project: "code", DebounceSeconds: 60, MaxWaitSeconds: 120}}
	first, err := q.ObserveEvent(spec, Event{ID: "a", Project: "code", Revision: "a", At: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.ObserveEvent(spec, Event{ID: "b", Project: "code", Revision: "b", At: now}, now.Add(time.Second))
	if err != nil || first.ID != second.ID || len(second.Batch.Events) != 1 || second.Batch.Events[0].Revision != "b" {
		t.Fatal("source events did not coalesce to latest snapshot", second, err)
	}
	q, err = Open(q.records.db)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.PruneLearning(now.Add(25 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	runs, err := q.List()
	if err != nil || len(runs) != 0 {
		t.Fatal("expired event inputs retained", len(runs), err)
	}
	if next, err := q.ObserveEvent(spec, Event{ID: "b", Project: "code", Revision: "b"}, now.Add(25*time.Hour)); err != nil || next.ID != "" {
		t.Fatal("unchanged source retriggered", next.ID, err)
	}
}

// A repository that returns to an earlier snapshot runs its job again, though
// the finished run first queued for that snapshot has the same identifier.
func TestRepositoryReturningToAnEarlierSnapshotRunsAgain(t *testing.T) {
	now := time.Now()
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "curation", Kind: "task", Actor: "robot", Project: "notes", ReadProjects: []string{"code"}, Output: "changes.md", Instruction: "Summarize source changes", ExternalProcessing: true, Trigger: &Trigger{Kind: "repository.updated", Project: "code", DebounceSeconds: 60, MaxWaitSeconds: 120}}
	observe := func(revision string, at time.Time) Run {
		t.Helper()
		r, err := q.ObserveEvent(spec, Event{ID: revision, Project: "code", Revision: revision, At: at}, at)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := observe("a", now)
	if second := observe("b", now.Add(time.Second)); second.ID != first.ID {
		t.Fatal("source events did not coalesce")
	}
	done, err := q.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	done.State = "completed"
	if err = q.write(&done); err != nil {
		t.Fatal(err)
	}
	// Force-pushed back to the first snapshot.
	again := observe("a", now.Add(5*time.Minute))
	if again.ID != first.ID || again.State != "queued" || len(again.Batch.Events) != 1 || again.Batch.Events[0].Revision != "a" {
		t.Fatalf("the earlier snapshot was not queued again: %s %s %+v", again.ID, again.State, again.Batch)
	}
	if got, err := q.Get(first.ID); err != nil || got.State != "queued" || got.Batch.Events[0].Revision != "a" {
		t.Fatalf("stored %s: %v", got.State, err)
	}
}
