package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaging"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSharedSlackRequiresApprovedNotebookActorAndSources(t *testing.T) {
	app, _ := fixture(t)
	configureSlackFixture(t, app)
	app.slack.config.Broadcasts = map[string]SlackBroadcast{"Cshared": {Project: "default", Actors: []string{catalog.Bootstrap}, Connections: []string{"metrics"}, AudienceApproved: true}}
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if r.URL.Path != "/chat.postMessage" {
			t.Error(r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["user"] != nil || body["parse"] != "none" {
			t.Error(body)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	app.slack.endpoint = server.URL + "/chat.postEphemeral"
	r := automation.Run{Spec: automation.Spec{Actor: catalog.Bootstrap, Project: "default"}, Answer: "Health report", Receipts: []connectors.Receipt{{Connection: "private"}}}
	d := automation.Delivery{Channel: "Cshared", Shared: true}
	if err := app.deliverSlack(t.Context(), &r, func() error { return nil }, d); err == nil || posts != 0 {
		t.Fatal("unapproved source broadcast", err)
	}
	r.Receipts = nil
	r.Spec.Connections = []string{"private"}
	if err := app.deliverSlack(t.Context(), &r, func() error { return nil }, d); err == nil || posts != 0 {
		t.Fatal("uncited connection data broadcast", err)
	}
	r.Spec.Connections = nil
	r.Spec.ReadProjects = []string{"private"}
	if err := app.deliverSlack(t.Context(), &r, func() error { return nil }, d); err == nil || posts != 0 {
		t.Fatal("uncited notebook data broadcast", err)
	}
	r.Spec.ReadProjects = nil
	r.Receipts = []connectors.Receipt{{Connection: "metrics"}}
	r.Receipts[0].Connection = "metrics"
	if err := app.deliverSlack(t.Context(), &r, func() error { return nil }, d); err != nil || posts != 1 {
		t.Fatal(err, posts)
	}
	r.DeliveryState = "sending"
	if err := app.deliverSlack(t.Context(), &r, func() error { return nil }, d); err == nil || posts != 1 {
		t.Fatal("ambiguous delivery resent", err)
	}
}

func TestAutomationBroadcastUsesCurrentManagedSlackPolicy(t *testing.T) {
	app, _ := fixture(t)
	configureSlackFixture(t, app)
	admin := recoverySession(t, app)
	owner := addLocal(t, app, admin, "alert-owner")
	service, err := app.catalog.CreateService(catalog.Bootstrap, "Monitor", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", service.ID, "read"); err != nil {
		t.Fatal(err)
	}
	client := app.slack.policy()
	client.Broadcasts = map[string]SlackBroadcast{"Ccore": {Project: "default", Actors: []string{service.ID}, AudienceApproved: true}}
	policy, err := app.catalog.SetChatClients(catalog.Bootstrap, catalog.ChatClients{Clients: map[string]messaging.Client{"slack": client}})
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"spec":{"id":"health","kind":"native","actor":%q,"project":"default","intervalSeconds":300,"native":{"task":"storage-health"},"notifyChannel":"Ccore"}}`, service.ID)
	if w := localRequest(app, "/api/organization/action/validate_automation", body, admin); w.Code != 200 {
		t.Fatalf("approved managed destination rejected: HTTP %d %s", w.Code, w.Body.String())
	}
	// An obsolete runtime-file grant must not revive a revoked managed audience.
	app.slack.config.Broadcasts = client.Broadcasts
	client.Broadcasts = nil
	policy.Clients["slack"] = client
	if _, err = app.catalog.SetChatClients(catalog.Bootstrap, policy); err != nil {
		t.Fatal(err)
	}
	if w := localRequest(app, "/api/organization/action/validate_automation", body, admin); w.Code == 200 {
		t.Fatal("obsolete file grant revived a revoked broadcast")
	}
}

func TestIncidentReplyKeepsReceiptsInRunHistory(t *testing.T) {
	app, _ := fixture(t)
	configureSlackFixture(t, app)
	run := automation.Run{ID: automation.Hash("incident-receipts"), Spec: automation.Spec{ID: "health", Kind: "native"}, Receipts: []connectors.Receipt{{ID: "PRIVATE_EXECUTION_RECEIPT", Connection: "logs", Operation: "read"}}}
	if err := app.recordAutomationIncident(&run, automation.Incident{Active: true, Severity: "critical", Summary: "Production database connections are failing across core services."}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if run.Answer != "Alert: Production database connections are failing across core services." || len(run.Receipts) != 1 || run.Receipts[0].ID != "PRIVATE_EXECUTION_RECEIPT" {
		t.Fatal("incident presentation lost its business answer or retained evidence", run.Answer)
	}
}

func TestSlackKnowledgeDebouncesSignedMappedMessages(t *testing.T) {
	app, _ := runnerFixture(t, func(http.ResponseWriter, *http.Request) {})
	configureSlackFixture(t, app)
	app.slack.config.KnowledgeChannels = map[string]string{"Cknowledge": "default"}
	spec := automation.Spec{ID: "knowledge", Kind: "task", Actor: "alice", Project: "default", Output: "decisions.md", Instruction: "Maintain source-linked decisions", ExternalProcessing: true, AIConnection: "local", Trigger: &automation.Trigger{Kind: "slack.knowledge", Channel: "Cknowledge", DebounceSeconds: 60, MaxWaitSeconds: 180}}
	setTestAutomationSpec(t, app, spec)
	send := func(id, user string) int {
		body := `{"type":"event_callback","team_id":"T123","event_id":"` + id + `","event":{"type":"message","channel":"Cknowledge","user":"` + user + `","text":"A product decision","ts":"1.0"}}`
		stamp := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte("fixture-signing"))
		mac.Write([]byte("v0:" + stamp + ":" + body))
		req := httptest.NewRequest("POST", "http://localhost/integrations/slack/events", strings.NewReader(body))
		req.Header.Set("X-Slack-Request-Timestamp", stamp)
		req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, req)
		return w.Code
	}
	for _, id := range []string{"event1", "event1", "event2"} {
		if code := send(id, "U123"); code != 200 {
			t.Fatal(code)
		}
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 1 || len(runs[0].Batch.Events) != 2 {
		t.Fatal(err, runs)
	}
	if code := send("event3", "unknown"); code != 403 {
		t.Fatal("unmapped source accepted", code)
	}
}
