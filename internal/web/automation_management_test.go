package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
)

func TestAutomationDefinitionAdministration(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	steward := addLocal(t, app, admin, "automation-owner")
	bot, err := app.catalog.CreateService(catalog.Bootstrap, "Background worker", steward)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", bot.ID, "edit"); err != nil {
		t.Fatal(err)
	}
	spec := fmt.Sprintf(`{"id":"daily","kind":"digest","actor":%q,"project":"default","output":"daily.md","calls":[{"connection":"metrics","operation":"read","arguments":{}}],"disabled":true}`, bot.ID)
	request := func(body string) (int, string) {
		w := localRequest(app, "/api/organization/action/save_automation", body, admin)
		return w.Code, w.Body.String()
	}
	code, body := request(`{"spec":` + spec + `,"revision":""}`)
	if code != 200 {
		t.Fatalf("create automation: HTTP %d %s", code, body)
	}
	var saved struct {
		Revision string `json:"revision"`
	}
	if err = json.Unmarshal([]byte(body), &saved); err != nil || saved.Revision == "" {
		t.Fatal("missing revision", body, err)
	}
	if code, body = request(`{"spec":` + spec + `,"revision":""}`); code != 409 {
		t.Fatalf("stale update: HTTP %d %s", code, body)
	}
	w := localRequest(app, "/api/organization/action/automation_status", `{}`, admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"managedBy":"instance"`) {
		t.Fatalf("status: HTTP %d %s", w.Code, w.Body.String())
	}
	credential, err := issueCredential(app.catalog, app.oauth, bot.ID, CredentialSpec{Name: "limited", Projects: []string{"default"}, Scopes: []string{"notes:read"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if w = apiCall(app, "POST", "/api/v1/actions/save_automation", `{"spec":`+spec+`,"revision":""}`, credential.Token); w.Code != 403 {
		t.Fatalf("ordinary token managed automation: HTTP %d %s", w.Code, w.Body.String())
	}
}

func TestAutomationChecksAcceptJSONAssertions(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	steward := addLocal(t, app, admin, "checks-owner")
	bot, err := app.catalog.CreateService(catalog.Bootstrap, "Check worker", steward)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", bot.ID, "edit"); err != nil {
		t.Fatal(err)
	}
	values := []string{`true`, `1`, `"ready"`, `null`, `{"ok":true}`, `[1,"two"]`}
	for i, value := range values {
		id := fmt.Sprintf("checks-%d", i)
		body := fmt.Sprintf(`{"spec":{"id":%q,"kind":"checks","actor":%q,"project":"default","disabled":true,"checks":[{"name":"result","call":{"connection":"metrics","operation":"read","arguments":{}},"assertions":[{"pointer":"/value","equals":%s}]}]},"revision":""}`, id, bot.ID, value)
		w := apiCall(app, "POST", "/api/v1/actions/save_automation", body, testToken)
		if w.Code != 200 {
			t.Fatalf("save assertion %s: HTTP %d %s", value, w.Code, w.Body.String())
		}
		saved, ok := app.automations.Definitions.Get(id)
		if !ok || string(saved.Spec.Checks[0].Assertions[0].Equals) != value {
			t.Fatalf("assertion was not preserved: %s: %+v", value, saved)
		}
	}
}

func TestAutomationDeterministicContextSurvivesDisabledAI(t *testing.T) {
	app, _ := fixture(t)
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "robot"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "robot", "read"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.catalog.SetAISettings(catalog.Bootstrap, catalog.AISettings{}); err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "health", Kind: "monitor", Actor: "robot", Project: "default"}
	ctx, err := app.automationContext(context.Background(), spec)
	if err != nil {
		t.Fatal("AI policy blocked deterministic monitor", err)
	}
	if err = app.checkAutomationAuth(ctx); err != nil {
		t.Fatal(err)
	}
	spec.AIConnection = "provider"
	if _, err = app.automationContext(context.Background(), spec); err == nil {
		t.Fatal("AI job bypassed disabled policy")
	}
}

func TestAutomationManualEventRunNeedsCapturedEvidence(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	spec := automation.Spec{ID: "event", Kind: "task", Actor: catalog.Bootstrap, Project: "default", Output: "summary.md", Instruction: "Summarize", ExternalProcessing: true, Trigger: &automation.Trigger{Kind: "slack.knowledge", Channel: "C1", DebounceSeconds: 60, MaxWaitSeconds: 120}}
	if err := app.automations.Definitions.SetFiles([]automation.Spec{spec}); err != nil {
		t.Fatal(err)
	}
	w := localRequest(app, "/api/organization/action/run_automation", `{"id":"event","requestId":"manual"}`, admin)
	if w.Code == 200 {
		t.Fatal("event run accepted without evidence", w.Body.String())
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 0 {
		t.Fatal("invalid event queued", runs, err)
	}
}

func TestAutomationStatusKeepsRunsThatNeedReconciliation(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	spec := automation.Spec{ID: "heartbeat", Kind: "task", Actor: catalog.Bootstrap, Project: "default", Output: "heartbeat.md", Instruction: "Summarize", ExternalProcessing: true}
	stuck, err := app.automations.Queue.Enqueue(spec, "interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.automations.Queue.RunNext(t.Context(), func(ctx context.Context, r *automation.Run, save func() error) error {
		r.Script = &automation.ScriptCheckpoint{Steps: map[string]*automation.ScriptStep{"model": {Operation: "ai.structured", State: "started", Effect: true}}}
		if err := save(); err != nil {
			return err
		}
		return errors.New("gateway stopped during the model call")
	}); err != nil {
		t.Fatal(err)
	}
	// A job blocked by an unresolved run stops scheduling, while other jobs
	// keep adding history that pushes the run out of the recent list.
	other := automation.Spec{ID: "busy", Kind: "task", Actor: catalog.Bootstrap, Project: "default", Output: "busy.md", Instruction: "Summarize", ExternalProcessing: true}
	for i := range 100 {
		r, err := app.automations.Queue.Enqueue(other, fmt.Sprintf("slot-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if err = app.automations.Queue.Cancel(r.ID); err != nil {
			t.Fatal(err)
		}
	}
	w := localRequest(app, "/api/organization/action/automation_status", `{}`, admin)
	var status struct {
		Runs []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"runs"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &status); w.Code != 200 || err != nil {
		t.Fatalf("status: HTTP %d %s %v", w.Code, w.Body.String(), err)
	}
	for _, r := range status.Runs {
		if r.ID == stuck.ID && r.State == "action-required" {
			return
		}
	}
	t.Fatalf("run needing reconciliation is missing from %d listed runs, so it cannot be cancelled", len(status.Runs))
}
