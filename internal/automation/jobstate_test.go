package automation

import (
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestJobStateCASReplayAndEffectsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "mail", Actor: "robot", Project: "finance"}
	value := json.RawMessage(`{"cursor":"9007199254740995"}`)
	saved, err := q.SaveJobState(spec, "", value, false)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := q.SaveJobState(spec, "", value, false)
	if err != nil || replay.Revision != saved.Revision {
		t.Fatal("crash-after-save replay", err)
	}
	e, created, err := q.BeginEffect(Effect{Scope: "mail", Action: "one", InputHash: Hash("payload")})
	if err != nil || !created {
		t.Fatal(err)
	}
	q, err = Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = q.BeginEffect(Effect{Scope: "mail", Action: "new-run-new-step", InputHash: Hash("different")}); err == nil {
		t.Fatal("uncertain effect lost across restart")
	}
	if _, err = q.FinishEffect(e.ID, "unknown", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = q.ResolveEffect(e.ID, "accepted", "Provider message message1 verified", json.RawMessage(`{"id":"message1"}`)); err != nil {
		t.Fatal(err)
	}
	got, created, err := q.BeginEffect(Effect{Scope: "mail", Action: "one", InputHash: Hash("payload")})
	if err != nil || created || string(got.Result) != `{"id":"message1"}` {
		t.Fatal(got, err)
	}
	if err = q.PruneLearning(time.Now().AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if state, err := q.JobState(spec); err != nil || state.Revision != saved.Revision {
		t.Fatal("state pruned", err)
	}
}

func TestSensitiveJournalRetentionKeepsOutcomeBarrier(t *testing.T) {
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	spec := scriptSpec(t)
	spec.ID = "mail"
	spec.Script.RetentionHours = 1
	now := time.Now()
	r := Run{ID: Hash("sensitive"), Spec: spec, State: "action-required", Created: now.Add(-2 * time.Hour), Updated: now.Add(-2 * time.Hour), Result: "private reply", Script: &ScriptCheckpoint{Steps: map[string]*ScriptStep{"write": {Operation: "connectors.submit", State: "started", Effect: true, Input: json.RawMessage(`{"text":"private mail"}`)}}}}
	if err = q.write(&r); err != nil {
		t.Fatal(err)
	}
	if _, _, err = q.BeginEffect(Effect{Scope: "mail", Action: "one", InputHash: Hash("payload")}); err != nil {
		t.Fatal(err)
	}
	if err = q.PruneLearning(now); err != nil {
		t.Fatal(err)
	}
	got, err := q.Get(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result != "" || len(got.Script.Steps["write"].Input) != 0 {
		t.Fatal("sensitive journal retained beyond window")
	}
	if !got.Script.Uncertain() {
		t.Fatal("unknown outcome erased")
	}
	if _, _, err = q.BeginEffect(Effect{Scope: "mail", Action: "new", InputHash: Hash("new")}); err == nil {
		t.Fatal("redaction enabled duplicate action")
	}
}
