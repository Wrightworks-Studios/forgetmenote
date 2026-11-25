package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/pg/pgtest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestQueueChatAttachmentsSurviveRestartWithinBoundedCapacity(t *testing.T) {
	t.Setenv("FMN_LIMIT_STATE_BYTES", "16777216")
	dir := t.TempDir()
	q, err := Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "chat-files", Kind: "assistant", Actor: "alice", Project: "default", Instruction: "Export the results", ExternalProcessing: true, Delivery: &Delivery{Client: "slack", Channel: "D123", User: "U123", Direct: true}}
	created, err := q.Enqueue(spec, "one")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = q.RunNext(ctx, func(ctx context.Context, r *Run, save func() error) error {
		r.GenerationStarted = true
		r.Artifacts = []messaging.Artifact{{Name: "result.csv", Data: make([]byte, 3<<20), Receipt: "verified"}}
		if err := save(); err != nil {
			t.Fatal("valid attachment exceeds queue checkpoint capacity", err)
		}
		cancel()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	q, err = Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	r, err := q.Get(created.ID)
	if err != nil || len(r.Artifacts) != 1 || len(r.Artifacts[0].Data) != 3<<20 || r.Version != 6 {
		t.Fatalf("lost attachment or old reader fence: version %d, %v", r.Version, err)
	}
	r.Artifacts[0].Data = make([]byte, 17<<20)
	if err := q.write(&r); err == nil {
		t.Fatal("unbounded chat record")
	}
	preserved, err := q.Get(created.ID)
	if err != nil || len(preserved.Artifacts[0].Data) != 3<<20 {
		t.Fatal("failed save replaced durable export", err)
	}
}

func TestQueueDeduplicatesAndRecoversPublication(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "daily", Kind: "digest", Actor: "robot", Project: "default", Output: "digest.md", Calls: []Call{{Connection: "test", Operation: "read"}}}
	r, err := q.Enqueue(spec, "slot-1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := q.Enqueue(spec, "slot-1")
	if err != nil || again.ID != r.ID {
		t.Fatal(again, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = q.RunNext(ctx, func(ctx context.Context, r *Run, save func() error) error {
		r.Publication = &Publication{Path: "digest.md", Content: "saved candidate", Revision: "base"}
		if err := save(); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	q, err = Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ok, err := q.RunNext(context.Background(), func(ctx context.Context, r *Run, save func() error) error {
		calls++
		if r.Publication == nil || r.Publication.Content != "saved candidate" {
			t.Fatal("lost staged publication")
		}
		return nil
	})
	if err != nil || !ok || calls != 1 {
		t.Fatal(ok, calls, err)
	}
	if again, err = q.Enqueue(spec, "slot-1"); err != nil || again.State != "completed" {
		t.Fatal(again, err)
	}
	if ok, _ = q.RunNext(context.Background(), func(context.Context, *Run, func() error) error { t.Fatal("repeated completed work"); return nil }); ok {
		t.Fatal("duplicate run")
	}
}

func TestNewAutomationPoliciesFenceOlderReaders(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "monitor", Kind: "monitor", Actor: "robot", Project: "default", Connections: []string{"metrics"}, Calls: []Call{{Connection: "metrics", Operation: "read"}}, NotifySlackUser: "U123", Condition: &Condition{Pointer: "/healthy", Equals: json.RawMessage(`false`)}}
	r, err := q.Enqueue(spec, "one")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := q.records.read(r.ID+".json", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	json.Unmarshal(b, &record)
	if record["version"] != float64(5) {
		t.Fatal("older queue readers could silently drop new execution policy")
	}
	q, err = Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := q.List()
	if err != nil || len(rows) != 1 || rows[0].Spec.Condition == nil {
		t.Fatal("new policy lost on restart", rows, err)
	}
}

func TestQueueFailureCancellationAndSchedule(t *testing.T) {
	q, _ := Open(pgtest.Org(t))
	spec := Spec{ID: "scheduled", Kind: "digest", Actor: "robot", Project: "default", IntervalSeconds: 60, Output: "digest.md", Calls: []Call{{Connection: "test", Operation: "read"}}}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := q.Schedule([]Spec{spec}, now); err != nil {
		t.Fatal(err)
	}
	q.RunNext(context.Background(), func(context.Context, *Run, func() error) error { return errors.New("safe failure") })
	q.Schedule([]Spec{spec}, now.Add(5*time.Second))
	runs, _ := q.List()
	if len(runs) != 1 || runs[0].State != "failed" {
		t.Fatal(runs)
	}
	q.Schedule([]Spec{spec}, now.Add(time.Minute))
	runs, _ = q.List()
	if len(runs) != 2 {
		t.Fatal(runs)
	}
	for _, r := range runs {
		if r.State == "queued" {
			if err := q.Cancel(r.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ok, _ := q.RunNext(context.Background(), func(context.Context, *Run, func() error) error { t.Fatal("cancelled task executed"); return nil }); ok {
		t.Fatal("cancelled task claimed")
	}
}

func TestQueueRetainsActiveWorkWhileAgingOutCompletedCapacity(t *testing.T) {
	for _, kind := range []string{"manual", "learning", "repository-event", "event-preview"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			q, err := Open(pgtest.Dir(t, dir))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			spec := Spec{ID: "retention", Kind: "digest", Actor: "robot", Project: "default", Output: "digest.md", Calls: []Call{{Connection: "test", Operation: "read"}}}
			eventSpec := Spec{ID: "source", Kind: "task", Actor: "robot", Project: "default", Output: "changes.md", Instruction: "Summarize source changes", ExternalProcessing: true, Trigger: &Trigger{Kind: "repository.updated", Project: "default", DebounceSeconds: 60, MaxWaitSeconds: 120}}
			old := now.Add(-time.Hour)
			for i := 0; i < 1000; i++ {
				r := Run{Version: 1, ID: Hash(i), Spec: spec, State: "completed", Created: old.Add(time.Duration(i) * time.Millisecond), Updated: old}
				if i == 0 {
					r.State = "queued"
				}
				if i == 2 && kind == "event-preview" {
					r.Spec = eventSpec
					r.Batch = &EventBatch{Expires: now.Add(time.Hour), Events: []Event{{ID: "a", Project: "default", Revision: "a", At: old}}}
				}
				b, _ := json.Marshal(r)
				if _, err = q.records.write(r.ID+".json", b, 0); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "manual":
				_, err = q.Enqueue(spec, "new-work")
			case "learning":
				_, err = q.ObserveLearning(learningSpec(), LearningEvent{ID: "new", At: now}, now)
			case "repository-event":
				_, err = q.ObserveEvent(eventSpec, Event{ID: "new", Project: "default", Revision: "new", At: now}, now)
			case "event-preview":
				_, err = q.PreviewEvidence(eventSpec, "new-preview", Hash(2))
			}
			if err != nil {
				t.Fatal("completed history stopped scheduling", err)
			}
			q, err = Open(pgtest.Dir(t, dir))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = q.Get(Hash(0)); err != nil {
				t.Fatal("active work evicted", err)
			}
			if _, err = q.Get(Hash(1)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("oldest completed record retained", err)
			}
			items, err := q.List()
			if err != nil || len(items) != 1000 {
				t.Fatal("capacity", len(items), err)
			}
		})
	}
}

func TestQueueRetentionPreservesCurrentScheduledSlot(t *testing.T) {
	for _, age := range []time.Duration{time.Hour, 8 * 24 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			dir := t.TempDir()
			q, err := Open(pgtest.Dir(t, dir))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			spec := Spec{ID: "monthly", Kind: "digest", Actor: "robot", Project: "default", IntervalSeconds: 31 * 86400, Output: "digest.md", Calls: []Call{{Connection: "test", Operation: "read"}}}
			currentID := Hash([]string{spec.ID, fmt.Sprintf("scheduled-%d", now.Unix()/int64(spec.IntervalSeconds))})
			for i := 0; i < 1000; i++ {
				r := Run{Version: 1, ID: Hash(i), Spec: spec, State: "completed", Created: now.Add(-age).Add(time.Duration(i) * time.Millisecond), Updated: now.Add(-age)}
				if i == 0 {
					r.ID = currentID
				}
				b, _ := json.Marshal(r)
				if _, err = q.records.write(r.ID+".json", b, 0); err != nil {
					t.Fatal(err)
				}
			}
			manual := spec
			manual.ID, manual.IntervalSeconds = "manual", 0
			if _, err = q.Enqueue(manual, "new-work"); err != nil {
				t.Fatal(err)
			}
			if err = q.Schedule([]Spec{spec}, now); err != nil {
				t.Fatal(err)
			}
			for {
				worked, err := q.RunNext(context.Background(), func(_ context.Context, r *Run, _ func() error) error {
					if r.Spec.ID == spec.ID {
						t.Error("scheduled work repeated in the same interval after retention cleanup")
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if !worked {
					break
				}
			}
		})
	}
}

// Scheduling keeps a current personal run however its scan summarized it.
func TestScheduleKeepsACurrentPersonalRunTheScansSummarized(t *testing.T) {
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	account := strings.Repeat("b", 32)
	personal := Spec{ID: "personal-" + account, Kind: "personal", Actor: "robot", Project: "assistant-" + account, ExternalProcessing: true, Personal: &Personal{Account: account, Settings: 1, Policy: 1}}
	run, err := q.Enqueue(personal, "key")
	if err != nil {
		t.Fatal(err)
	}
	// A scan after the change reads the record; later ones keep its summary.
	q.mu.Lock()
	_, err = q.scan(false)
	q.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Schedule([]Spec{personal}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, err := q.Get(run.ID); err != nil || got.State != "queued" {
		t.Fatalf("a current personal run was left %s: %v", got.State, err)
	}
	// One whose consent changed is cancelled.
	changed := personal
	changed.Personal = &Personal{Account: account, Settings: 2, Policy: 1}
	if err = q.Schedule([]Spec{changed}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, err := q.Get(run.ID); err != nil || got.State != "cancelled" {
		t.Fatalf("a superseded personal run was left %s: %v", got.State, err)
	}
}
