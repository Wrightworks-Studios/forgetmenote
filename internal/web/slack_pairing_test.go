package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
)

// newEmailPairingFixture gives alice and bob verified workspace sign-ins and
// lists alice (UNEW) and a guest in Slack. The client does not pair by email yet.
func newEmailPairingFixture(t *testing.T) (*slackLinkFixture, automation.Spec) {
	t.Helper()
	f := newSlackLinkFixture(t)
	app := f.app
	f.members = []map[string]any{
		{"id": "UNEW", "team_id": "T123", "is_email_confirmed": true, "profile": map[string]any{"email": "Alice@Example.com"}},
		{"id": "UGUEST", "team_id": "T123", "is_restricted": true, "profile": map[string]any{"email": "bob@example.com"}},
	}
	for user, email := range map[string]string{"alice": "alice@example.com", "bob": "bob@example.com"} {
		if err := app.catalog.BindExternalIdentity(catalog.Bootstrap, "https://accounts.example", user+"-sub", user); err != nil {
			t.Fatal(err)
		}
		if err := app.catalog.ObserveOIDCEmail("https://accounts.example", user+"-sub", email, "example.com"); err != nil {
			t.Fatal(err)
		}
	}
	return f, automation.Spec{ID: "pairing", Kind: "native", Actor: f.service, Project: "default", TimeoutSeconds: 60, Native: &automation.NativeTask{Task: "email-pairing", Client: "slack"}}
}

func pairByEmail(t *testing.T, app *Server) {
	t.Helper()
	clients := app.catalog.ChatClients()
	policy := clients.Clients["slack"]
	policy.Members.PairByEmail = []string{"example.com"}
	clients.Clients["slack"] = policy
	if _, err := app.catalog.SetChatClients(catalog.Bootstrap, clients); err != nil {
		t.Fatal(err)
	}
}

func TestEmailPairingBringsMemoryToSlackDirectMessages(t *testing.T) {
	f, spec := newEmailPairingFixture(t)
	app := f.app
	if err := app.validateAutomationDefinition(t.Context(), spec); err == nil {
		t.Fatal("email pairing was accepted for a client that names no workspace domain")
	}
	pairByEmail(t, app)
	if err := app.validateAutomationDefinition(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	other := spec
	other.Actor = catalog.Bootstrap
	if err := app.validateAutomationDefinition(t.Context(), other); err == nil {
		t.Fatal("email pairing ran as an actor other than the client's members actor")
	}

	preview := automation.Run{ID: automation.Hash("preview"), DryRun: true, Spec: spec}
	if err := app.automationrun.RunAutomation(t.Context(), &preview, func() error { return nil }); err != nil || !strings.Contains(preview.Result, "UNEW") {
		t.Fatalf("test run: %v\n%s", err, preview.Result)
	}
	if app.catalog.MessageLink("slack", "T123", "UNEW") != "" {
		t.Fatal("a test run paired a member")
	}
	run := automation.Run{ID: automation.Hash("pair"), Spec: spec}
	if err := app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if links := app.catalog.MessageLinks("alice"); len(links) != 1 || links[0].User != "UNEW" || links[0].Source != "email" {
		t.Fatalf("alice's links after pairing: %+v", links)
	}
	if app.catalog.MessageLink("slack", "T123", "UGUEST") != "" {
		t.Fatal("a guest was paired")
	}
	if strings.Contains(strings.ToLower(run.Result+preview.Result), "@example.com") {
		t.Fatalf("the pairing report carries addresses:\n%s", run.Result)
	}

	f.dm(t, "paired", "What do I usually check on Mondays?")
	if len(f.requests.prompts) != 1 || !strings.Contains(f.requests.prompts[0], "Alice owns the Denver office rollout.") {
		t.Fatalf("an email-paired member's DM did not use their memory:\n%v", f.requests.prompts)
	}
}

func slackMemberEvent(t *testing.T, app *Server, id string, event map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"type": "event_callback", "team_id": "T123", "event_id": id, "event": event})
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("fixture-signing"))
	mac.Write(append([]byte("v0:"+stamp+":"), body...))
	r := httptest.NewRequest("POST", "http://localhost/integrations/slack/events", strings.NewReader(string(body)))
	r.Header.Set("X-Slack-Request-Timestamp", stamp)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("Slack %s event: HTTP %d %s", event["type"], w.Code, w.Body.String())
	}
}

func TestSlackMemberEventsRepairOnlyWhenPairingCouldChange(t *testing.T) {
	f, spec := newEmailPairingFixture(t)
	app := f.app
	pairByEmail(t, app)
	spec.Schedule = &automation.Schedule{Timezone: "UTC", Hours: []int{3}, Minutes: []int{0}}
	if _, err := app.automations.Definitions.Save(spec, ""); err != nil {
		t.Fatal(err)
	}
	queued := func() int {
		t.Helper()
		runs, err := app.automations.Queue.List()
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range runs {
			if r.Spec.ID == spec.ID && r.State == "queued" {
				n++
			}
		}
		return n
	}
	runQueued := func() {
		t.Helper()
		if ran, err := app.automations.Queue.RunNext(t.Context(), app.automationrun.RunAutomation); err != nil || !ran {
			t.Fatalf("the pairing run did not run: %v %v", ran, err)
		}
	}
	alice := map[string]any{"id": "UNEW", "team_id": "T123", "is_email_confirmed": true, "profile": map[string]any{"email": "alice@example.com", "status_text": "Lunch"}}

	slackMemberEvent(t, app, "Ev-join-alice", map[string]any{"type": "team_join", "user": alice})
	if queued() != 1 {
		t.Fatal("a joining member with a verified account did not start pairing")
	}
	runQueued()
	if app.catalog.MessageLink("slack", "T123", "UNEW") != "alice" {
		t.Fatal("the joining member was not paired")
	}

	slackMemberEvent(t, app, "Ev-status", map[string]any{"type": "user_change", "user": alice})
	slackMemberEvent(t, app, "Ev-stranger", map[string]any{"type": "team_join", "user": map[string]any{"id": "UNOBODY", "team_id": "T123", "profile": map[string]any{"email": "nobody@example.com"}}})
	if queued() != 0 {
		t.Fatal("a change that cannot affect pairing started a run")
	}

	left := map[string]any{"id": "UNEW", "team_id": "T123", "deleted": true, "profile": map[string]any{"email": "alice@example.com"}}
	f.members = nil
	slackMemberEvent(t, app, "Ev-left", map[string]any{"type": "user_change", "user": left})
	slackMemberEvent(t, app, "Ev-left", map[string]any{"type": "user_change", "user": left})
	if queued() != 1 {
		t.Fatal("a deactivated paired member did not start exactly one run")
	}
	f.members = []map[string]any{{"id": "UOTHER", "team_id": "T123", "profile": map[string]any{"email": "other@example.com"}}}
	runQueued()
	if app.catalog.MessageLink("slack", "T123", "UNEW") != "" {
		t.Fatal("a deactivated member stayed paired")
	}
}
