package personal

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"forgetmenote/internal/connectors"
)

func TestPersonalChangedEvidenceSupersedesInsteadOfBlocking(t *testing.T) {
	old := Observation{Key: "email:t1", Revision: "old", Source: connectors.PersonalOperation{Provider: "google", Generation: 1, Operation: "email.thread", Arguments: map[string]any{"id": "t1"}}}
	next := old
	next.Revision = "new"
	id := personalFingerprint(old)
	state := State{Seen: map[string]string{}, Drafts: map[string]Draft{id: {ID: id, State: "ready", Source: old}}, Pending: []Observation{old}}
	supersedePersonal(&state, old)
	if len(state.Reads) != 1 || state.Drafts[id].State != "superseded" {
		t.Fatal("old work blocks new evidence")
	}
	observePersonal(&state, next)
	observePersonal(&state, next)
	if len(state.Pending) != 1 || state.Pending[0].Revision != "new" {
		t.Fatal("stale or duplicate input retained")
	}
	resolvePersonal(&state, old.Key, "resolved")
}

func TestPersonalCalendarReminderUsesEventAndEmployeeZone(t *testing.T) {
	for _, test := range []struct{ event, want string }{
		{`{"start":{"dateTime":"2026-10-09T10:00:00-06:00"}}`, "2026-10-09T15:30:00Z"},
		{`{"start":{"date":"2026-10-09"}}`, "2026-10-09T14:30:00Z"},
	} {
		got := personalCalendarReminder(Observation{Content: json.RawMessage(test.event)}, "America/Denver")
		if got != test.want {
			t.Fatalf("reminder=%s want=%s", got, test.want)
		}
	}
	if personalCalendarReminder(Observation{Content: json.RawMessage(`{}`)}, "UTC") != "" {
		t.Fatal("invented event time")
	}
}

func TestPersonalQuietHours(t *testing.T) {
	now := time.Date(2026, 10, 9, 5, 0, 0, 0, time.UTC)
	if !personalQuiet(now, "America/Denver", "22:00", "07:00") || personalQuiet(now, "America/Denver", "08:00", "17:00") {
		t.Fatal("overnight or timezone quiet hours incorrect")
	}
}

func TestQueueSlackThreadsQueuesEachThreadOnce(t *testing.T) {
	read := func(generation uint64, operation string, args map[string]any) connectors.PersonalOperation {
		return connectors.PersonalOperation{Provider: "slack", Generation: generation, Operation: operation, Arguments: args}
	}
	history := read(1, "slack.history", map[string]any{"channel": "D1", "oldest": "1"})
	state := State{Reads: []connectors.PersonalOperation{
		history,
		read(1, "slack.thread", map[string]any{"channel": "D1", "ts": "1.1"}),
		read(2, "slack.thread", map[string]any{"channel": "D1", "ts": "1.2"}),
		read(1, "slack.history", map[string]any{"channel": "D1", "ts": "1.4"}),
	}}
	messages := []slackMessage{
		{user: "U2", ts: "1.1"},                // already queued
		{user: "U2", ts: "1.3", thread: "1.1"}, // a reply in a queued thread
		{ts: "1.9", automated: true},           // a bot or join message
		{user: "U2", ts: "1.2"},                // queued only for another connection generation
		{user: "U2", ts: "1.4"},                // a history read is not a thread read
		{user: "U2", ts: "1.5", thread: "1.4"}, // the same thread again on this page
	}
	if err := queueSlackThreads(&state, history, "D1", messages); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range state.Reads[4:] {
		raw, _ := json.Marshal(r)
		got = append(got, string(raw))
	}
	samePersonal(t, "queued threads", got, []string{
		`{"provider":"slack","generation":1,"operation":"slack.thread","arguments":{"channel":"D1","ts":"1.2"}}`,
		`{"provider":"slack","generation":1,"operation":"slack.thread","arguments":{"channel":"D1","ts":"1.4"}}`,
	})
	if err := queueSlackThreads(&state, history, "D1", []slackMessage{{user: "U2"}}); err == nil || err.Error() != "Slack message omitted timestamp" {
		t.Fatalf("message without timestamp accepted: %v", err)
	}
}

// BenchmarkQueueSlackThreads queues the threads of one full Slack history page
// (100 messages in 50 threads) behind reads already pending.
func BenchmarkQueueSlackThreads(b *testing.B) {
	call := connectors.PersonalOperation{Provider: "slack", Generation: 1, Operation: "slack.history", Arguments: map[string]any{"channel": "D1", "oldest": "1"}}
	messages := []slackMessage{}
	for i := range 50 {
		ts := fmt.Sprintf("2.%04d", i)
		messages = append(messages, slackMessage{user: "U2", ts: ts}, slackMessage{user: "U3", ts: ts + "1", thread: ts})
	}
	for _, pending := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("pending-%d", pending), func(b *testing.B) {
			queued := make([]connectors.PersonalOperation, pending)
			for i := range queued {
				queued[i] = connectors.PersonalOperation{Provider: "slack", Generation: 1, Operation: "slack.thread", Arguments: map[string]any{"channel": "D0", "ts": fmt.Sprintf("1.%04d", i)}}
			}
			for b.Loop() {
				state := State{Reads: queued[:pending:pending]}
				if err := queueSlackThreads(&state, call, "D1", messages); err != nil || len(state.Reads) != pending+50 {
					b.Fatal(err, len(state.Reads))
				}
			}
		})
	}
}

func samePersonal(t *testing.T, what string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("%s:\n got %s\nwant %s", what, g, w)
	}
}
