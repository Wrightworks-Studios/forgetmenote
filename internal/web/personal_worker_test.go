package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/personal"
)

func connectPersonalFixture(t *testing.T, app *Server, user string, google func(*http.Request) string) {
	t.Helper()
	connectPersonalProviderFixture(t, app, user, "google", google)
}
func connectPersonalProviderFixture(t *testing.T, app *Server, user, provider string, respond func(*http.Request) string) {
	t.Helper()
	connectPersonalProviderHandler(t, app, user, provider, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, respond(r)) })
}

// connectPersonalProviderHandler is connectPersonalProviderFixture for a
// provider fake that also chooses its response status.
func connectPersonalProviderHandler(t *testing.T, app *Server, user, provider string, handler http.HandlerFunc) {
	t.Helper()
	googleServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(googleServer.Close)
	old := http.DefaultTransport
	transport := googleServer.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = googleServer.Certificate().DNSNames[0]
	googleAddress := strings.TrimPrefix(googleServer.URL, "https://")
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(address)
		if strings.HasSuffix(host, "googleapis.com") || host == "slack.com" {
			address = googleAddress
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	http.DefaultTransport = transport
	t.Cleanup(func() {
		app.Close()
		http.DefaultTransport = old
		transport.CloseIdleConnections()
	})
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	secret := filepath.Join(dir, "secret")
	os.WriteFile(key, []byte(strings.Repeat("x", 32)), 0600)
	os.WriteFile(secret, []byte("fixture-secret"), 0600)
	var err error
	config := connectors.PersonalConfig{Version: 1, KeyFile: key}
	if provider == "google" {
		config.Google = &connectors.OAuthApplication{ClientID: "test", SecretFile: secret}
	} else {
		requireSlack(t)
		config.Slack = &connectors.OAuthApplication{ClientID: "test", SecretFile: secret, Workspace: "T123"}
	}
	app.personalConnections, err = connectors.OpenPersonalConnections(app.db, config)
	if err != nil {
		t.Fatal(err)
	}
	account, _ := app.catalog.Account(user)
	target, err := app.personalConnections.Begin(account.ID, provider, "browser", "http://localhost/callback")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(target)
	if _, err = app.personalConnections.Finish(t.Context(), account.ID, provider, "browser", parsed.Query().Get("state"), "synthetic-code", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestPersonalAssistantChangedItemsAreJudgedOnce(t *testing.T) {
	var model, jev, writes atomic.Int32
	var opened atomic.Bool
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		model.Add(1)
		var request map[string]any
		json.NewDecoder(r.Body).Decode(&request)
		raw, _ := json.Marshal(request)
		if !strings.Contains(string(raw), "Can you send the launch plan") {
			t.Error("source did not reach Luna")
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": `{"title":"Reply about the launch plan","summary":"A colleague needs the current launch plan.","reply":"I will check the current plan and follow up.","due":"","report":"Source is the connected email thread.","noteRevision":"forged","mailboxID":"forged","notified":true}`}, "finish_reason": "stop"}}})
	})
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		jev.Add(1)
		var in struct {
			Questions map[string]any `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		answers := map[string]any{}
		for k := range in.Questions {
			answers[k] = map[string]any{"type": "noul", "noul": .96}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 40, "output_tokens": 4}})
	})
	_, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true, AIConnection: "local", Model: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"email"}})
	if err != nil {
		t.Fatal(err)
	}
	mail := map[string]any{"id": "m1", "threadId": "t1", "historyId": "101", "internalDate": fmt.Sprint(time.Now().UnixMilli()), "labelIds": []string{"INBOX"}, "payload": map[string]any{"mimeType": "text/plain", "headers": []any{map[string]string{"name": "From", "value": "colleague@example.test"}, map[string]string{"name": "To", "value": "alice@example.test"}, map[string]string{"name": "Subject", "value": "Launch plan"}, map[string]string{"name": "Message-ID", "value": "<m1@example.test>"}}, "body": map[string]string{"data": base64.RawURLEncoding.EncodeToString([]byte("Can you send the launch plan?"))}}}
	connectPersonalFixture(t, app, "alice", func(r *http.Request) string {
		if r.Method != "GET" && r.URL.Path != "/token" {
			writes.Add(1)
		}
		switch r.URL.Path {
		case "/token":
			return `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"scope":"openid email profile https://www.googleapis.com/auth/gmail.modify"}`
		case "/v1/userinfo":
			return `{"sub":"google-alice","email":"alice@example.test","email_verified":true}`
		case "/gmail/v1/users/me/profile":
			return `{"emailAddress":"alice@example.test","historyId":"100"}`
		case "/gmail/v1/users/me/messages":
			return `{"messages":[{"id":"m1","threadId":"t1"}]}`
		case "/gmail/v1/users/me/messages/m1":
			snapshot := maps.Clone(mail)
			if opened.Load() {
				snapshot["historyId"] = "102"
				snapshot["labelIds"] = []string{"INBOX", "STARRED"}
			}
			raw, _ := json.Marshal(snapshot)
			return string(raw)
		case "/gmail/v1/users/me/threads/t1":
			snapshot := maps.Clone(mail)
			revision := "101"
			if opened.Load() {
				revision = "102"
				snapshot["historyId"] = revision
				snapshot["labelIds"] = []string{"INBOX", "STARRED"}
			}
			messages := []any{snapshot}
			if writes.Load() > 0 {
				draft := maps.Clone(snapshot)
				draft["id"], draft["labelIds"] = "md1", []string{"DRAFT"}
				messages = append(messages, draft)
			}
			raw, _ := json.Marshal(map[string]any{"id": "t1", "historyId": revision, "messages": messages})
			return string(raw)
		case "/gmail/v1/users/me/drafts":
			return `{"id":"d1","message":{"id":"md1","threadId":"t1"}}`
		case "/gmail/v1/users/me/history":
			return `{"historyId":"102","history":[{"id":"102","labelsAdded":[{"message":{"id":"m1","threadId":"t1"},"labelIds":["STARRED"]}]}]}`
		default:
			t.Errorf("unexpected Google path %s", r.URL.Path)
			return `{}`
		}
	})
	spec, err := app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	run := automation.Run{ID: automation.Hash("personal-one"), Spec: spec, Created: time.Now()}
	if err = app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, state, err := app.personal.ReadState(spec)
	if err != nil || len(state.Drafts) != 1 {
		t.Fatalf("drafts=%d error=%v", len(state.Drafts), err)
	}
	for _, draft := range state.Drafts {
		if draft.NoteRevision == "forged" || draft.MailboxID != "" || draft.Notified {
			t.Fatal("model controlled operational state")
		}
		store, _ := app.catalog.Access("alice", settings.Project, "read")
		doc, e := store.Read(t.Context(), draft.Path, "")
		if e != nil || !strings.Contains(string(doc.Content), "launch plan") {
			t.Fatalf("draft note not committed: %v", e)
		}
	}
	opened.Store(true)
	run.ID = automation.Hash("personal-two")
	if err = app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if model.Load() != 1 || jev.Load() != 1 || writes.Load() != 0 {
		t.Fatalf("repeated work or unsolicited write: model=%d Jev=%d writes=%d", model.Load(), jev.Load(), writes.Load())
	}
	settings.MailboxDrafts = true
	settings, err = app.catalog.SaveAssistant("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	spec, err = app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, draft := range state.Drafts {
		if _, err = app.personalMailboxDraft(ctx, draft, true); err != nil {
			t.Fatal("consented mailbox draft failed", err)
		}
		app.automations.Queue, err = automation.Open(app.db)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = app.personalMailboxDraft(ctx, draft, true); err != nil {
			t.Fatal("accepted draft did not recover", err)
		}
		if err = app.personalCurrent(ctx, draft.Source); err != nil {
			t.Fatal("our mailbox draft invalidated the original request", err)
		}
	}
	if writes.Load() != 1 {
		t.Fatal("restart repeated mailbox write", writes.Load())
	}
	run.Spec = spec
	if err = app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if model.Load() != 1 || jev.Load() != 1 || writes.Load() != 1 {
		t.Fatal("mailbox draft triggered repeated paid work or another draft")
	}
	settings.Enabled = false
	if _, err = app.catalog.SaveAssistant("alice", settings); err != nil {
		t.Fatal(err)
	}
	if err = app.runAutomation(t.Context(), &run, func() error { return nil }); err == nil {
		t.Fatal("paused assistant continued")
	}
}
func TestPersonalAssistantContextDoesNotBroadenPermissions(t *testing.T) {
	app, _ := fixture(t)
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "read")
	app.catalog.Account("bob")
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := app.automationContext(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.clientAccess(ctx, "alice", "default", "edit"); err == nil {
		t.Fatal("personal assistant wrote to read-only company notebook")
	}
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "")
	if err = app.checkAutomationAuth(ctx); err == nil {
		t.Fatal("revoked company access accepted by active job")
	}
	if _, err = app.catalog.Access("bob", settings.Project, "read"); err == nil {
		t.Fatal("private assistant leaked")
	}
}

func TestPersonalAssistantCanUseMoreThanTenAuthorizedNotebooks(t *testing.T) {
	app, _ := fixture(t)
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	for i := range 12 {
		id := fmt.Sprintf("team-%d", i)
		if err := app.catalog.Create(catalog.Bootstrap, id, id); err != nil {
			t.Fatal(err)
		}
		if err := app.catalog.Member(catalog.Bootstrap, id, "alice", "read"); err != nil {
			t.Fatal(err)
		}
	}
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = app.automationSourceContext(ctx, &automation.Run{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.RunScopeFrom(ctx).Run.Options.Corpus.NoteProjects) != 12 {
		t.Fatal("authorized notebooks omitted")
	}
}

func TestPersonalUIChatCanReadItsDelegatedAccount(t *testing.T) {
	var calls atomic.Int32
	app, conversation := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		n := calls.Add(1)
		if n <= 3 && len(personal.Array(body["tools"])) > 0 && body["tool_choice"] != "none" {
			tools, _ := json.Marshal(body["tools"])
			if !strings.Contains(string(tools), "read_personal_source") || strings.Contains(string(tools), "submit_personal_action") {
				t.Error("chat must offer personal reads without external submissions")
			}
			name, args := "assistant_status", `{}`
			if n == 2 {
				name, args = "read_personal_source", `{"provider":"google","generation":1,"operation":"email.messages","arguments":"{}"}`
			}
			if n == 3 {
				name, args = "read_personal_source", `{"provider":"google","generation":1,"operation":"email.thread","arguments":"{\"id\":\"t1\"}"}`
			}
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": fmt.Sprint(n), "type": "function", "function": map[string]string{"name": name, "arguments": args}}}}, "finish_reason": "tool_calls"}}})
		} else {
			raw, _ := json.Marshal(body["messages"])
			if !strings.Contains(string(raw), "Please review the launch plan") {
				t.Error("complete personal source did not reach the employee's chat")
			}
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Your colleague asked you to review the launch plan."},"finish_reason":"stop"}]}`))
		}
	})
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	if _, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"email"}}); err != nil {
		t.Fatal(err)
	}
	connectPersonalFixture(t, app, "alice", func(r *http.Request) string {
		switch r.URL.Path {
		case "/token":
			return `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"scope":"openid email https://www.googleapis.com/auth/gmail.modify"}`
		case "/v1/userinfo":
			return `{"sub":"google-alice","email":"alice@example.test","email_verified":true}`
		case "/gmail/v1/users/me/profile":
			return `{"emailAddress":"alice@example.test","historyId":"100"}`
		case "/gmail/v1/users/me/messages":
			return `{"messages":[{"id":"m1","threadId":"t1"}]}`
		case "/gmail/v1/users/me/threads/t1":
			raw, _ := json.Marshal(map[string]any{"id": "t1", "historyId": "101", "messages": []any{map[string]any{"id": "m1", "threadId": "t1", "historyId": "101", "internalDate": fmt.Sprint(time.Now().UnixMilli()), "payload": map[string]any{"mimeType": "text/plain", "headers": []any{map[string]string{"name": "From", "value": "colleague@example.test"}, map[string]string{"name": "Subject", "value": "Launch plan"}}, "body": map[string]string{"data": base64.RawURLEncoding.EncodeToString([]byte("Please review the launch plan"))}}}}})
			return string(raw)
		default:
			t.Errorf("unexpected personal source %s", r.URL.Path)
			return `{}`
		}
	})
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+conversation.ID+"/turns", `{"submissionId":"personal-chat","text":"What is my latest email asking for?","options":{"mode":"ask"}}`)
	if w.Code != 202 && w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	got := waitChatRun(t, app, conversation.ID)
	run := got.Runs[len(got.Runs)-1]
	if run.State != "completed" || calls.Load() != 4 {
		t.Fatalf("personal chat %s: %s (%d calls)", run.State, run.Error, calls.Load())
	}
}

