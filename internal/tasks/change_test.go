package tasks

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	alice = "p-aaaaaaaaaa"
	bob   = "p-bbbbbbbbbb"
	carol = "p-cccccccccc"
)

func text(s string) *string { return &s }
func days(n int) *int       { return &n }

// filed is a task bob filed in alice's queue, read at revision r1.
func filed() Task {
	return Task{ID: "t-aaaaaaaa", Text: "Book the venue", From: bob, Added: "2026-11-02", Revision: "r1"}
}

// series is filed repeating weekly from 2026-11-09, due then, snoozed and
// with a lead time.
func series() Task {
	t := filed()
	t.Due, t.Snoozed, t.Lead = "2026-11-09", "2026-11-12", 3
	t.Every = &Recurrence{N: 1, Unit: "weeks", Start: "2026-11-09"}
	return t
}

func with(t Task, change func(*Task)) Task {
	change(&t)
	return t
}

func TestDecideRefusesWhatTheCallerMayNotDo(t *testing.T) {
	const badReason = "A reason is one line of at most 200 bytes, without backticks or credentials"
	edits := map[string]Change{
		"text":     {Text: text("x")},
		"due":      {Due: text("")},
		"priority": {Priority: text("high")},
		"snooze":   {SnoozeUntil: text("")},
		"every":    {Every: text("weekly")},
		"lead":     {Lead: days(0)},
	}
	type refusal struct {
		name   string
		task   Task
		caller string
		change Change
		kind   error
		says   string
	}
	cases := []refusal{
		{"a reason over 200 bytes", filed(), alice, Change{Outcome: "done", Reason: strings.Repeat("x", 201)}, ErrInvalid, badReason},
		{"a reason of two lines", filed(), alice, Change{Outcome: "done", Reason: "two\nlines"}, ErrInvalid, badReason},
		{"a reason with a carriage return", filed(), alice, Change{Outcome: "done", Reason: "two\rlines"}, ErrInvalid, badReason},
		{"a reason with a backtick", filed(), alice, Change{Outcome: "done", Reason: "see `notes`"}, ErrInvalid, badReason},
		{"a reason with a credential", filed(), alice, Change{Outcome: "done", Reason: "password: hunter2hunter2"}, ErrInvalid, badReason},
		{"a stranger's bad reason", filed(), carol, Change{Outcome: "done", Reason: "two\nlines"}, ErrInvalid, badReason},
		{"a stranger", filed(), carol, Change{Revision: "r1", Text: text("Mine now")}, ErrNotFound, "No open task t-aaaaaaaa in your queue or among the tasks you filed"},
		{"a stranger with a stale revision", filed(), carol, Change{Revision: "r0", Outcome: "done"}, ErrNotFound, "No open task t-aaaaaaaa in your queue or among the tasks you filed"},
		{"no caller", filed(), "", Change{Revision: "r1", Outcome: "done"}, ErrNotFound, "No open task t-aaaaaaaa in your queue or among the tasks you filed"},
		{"a stale revision", filed(), alice, Change{Revision: "r0", Outcome: "done"}, ErrStale, "Task t-aaaaaaaa changed since you read it; list it again and retry"},
		{"a stale revision before the role", filed(), bob, Change{Revision: "r0", Outcome: "done"}, ErrStale, "Task t-aaaaaaaa changed since you read it; list it again and retry"},
		{"the requester closes", filed(), bob, Change{Revision: "r1", Outcome: "done"}, ErrForbidden, "Only the queue's owner marks a task done or not applicable; you can withdraw a task you filed"},
		{"the requester finds it not applicable", filed(), bob, Change{Revision: "r1", Outcome: "not_applicable", Reason: "Moot"}, ErrForbidden, "Only the queue's owner marks a task done or not applicable; you can withdraw a task you filed"},
		{"not applicable without a reason", filed(), alice, Change{Revision: "r1", Outcome: "not_applicable", Reason: "  "}, ErrInvalid, "Say why the task is not applicable"},
		{"the owner withdraws", filed(), alice, Change{Revision: "r1", Outcome: "withdrawn"}, ErrForbidden, "Only the person who filed a task withdraws it"},
		{"an unknown outcome", filed(), bob, Change{Revision: "r1", Outcome: "cancelled"}, ErrInvalid, "Outcome must be done, not_applicable or withdrawn"},
		{"the requester reassigns", filed(), bob, Change{Revision: "r1", ReassignTo: carol}, ErrForbidden, "Only the queue's owner reassigns a task"},
		{"a reassignment to the same queue", filed(), alice, Change{Revision: "r1", ReassignTo: alice}, ErrInvalid, "The task is already in that queue"},
		{"the requester sets priority", filed(), bob, Change{Revision: "r1", Priority: text("high")}, ErrForbidden, "Only the queue's owner sets priority"},
		{"the requester snoozes", filed(), bob, Change{Revision: "r1", SnoozeUntil: text("2026-11-25")}, ErrForbidden, "Only the queue's owner snoozes a task"},
		{"priority is refused before snooze", filed(), bob, Change{Revision: "r1", SnoozeUntil: text("2026-11-25"), Priority: text("low")}, ErrForbidden, "Only the queue's owner sets priority"},
		{"an unknown priority", filed(), alice, Change{Revision: "r1", Priority: text("urgent")}, ErrInvalid, "Priority must be high, normal or low"},
		{"stopping a task that does not repeat", filed(), alice, Change{Revision: "r1", Every: text(""), Reason: "Done"}, ErrInvalid, "The task does not repeat"},
		{"stopping a schedule without a reason", series(), bob, Change{Revision: "r1", Every: text(" ")}, ErrInvalid, "Say why the task stops repeating"},
		{"a schedule without a due date", filed(), alice, Change{Revision: "r1", Every: text("monthly")}, ErrInvalid, "A recurring task needs a due date: its first date"},
		{"a schedule whose due date is cleared", series(), alice, Change{Revision: "r1", Due: text(""), Every: text("monthly")}, ErrInvalid, "A recurring task needs a due date: its first date"},
		{"an unknown schedule", with(filed(), func(t *Task) { t.Due = "2026-12-01" }), alice, Change{Revision: "r1", Every: text("fortnightly")}, ErrInvalid, `a schedule repeats every 1 to 24 days, weeks, months or years, such as "3 months" or "quarterly"`},
		{"a series loses its due date", series(), bob, Change{Revision: "r1", Due: text(" ")}, ErrInvalid, "A recurring task keeps a due date; stop repeating it first"},
		{"nothing to change", filed(), alice, Change{Revision: "r1", Reason: "Just because"}, ErrInvalid, "Nothing to change"},
		{"empty text", filed(), bob, Change{Revision: "r1", Text: text("   ")}, ErrInvalid, "task text must be one line of 1 to 500 characters"},
		{"a credential in the text", filed(), bob, Change{Revision: "r1", Text: text("token xoxb-1234567890-abcdefghij")}, ErrInvalid, "this looks like a credential; tasks never store secrets. Say where the secret lives instead"},
		{"an invalid due date", filed(), bob, Change{Revision: "r1", Due: text("2026-13-01")}, ErrInvalid, "dates must look like 2026-11-21"},
		{"an invalid snooze date", filed(), alice, Change{Revision: "r1", SnoozeUntil: text("tomorrow")}, ErrInvalid, "dates must look like 2026-11-21"},
		{"a lead time on a one-off task", filed(), alice, Change{Revision: "r1", Lead: days(5)}, ErrInvalid, "only a recurring task has a lead time, of 1 to 60 working days"},
		{"a lead time over 60 days", series(), alice, Change{Revision: "r1", Lead: days(61)}, ErrInvalid, "only a recurring task has a lead time, of 1 to 60 working days"},
		{"a negative lead time", series(), alice, Change{Revision: "r1", Lead: days(-1)}, ErrInvalid, "only a recurring task has a lead time, of 1 to 60 working days"},
	}
	for field, edit := range edits {
		edit.Revision, edit.ReassignTo = "r1", carol
		cases = append(cases, refusal{"a reassignment that also changes " + field, filed(), alice, edit, ErrInvalid, "Reassign a task on its own, then change it"})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := c.task
			d, err := Decide(c.task, alice, c.caller, c.change, date("2026-11-20"))
			var refused Refusal
			if !errors.As(err, &refused) || !errors.Is(err, c.kind) || err.Error() != c.says {
				t.Fatalf("Decide = %+v, %v; want %v %q", d, err, c.kind, c.says)
			}
			if refused.Kind != c.kind || refused.Message != c.says {
				t.Fatalf("refusal = %+v", refused)
			}
			if !reflect.DeepEqual(c.task, before) {
				t.Fatalf("the task changed: %+v", c.task)
			}
		})
	}
}

