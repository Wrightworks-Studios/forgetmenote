package web

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/reposource"
)

// outsideWorld stands for every service a paused instance must not reach.
func outsideWorld(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Errorf("paused instance called outside: %s %s", r.Method, r.URL.Path)
		http.Error(w, "outgoing actions are paused", 500)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func pausedFixture(t *testing.T) *Server {
	t.Helper()
	app, _ := fixture(t)
	if err := app.ConfigureOutgoing("paused"); err != nil {
		t.Fatal(err)
	}
	return app
}

// Repository sources and managed backups against a GitHub that is outside.
func githubDuties(t *testing.T, app *Server, github *httptest.Server) {
	t.Helper()
	dir := filepath.Join(app.data, "github-duties")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	for name, b := range map[string][]byte{"key": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), "client": []byte("synthetic"), "webhook": []byte(strings.Repeat("s", 32))} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	g, err := reposource.NewGitHub(reposource.Config{AppID: "1", Slug: "fixture", ClientID: "fixture", PrivateKeyFile: filepath.Join(dir, "key"), ClientSecretFile: filepath.Join(dir, "client"), WebhookSecretFile: filepath.Join(dir, "webhook")})
	if err != nil {
		t.Fatal(err)
	}
	g.API, g.HTTP = github.URL, github.Client()
	m, err := reposource.Open(app.db, filepath.Join(dir, "sources"), nil, g, func(p, a string) error { return app.catalog.CheckNotebookOwner(a, p) })
	if err != nil {
		t.Fatal(err)
	}
	app.repositories = m
	app.repositoryConnections = &repositoryConnections{github: g}
	backupApp, err := githubapp.New(g.Client.Config, "write")
	if err != nil {
		t.Fatal(err)
	}
	backupApp.API, backupApp.HTTP = github.URL, github.Client()
	bm, err := backups.Open(app.db, backupApp, func(backups.Binding) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	app.backups = &backupService{manager: bm, app: backupApp, config: backupConfig{Workers: map[string]backupWorker{"fixture": {Owner: "local"}}}}
}

func heldDuties(t *testing.T, app *Server) map[string]bool {
	t.Helper()
	rows, err := app.db.Pool.Query(t.Context(), `SELECT name FROM leases WHERE org = $1 AND name NOT LIKE 'gateway/%' AND expires > now()`, app.db.Org)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	held := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		held[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return held
}

// A copy restored beside the live instance keeps the duties that only recover
// its own state and never takes one that acts outward as that instance.
func TestPausedInstanceTakesNoOutwardDuties(t *testing.T) {
	outward := []string{"automation-scheduler", "jev-scheduler", "repository-refresh", "backup-expiry"}
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "paused"}[paused], func(t *testing.T) {
			github, calls := outsideWorld(t)
			app, _ := fixture(t)
			githubDuties(t, app, github)
			if paused {
				if err := app.ConfigureOutgoing("paused"); err != nil {
					t.Fatal(err)
				}
			}
			if err := app.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			held := heldDuties(t, app)
			for deadline := time.Now().Add(10 * time.Second); !(held["chat-recovery"] && held["quicknotes-routing"]); held = heldDuties(t, app) {
				if time.Now().After(deadline) {
					t.Fatalf("local recovery duties were not taken: %v", held)
				}
				time.Sleep(20 * time.Millisecond)
			}
			// Every duty starts claiming at once; give the outward ones as long again.
			time.Sleep(time.Second)
			held = heldDuties(t, app)
			for _, name := range outward {
				if held[name] == paused {
					t.Errorf("paused=%v but duty %s held=%v: %v", paused, name, held[name], held)
				}
			}
			// Quicknotes keeps its duty for local recovery and pauses routing itself.
			if app.quicknotes.Outgoing.Paused() != paused {
				t.Error("Quicknotes routing does not follow the instance's outgoing mode")
			}
			if calls.Load() != 0 {
				t.Fatalf("outside called %d times", calls.Load())
			}
		})
	}
}

