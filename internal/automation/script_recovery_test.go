package automation

import (
	"context"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestUnknownScriptOutcomeBlocksScheduledAndManualRepeats(t *testing.T) {
	spec := scriptSpec(t)
	spec.Disabled = false
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	original, err := q.Enqueue(spec, "first")
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.RunNext(t.Context(), func(ctx context.Context, r *Run, save func() error) error {
		r.Script = &ScriptCheckpoint{Steps: map[string]*ScriptStep{"paid": {State: "started", Effect: true}}}
		if err := save(); err != nil {
			return err
		}
		return errors.New("model outcome unknown")
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := q.Get(original.ID)
	if got.State != "action-required" {
		t.Fatalf("unknown outcome can repeat: %+v", got)
	}
	if _, err = q.Enqueue(spec, "manual"); err == nil {
		t.Fatal("manual replay bypassed reconciliation")
	}
	if err = q.Schedule([]Spec{spec}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	runs, err := q.List()
	if err != nil || len(runs) != 1 {
		t.Fatal("scheduled another paid attempt", len(runs), err)
	}
}

func TestScriptConfigurationChangeRetainsUncertainCheckpoint(t *testing.T) {
	spec := scriptSpec(t)
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	original, err := q.Enqueue(spec, "first")
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.RunNext(t.Context(), func(ctx context.Context, r *Run, save func() error) error {
		r.Script = &ScriptCheckpoint{Steps: map[string]*ScriptStep{"paid": {State: "started", Effect: true}}}
		if err := save(); err != nil {
			return err
		}
		if err := q.CancelSuperseded(spec.ID, "changed"); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := q.Get(original.ID)
	if got.State != "action-required" || !got.Script.Uncertain() {
		t.Fatalf("changed definition lost pending generation: %+v", got)
	}
}
