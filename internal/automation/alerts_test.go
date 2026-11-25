package automation

import (
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestIncidentRestartPreviewAndDailyIdentity(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "monitor", Alert: &AlertPolicy{ActiveAfter: 2, ResolvedAfter: 2, NotifyResolved: true}}
	now := time.Now()
	bad := Incident{Active: true, Severity: "warning", Key: "day-1", Summary: "late"}
	step := func(id string, in Incident, preview bool, want bool) {
		t.Helper()
		d, e := q.Incident(spec, id, in, now, preview)
		if e != nil || d.Notify != want {
			t.Fatalf("%s: %+v %v", id, d, e)
		}
	}
	step("preview", bad, true, false)
	step("first", bad, false, false)
	q, err = Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	step("second", bad, false, true)
	step("second", bad, false, true)
	step("third", bad, false, false)
	step("clear-first", Incident{}, false, false)
	step("clear-second", Incident{}, false, true)
	step("new-first", bad, false, false)
	step("new-second", bad, false, true)
	bad.Key = "day-2"
	step("next-day-first", bad, false, false)
	step("next-day-second", bad, false, true)
}

func TestTimeBindingsDoNotMutateDefinition(t *testing.T) {
	call := Call{Arguments: map[string]any{"start": "old"}, Time: map[string]TimeArgument{"start": {OffsetSeconds: -60, Format: "rfc3339"}}}
	bound, err := call.At(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	if err != nil || bound.Arguments["start"] != "2026-10-06T11:59:00Z" || call.Arguments["start"] != "old" {
		t.Fatal(bound, call, err)
	}
}
