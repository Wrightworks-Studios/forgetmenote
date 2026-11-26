package web

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

// A repository refresh needs the notebook's owner. A run whose actor may only
// edit the notebook, as a service account at most may, leaves the notebook's
// repository source as it was; the same run by an owner refreshes it.
func TestAutomationRepositoryRefreshNeedsAnOwner(t *testing.T) {
	app, _ := fixture(t)
	configureSearchSource(t, app)
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	bot, err := app.catalog.CreateService(catalog.Bootstrap, "Repository refresher", "alice")
	if err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{bot.ID: "edit", "alice": "owner"} {
		if err = app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	before, err := app.repositories.Status("default")
	if err != nil {
		t.Fatal(err)
	}
	refresh := func(actor string) error {
		run := automation.Run{ID: automation.Hash(actor), Spec: automation.Spec{ID: "refresh", Kind: "native", Actor: actor, Project: "default", Native: &automation.NativeTask{Task: "repository-refresh"}}}
		return app.runAutomation(t.Context(), &run, func() error { return nil })
	}
	if err = refresh(bot.ID); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("an editor's refresh was not refused", err)
	}
	after, err := app.repositories.Status("default")
	if err != nil || !after.LastRefresh.Equal(before.LastRefresh) || after.Downloaded != before.Downloaded || after.Failures != before.Failures || after.Error != before.Error {
		t.Fatalf("an editor's run refreshed the repository: %v %+v", err, after)
	}
	if err = refresh("alice"); err != nil {
		t.Fatal("fixture: an owner's refresh failed", err)
	}
	if after, err = app.repositories.Status("default"); err != nil || !after.LastRefresh.After(before.LastRefresh) {
		t.Fatalf("fixture: an owner's run did not refresh the repository: %v %+v", err, after)
	}
}

// The scheduler neither queues nor claims a job the instance AI policy
// refuses. With instance AI writing off, an AI job that comes due gets no run
// and one queued earlier stays queued, while a deterministic job still runs.
func TestAutomationSchedulerSkipsJobsTheInstanceAIPolicyRefuses(t *testing.T) {
	app, _ := fixture(t)
	if err := app.initAutomations(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.catalog.SetAISettings(catalog.Bootstrap, catalog.AISettings{}); err != nil {
		t.Fatal(err)
	}
	due := automation.Spec{ID: "ai-due", Kind: "task", Actor: catalog.Bootstrap, Project: "default", AIConnection: "local", ExternalProcessing: true, Instruction: "Summarize", Output: "due.md", IntervalSeconds: 3600}
	queued := automation.Spec{ID: "ai-queued", Kind: "task", Actor: catalog.Bootstrap, Project: "default", AIConnection: "local", ExternalProcessing: true, Instruction: "Summarize", Output: "queued.md"}
	health := automation.Spec{ID: "health", Kind: "native", Actor: catalog.Bootstrap, Project: "default", Native: &automation.NativeTask{Task: "notebook-health"}, IntervalSeconds: 3600}
	for _, spec := range []automation.Spec{due, queued, health} {
		setTestAutomationSpec(t, app, spec)
	}
	waiting, err := app.automations.Queue.Enqueue(queued, "before")
	if err != nil {
		t.Fatal(err)
	}
	app.StartAutomations(t.Context())
	// The claimer looks at the waiting AI run as soon as it starts, before the
	// scheduler queues the deterministic job, so once that job has run the AI
	// run would have been claimed too.
	var runs []automation.Run
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if runs, err = app.automations.Queue.List(); err != nil {
			t.Fatal(err)
		}
		ran := false
		for _, r := range runs {
			ran = ran || r.Spec.ID == health.ID && r.State != "queued" && r.State != "running"
		}
		if ran {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture: the deterministic job never ran")
		}
	}
	for _, r := range runs {
		if r.Spec.ID == due.ID {
			t.Errorf("queued AI job %s while instance AI writing is off: %s", r.Spec.ID, r.State)
		}
		if r.ID == waiting.ID && r.State != "queued" {
			t.Errorf("claimed AI job %s while instance AI writing is off: %s %s", r.Spec.ID, r.State, r.Error)
		}
	}
}

// A personal assistant's answer to a direct message is bound to the consent
// in force when it started, since it carries the person's private context. If
// the person withdraws consent to one of the assistant's features while the
// answer is written, it is not delivered.
func TestPersonalDirectMessageIsNotDeliveredAfterConsentChanges(t *testing.T) {
	var app *Server
	var answered, private atomic.Bool
	app, _ = runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if answered.CompareAndSwap(false, true) {
			private.Store(strings.Contains(string(body), "Private employee context"))
			settings, err := app.catalog.Assistant("alice")
			if err != nil {
				t.Error(err)
			}
			settings.Features = []string{"slack"}
			if _, err = app.catalog.SaveAssistant("alice", settings); err != nil {
				t.Error(err)
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Private DM answer"},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	if _, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true, AIConnection: "local", Model: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"slack", "email"}}); err != nil {
		t.Fatal(err)
	}
	// Linking her own Slack account binds alice's direct messages to her
	// assistant notebook.
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
	var posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users.info":
			w.Write([]byte(`{"ok":true,"user":{"id":"U123","team_id":"T123","deleted":false,"is_bot":false}}`))
			return
		case "/chat.postMessage", "/chat.postEphemeral":
			posts.Add(1)
		}
		w.Write([]byte(`{"ok":true,"ts":"1.1"}`))
	}))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL
	signedSlackMessage(t, app, "EvConsent", "message", "D123", "", "123.456", "What should I follow up on today?")
	if _, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil {
		t.Fatal(err)
	}
	if !answered.Load() || !private.Load() {
		t.Fatal("fixture: the model did not answer with alice's private context")
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 1 {
		t.Fatal("fixture: the direct message run", runs, err)
	}
	if posts.Load() != 0 || runs[0].State != "failed" {
		t.Fatalf("delivered %d answers after alice withdrew consent; the run %s", posts.Load(), runs[0].State)
	}
}