func TestDecideClosesReassignsAndEdits(t *testing.T) {
	cases := []struct {
		name   string
		task   Task
		caller string
		change Change
		want   Decision
	}{
		// Closing.
		{"the owner closes", filed(), alice, Change{Revision: "r1", Outcome: "done"},
			Decision{Task: filed(), Owner: alice, Closed: "done", Summary: "Task t-aaaaaaaa: done by " + alice}},
		{"a closing reason is trimmed", filed(), alice, Change{Revision: "r1", Outcome: "done", Reason: "\n Sent on Friday \t"},
			Decision{Task: filed(), Owner: alice, Closed: "done", Reason: "Sent on Friday", Summary: "Task t-aaaaaaaa: done by " + alice + ": Sent on Friday"}},
		{"a reason of 200 bytes", filed(), alice, Change{Revision: "r1", Outcome: "done", Reason: strings.Repeat("é", 100)},
			Decision{Task: filed(), Owner: alice, Closed: "done", Reason: strings.Repeat("é", 100), Summary: "Task t-aaaaaaaa: done by " + alice + ": " + strings.Repeat("é", 100)}},
		{"not applicable", filed(), alice, Change{Revision: "r1", Outcome: "not_applicable", Reason: "The venue closed"},
			Decision{Task: filed(), Owner: alice, Closed: "not_applicable", Reason: "The venue closed", Summary: "Task t-aaaaaaaa: not applicable by " + alice + ": The venue closed"}},
		{"the requester withdraws", filed(), bob, Change{Revision: "r1", Outcome: "withdrawn"},
			Decision{Task: filed(), Owner: alice, Closed: "withdrawn", Summary: "Task t-aaaaaaaa: withdrawn by " + bob}},
		{"an own task is withdrawn", with(filed(), func(t *Task) { t.From = alice }), alice, Change{Revision: "r1", Outcome: "withdrawn", Reason: "Not needed"},
			Decision{Task: with(filed(), func(t *Task) { t.From = alice }), Owner: alice, Closed: "withdrawn", Reason: "Not needed", Summary: "Task t-aaaaaaaa: withdrawn by " + alice + ": Not needed"}},
		{"an own task is closed", with(filed(), func(t *Task) { t.From = alice }), alice, Change{Revision: "r1", Outcome: "done"},
			Decision{Task: with(filed(), func(t *Task) { t.From = alice }), Owner: alice, Closed: "done", Summary: "Task t-aaaaaaaa: done by " + alice}},
		{"an outcome ignores edits and reassignment", filed(), alice, Change{Revision: "r1", Outcome: "done", Text: text("Other"), Priority: text("urgent"), ReassignTo: carol},
			Decision{Task: filed(), Owner: alice, Closed: "done", Summary: "Task t-aaaaaaaa: done by " + alice}},
		{"a late occurrence moves to the next date and wakes", series(), alice, Change{Revision: "r1", Outcome: "done"},
			Decision{Task: with(series(), func(t *Task) { t.Due, t.Snoozed = "2026-11-23", "" }), Owner: alice, Occurrence: &Closing{Closed: "2026-11-16", Missed: 1, Next: "2026-11-23"},
				Summary: "Task t-aaaaaaaa: done for 2026-11-16 by " + alice + "; missed 1; next 2026-11-23"}},
		{"an early occurrence", with(series(), func(t *Task) { t.Due = "2026-11-23" }), alice, Change{Revision: "r1", Outcome: "not_applicable", Reason: "Holiday"},
			Decision{Task: with(series(), func(t *Task) { t.Due, t.Snoozed = "2026-11-30", "" }), Owner: alice, Occurrence: &Closing{Closed: "2026-11-23", Next: "2026-11-30"}, Reason: "Holiday",
				Summary: "Task t-aaaaaaaa: not applicable for 2026-11-23 by " + alice + "; next 2026-11-30: Holiday"}},
		{"a withdrawn series leaves", series(), bob, Change{Revision: "r1", Outcome: "withdrawn"},
			Decision{Task: series(), Owner: alice, Closed: "withdrawn", Summary: "Task t-aaaaaaaa: withdrawn by " + bob}},

		// Reassigning.
		{"the owner reassigns", with(series(), func(t *Task) { t.Priority = "high" }), alice, Change{Revision: "r1", ReassignTo: carol},
			Decision{Task: with(series(), func(t *Task) { t.Snoozed = "" }), Owner: carol, Closed: "reassigned", Summary: "Task t-aaaaaaaa: reassigned from " + alice + " to " + carol + " by " + alice}},
		{"a task reassigned to its requester", filed(), alice, Change{Revision: "r1", ReassignTo: bob, Reason: "Yours again"},
			Decision{Task: filed(), Owner: bob, Closed: "reassigned", Reason: "Yours again", Summary: "Task t-aaaaaaaa: reassigned from " + alice + " to " + bob + " by " + alice + ": Yours again"}},

		// Editing.
		{"the requester edits the text", filed(), bob, Change{Revision: "r1", Text: text("  Book the bigger venue ")},
			Decision{Task: with(filed(), func(t *Task) { t.Text = "Book the bigger venue" }), Owner: alice, Summary: "Task t-aaaaaaaa: changed text by " + bob}},
		{"an edit with a reason", filed(), alice, Change{Revision: "r1", Text: text("Book it"), Reason: "Typo"},
			Decision{Task: with(filed(), func(t *Task) { t.Text = "Book it" }), Owner: alice, Reason: "Typo", Summary: "Task t-aaaaaaaa: changed text by " + alice + ": Typo"}},
		{"the requester sets a due date", filed(), bob, Change{Revision: "r1", Due: text(" 2026-12-03 ")},
			Decision{Task: with(filed(), func(t *Task) { t.Due = "2026-12-03" }), Owner: alice, Summary: "Task t-aaaaaaaa: changed due date by " + bob}},
		{"the requester clears a due date", with(filed(), func(t *Task) { t.Due = "2026-12-03" }), bob, Change{Revision: "r1", Due: text("")},
			Decision{Task: filed(), Owner: alice, Summary: "Task t-aaaaaaaa: changed due date by " + bob}},
		{"the owner sets priority", filed(), alice, Change{Revision: "r1", Priority: text(" HIGH")},
			Decision{Task: with(filed(), func(t *Task) { t.Priority = "high" }), Owner: alice, Summary: "Task t-aaaaaaaa: changed priority by " + alice}},
		{"normal priority clears it", with(filed(), func(t *Task) { t.Priority = "low" }), alice, Change{Revision: "r1", Priority: text("Normal")},
			Decision{Task: filed(), Owner: alice, Summary: "Task t-aaaaaaaa: changed priority by " + alice}},
		{"empty priority clears it", with(filed(), func(t *Task) { t.Priority = "low" }), alice, Change{Revision: "r1", Priority: text("")},
			Decision{Task: filed(), Owner: alice, Summary: "Task t-aaaaaaaa: changed priority by " + alice}},
		{"the owner snoozes", filed(), alice, Change{Revision: "r1", SnoozeUntil: text("2026-11-25 ")},
			Decision{Task: with(filed(), func(t *Task) { t.Snoozed = "2026-11-25" }), Owner: alice, Summary: "Task t-aaaaaaaa: changed snooze by " + alice}},
		{"the owner wakes a task", with(filed(), func(t *Task) { t.Snoozed = "2026-11-25" }), alice, Change{Revision: "r1", SnoozeUntil: text("")},
			Decision{Task: filed(), Owner: alice, Summary: "Task t-aaaaaaaa: changed snooze by " + alice}},
		{"the requester sets a schedule", with(filed(), func(t *Task) { t.Due = "2026-12-01" }), bob, Change{Revision: "r1", Every: text("monthly")},
			Decision{Task: with(filed(), func(t *Task) { t.Due, t.Every = "2026-12-01", &Recurrence{N: 1, Unit: "months", Start: "2026-12-01"} }), Owner: alice, Summary: "Task t-aaaaaaaa: changed schedule by " + bob}},
		{"a schedule from a new due date", filed(), alice, Change{Revision: "r1", Due: text("2026-12-02"), Every: text("2 weeks")},
			Decision{Task: with(filed(), func(t *Task) { t.Due, t.Every = "2026-12-02", &Recurrence{N: 2, Unit: "weeks", Start: "2026-12-02"} }), Owner: alice, Summary: "Task t-aaaaaaaa: changed due date, schedule by " + alice}},
		{"a new schedule for a series", series(), alice, Change{Revision: "r1", Every: text("quarterly")},
			Decision{Task: with(series(), func(t *Task) { t.Every = &Recurrence{N: 3, Unit: "months", Start: "2026-11-09"} }), Owner: alice, Summary: "Task t-aaaaaaaa: changed schedule by " + alice}},
		{"the requester stops a schedule", series(), bob, Change{Revision: "r1", Every: text(""), Reason: "Contract ended"},
			Decision{Task: with(series(), func(t *Task) { t.Every, t.Lead = nil, 0 }), Owner: alice, Reason: "Contract ended", Summary: "Task t-aaaaaaaa: changed stopped repeating by " + bob + ": Contract ended"}},
		{"a new due date restarts a series", series(), alice, Change{Revision: "r1", Due: text("2026-11-26")},
			Decision{Task: with(series(), func(t *Task) { t.Due, t.Every = "2026-11-26", &Recurrence{N: 1, Unit: "weeks", Start: "2026-11-26"} }), Owner: alice, Summary: "Task t-aaaaaaaa: changed due date by " + alice}},
		{"the same due date keeps a series", with(series(), func(t *Task) { t.Due = "2026-11-16" }), alice, Change{Revision: "r1", Due: text("2026-11-16")},
			Decision{Task: with(series(), func(t *Task) { t.Due = "2026-11-16" }), Owner: alice, Summary: "Task t-aaaaaaaa: changed due date by " + alice}},
		{"the requester sets a lead time", series(), bob, Change{Revision: "r1", Lead: days(4)},
			Decision{Task: with(series(), func(t *Task) { t.Lead = 4 }), Owner: alice, Summary: "Task t-aaaaaaaa: changed lead time by " + bob}},
		{"a lead time of 0 restores the default", series(), alice, Change{Revision: "r1", Lead: days(0)},
			Decision{Task: with(series(), func(t *Task) { t.Lead = 0 }), Owner: alice, Summary: "Task t-aaaaaaaa: changed lead time by " + alice}},
		{"every field at once, in order", series(), alice, Change{Revision: "r1", Lead: days(2), Every: text("2 weeks"), SnoozeUntil: text(""), Priority: text("low"), Due: text("2026-11-12"), Text: text("Book the hall"), Reason: "Rescoped"},
			Decision{Task: with(series(), func(t *Task) {
				t.Text, t.Due, t.Priority, t.Snoozed, t.Lead = "Book the hall", "2026-11-12", "low", "", 2
				t.Every = &Recurrence{N: 2, Unit: "weeks", Start: "2026-11-12"}
			}), Owner: alice, Reason: "Rescoped", Summary: "Task t-aaaaaaaa: changed text, due date, priority, snooze, schedule, lead time by " + alice + ": Rescoped"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := c.task
			start := Recurrence{}
			if before.Every != nil {
				start = *before.Every
			}
			got, err := Decide(c.task, alice, c.caller, c.change, date("2026-11-20"))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Decide =\n%+v %+v %+v\nwant\n%+v %+v %+v", got, got.Task.Every, got.Occurrence, c.want, c.want.Task.Every, c.want.Occurrence)
			}
			// The task as read is left as it was, schedule included.
			if !reflect.DeepEqual(c.task, before) || before.Every != nil && *before.Every != start {
				t.Fatalf("the task read changed: %+v %+v", c.task, c.task.Every)
			}
		})
	}
}

