//go:build !fmn_omit_slack

package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/messaginghttp"
	"forgetmenote/internal/taskqueue"
)

// effectStates lists the states of the effects recorded in scope.
func effectStates(t *testing.T, app *Server, scope string) []string {
	t.Helper()
	effects, err := app.automations.Queue.Effects(scope)
	if err != nil {
		t.Fatal(err)
	}
	states := []string{}
	for _, e := range effects {
		states = append(states, e.State)
	}
	slices.Sort(states)
	return states
}

func TestSendOnceTriesARefusedMessageAgainAndNeverRepeatsOneThatMayHaveArrived(t *testing.T) {
	f := newSlackLinkFixture(t)
	app := f.app
	d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "UALICE", User: "UALICE", Direct: true}
	reply := messaging.Reply{Text: "Your task is due"}
	send := func(ctx context.Context, effect automation.Effect) (bool, error) {
		t.Helper()
		return app.messaging.SendOnce(ctx, effect, app.slack, d, reply, time.Second, nil)
	}

	// Slack refuses it: nothing arrived, so the next call sends it, once.
	refused := automation.Effect{Scope: automation.Hash("refused"), Action: "notice", Actor: "alice"}
	f.refusePosts = 1
	if sent, err := send(t.Context(), refused); sent || !errors.Is(err, messaging.ErrNotPosted) {
		t.Fatalf("a refused send: sent %v, %v", sent, err)
	}
	if states := effectStates(t, app, refused.Scope); !slices.Equal(states, []string{"rejected"}) {
		t.Fatalf("a refused send was recorded %v", states)
	}
	if sent, err := send(t.Context(), refused); !sent || err != nil || len(f.posts) != 1 {
		t.Fatalf("the second try: sent %v, %v, %d posts", sent, err, len(f.posts))
	}
	if sent, err := send(t.Context(), refused); sent || err != nil || len(f.posts) != 1 {
		t.Fatalf("a delivered message was sent again: sent %v, %v, %d posts", sent, err, len(f.posts))
	}

	// An unknown outcome is never repeated, and holds back the scope.
	unknown := automation.Effect{Scope: automation.Hash("unknown"), Action: "notice", Actor: "alice"}
	f.failPosts = 1
	if sent, err := send(t.Context(), unknown); sent || err == nil || errors.Is(err, messaging.ErrNotPosted) {
		t.Fatalf("an unknown outcome: sent %v, %v", sent, err)
	}
	later := unknown
	later.Action = "later notice"
	for _, effect := range []automation.Effect{unknown, later} {
		if sent, err := send(t.Context(), effect); sent || err == nil || len(f.posts) != 1 {
			t.Fatalf("%s after an unknown outcome: sent %v, %v, %d posts", effect.Action, sent, err, len(f.posts))
		}
	}
	if states := effectStates(t, app, unknown.Scope); !slices.Equal(states, []string{"unknown"}) {
		t.Fatalf("the unknown outcome was recorded %v", states)
	}

	// A message Slack keeps refusing is tried messaginghttp.SendAttempts times.
	stubborn := automation.Effect{Scope: automation.Hash("stubborn"), Action: "notice", Actor: "alice"}
	f.refusePosts = 100
	for range messaginghttp.SendAttempts + 2 {
		if sent, err := send(t.Context(), stubborn); sent || err == nil {
			t.Fatalf("a refused message: sent %v, %v", sent, err)
		}
	}
	if left := 100 - f.refusePosts; left != messaginghttp.SendAttempts {
		t.Fatalf("Slack was asked %d times; want %d", left, messaginghttp.SendAttempts)
	}
	f.refusePosts = 0

	// A caller that has gone away does not cancel a send it began.
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if sent, err := send(gone, automation.Effect{Scope: automation.Hash("gone"), Action: "notice", Actor: "alice"}); !sent || err != nil || len(f.posts) != 2 {
		t.Fatalf("a send whose caller left: sent %v, %v, %d posts", sent, err, len(f.posts))
	}

	// Every send ends within its timeout, its outcome unknown.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer slow.Close()
	defer close(release)
	stalled := &messaging.Slack{Team: "T123", Endpoint: slow.URL + "/chat.postEphemeral", Client: slow.Client(), Token: func() (string, error) { return "token", nil }}
	effect := automation.Effect{Scope: automation.Hash("stalled"), Action: "notice", Actor: "alice"}
	started := time.Now()
	if sent, err := app.messaging.SendOnce(t.Context(), effect, stalled, d, reply, 200*time.Millisecond, nil); sent || err == nil || time.Since(started) > 5*time.Second {
		t.Fatalf("a stalled send: sent %v, %v after %v", sent, err, time.Since(started))
	}
	if states := effectStates(t, app, effect.Scope); !slices.Equal(states, []string{"unknown"}) {
		t.Fatalf("a stalled send was recorded %v", states)
	}
}