func TestPersonalMemoryUsesOnlyDirectInteractionsAndPreservesEdits(t *testing.T) {
	calls := 0
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request map[string]any
		json.NewDecoder(r.Body).Decode(&request)
		raw, _ := json.Marshal(request)
		if !strings.Contains(string(raw), "Please keep replies brief") {
			t.Error("confirmed preference missing")
		}
		content, _ := json.Marshal(map[string]string{"AGENTS.md": "# Work habits\n\nCite sources.\n", "SOUL.md": "# Preferences\n\nKeep replies brief, as requested by the employee.\n", "CONTEXT.md": "# Context\n\nNo new confirmed priorities.\n", "commitments.md": "# Commitments\n\nNo open promises.\n"})
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": string(content)}, "finish_reason": "stop"}}})
	})
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true, AIConnection: "local", Model: "synthetic"})
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, MaintainMemory: true, TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := app.personalSpec("alice", settings)
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.personal.Seed(ctx, settings); err != nil {
		t.Fatal(err)
	}
	in := personal.Interaction{ID: "direct-one", UserText: "Please keep replies brief", Response: "Understood", Source: "private-chat"}
	if err = app.recordPersonalInteraction(ctx, "bob", in); err == nil {
		t.Fatal("another user could be selected")
	}
	if err = app.recordPersonalInteraction(ctx, "alice", in); err != nil {
		t.Fatal(err)
	}
	r := automation.Run{ID: automation.Hash("memory"), Spec: spec}
	if err = app.personal.MaintainMemory(ctx, &r); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", settings.Project, "read")
	doc, err := store.Read(ctx, "SOUL.md", "")
	if err != nil || !strings.Contains(string(doc.Content), "brief") {
		t.Fatalf("memory not maintained %v", err)
	}
	if err = app.recordPersonalInteraction(ctx, "alice", in); err != nil {
		t.Fatal(err)
	}
	if err = app.personal.MaintainMemory(ctx, &r); err != nil || calls != 1 {
		t.Fatalf("duplicate interaction repurchased: %d %v", calls, err)
	}
}

func TestPersonalAssistantPageAndActivitiesArePrivate(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	alice := addLocal(t, app, admin, "page-alice")
	addLocal(t, app, admin, "page-bob")
	settings, _ := app.catalog.SaveAssistant(alice, catalog.AssistantSettings{TimeZone: "UTC"})
	for _, name := range []string{"page-alice", "page-bob"} {
		login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {name}, "password": {"test-password-long"}}.Encode())
		cookie := login.Result().Cookies()[0]
		w := oidcRequest(app, "GET", "/account/assistant", "", cookie)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "assistant-preferences") {
			t.Fatalf("assistant page: %d %s", w.Code, w.Body.String())
		}
		w = localRequest(app, "/api/assistant/action/assistant_context", `{}`, cookie)
		if name == "page-bob" && strings.Contains(w.Body.String(), settings.Project) {
			t.Fatal("another employee's notebook leaked")
		}
	}
}
