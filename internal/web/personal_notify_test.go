package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/personal"
)

func TestPersonalNotificationsRecoverAcceptedPrivateDelivery(t *testing.T) {
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
		default:
			t.Errorf("unexpected personal Slack call %s", r.URL.Path)
			return `{}`
		}
	})
	sent := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.URL.Path {
		case "/users.info":
			w.Write([]byte(`{"ok":true,"user":{"id":"U123","team_id":"T123","deleted":false,"is_bot":false}}`))
		case "/chat.postMessage":
			sent++
			var message struct{ Channel, Text string }
			if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
				t.Error(err)
			}
			if message.Channel != "U123" || strings.Contains(message.Text, "PRIVATE CONTENT") {
				t.Error("notification disclosed content or targeted someone else")
			}
			if !strings.Contains(message.Text, "Launch plan") {
				t.Error("notification omitted the actionable suggestion title")
			}
			w.Write([]byte(`{"ok":true,"ts":"1.1"}`))
		default:
			t.Errorf("unexpected bot call %s", r.URL.Path)
			w.Write([]byte(`{"ok":false}`))
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
	state := personal.State{Drafts: map[string]personal.Draft{
		"one": {ID: "one", State: "ready", Title: "Launch plan", Path: "drafts/one.md", NoteRevision: "committed", Summary: "PRIVATE CONTENT", Source: personal.Observation{Kind: "briefing"}},
		"two": {ID: "two", State: "ready", NoteRevision: "committed", Source: personal.Observation{Kind: "briefing"}},
	}}
	if err = app.personal.Notify(ctx, run, &state, func() error { return errors.New("checkpoint interrupted") }); err == nil || sent != 1 {
		t.Fatalf("expected accepted send and interrupted checkpoint: sends=%d err=%v", sent, err)
	}
	// A concurrent dismissal changes the pending batch. Recovery must apply the
	// accepted receipt before computing a new batch identity.
	delete(state.Drafts, "one")
	d := state.Drafts["two"]
	d.Notified = false
	state.Drafts["two"] = d
	app.automations.Queue, err = automation.Open(app.db)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.personal.Notify(ctx, run, &state, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if sent != 1 || !state.Drafts["two"].Notified {
		t.Fatal("accepted notification repeated after restart")
	}
	account, _ := app.catalog.Account("alice")
	if err = app.personalConnections.Disconnect(account.ID, "slack"); err != nil {
		t.Fatal(err)
	}
	state.Drafts["three"] = personal.Draft{ID: "three", State: "ready", NoteRevision: "committed", Source: personal.Observation{Kind: "briefing"}}
	if err = app.personal.Notify(ctx, run, &state, func() error { return nil }); err != nil || sent != 1 {
		t.Fatal("disconnected Slack received a notification", err)
	}
}

func TestPersonalDisconnectedQueueDoesNotBlockOtherWork(t *testing.T) {
	app, _ := fixture(t)
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	old := personal.Observation{Key: "email:t1", Kind: "email", Revision: "old", Source: connectors.PersonalOperation{Provider: "google", Generation: 1, Operation: "email.thread", Arguments: map[string]any{"id": "t1"}}, Content: json.RawMessage(`{"secret":"old email"}`), Observed: time.Now()}
	id := personal.Fingerprint(old)
	state := personal.State{Pending: []personal.Observation{old}, Reads: []connectors.PersonalOperation{old.Source}, Drafts: map[string]personal.Draft{id: {ID: id, State: "ready", Source: old}}}
	raw, _ := json.Marshal(state)
	if _, err = app.automations.Queue.SaveJobState(spec, "", raw, false); err != nil {
		t.Fatal(err)
	}
	if err = app.runAutomation(t.Context(), &automation.Run{Spec: spec}, func() error { return nil }); err != nil {
		t.Fatal("disconnected evidence blocked remaining work", err)
	}
	_, state, err = app.personal.ReadState(spec)
	if err != nil || len(state.Pending) != 0 || len(state.Reads) != 0 || state.Drafts[id].State != "disconnected" {
		t.Fatalf("revoked work retained: %+v %v", state, err)
	}
}