func TestARefusedConversationReplyIsNotHeldAsSending(t *testing.T) {
	f := newSlackLinkFixture(t)
	app := f.app
	d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "DNEW", User: "UNEW", Direct: true}
	reply := messaging.Reply{Text: "Here is the answer."}

	f.refusePosts = 1
	run := automation.Run{ID: "refused"}
	if err := app.messaging.SendMessage(t.Context(), app.slack, d, reply, &run, func() error { return nil }); !errors.Is(err, messaging.ErrNotPosted) || run.DeliveryState != "" {
		t.Fatalf("a refused reply: %v, delivery %q", err, run.DeliveryState)
	}
	if err := app.messaging.SendMessage(t.Context(), app.slack, d, reply, &run, func() error { return nil }); err != nil || run.DeliveryState != "sent" || len(f.posts) != 1 {
		t.Fatalf("the reply sent again: %v, delivery %q, %d posts", err, run.DeliveryState, len(f.posts))
	}

	f.failPosts = 1
	run = automation.Run{ID: "unknown"}
	if err := app.messaging.SendMessage(t.Context(), app.slack, d, reply, &run, func() error { return nil }); err == nil || run.DeliveryState != "sending" {
		t.Fatalf("an unknown outcome: %v, delivery %q", err, run.DeliveryState)
	}
	if err := app.messaging.SendMessage(t.Context(), app.slack, d, reply, &run, func() error { return nil }); err == nil || len(f.posts) != 1 {
		t.Fatalf("a reply that may have arrived was sent again: %v, %d posts", err, len(f.posts))
	}
}

// reminderReport is the JSON report of a task-reminder run.
type reminderReport struct {
	Reminded []string       `json:"reminded"`
	Failed   []string       `json:"failed"`
	Skipped  map[string]int `json:"skipped"`
}

func readReminderReport(t *testing.T, run automation.Run) reminderReport {
	t.Helper()
	_, raw, _ := strings.Cut(run.Result, "```json\n")
	raw, _, _ = strings.Cut(raw, "```")
	var out reminderReport
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("report %q: %v", run.Result, err)
	}
	return out
}

