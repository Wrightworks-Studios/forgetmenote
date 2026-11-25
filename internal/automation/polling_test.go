package automation

import (
	"context"
	"forgetmenote/internal/pg/pgtest"
	"strings"
	"testing"
	"time"
)

func TestPollingReloadsReplacedRecordsAndRejectsDamagedOnes(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "digest", Kind: "digest", Actor: "robot", Project: "default", Output: "digest.md", Instruction: "Keep this instruction", Calls: []Call{{Connection: "test", Operation: "read", Arguments: map[string]any{"path": "input"}}}}
	r, err := q.Enqueue(spec, "one")
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Schedule(nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A replacement of the same size, as a restore can write, is still reread.
	r.State = "failed"
	if err := q.write(&r); err != nil {
		t.Fatal(err)
	}
	if ran, err := q.RunNext(t.Context(), func(context.Context, *Run, func() error) error { t.Error("stale queued state executed"); return nil }); err != nil || ran {
		t.Fatal(ran, err)
	}
	r.State = "queued"
	if err := q.write(&r); err != nil {
		t.Fatal(err)
	}
	if err := q.Schedule(nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ran, err := q.RunNext(t.Context(), func(_ context.Context, run *Run, _ func() error) error {
		if Hash(run.Spec) != Hash(spec) {
			t.Error("worker received scheduling metadata instead of its complete inputs")
		}
		return nil
	}); err != nil || !ran {
		t.Fatal(ran, err)
	}
	if err := q.Schedule(nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := q.records.replace(r.ID+".json", []byte(`{"version":5`)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RunNext(t.Context(), func(context.Context, *Run, func() error) error { t.Error("damaged record executed"); return nil }); err == nil {
		t.Fatal("cached metadata bypassed record validation")
	}
}

func BenchmarkIdleQueuePolling(b *testing.B) {
	q, err := Open(pgtest.Org(b))
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	spec := learningSpec()
	for i := 0; i < 64; i++ {
		events := make([]LearningEvent, 8)
		for j := range events {
			events[j] = LearningEvent{ID: Hash([]int{i, j}), Call: Call{Arguments: map[string]any{"query": strings.Repeat("sample ", 1024)}}}
		}
		r := Run{ID: Hash(i), Spec: spec, State: "completed", Created: now, Updated: now, Learning: &LearningBatch{Expires: now.Add(time.Hour), Events: events}}
		if err := q.write(&r); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := q.List(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := q.PruneLearning(now); err != nil {
			b.Fatal(err)
		}
		if err := q.Schedule([]Spec{spec}, now); err != nil {
			b.Fatal(err)
		}
		if ran, err := q.RunNext(context.Background(), func(context.Context, *Run, func() error) error { b.Fatal("completed work reran"); return nil }); err != nil || ran {
			b.Fatal(ran, err)
		}
	}
}