// Slack events, queued replies and the transport itself all act in Slack as
// the instance, so a paused copy refuses each before a model or Slack call.
func TestPausedInstanceSendsNoSlackMessages(t *testing.T) {
	outside, calls := outsideWorld(t)
	app, _ := runnerFixture(t, outside.Config.Handler.ServeHTTP)
	configureSlackFixture(t, app)
	app.slack.endpoint = outside.URL
	if err := app.ConfigureOutgoing("paused"); err != nil {
		t.Fatal(err)
	}
	w := signedSlackEvent(app, "mention", "app_mention", "C123", "", "1.000001", "Please help.")
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"outgoing_paused"`) || !strings.Contains(w.Body.String(), "outgoing actions are paused on this instance") {
		t.Fatalf("paused Slack event: HTTP %d %s", w.Code, w.Body)
	}
	if runs, err := app.automations.Queue.List(); err != nil || len(runs) != 0 {
		t.Fatalf("paused Slack event queued a reply: %v %v", runs, err)
	}
	// A reply that was queued on the live instance before the copy was taken.
	run := automation.Run{ID: automation.Hash("copied"), Spec: automation.Spec{ID: "slack-copied", Kind: "assistant", Actor: "alice", Project: "default", Instruction: "How many records?", AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", Thread: "123.456", SlackUser: "U123"}}}
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); !errors.Is(err, outgoing.ErrPaused) {
		t.Fatalf("paused instance ran a queued Slack reply: %v", err)
	}
	if err := app.slack.Send(t.Context(), messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", User: "U123"}, messaging.Reply{Text: "Hello"}); !errors.Is(err, outgoing.ErrPaused) {
		t.Fatalf("paused Slack transport: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("outside called %d times", calls.Load())
	}
}

// Administrator runs and tests execute automations, which deliver, publish
// and call models as the instance; a paused copy queues neither.
func TestPausedInstanceQueuesNoAutomationRuns(t *testing.T) {
	app := pausedFixture(t)
	admin := recoverySession(t, app)
	spec := automation.Spec{ID: "daily", Kind: "task", Actor: catalog.Bootstrap, Project: "default", Output: "summary.md", Instruction: "Summarize", ExternalProcessing: true}
	if err := app.automations.Definitions.SetFiles([]automation.Spec{spec}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"run_automation", "test_automation"} {
		w := localRequest(app, "/api/organization/action/"+tool, `{"id":"daily","requestId":"rehearsal"}`, admin)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "outgoing actions are paused on this instance") {
			t.Fatalf("paused %s: HTTP %d %s", tool, w.Code, w.Body)
		}
	}
	if runs, err := app.automations.Queue.List(); err != nil || len(runs) != 0 {
		t.Fatalf("paused instance queued runs: %v %v", runs, err)
	}
}

// A copied personal grant holds the live instance's refresh token. Refreshing
// it, or calling the provider, or starting a new grant from the copy could
// revoke or replace what the live instance uses, so none reaches the provider.
func TestPausedInstanceLeavesPersonalGrantsAlone(t *testing.T) {
	app := chatFixture(t)
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	if _, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"email"}}); err != nil {
		t.Fatal(err)
	}
	var paused atomic.Bool
	var calls atomic.Int32
	connectPersonalFixture(t, app, "alice", func(r *http.Request) string {
		if paused.Load() {
			calls.Add(1)
			t.Errorf("paused instance called the provider: %s %s", r.Method, r.URL.Path)
			return `{}`
		}
		switch r.URL.Path {
		case "/token":
			// Expired at once, so the next call must refresh.
			return `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":1,"scope":"openid email https://www.googleapis.com/auth/gmail.modify"}`
		case "/v1/userinfo":
			return `{"sub":"google-alice","email":"alice@example.test","email_verified":true}`
		}
		return `{}`
	})
	account, err := app.catalog.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	before, err := app.personalConnections.List(account.ID)
	if err != nil || len(before) != 1 || before[0].State != "connected" {
		t.Fatalf("fixture connection missing: %+v %v", before, err)
	}
	if err = app.ConfigureOutgoing("paused"); err != nil {
		t.Fatal(err)
	}
	paused.Store(true)
	client := actionClient(t, app, "alice", "notes:read data:read")
	read := map[string]any{"provider": "google", "generation": before[0].Generation, "operation": "email.thread", "arguments": map[string]any{"id": "t1"}}
	if text := mcpToolError(t, client, "read_personal_source", read); !strings.Contains(text, outgoing.ErrPaused.Error()) {
		t.Fatalf("paused personal read: %q", text)
	}
	write, _ := json.Marshal(map[string]any{"actionID": "rehearsal", "call": map[string]any{"provider": "google", "generation": before[0].Generation, "operation": "email.draft-create", "arguments": map[string]any{"to": []string{"colleague@example.test"}, "subject": "Plan", "text": "Synthetic"}}})
	if w := proxyCall(app, "alice", "POST", "/api/assistant/action/submit_personal_action", string(write)); w.Code != 503 || !strings.Contains(w.Body.String(), outgoing.ErrPaused.Error()) {
		t.Fatalf("paused personal write: %d %s", w.Code, w.Body)
	}
	if effects, err := app.automations.Queue.Effects(automation.Hash([]string{"personal", account.ID, "google"})); err != nil || len(effects) != 0 {
		t.Fatalf("paused personal write recorded an effect: %+v %v", effects, err)
	}
	if w := proxyCall(app, "alice", "POST", "/account/connections/google/connect", ""); w.Code != 503 || !strings.Contains(w.Body.String(), outgoing.ErrPaused.Error()) {
		t.Fatalf("paused personal connect: %d %s", w.Code, w.Body)
	}
	after, err := app.personalConnections.List(account.ID)
	if err != nil || len(after) != 1 || after[0].State != "connected" || after[0].Generation != before[0].Generation {
		t.Fatalf("paused instance changed the copied grant: %+v %v", after, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("provider called %d times", calls.Load())
	}
}