func TestNoticeNamesWhatOthersHear(t *testing.T) {
	dated := func(due string) Task { return with(filed(), func(t *Task) { t.Due = due }) }
	cases := []struct {
		name          string
		decision      Decision
		change        Change
		caller        string
		before, after Task
		owner         string
		want          string
	}{
		{"a closed task", Decision{Closed: "done"}, Change{Outcome: "done"}, alice, filed(), filed(), alice, "done"},
		{"a task not applicable", Decision{Closed: "not_applicable"}, Change{Outcome: "not_applicable"}, alice, filed(), filed(), alice, "not_applicable"},
		{"a withdrawn task", Decision{Closed: "withdrawn"}, Change{Outcome: "withdrawn"}, bob, filed(), filed(), alice, "withdrawn"},
		{"a closed occurrence", Decision{Occurrence: &Closing{}}, Change{Outcome: "not_applicable"}, alice, series(), series(), alice, "not_applicable"},
		{"a reassigned task", Decision{Closed: "reassigned"}, Change{ReassignTo: carol}, alice, filed(), filed(), carol, "reassigned"},
		{"a date the requester moved earlier", Decision{}, Change{Due: text("2026-12-03")}, bob, dated("2026-12-10"), dated("2026-12-03"), alice, "moved earlier"},
		{"a date the owner moved earlier", Decision{}, Change{Due: text("2026-12-03")}, alice, dated("2026-12-10"), dated("2026-12-03"), alice, ""},
		{"a later date", Decision{}, Change{Due: text("2026-12-17")}, bob, dated("2026-12-10"), dated("2026-12-17"), alice, ""},
		{"a first date", Decision{}, Change{Due: text("2026-12-03")}, bob, filed(), dated("2026-12-03"), alice, ""},
		{"a cleared date", Decision{}, Change{Due: text("")}, bob, dated("2026-12-10"), filed(), alice, ""},
		{"an earlier date the change did not set", Decision{}, Change{Text: text("x")}, bob, dated("2026-12-10"), dated("2026-12-03"), alice, ""},
		{"an edit", Decision{}, Change{Text: text("x")}, bob, filed(), filed(), alice, ""},
	}
	for _, c := range cases {
		if got := c.decision.Notice(c.change, c.caller, c.before, c.after, c.owner); got != c.want {
			t.Errorf("%s: Notice = %q, want %q", c.name, got, c.want)
		}
	}
}
