package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
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

// metricsConnection configures a synthetic data connection with a read and a
// write operation, served by handler.
func metricsConnection(t *testing.T, app *Server, handler http.HandlerFunc) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "read", ReadOnly: true, Method: "GET", Path: "/"}, {Name: "list", ReadOnly: true, Method: "GET", Path: "/list"}, {Name: "reset", Method: "POST", Path: "/reset"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = manager
}

func grantMetrics(t *testing.T, app *Server, actor string, operations ...string) {
	t.Helper()
	if _, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Users: map[string][]string{actor: operations}}); err != nil {
		t.Fatal(err)
	}
}

// waitAutomationRun waits for a run the started runtime claims to finish.
func waitAutomationRun(t *testing.T, app *Server, id string) automation.Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		run, err := app.automations.Queue.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if !run.Unresolved() {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not finish: %s %s", run.State, run.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Only an instance administrator manages, runs or inspects automations. A
// notebook owner's browser session is refused every automation tool, each
// with a request an administrator could make, and changes nothing.
func TestAutomationToolsRefuseNotebookOwnersWhoAreNotAdministrators(t *testing.T) {
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
	metricsConnection(t, app, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"count":7}`) })
	grantMetrics(t, app, bot.ID, "read")
	owner := addLocal(t, app, admin, "notebook-owner")
	if err = app.catalog.Member(catalog.Bootstrap, "default", owner, "owner"); err != nil {
		t.Fatal(err)
	}
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"notebook-owner"}, "password": {"test-password-long"}}.Encode())
	if login.Code != 303 {
		t.Fatal(login.Code, login.Body.String())
	}
	session := login.Result().Cookies()[0]

	spec, _ := json.Marshal(automation.Spec{ID: "daily", Kind: "digest", Actor: bot.ID, Project: "default", Output: "daily.md", Connections: []string{"metrics"}, Calls: []automation.Call{{Connection: "metrics", Operation: "read"}}})
	call := func(tool, body string, cookie *http.Cookie) []byte {
		t.Helper()
		w := localRequest(app, "/api/organization/action/"+tool, body, cookie)
		if w.Code != 200 {
			t.Fatalf("administrator %s: HTTP %d %s", tool, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	var saved automation.Definition
	json.Unmarshal(call("save_automation", `{"spec":`+string(spec)+`,"revision":""}`, admin), &saved)
	// One finished run, with a result and receipts, and one still queued.
	var finished, queued automationRunStatus
	json.Unmarshal(call("run_automation", `{"id":"daily","requestId":"finished"}`, admin), &finished)
	if _, err = app.automations.Queue.RunNext(t.Context(), app.automationrun.RunAutomation); err != nil {
		t.Fatal(err)
	}
	if r, err := app.automations.Queue.Get(finished.ID); err != nil || r.State != "completed" || r.Result == "" || len(r.Receipts) != 1 {
		t.Fatalf("fixture run did not finish with a result: %+v %v", r, err)
	}
	json.Unmarshal(call("run_automation", `{"id":"daily","requestId":"queued"}`, admin), &queued)

	for _, c := range []struct{ tool, body string }{
		{"automation_status", `{}`},
		{"automation_run", fmt.Sprintf(`{"id":%q}`, finished.ID)},
		{"validate_automation", `{"spec":` + string(spec) + `}`},
		{"test_automation", `{"id":"daily","requestId":"owner"}`},
		{"run_automation", `{"id":"daily","requestId":"owner"}`},
		{"cancel_automation", fmt.Sprintf(`{"id":%q}`, queued.ID)},
		{"delete_automation", fmt.Sprintf(`{"id":"daily","revision":%q}`, saved.Revision)},
	} {
		if w := localRequest(app, "/api/organization/action/"+c.tool, c.body, session); w.Code != 403 {
			t.Errorf("notebook owner could %s: HTTP %d %s", c.tool, w.Code, w.Body.String())
		}
	}
	if current, ok := app.automations.Definitions.Get("daily"); !ok || current.Revision != saved.Revision {
		t.Error("refused delete changed the definition")
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 2 {
		t.Fatalf("refused runs were queued: %d %v", len(runs), err)
	}
	if r, _ := app.automations.Queue.Get(queued.ID); r.State != "queued" {
		t.Errorf("refused cancel stopped the run: %s", r.State)
	}
}

// Validation refuses an enabled job its actor could not carry out: one that
// writes its output with only read access, one that calls an operation the
// actor was not granted, and one granted a write the actor may not make.
func TestAutomationValidationRefusesJobsBeyondTheActorsAccess(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	steward := addLocal(t, app, admin, "automation-owner")
	bot, err := app.catalog.CreateService(catalog.Bootstrap, "Background worker", steward)
	if err != nil {
		t.Fatal(err)
	}
	metricsConnection(t, app, func(w http.ResponseWriter, r *http.Request) { t.Error("validation called the connection") })
	grantMetrics(t, app, bot.ID, "read")
	validate := func(spec automation.Spec) (int, string) {
		raw, _ := json.Marshal(map[string]any{"spec": spec})
		w := localRequest(app, "/api/organization/action/validate_automation", string(raw), admin)
		return w.Code, w.Body.String()
	}
	digest := automation.Spec{ID: "daily", Kind: "digest", Actor: bot.ID, Project: "default", Output: "daily.md", Connections: []string{"metrics"}, Calls: []automation.Call{{Connection: "metrics", Operation: "read"}}}

	if err = app.catalog.Member(catalog.Bootstrap, "default", bot.ID, "read"); err != nil {
		t.Fatal(err)
	}
	if code, body := validate(digest); code != 403 {
		t.Fatalf("output written with read access: HTTP %d %s", code, body)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", bot.ID, "edit"); err != nil {
		t.Fatal(err)
	}
	if code, body := validate(digest); code != 200 {
		t.Fatalf("output written with edit access: HTTP %d %s", code, body)
	}

	ungranted := digest
	ungranted.Calls = []automation.Call{{Connection: "metrics", Operation: "list"}}
	if code, body := validate(ungranted); code != 403 {
		t.Errorf("call to an operation the actor was not granted: HTTP %d %s", code, body)
	}
	write := digest
	write.Operations = map[string][]string{"metrics": {"read", "reset"}}
	if code, body := validate(write); code != 403 {
		t.Errorf("grant of a write the actor may not make: HTTP %d %s", code, body)
	}
}

// A running job is fenced to the definition it started from. Another gateway
// that changes, pauses or deletes the job saves the definition before it
// cancels the run, so the run's next check stops it: the model is not asked
// again and nothing is published.
func TestAutomationRunStopsOnceItsDefinitionChanges(t *testing.T) {
	for _, change := range []string{"changed", "paused", "deleted"} {
		t.Run(change, func(t *testing.T) {
			var requests atomic.Int32
			app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if requests.Add(1) == 1 {
					fmt.Fprint(w, automationToolCalls(`call_data_operation {"project":"default","connection":"metrics","operation":"read","arguments":{}}`))
					return
				}
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"# Count\nSeven."},"finish_reason":"stop"}]}`)
			})
			if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
				t.Fatal(err)
			}
			other, err := automation.OpenDefinitions(app.db)
			if err != nil {
				t.Fatal(err)
			}
			var saved automation.Definition
			metricsConnection(t, app, func(w http.ResponseWriter, r *http.Request) {
				next := saved.Spec
				var err error
				switch change {
				case "changed":
					next.Instruction = "Count something else."
					_, err = other.Save(next, saved.Revision)
				case "paused":
					next.Disabled = true
					_, err = other.Save(next, saved.Revision)
				case "deleted":
					err = other.Delete(next.ID, saved.Revision)
				}
				if err != nil {
					t.Error(err)
				}
				fmt.Fprint(w, `{"count":7}`)
			})
			grantMetrics(t, app, "alice", "read")
			spec := automation.Spec{ID: "count", Kind: "task", Actor: "alice", Project: "default", AIConnection: "local", ExternalProcessing: true, Connections: []string{"metrics"}, Output: "count.md", Instruction: "Count the items."}
			if saved, err = app.automations.Definitions.Save(spec, ""); err != nil {
				t.Fatal(err)
			}
			queued, err := app.automations.Queue.Enqueue(saved.Spec, "one")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			app.automationrun.Start(ctx)
			run := waitAutomationRun(t, app, queued.ID)
			if run.State != "failed" || run.Error != "automation configuration changed" || requests.Load() != 1 {
				t.Fatalf("run %s (%s) after %d model requests", run.State, run.Error, requests.Load())
			}
			store, _ := app.catalog.Access(catalog.Bootstrap, "default", "read")
			if _, err = store.Read(t.Context(), "count.md", ""); !os.IsNotExist(err) {
				t.Fatal("run published after its definition changed", err)
			}
		})
	}
}