// Data writes, such as webhooks, email and sheet submissions, act outside as
// the instance; a read-only query changes nothing there and still answers.
func TestPausedInstanceRefusesDataWritesButAnswersQueries(t *testing.T) {
	app := pausedFixture(t)
	var reads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/read" {
			t.Errorf("paused instance wrote outside: %s %s", r.Method, r.URL.Path)
			http.Error(w, "outgoing actions are paused", 500)
			return
		}
		reads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"value":7}`))
	}))
	defer upstream.Close()
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: upstream.URL, Operations: []connectors.Operation{{Name: "read", Method: "GET", Path: "/read", ReadOnly: true}, {Name: "notify", Method: "POST", Path: "/notify"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	app.dataConnections = manager
	if err = app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Users: map[string][]string{"alice": {"read", "notify"}}}); err != nil {
		t.Fatal(err)
	}
	cred, err := issueCredential(app.catalog, app.oauth, "alice", CredentialSpec{Name: "data writer", Projects: []string{"default"}, Connections: []string{"metrics"}, Scopes: []string{"data:read", "data:write"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"connection":"metrics","operation":"notify","arguments":{}}`, cred.Token)
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"outgoing_paused"`) {
		t.Fatalf("paused data write: %d %s", w.Code, w.Body)
	}
	organization, err := app.catalog.Organization(catalog.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range organization.Activity {
		if a.Action == "data.call" {
			t.Fatalf("paused data write recorded an intent: %+v", a)
		}
	}
	if w = apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"connection":"metrics","operation":"read","arguments":{}}`, cred.Token); w.Code != 200 || !strings.Contains(w.Body.String(), `7`) || reads.Load() != 1 {
		t.Fatalf("paused data query: %d %s reads=%d", w.Code, w.Body, reads.Load())
	}
}

