package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/taskqueue"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// taskChangeHarness changes tasks as alice (member UALICE), bob (UBOB), carol
// (an account with a queue and no member) and dave (an account with no queue),
// through MCP and through the HTTP action API.
type taskChangeHarness struct {
	f      *slackLinkFixture
	mcp    map[string]*mcp.ClientSession
	tokens map[string]string
	// names writes person IDs as user names.
	names *strings.Replacer
}

func newTaskChangeHarness(t *testing.T) *taskChangeHarness {
	t.Helper()
	f := newTaskFixture(t)
	app := f.app
	now := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	app.tasks.Clock = func() time.Time { return now }
	f.members = []map[string]any{
		{"id": "UALICE", "team_id": "T123", "profile": map[string]any{"real_name": "Alice Lee"}},
		{"id": "UBOB", "team_id": "T123", "profile": map[string]any{"real_name": "Bob Ray"}},
	}
	for _, u := range []catalog.User{{ID: "carol", DisplayName: "Carol Diaz"}, {ID: "dave"}} {
		if err := app.catalog.SetUser(catalog.Bootstrap, u); err != nil {
			t.Fatal(err)
		}
	}
	users := []string{"alice", "bob", "carol", "dave"}
	sessions := taskClients(t, app, users...)
	h := &taskChangeHarness{f: f, mcp: map[string]*mcp.ClientSession{}, tokens: map[string]string{}}
	pairs := []string{}
	for i, user := range users {
		h.mcp[user] = sessions[i]
		grant, err := issueCredential(app.catalog, app.oauth, user, CredentialSpec{Name: "tasks", AllProjects: true, Scopes: []string{"notes:read", "notes:write"}, Audiences: []string{"api"}, Days: 1})
		if err != nil {
			t.Fatal(err)
		}
		h.tokens[user] = grant.Token
		if user == "dave" {
			continue
		}
		person, err := app.catalog.TaskPersonForAccount(user, true)
		if err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, person.ID, user)
	}
	h.names = strings.NewReplacer(pairs...)
	return h
}

// posts is how many messages the fake Slack has received.
func (h *taskChangeHarness) posts() int {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	return len(h.f.posts)
}

// notices describes the messages sent since the first one, one per line.
func (h *taskChangeHarness) notices(from int, id string) []string {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	out := []string{}
	for _, p := range h.f.posts[from:] {
		text, _ := p["text"].(string)
		out = append(out, fmt.Sprintf("notice %v: %s", p["channel"], strings.ReplaceAll(strings.ReplaceAll(text, id, "TASK"), "\n", " / ")))
	}
	return out
}

// update sends update_task as user and returns the result, or the refusal as
// "status code: message" over HTTP and as the message alone over MCP.
func (h *taskChangeHarness) update(t *testing.T, transport, user string, args map[string]any) (taskqueue.Result, string) {
	t.Helper()
	if transport == "mcp" {
		result, err := h.mcp[user].CallTool(t.Context(), &mcp.CallToolParams{Name: "update_task", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			texts := []string{}
			for _, c := range result.Content {
				if text, ok := c.(*mcp.TextContent); ok {
					texts = append(texts, text.Text)
				}
			}
			return taskqueue.Result{}, strings.Join(texts, "\n")
		}
		raw, _ := json.Marshal(result.StructuredContent)
		return decodeTask[taskqueue.Result](t, raw), ""
	}
	body, _ := json.Marshal(args)
	w := apiCall(h.f.app, "POST", "/api/v1/actions/update_task", string(body), h.tokens[user])
	if w.Code != 200 {
		var failure struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body)
		}
		return taskqueue.Result{}, fmt.Sprintf("%d %s: %s", w.Code, failure.Error.Code, failure.Error.Message)
	}
	return decodeTask[taskqueue.Result](t, w.Body.Bytes()), ""
}

// history is a task's changes, newest first, as its requester reads them.
func (h *taskChangeHarness) history(t *testing.T, requester, id string) []taskqueue.HistoryEntry {
	t.Helper()
	return decodeTask[[]taskqueue.HistoryEntry](t, actionCall(t, h.mcp[requester], "task_history", map[string]any{"id": id}, false))
}

