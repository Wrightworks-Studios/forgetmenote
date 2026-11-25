package tasks

import (
	"strings"
	"testing"
	"time"
)

func TestRemindRanksTodaysTasksByTheFixedRule(t *testing.T) {
	from := "p-bbbbbbbbbb"
	task := func(id, text string, edit func(*Task)) Task {
		t := Task{ID: id, Text: text, From: from, Added: "2026-11-19"}
		edit(&t)
		return t
	}
	monthly := func(due string) *Recurrence { return &Recurrence{N: 1, Unit: "months", Start: due} }
	weekly := func(due string) *Recurrence { return &Recurrence{N: 1, Unit: "weeks", Start: due} }
	q := Queue{Tasks: []Task{
		task("t-aaaaaaaa", "Old request", func(t *Task) { t.Added = "2026-11-02" }),
		task("t-bbbbbbbb", "Overdue", func(t *Task) { t.Due = "2026-11-18" }),
		task("t-cccccccc", "Due Tuesday", func(t *Task) { t.Due = "2026-11-24" }),
		task("t-dddddddd", "Due today", func(t *Task) { t.Due = "2026-11-20" }),
		task("t-eeeeeeee", "Important, later", func(t *Task) { t.Priority, t.Due = "high", "2026-12-15" }),
		task("t-ffffffff", "Someday", func(t *Task) { t.Priority, t.Added = "low", "2026-10-01" }),
		task("t-gggggggg", "Snoozed", func(t *Task) { t.Snoozed = "2026-11-23" }),
		task("t-hhhhhhhh", "Snooze over", func(t *Task) { t.Snoozed, t.Added = "2026-11-20", "2026-11-10" }),
		task("t-iiiiiiii", "Monthly, ten working days out", func(t *Task) { t.Due, t.Every = "2026-12-04", monthly("2026-12-04") }),
		task("t-jjjjjjjj", "Monthly, five working days out", func(t *Task) { t.Due, t.Every, t.Added = "2026-11-27", monthly("2026-11-27"), "2026-11-01" }),
		task("t-kkkkkkkk", "Weekly, next working day", func(t *Task) { t.Due, t.Every = "2026-11-23", weekly("2026-11-23") }),
		task("t-llllllll", "Weekly, two working days out", func(t *Task) { t.Due, t.Every = "2026-11-24", weekly("2026-11-24") }),
	}}
	// Friday: two working days ahead is Tuesday.
	got := []string{}
	for _, r := range q.Remind(date("2026-11-20"), nil) {
		got = append(got, r.Task.Text+": "+r.Reason)
	}
	want := []string{
		"Overdue: overdue since Wed Nov 18",
		"Due today: due today",
		"Weekly, next working day: due Mon Nov 23",
		"Due Tuesday: due Tue Nov 24",
		"Important, later: high priority, due Tue Dec 15",
		"Monthly, five working days out: due Fri Nov 27",
		"Old request: waiting 18 days",
		"Snooze over: waiting 10 days",
		"Someday: low priority, waiting 50 days",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("reminder order:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A day off does not count as a working day. With Thursday off, Friday is
	// within two working days of Tuesday, so the monthly task joins the tasks
	// due soon, after those overdue and due today.
	working := func(day time.Time) bool {
		return day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && day.Format(time.DateOnly) != "2026-11-26"
	}
	tuesday := q.Remind(date("2026-11-24"), working)
	if len(tuesday) < 6 || tuesday[5].Task.ID != "t-jjjjjjjj" || tuesday[5].Reason != "due Fri Nov 27" {
		t.Fatalf("Tuesday's reminder before a day off: %+v", tuesday)
	}
	if plain := q.Remind(date("2026-11-24"), nil); len(plain) < 6 || plain[5].Task.ID != "t-eeeeeeee" {
		t.Fatalf("Tuesday's reminder in a full week: %+v", plain)
	}
	if thursday := q.Remind(date("2026-11-19"), nil); len(thursday) < 2 || thursday[1].Reason != "due tomorrow" {
		t.Fatalf("Thursday's reminder: %+v", thursday)
	}

	// A lead time of its own brings a recurring task in earlier.
	early := Queue{Tasks: []Task{task("t-mmmmmmmm", "Quarterly board pack", func(t *Task) { t.Due, t.Every, t.Lead = "2026-12-04", monthly("2026-12-04"), 10 })}}
	if got := early.Remind(date("2026-11-20"), nil); len(got) != 1 || got[0].Reason != "due Fri Dec 4" {
		t.Fatalf("a ten-day lead on Friday: %+v", got)
	}
	if got := early.Remind(date("2026-11-19"), nil); len(got) != 0 {
		t.Fatalf("a ten-day lead eleven working days out: %+v", got)
	}
	if err := (Task{ID: "t-nnnnnnnn", Text: "Once", From: from, Added: "2026-11-19", Due: "2026-12-04", Lead: 3}).Check(); err == nil {
		t.Fatal("a task that does not repeat took a lead time")
	}
	if err := (Task{ID: "t-nnnnnnnn", Text: "Too early", From: from, Added: "2026-11-19", Due: "2026-12-04", Every: monthly("2026-12-04"), Lead: 61}).Check(); err == nil {
		t.Fatal("a lead time over 60 working days was accepted")
	}
}