// The backup worker protocol and the setup flows reach GitHub as the instance.
// A paused copy grants no lease, so no token is minted, and starts no setup.
func TestPausedInstanceGrantsNoBackupLease(t *testing.T) {
	github, calls := outsideWorld(t)
	app := pausedFixture(t)
	dir := t.TempDir()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	worker := strings.Repeat("w", 32)
	for name, b := range map[string][]byte{"key": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), "client": []byte("synthetic"), "webhook": []byte(strings.Repeat("s", 32)), "worker": []byte(worker)} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.ConfigureBackupsConfig(backups.AppConfig{App: githubapp.Config{AppID: "2", Slug: "backups", ClientID: "backups", PrivateKeyFile: filepath.Join(dir, "key"), ClientSecretFile: filepath.Join(dir, "client"), WebhookSecretFile: filepath.Join(dir, "webhook")}, Workers: map[string]backups.WorkerConfig{"fixture": {Owner: "local", TokenFile: filepath.Join(dir, "worker")}}}); err != nil {
		t.Fatal(err)
	}
	app.backups.app.API, app.backups.app.HTTP = github.URL, github.Client()
	r := httptest.NewRequest("POST", "http://localhost/internal/backups/v1/lease", strings.NewReader(`{"project":"default","generation":1,"revision":"`+strings.Repeat("a", 40)+`"}`))
	r.Header.Set("X-Backup-Worker", "fixture")
	r.Header.Set("Authorization", "Bearer "+worker)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"outgoing_paused"`) {
		t.Fatalf("paused backup lease: %d %s", w.Code, w.Body)
	}
	if w = call(app, "POST", "/api/managed-backup/connect", `{}`, testToken); w.Code != 503 || !strings.Contains(w.Body.String(), outgoing.ErrPaused.Error()) {
		t.Fatalf("paused backup setup: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 0 {
		t.Fatalf("GitHub called %d times", calls.Load())
	}
}

// Jev reviews are paid background work. A paused copy starts none and does
// not resume one copied from the live instance.
func TestPausedInstanceRunsNoJevReview(t *testing.T) {
	var paused atomic.Bool
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if paused.Load() {
			calls.Add(1)
			t.Errorf("paused instance called Jev: %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, "synthetic outage", 503)
	})
	copied := startReview(t, app, `{"kind":"consistency","operationId":"copied"}`)
	// The outage leaves the review waiting for someone to continue it.
	if state := waitReview(t, app, copied)["state"]; state != "paused" {
		t.Fatalf("fixture review state %v", state)
	}
	if err := app.ConfigureOutgoing("paused"); err != nil {
		t.Fatal(err)
	}
	paused.Store(true)
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+copied+"/continue", `{}`); w.Code != 503 || !strings.Contains(w.Body.String(), outgoing.ErrPaused.Error()) {
		t.Fatalf("paused review resume: %d %s", w.Code, w.Body)
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs", `{"kind":"consistency","operationId":"rehearsal"}`); w.Code != 503 || !strings.Contains(w.Body.String(), outgoing.ErrPaused.Error()) {
		t.Fatalf("paused review start: %d %s", w.Code, w.Body)
	}
	app.launchNextJevJob()
	if calls.Load() != 0 {
		t.Fatalf("Jev called %d times", calls.Load())
	}
}

// Hosted extraction sends an upload to a paid vision provider in the
// background. A paused copy refuses it at admission; local extraction runs.
func TestPausedInstanceRefusesHostedExtraction(t *testing.T) {
	outside, calls := outsideWorld(t)
	app, _ := hostedSourceFixture(t, outside.Config.Handler.ServeHTTP)
	if err := app.ConfigureOutgoing("paused"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../sources/testdata/reference.png")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"filename": "image.png", "data": base64.StdEncoding.EncodeToString(raw), "externalOCR": true})
	if w := call(app, "POST", "/api/sources/jobs", string(body), testToken); w.Code != 503 || !strings.Contains(w.Body.String(), outgoing.ErrPaused.Error()) {
		t.Fatalf("paused hosted extraction: %d %s", w.Code, w.Body)
	}
	body, _ = json.Marshal(map[string]any{"filename": "notes.txt", "data": base64.StdEncoding.EncodeToString([]byte("Local rehearsal text"))})
	w := call(app, "POST", "/api/sources/jobs", string(body), testToken)
	var out sourceJobOutput
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("paused local extraction: %d %s", w.Code, w.Body)
	}
	if done := sourceJobUntilFinished(t, app, out.Job.ID); done.Prepared == nil || !strings.Contains(done.Prepared.Preview, "Local rehearsal text") {
		t.Fatalf("paused local extraction result: %+v", done)
	}
	if calls.Load() != 0 {
		t.Fatalf("provider called %d times", calls.Load())
	}
}

// Embedding documents is paid background work; a paused copy keeps the
// vectors it was restored with and still embeds a tester's search query.
func TestPausedInstanceEmbedsOnlySearchQueries(t *testing.T) {
	var queries atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&input)
		for _, text := range input.Input {
			if strings.Contains(text, "rehearsal passage") {
				t.Errorf("paused instance embedded a document: %q", text)
			}
		}
		queries.Add(1)
		vectors := make([][]float32, len(input.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	t.Cleanup(endpoint.Close)
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureOutgoing("paused"); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureEmbeddings(endpoint.URL+"/api/embed", ""); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"config": knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}})
	if w := proxyCall(app, "alice", "POST", "/p/default/api/knowledge/config", string(body)); w.Code != 200 {
		t.Fatal("configure", w.Code, w.Body)
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/note", `{"path":"rehearsal.md","content":"# Rehearsal\nA copied rehearsal passage.","revision":""}`); w.Code != 200 {
		t.Fatal("save", w.Code, w.Body)
	}
	var status struct{ State, Message string }
	for deadline := time.Now().Add(10 * time.Second); status.Message != outgoing.ErrPaused.Error(); {
		if time.Now().After(deadline) {
			t.Fatalf("background embedding did not report the pause: %+v", status)
		}
		time.Sleep(20 * time.Millisecond)
		w := proxyCall(app, "alice", "GET", "/p/default/api/knowledge/status", "")
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil {
			t.Fatal("status", w.Code, w.Body)
		}
	}
	if queries.Load() != 0 {
		t.Fatalf("endpoint called %d times before any search", queries.Load())
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/knowledge/search", `{"text":"paused synthetic query"}`); w.Code != 200 || queries.Load() != 1 {
		t.Fatalf("paused query embedding: %d calls=%d %s", w.Code, queries.Load(), w.Body)
	}
}
