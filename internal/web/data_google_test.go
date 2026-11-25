package web

import (
	"encoding/json"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGoogleWriteRecoveryAppliesToDirectAPI(t *testing.T) {
	app, _ := fixture(t)
	dir := t.TempDir()
	queue, err := automation.Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	app.automations.Queue = queue
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			w.Write([]byte(`{"access_token":"fixture","token_type":"Bearer","expires_in":3600}`))
		case "/gmail/v1/users/me/profile":
			w.Write([]byte(`{"emailAddress":"finance@example.test","historyId":"99"}`))
		case "/gmail/v1/users/me/drafts":
			writes.Add(1)
			if fail.Load() {
				w.WriteHeader(500)
				w.Write([]byte(`{"error":"unknown"}`))
				return
			}
			w.Write([]byte(`{"id":"draft1","message":{"id":"message1","threadId":"thread1"}}`))
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	t.Setenv("TEST_GOOGLE_SECRET", "synthetic")
	t.Setenv("TEST_GOOGLE_REFRESH", "synthetic")
	raw := `{"version":1,"connections":[{"id":"mail","kind":"email","projects":["default"],"endpoint":"` + upstream.URL + `","oauthRefresh":{"tokenURL":"` + upstream.URL + `/token","clientID":"fixture","clientSecretEnv":"TEST_GOOGLE_SECRET","refreshTokenEnv":"TEST_GOOGLE_REFRESH"},"email":{"provider":"gmail","mailbox":"finance@example.test","workflow":"finance","recipients":["client@example.test"]},"operations":[{"name":"status","readOnly":true},{"name":"draft-create","readOnly":false}]}]}`
	var config connectors.Config
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	manager, err := connectors.New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	app.dataConnections = manager
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "mail", Users: map[string][]string{"alice": {"status", "draft-create"}}}); err != nil {
		t.Fatal(err)
	}
	cred, err := issueCredential(app.catalog, app.oauth, "alice", CredentialSpec{Name: "writer", Projects: []string{"default"}, Connections: []string{"mail"}, Scopes: []string{"notes:read", "data:read", "data:write"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"connection":"mail","operation":"draft-create","arguments":{"actionID":"reply-one","to":["client@example.test"],"subject":"Invoice 0012","text":"A proposed reply"}}`
	for i := 0; i < 2; i++ {
		w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", body, cred.Token)
		if w.Code != 200 {
			t.Fatalf("submission/replay %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	fail.Store(true)
	for _, action := range []string{"reply-two", "reply-three"} {
		w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", strings.Replace(body, "reply-one", action, 1), cred.Token)
		if w.Code == 200 {
			t.Fatal("uncertain write accepted", w.Body.String())
		}
	}
	if writes.Load() != 2 {
		t.Fatal("new action bypassed uncertain effect", writes.Load())
	}
	// Reconcile the known fixture failure, then simulate a server crash after a
	// real provider acknowledgment but before FinishEffect can persist it.
	effects, err := queue.Effects("connector:mail")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range effects {
		if e.State == "unknown" {
			if _, err = queue.ResolveEffect(e.ID, "rejected", "Fixture confirmed request was rejected", nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	fail.Store(false)
	crashBody := strings.Replace(body, "reply-one", "reply-crash", 1)
	var in dataCall
	if err = json.Unmarshal([]byte(crashBody), &in); err != nil {
		t.Fatal(err)
	}
	contract, _ := manager.Contract("mail")
	_, created, err := queue.BeginEffect(automation.Effect{Scope: "connector:mail", Action: "reply-crash", InputHash: automation.Hash([]any{contract, in.Operation, in.Arguments}), Actor: "alice", Connection: "mail", Operation: in.Operation})
	if err != nil || !created {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(in.Arguments)
	if _, err = manager.Call(connectors.WithSubmission(t.Context()), "mail", in.Operation, arguments, ""); err != nil {
		t.Fatal(err)
	}
	app.automations.Queue, err = automation.Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"reply-crash", "reply-after-crash"} {
		w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", strings.Replace(body, "reply-one", action, 1), cred.Token)
		if w.Code == 200 {
			t.Fatal("crash barrier bypassed", w.Body.String())
		}
	}
	if writes.Load() != 3 {
		t.Fatal("crash recovery duplicated provider action", writes.Load())
	}
	// A connection grant does not bypass its restricted notebook membership.
	if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", ""); err != nil {
		t.Fatal(err)
	}
	w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"connection":"mail","operation":"status","arguments":{}}`, cred.Token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("revoked notebook still exposes mailbox: %d %s", w.Code, w.Body.String())
	}
}

func TestGoogleOutcomeAdministrationAccess(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	addLocal(t, app, admin, "effect-reader")
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"effect-reader"}, "password": {"test-password-long"}}.Encode())
	if login.Code != 303 {
		t.Fatalf("fixture login: %d %s", login.Code, login.Body.String())
	}
	reader := login.Result().Cookies()[0]
	e, _, err := app.automations.Queue.BeginEffect(automation.Effect{Scope: "connector:mail", Action: "one", InputHash: automation.Hash("fixture")})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"connection":"mail"}`
	w := localRequest(app, "/api/organization/action/data_connection_effects", body, admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), e.ID) {
		t.Fatalf("admin inspection: %d %s", w.Code, w.Body.String())
	}
	for action, input := range map[string]string{"data_connection_effects": body, "automation_effects": `{"id":"finance"}`, "resolve_automation_effect": `{"id":"` + e.ID + `","state":"rejected","evidence":"synthetic evidence"}`} {
		w = localRequest(app, "/api/organization/action/"+action, input, reader)
		if w.Code != 403 {
			t.Fatalf("nonadmin %s: %d %s", action, w.Code, w.Body.String())
		}
	}
	raw, _ := json.Marshal(map[string]string{"id": e.ID, "state": "accepted", "evidence": "Provider fixture confirmed message1 was accepted"})
	w = localRequest(app, "/api/organization/action/resolve_automation_effect", string(raw), admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"accepted"`) {
		t.Fatalf("admin resolution: %d %s", w.Code, w.Body.String())
	}
}
