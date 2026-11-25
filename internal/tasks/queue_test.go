package tasks

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func date(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestQueueNoteRoundTripsEveryField(t *testing.T) {
	q := Queue{Tasks: []Task{
		{ID: "t-7k2p9abc", Text: "Review the Q4 pricing deck", From: "p-bbbbbbbbbb", Added: "2026-11-17", Due: "2026-11-21", Priority: "high"},
		{ID: "t-2b8c1def", Text: "Draft holiday ad copy · with a dot", From: "p-aaaaaaaaaa", Added: "2026-11-15", Snoozed: "2026-11-24", Source: "https://example.slack.com/archives/C1/p123"},
		{ID: "t-quarterl", Text: "Email the Acme client", From: "p-aaaaaaaaaa", Added: "2026-11-17", Due: "2027-01-15", Every: &Recurrence{N: 3, Unit: "months", Start: "2027-01-15"}, Lead: 10},
	}}
	content := q.Format()
	if !strings.HasPrefix(content, "# Tasks\n") || !strings.Contains(content, "\n- Review the Q4 pricing deck\n  `t-7k2p9abc · from: p-bbbbbbbbbb · added: 2026-11-17 · due: 2026-11-21 · priority: high`\n") {
		t.Fatalf("unexpected note:\n%s", content)
	}
	parsed, err := Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Tasks) != 3 {
		t.Fatalf("parsed %d tasks from:\n%s", len(parsed.Tasks), content)
	}
	for i := range q.Tasks {
		got, want := parsed.Tasks[i], q.Tasks[i]
		if got.Every != nil && want.Every != nil && *got.Every == *want.Every {
			got.Every, want.Every = nil, nil
		}
		if got.Revision == "" {
			t.Fatalf("task %d has no revision", i)
		}
		got.Revision = ""
		if got != want {
			t.Fatalf("task %d = %+v, want %+v", i, got, want)
		}
	}
	if parsed.Tasks[0].Revision == parsed.Tasks[1].Revision {
		t.Fatal("two tasks share a revision")
	}
	if _, err = Parse("# Tasks\n\n- Lost metadata\n"); err == nil {
		t.Fatal("a task without its metadata line parsed")
	}
	if empty, err := Parse(""); err != nil || len(empty.Tasks) != 0 {
		t.Fatalf("an empty note: %+v %v", empty, err)
	}
}

func TestTaskInputIsOneSafeLine(t *testing.T) {
	for _, text := range []string{"", "two\nlines", strings.Repeat("x", 501), "token xoxb-1234567890-abcdefghij", "password: hunter2hunter2"} {
		if CheckText(text) == nil {
			t.Errorf("accepted %q", text)
		}
	}
	if err := CheckText("Send the `deck` to Acme · today"); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"javascript:alert(1)", "https://example.com/a b", "https://example.com/`x`", "ftp://example.com"} {
		if CheckSource(source) == nil {
			t.Errorf("accepted source %q", source)
		}
	}
	if CheckSource("https://example.slack.com/archives/C1/p123") != nil || CheckSource("") != nil {
		t.Fatal("refused a plain link")
	}
}

func TestRecurrenceCountsFromTheStartAndNeverPilesUp(t *testing.T) {
	quarterly := Recurrence{N: 3, Unit: "months", Start: "2026-01-31"}
	got := []string{}
	for k := range 5 {
		got = append(got, quarterly.Occurrence(k).Format(time.DateOnly))
	}
	if want := []string{"2026-01-31", "2026-04-30", "2026-07-31", "2026-10-31", "2027-01-31"}; !slices.Equal(got, want) {
		t.Fatalf("quarterly from Jan 31 = %v, want %v", got, want)
	}
	if r, err := ParseRecurrence("every 2 weeks", "2026-11-03"); err != nil || r.N != 2 || r.Unit != "weeks" || r.Start != "2026-11-03" {
		t.Fatalf("parse: %+v %v", r, err)
	}
	for _, bad := range []string{"0 days", "25 months", "3 fortnights", "monthly please"} {
		if _, err := ParseRecurrence(bad, "2026-11-03"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if r, err := ParseRecurrence("quarterly", "2026-11-03"); err != nil || r.N != 3 || r.Unit != "months" {
		t.Fatalf("quarterly: %+v %v", r, err)
	}

	weekly := Recurrence{N: 1, Unit: "weeks", Start: "2026-11-02"}
	// Closed early: the due occurrence is the one closed.
	c := weekly.Close("2026-11-09", date("2026-11-05"))
	if c.Closed != "2026-11-09" || c.Missed != 0 || c.Next != "2026-11-16" {
		t.Fatalf("early close = %+v", c)
	}
	// Closed three weeks late: the latest date that went by is closed, the
	// earlier ones are missed, and the next date is after today.
	c = weekly.Close("2026-11-09", date("2026-11-27"))
	if c.Closed != "2026-11-23" || c.Missed != 2 || c.Next != "2026-11-30" {
		t.Fatalf("late close = %+v", c)
	}
	// Closed on its own date.
	c = weekly.Close("2026-11-09", date("2026-11-09"))
	if c.Closed != "2026-11-09" || c.Missed != 0 || c.Next != "2026-11-16" {
		t.Fatalf("on-time close = %+v", c)
	}
	next := weekly.NextAfter(date("2026-11-10"), 2)
	if len(next) != 2 || next[0] != "2026-11-16" || next[1] != "2026-11-23" {
		t.Fatalf("next two = %v", next)
	}
}
