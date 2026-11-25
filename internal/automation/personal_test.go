package automation

import (
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestPersonalQueueCancelsSupersededConsentAfterRestart(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	account := "0123456789abcdef0123456789abcdef"
	spec := Spec{ID: "personal-" + account, Kind: "personal", Actor: "alice", Project: "assistant-" + account, Personal: &Personal{Account: account, Settings: 1, Policy: 1}, IntervalSeconds: 60, ExternalProcessing: true}
	created, err := q.Enqueue(spec, "old")
	if err != nil {
		t.Fatal(err)
	}
	q, err = Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	spec.Personal.Settings++
	if err = q.Schedule([]Spec{spec}, time.Now()); err != nil {
		t.Fatal(err)
	}
	old, err := q.Get(created.ID)
	if err != nil || old.State != "cancelled" {
		t.Fatal("stale consent kept the queue busy", err)
	}
	if err = q.Schedule(nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	runs, err := q.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if !terminal(run.State) {
			t.Fatal("disabled employee remained scheduled")
		}
	}
	if err = spec.ValidateConfigured(); err == nil {
		t.Fatal("operator file could impersonate personal consent")
	}
}