// holder names whose queue holds a task now, or nobody.
func (h *taskChangeHarness) holder(t *testing.T, id string) string {
	t.Helper()
	_, store, ok := h.f.app.catalog.TaskQueues()
	if !ok {
		t.Fatal("task queues are not enabled")
	}
	book, err := taskqueue.ReadBook(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	if person, i := book.Find(id); i >= 0 {
		return h.names.Replace(person)
	}
	return "nobody"
}

// describeTask lists a task's owner and the fields a change can set.
func (h *taskChangeHarness) describeTask(v taskqueue.View) string {
	parts := []string{h.names.Replace(v.Owner.ID) + ": " + v.Text}
	for _, field := range [][2]string{{"due", v.Due}, {"priority", v.Priority}, {"snoozed", v.Snoozed}, {"next", strings.Join(v.NextDates, ", ")}} {
		if field[1] != "" {
			parts = append(parts, field[0]+" "+field[1])
		}
	}
	if v.Every != nil {
		parts = append(parts, "every "+v.Every.String())
	}
	if v.Lead != 0 {
		parts = append(parts, fmt.Sprintf("lead %d", v.Lead))
	}
	return strings.Join(parts, " · ")
}

// taskChangeCase files a task, has its owner prepare it, then changes it.
type taskChangeCase struct {
	name string
	// from files the task in to's queue, with add's further add_task fields.
	from, to string
	add      map[string]any
	// prep are changes the owner makes first.
	prep []map[string]any
	// as changes the task with change; id and revision default to the task's.
	as     string
	change map[string]any
	// refused is the refusal as "status code: message". Otherwise want is the
	// result, where the task is, its newest history entry and every message
	// sent, one per line.
	refused, want string
}

// TestTaskChangesKeepTheirRules characterizes update_task over MCP and the
// HTTP action API: every close, reassign and edit rule, the refusal each
// transport shows, the commit summary history reads back and the messages
// people other than the caller get. A refusal changes nothing and tells no one.
func TestTaskChangesKeepTheirRules(t *testing.T) {
	h := newTaskChangeHarness(t)
	const notFound = "404 not_found: No open task TASK in your queue or among the tasks you filed"
	const badReason = "400 invalid_request: A reason is one line of at most 200 bytes, without backticks or credentials"
	const noOne = "404 not_found: No one by that ID can have tasks. Name a person by Slack mention or member ID, account user ID or person ID"
	const footer = " /  / _Task TASK. Reply `tasks` to see your queue._"
	weekly := map[string]any{"due": "2026-11-09", "every": "weekly"}
	cases := []taskChangeCase{
		// Who may see the task at all, and what is checked before it is read.
		{name: "an ID that is no task ID", as: "alice", change: map[string]any{"id": "t-NOTANID", "outcome": "done"}, refused: "404 not_found: No open task t-NOTANID in your queue or among the tasks you filed"},
		{name: "a caller without a queue", as: "dave", change: map[string]any{"outcome": "done"}, refused: notFound},
		{name: "a stranger", as: "carol", change: map[string]any{"text": "Mine now"}, refused: notFound},
		{name: "a stranger's reason is checked first", as: "carol", change: map[string]any{"outcome": "done", "reason": "two\nlines"}, refused: badReason},
		{name: "a stranger's new owner is resolved first", as: "carol", change: map[string]any{"reassignTo": "nobody"}, refused: noOne},
		{name: "a reason over 200 bytes", as: "alice", change: map[string]any{"outcome": "done", "reason": strings.Repeat("x", 201)}, refused: badReason},
		{name: "a reason with a backtick", as: "alice", change: map[string]any{"outcome": "done", "reason": "see `notes`"}, refused: badReason},
		{name: "a reason with a credential", as: "alice", change: map[string]any{"outcome": "done", "reason": "password: hunter2hunter2"}, refused: badReason},
		{name: "a stale revision", as: "alice", change: map[string]any{"revision": "000000000000", "outcome": "done"}, refused: "409 conflict: Task TASK changed since you read it; list it again and retry"},
		{name: "a stale revision before the role", as: "bob", change: map[string]any{"revision": "000000000000", "outcome": "done"}, refused: "409 conflict: Task TASK changed since you read it; list it again and retry"},

		// Closing.
		{name: "the owner closes", as: "alice", change: map[string]any{"outcome": "done", "reason": "  Sent on Friday  "}, want: "closed done\nheld by nobody\nhistory done by alice: Sent on Friday\nnotice UBOB: Alice Lee marked your task done: / > Book the venue / Reason: Sent on Friday" + footer},
		{name: "the requester cannot close", as: "bob", change: map[string]any{"outcome": "done"}, refused: "403 forbidden: Only the queue's owner marks a task done or not applicable; you can withdraw a task you filed"},
		{name: "not applicable needs a reason", as: "alice", change: map[string]any{"outcome": "not_applicable"}, refused: "400 invalid_request: Say why the task is not applicable"},
		{name: "not applicable", as: "alice", add: map[string]any{"due": "2026-12-01"}, change: map[string]any{"outcome": "not_applicable", "reason": "The venue closed"}, want: "closed not_applicable\nheld by nobody\nhistory not applicable by alice: The venue closed\nnotice UBOB: Alice Lee marked your task not applicable: / > Book the venue / > Due 2026-12-01 / Reason: The venue closed" + footer},
		{name: "the requester withdraws", as: "bob", change: map[string]any{"outcome": "withdrawn"}, want: "closed withdrawn\nheld by nobody\nhistory withdrawn by bob\nnotice UALICE: Bob Ray withdrew a task they gave you: / > Book the venue" + footer},
		{name: "the owner cannot withdraw", as: "alice", change: map[string]any{"outcome": "withdrawn"}, refused: "403 forbidden: Only the person who filed a task withdraws it"},
		{name: "an own task is withdrawn quietly", from: "alice", as: "alice", change: map[string]any{"outcome": "withdrawn", "reason": "Not needed"}, want: "closed withdrawn\nheld by nobody\nhistory withdrawn by alice: Not needed"},
		{name: "an own task is closed quietly", from: "alice", as: "alice", change: map[string]any{"outcome": "done"}, want: "closed done\nheld by nobody\nhistory done by alice"},
		{name: "an unknown outcome", as: "alice", change: map[string]any{"outcome": "cancelled"}, refused: "400 invalid_request: Outcome must be done, not_applicable or withdrawn"},
		{name: "an outcome ignores edits and reassignment", as: "alice", change: map[string]any{"outcome": "done", "text": "Book a bigger venue", "priority": "high", "reassignTo": "carol"}, want: "closed done\nheld by nobody\nhistory done by alice\nnotice UBOB: Alice Lee marked your task done: / > Book the venue" + footer},
		{name: "a late occurrence", add: weekly, prep: []map[string]any{{"snoozeUntil": "2026-11-25"}}, as: "alice", change: map[string]any{"outcome": "done"}, want: "occurrence 2026-11-16 missed 1 next 2026-11-23\ntask alice: Book the venue · due 2026-11-23 · next 2026-11-23, 2026-11-30 · every 1 weeks from 2026-11-09\nheld by alice\nhistory done for 2026-11-16 by alice; missed 1; next 2026-11-23\nnotice UBOB: Alice Lee marked your recurring task done for 2026-11-16; it is next due 2026-11-23: / > Book the venue" + footer},
		{name: "an early occurrence", add: map[string]any{"due": "2026-11-27", "every": "weekly"}, as: "alice", change: map[string]any{"outcome": "not_applicable", "reason": "Holiday"}, want: "occurrence 2026-11-27 next 2026-12-04\ntask alice: Book the venue · due 2026-12-04 · next 2026-12-04, 2026-12-11 · every 1 weeks from 2026-11-27\nheld by alice\nhistory not applicable for 2026-11-27 by alice; next 2026-12-04: Holiday\nnotice UBOB: Alice Lee marked your recurring task not applicable for 2026-11-27; it is next due 2026-12-04: / > Book the venue / Reason: Holiday" + footer},
		{name: "a withdrawn series", add: weekly, as: "bob", change: map[string]any{"outcome": "withdrawn"}, want: "closed withdrawn\nheld by nobody\nhistory withdrawn by bob\nnotice UALICE: Bob Ray withdrew a task they gave you: / > Book the venue / > Due 2026-11-09, repeating every 1 weeks" + footer},

		// Reassigning.
		{name: "the owner reassigns", prep: []map[string]any{{"priority": "high"}, {"snoozeUntil": "2026-11-25"}}, as: "alice", change: map[string]any{"reassignTo": "carol"}, want: "closed reassigned\ntask carol: Book the venue\nheld by carol\nhistory reassigned from alice to carol by alice\nnotice UBOB: Alice Lee passed your task to Carol Diaz: / > Book the venue" + footer},
		// The requester, now its owner, hears once: the message as requester
		// shares the one as owner's scope and is not sent.
		{name: "a task reassigned to its requester", as: "alice", change: map[string]any{"reassignTo": "<@UBOB>", "reason": "Yours again"}, want: "closed reassigned\ntask bob: Book the venue\nheld by bob\nhistory reassigned from alice to bob by alice: Yours again\nnotice UBOB: New task from Bob Ray, passed to you by Alice Lee: / > Book the venue / Reason: Yours again" + footer},
		{name: "the requester cannot reassign", as: "bob", change: map[string]any{"reassignTo": "carol"}, refused: "403 forbidden: Only the queue's owner reassigns a task"},
		{name: "a reassignment to the same queue", as: "alice", change: map[string]any{"reassignTo": "me"}, refused: "400 invalid_request: The task is already in that queue"},
		{name: "a reassignment with an edit", as: "alice", change: map[string]any{"reassignTo": "carol", "lead": 0}, refused: "400 invalid_request: Reassign a task on its own, then change it"},
		{name: "a reassignment to no one", as: "alice", change: map[string]any{"reassignTo": "nobody"}, refused: noOne},

		// Editing.
		{name: "the requester edits the text", as: "bob", change: map[string]any{"text": "  Book the bigger venue "}, want: "task alice: Book the bigger venue\nheld by alice\nhistory changed text by bob"},
		{name: "an edit with a reason", as: "alice", change: map[string]any{"text": "Book the venue today", "reason": "Typo"}, want: "task alice: Book the venue today\nheld by alice\nhistory changed text by alice: Typo"},
		{name: "a later due date", add: map[string]any{"due": "2026-12-01"}, as: "bob", change: map[string]any{"due": "2026-12-10"}, want: "task alice: Book the venue · due 2026-12-10\nheld by alice\nhistory changed due date by bob"},
		{name: "the requester moves the due date earlier", add: map[string]any{"due": "2026-12-10"}, as: "bob", change: map[string]any{"due": "2026-12-03"}, want: "task alice: Book the venue · due 2026-12-03\nheld by alice\nhistory changed due date by bob\nnotice UALICE: Bob Ray moved a task's due date earlier: / > Book the venue / > Due 2026-12-03" + footer},
		{name: "the owner moves the due date earlier", add: map[string]any{"due": "2026-12-10"}, as: "alice", change: map[string]any{"due": "2026-12-03"}, want: "task alice: Book the venue · due 2026-12-03\nheld by alice\nhistory changed due date by alice"},
		{name: "a first due date", as: "bob", change: map[string]any{"due": "2026-12-03"}, want: "task alice: Book the venue · due 2026-12-03\nheld by alice\nhistory changed due date by bob"},
		{name: "a cleared due date", add: map[string]any{"due": "2026-12-10"}, as: "bob", change: map[string]any{"due": " "}, want: "task alice: Book the venue\nheld by alice\nhistory changed due date by bob"},
		{name: "an invalid due date", as: "bob", change: map[string]any{"due": "2026-13-01"}, refused: "400 invalid_request: dates must look like 2026-11-21"},
		{name: "the requester cannot set priority", as: "bob", change: map[string]any{"priority": "high"}, refused: "403 forbidden: Only the queue's owner sets priority"},
		{name: "the owner sets priority", as: "alice", change: map[string]any{"priority": " HIGH"}, want: "task alice: Book the venue · priority high\nheld by alice\nhistory changed priority by alice"},
		{name: "normal priority clears it", prep: []map[string]any{{"priority": "low"}}, as: "alice", change: map[string]any{"priority": "normal"}, want: "task alice: Book the venue\nheld by alice\nhistory changed priority by alice"},
		{name: "an unknown priority", as: "alice", change: map[string]any{"priority": "urgent"}, refused: "400 invalid_request: Priority must be high, normal or low"},
		{name: "the requester cannot snooze", as: "bob", change: map[string]any{"snoozeUntil": "2026-11-25"}, refused: "403 forbidden: Only the queue's owner snoozes a task"},
		{name: "priority is refused before snooze", as: "bob", change: map[string]any{"snoozeUntil": "2026-11-25", "priority": "low"}, refused: "403 forbidden: Only the queue's owner sets priority"},
		{name: "the owner snoozes", as: "alice", change: map[string]any{"snoozeUntil": "2026-11-25"}, want: "task alice: Book the venue · snoozed 2026-11-25\nheld by alice\nhistory changed snooze by alice"},
		{name: "the owner wakes a task", prep: []map[string]any{{"snoozeUntil": "2026-11-25"}}, as: "alice", change: map[string]any{"snoozeUntil": ""}, want: "task alice: Book the venue\nheld by alice\nhistory changed snooze by alice"},
		{name: "an invalid snooze date", as: "alice", change: map[string]any{"snoozeUntil": "tomorrow"}, refused: "400 invalid_request: dates must look like 2026-11-21"},
		{name: "the requester sets a schedule", add: map[string]any{"due": "2026-12-01"}, as: "bob", change: map[string]any{"every": "monthly"}, want: "task alice: Book the venue · due 2026-12-01 · next 2026-12-01, 2027-01-01 · every 1 months from 2026-12-01\nheld by alice\nhistory changed schedule by bob"},
		{name: "a schedule needs a due date", as: "alice", change: map[string]any{"every": "monthly"}, refused: "400 invalid_request: A recurring task needs a due date: its first date"},
		{name: "a schedule from a new due date", as: "alice", change: map[string]any{"due": "2026-12-02", "every": "2 weeks"}, want: "task alice: Book the venue · due 2026-12-02 · next 2026-12-02, 2026-12-16 · every 2 weeks from 2026-12-02\nheld by alice\nhistory changed due date, schedule by alice"},
		{name: "an unknown schedule", add: map[string]any{"due": "2026-12-01"}, as: "alice", change: map[string]any{"every": "fortnightly"}, refused: `400 invalid_request: a schedule repeats every 1 to 24 days, weeks, months or years, such as "3 months" or "quarterly"`},
		{name: "stopping a task that does not repeat", as: "alice", change: map[string]any{"every": "", "reason": "Done with it"}, refused: "400 invalid_request: The task does not repeat"},
		{name: "stopping a schedule needs a reason", add: weekly, as: "alice", change: map[string]any{"every": " "}, refused: "400 invalid_request: Say why the task stops repeating"},
		{name: "the requester stops a schedule", add: map[string]any{"due": "2026-11-09", "every": "weekly", "lead": 3}, as: "bob", change: map[string]any{"every": "", "reason": "Contract ended"}, want: "task alice: Book the venue · due 2026-11-09\nheld by alice\nhistory changed stopped repeating by bob: Contract ended"},
		{name: "a new due date restarts a schedule", add: weekly, as: "alice", change: map[string]any{"due": "2026-11-26"}, want: "task alice: Book the venue · due 2026-11-26 · next 2026-11-26, 2026-12-03 · every 1 weeks from 2026-11-26\nheld by alice\nhistory changed due date by alice"},
		{name: "a schedule keeps its due date", add: weekly, as: "alice", change: map[string]any{"due": ""}, refused: "400 invalid_request: A recurring task keeps a due date; stop repeating it first"},
		{name: "an earlier date in a series", add: map[string]any{"due": "2026-12-10", "every": "monthly"}, as: "bob", change: map[string]any{"due": "2026-12-03"}, want: "task alice: Book the venue · due 2026-12-03 · next 2026-12-03, 2027-01-03 · every 1 months from 2026-12-03\nheld by alice\nhistory changed due date by bob\nnotice UALICE: Bob Ray moved a task's due date earlier: / > Book the venue / > Due 2026-12-03, repeating every 1 months" + footer},
		{name: "the requester sets a lead time", add: weekly, as: "bob", change: map[string]any{"lead": 4}, want: "task alice: Book the venue · due 2026-11-09 · next 2026-11-09, 2026-11-16 · every 1 weeks from 2026-11-09 · lead 4\nheld by alice\nhistory changed lead time by bob"},
		{name: "a lead time of 0 restores the default", add: map[string]any{"due": "2026-11-09", "every": "weekly", "lead": 10}, as: "alice", change: map[string]any{"lead": 0}, want: "task alice: Book the venue · due 2026-11-09 · next 2026-11-09, 2026-11-16 · every 1 weeks from 2026-11-09\nheld by alice\nhistory changed lead time by alice"},
		{name: "a one-off task has no lead time", as: "alice", change: map[string]any{"lead": 5}, refused: "400 invalid_request: only a recurring task has a lead time, of 1 to 60 working days"},
		{name: "several fields at once", add: weekly, as: "bob", change: map[string]any{"text": "Book the hall", "due": "2026-11-12", "lead": 2, "reason": "Rescoped"}, want: "task alice: Book the hall · due 2026-11-12 · next 2026-11-12, 2026-11-19 · every 1 weeks from 2026-11-12 · lead 2\nheld by alice\nhistory changed text, due date, lead time by bob: Rescoped"},
		{name: "nothing to change", as: "alice", change: map[string]any{"reason": "Just because"}, refused: "400 invalid_request: Nothing to change"},
		{name: "empty text", as: "bob", change: map[string]any{"text": "   "}, refused: "400 invalid_request: task text must be one line of 1 to 500 characters"},
		{name: "a credential in the text", as: "bob", change: map[string]any{"text": "token xoxb-1234567890-abcdefghij"}, refused: "400 invalid_request: this looks like a credential; tasks never store secrets. Say where the secret lives instead"},
	}
	for _, c := range cases {
		for _, transport := range []string{"mcp", "http"} {
			t.Run(c.name+"/"+transport, func(t *testing.T) {
				from, to := c.from, c.to
				if from == "" {
					from = "bob"
				}
				if to == "" {
					to = "alice"
				}
				person := map[string]string{"alice": "<@UALICE>", "bob": "<@UBOB>", "carol": "carol"}[to]
				if to == from {
					person = "me"
				}
				add := map[string]any{"person": person, "text": "Book the venue"}
				for k, v := range c.add {
					add[k] = v
				}
				added := decodeTask[taskqueue.View](t, actionCall(t, h.mcp[from], "add_task", add, false))
				id, revision := added.ID, added.Revision
				for _, prep := range c.prep {
					args := map[string]any{"id": id, "revision": revision}
					for k, v := range prep {
						args[k] = v
					}
					revision = decodeTask[taskqueue.Result](t, actionCall(t, h.mcp[to], "update_task", args, false)).Task.Revision
				}
				args := map[string]any{"id": id, "revision": revision}
				for k, v := range c.change {
					args[k] = v
				}
				before, posts := h.history(t, from, id), h.posts()
				result, refusal := h.update(t, transport, c.as, args)
				refusal = strings.ReplaceAll(refusal, id, "TASK")
				if c.refused != "" {
					want := c.refused
					if transport == "mcp" {
						_, want, _ = strings.Cut(want, ": ")
					}
					if refusal != want {
						t.Fatalf("refusal = %q, want %q", refusal, want)
					}
					if after := h.history(t, from, id); len(after) != len(before) {
						t.Fatalf("a refused change was recorded: %+v", after)
					}
					if notices := h.notices(posts, id); len(notices) != 0 {
						t.Fatalf("a refused change told %v", notices)
					}
					return
				}
				if refusal != "" {
					t.Fatalf("refused: %s", refusal)
				}
				if result.Revision == "" {
					t.Fatalf("no revision in %+v", result)
				}
				got := []string{}
				if result.Closed != "" {
					got = append(got, "closed "+result.Closed)
				}
				if o := result.Occurrence; o != nil {
					line := "occurrence " + o.Closed
					if o.Missed > 0 {
						line += fmt.Sprintf(" missed %d", o.Missed)
					}
					got = append(got, line+" next "+o.Next)
				}
				if result.Task != nil {
					if result.Task.ID != id {
						t.Fatalf("the result shows %+v", *result.Task)
					}
					got = append(got, "task "+h.describeTask(*result.Task))
				}
				got = append(got, "held by "+h.holder(t, id))
				history := h.history(t, from, id)
				if len(history) != len(before)+1 || history[0].Revision != result.Revision {
					t.Fatalf("history %+v after %+v, result revision %s", history, before, result.Revision)
				}
				got = append(got, "history "+h.names.Replace(history[0].Change))
				got = append(got, h.notices(posts, id)...)
				if strings.Join(got, "\n") != c.want {
					t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), c.want)
				}
			})
		}
	}
}