func TestARefusedReminderIsSentAgainAndABrokenRecordIsNoReminder(t *testing.T) {
	f := newTaskFixture(t)
	app := f.app
	now := time.Date(2026, 11, 17, 16, 30, 0, 0, time.UTC)
	app.tasks.Clock = func() time.Time { return now }
	f.members = []map[string]any{{"id": "UALICE", "team_id": "T123", "tz": "America/Denver", "profile": map[string]any{}}}
	bob := taskClients(t, app, "bob")[0]
	added := decodeTask[taskqueue.View](t, actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Renew the parking permit"}, false))
	person := added.Owner.ID
	spec := automation.Spec{ID: "task-reminder", Kind: "native", Actor: f.service, Project: "default", TimeoutSeconds: 60, Native: &automation.NativeTask{Task: "task-reminder", Client: "slack"}}
	remind := func(id string) (reminderReport, []map[string]any) {
		t.Helper()
		before := len(f.posts)
		run := automation.Run{ID: automation.Hash(id), Spec: spec}
		if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return readReminderReport(t, run), f.posts[before:]
	}

	f.refusePosts = 1
	if report, posts := remind("refused"); len(posts) != 0 || !slices.Equal(report.Failed, []string{person}) {
		t.Fatalf("a refused reminder: %+v %v", report, posts)
	}
	if report, posts := remind("again"); len(posts) != 1 || !slices.Equal(report.Reminded, []string{person}) {
		t.Fatalf("the reminder after a refusal: %+v %v", report, posts)
	}
	if report, posts := remind("once"); len(posts) != 0 || report.Skipped["alreadyReminded"] != 1 {
		t.Fatalf("a third run: %+v %v", report, posts)
	}

	// The next day's reminder cannot be recorded: it is reported failed, not
	// as a reminder already sent.
	now = now.AddDate(0, 0, 1)
	date := "2026-11-18"
	taken, _, err := app.automations.Queue.BeginEffect(automation.Effect{Scope: automation.Hash([]string{"task-reminder", person, date}), Action: date, InputHash: "x", Actor: catalog.Bootstrap})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.automations.Queue.FinishEffect(taken.ID, "rejected", nil); err != nil {
		t.Fatal(err)
	}
	if report, posts := remind("unrecorded"); len(posts) != 0 || !slices.Equal(report.Failed, []string{person}) || report.Skipped["alreadyReminded"] != 0 {
		t.Fatalf("an unrecorded reminder: %+v %v", report, posts)
	}
}

func TestAPersonalNotificationSlackRefusedIsSentNextTime(t *testing.T) {
	app, _ := fixture(t)
	configureSlackFixture(t, app)
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, Notifications: true, TimeZone: "UTC", Features: []string{"slack", "briefing"}})
	if err != nil {
		t.Fatal(err)
	}
	connectPersonalProviderFixture(t, app, "alice", "slack", func(r *http.Request) string {
		switch r.URL.Path {
		case "/api/oauth.v2.access":
			return `{"ok":true,"team":{"id":"T123"},"authed_user":{"id":"U123","token_type":"user","access_token":"fixture-user","scope":"im:history,chat:write"}}`
		case "/api/auth.test":
			return `{"ok":true,"user_id":"U123","team_id":"T123"}`
		}
		t.Errorf("unexpected personal Slack call %s", r.URL.Path)
		return `{}`
	})
	posts, refuse := 0, 1
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users.info":
			w.Write([]byte(`{"ok":true,"user":{"id":"U123","team_id":"T123"}}`))
		case "/chat.postMessage":
			if refuse > 0 {
				refuse--
				w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
				return
			}
			posts++
			w.Write([]byte(`{"ok":true,"ts":"1.1"}`))
		default:
			t.Errorf("unexpected bot call %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL
	spec, err := app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	run := &automation.Run{Spec: spec}
	state := personalState{Drafts: map[string]personalDraft{
		"one": {ID: "one", State: "ready", Title: "Launch plan", Path: "drafts/one.md", NoteRevision: "committed", Source: personalObservation{Kind: "briefing"}},
	}}
	if err = app.notifyPersonal(ctx, run, &state, func() error { return nil }); !errors.Is(err, messaging.ErrNotPosted) || posts != 0 || state.Drafts["one"].Notified {
		t.Fatalf("a refused notification: %v, %d posts, notified %v", err, posts, state.Drafts["one"].Notified)
	}
	if err = app.notifyPersonal(ctx, run, &state, func() error { return nil }); err != nil || posts != 1 || !state.Drafts["one"].Notified {
		t.Fatalf("the notification after a refusal: %v, %d posts, notified %v", err, posts, state.Drafts["one"].Notified)
	}
	state.Drafts["one"] = personalDraft{ID: "one", State: "ready", Title: "Launch plan", Path: "drafts/one.md", NoteRevision: "committed", Source: personalObservation{Kind: "briefing"}}
	if err = app.notifyPersonal(ctx, run, &state, func() error { return nil }); err != nil || posts != 1 || !state.Drafts["one"].Notified {
		t.Fatalf("a delivered notification was repeated: %v, %d posts", err, posts)
	}
}
