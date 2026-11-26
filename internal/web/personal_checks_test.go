package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/personal"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
)

// personalRunOptions are the points where a test acts inside a personal
// assistant run.
type personalRunOptions struct {
	// setup runs before the instance assistant policy is saved.
	setup func(app *Server, policy *catalog.AssistantPolicy)
	// model runs inside each model call, with its request body.
	model func(app *Server, body []byte)
	// jev runs inside each Jev request, with its request body.
	jev func(app *Server, body []byte)
	// google runs before each Google response, with the request path.
	google func(app *Server, path string)
	// replied, once set, adds a reply to the mail thread.
	replied *atomic.Bool
}

// personalRunFixture connects alice's Google account, which has one mail
// thread. Jev selects the thread and the model drafts a reply to it.
func personalRunFixture(t *testing.T, o personalRunOptions) (*Server, automation.Spec) {
	t.Helper()
	if o.replied == nil {
		o.replied = new(atomic.Bool)
	}
	var app *Server
	app, _ = runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if o.model != nil {
			o.model(app, body)
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": `{"title":"Reply about the launch plan","summary":"A colleague needs the launch plan.","reply":"I will follow up.","due":"","report":"Source is the connected email thread."}`}, "finish_reason": "stop"}}})
	})
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if o.jev != nil {
			o.jev(app, body)
		}
		var in struct {
			Questions map[string]any `json:"questions"`
		}
		json.Unmarshal(body, &in)
		answers := map[string]any{}
		for k := range in.Questions {
			answers[k] = map[string]any{"type": "noul", "noul": .96}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 40, "output_tokens": 4}})
	})
	policy := catalog.AssistantPolicy{Enabled: true, AIConnection: "local", Model: "synthetic"}
	if o.setup != nil {
		o.setup(app, &policy)
	}
	if _, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, policy); err != nil {
		t.Fatal(err)
	}
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"email"}})
	if err != nil {
		t.Fatal(err)
	}
	mail := map[string]any{"id": "m1", "threadId": "t1", "historyId": "101", "internalDate": fmt.Sprint(time.Now().UnixMilli()), "labelIds": []string{"INBOX"}, "payload": map[string]any{"mimeType": "text/plain", "headers": []any{map[string]string{"name": "From", "value": "colleague@example.test"}, map[string]string{"name": "To", "value": "alice@example.test"}, map[string]string{"name": "Subject", "value": "Launch plan"}, map[string]string{"name": "Message-ID", "value": "<m1@example.test>"}}, "body": map[string]string{"data": base64.RawURLEncoding.EncodeToString([]byte("Can you send the launch plan?"))}}}
	connectPersonalFixture(t, app, "alice", func(r *http.Request) string {
		if o.google != nil {
			o.google(app, r.URL.Path)
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
			raw, _ := json.Marshal(mail)
			return string(raw)
		case "/gmail/v1/users/me/threads/t1":
			messages, history := []any{mail}, "101"
			if o.replied.Load() {
				reply := maps.Clone(mail)
				reply["id"], reply["historyId"], history = "m2", "102", "102"
				reply["payload"] = map[string]any{"mimeType": "text/plain", "headers": []any{map[string]string{"name": "From", "value": "colleague@example.test"}, map[string]string{"name": "Subject", "value": "Re: Launch plan"}}, "body": map[string]string{"data": base64.RawURLEncoding.EncodeToString([]byte("Never mind, I found it."))}}
				messages = append(messages, reply)
			}
			raw, _ := json.Marshal(map[string]any{"id": "t1", "historyId": history, "messages": messages})
			return string(raw)
		case "/gmail/v1/users/me/drafts":
			return `{"id":"d1","message":{"id":"md1","threadId":"t1"}}`
		default:
			t.Errorf("unexpected Google path %s", r.URL.Path)
			return `{}`
		}
	})
	spec, err := app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	return app, spec
}

// runPersonal runs the personal assistant once.
func runPersonal(t *testing.T, app *Server, spec automation.Spec) error {
	t.Helper()
	run := automation.Run{ID: automation.Hash(t.Name()), Spec: spec, Created: time.Now()}
	return app.runAutomation(t.Context(), &run, func() error { return nil })
}

// publishedDrafts lists the drafts the run wrote to the assistant notebook.
func publishedDrafts(t *testing.T, app *Server, spec automation.Spec) []personal.Draft {
	t.Helper()
	_, state, err := app.personal.ReadState(spec)
	if err != nil {
		t.Fatal(err)
	}
	var out []personal.Draft
	for _, d := range state.Drafts {
		if d.NoteRevision != "" {
			out = append(out, d)
		}
	}
	return out
}

// The worker rereads a source before it publishes a draft of it. A thread that
// gained a reply while the model drafted is not published.
func TestPersonalDraftOfAChangedSourceIsNotPublished(t *testing.T) {
	var replied atomic.Bool
	app, spec := personalRunFixture(t, personalRunOptions{replied: &replied, model: func(*Server, []byte) { replied.Store(true) }})
	if err := runPersonal(t, app, spec); err != nil {
		t.Fatal(err)
	}
	if !replied.Load() {
		t.Fatal("the model never drafted")
	}
	if published := publishedDrafts(t, app, spec); len(published) != 0 {
		t.Fatalf("published a draft of a thread that changed after it was read: %v", published[0].Path)
	}
}

// Each read of a person's mail rechecks the run's authority. Once the instance
// pauses outgoing work, collection reads nothing more.
func TestPersonalCollectionStopsReadingWhenTheInstanceIsPaused(t *testing.T) {
	var paused atomic.Bool
	var mu sync.Mutex
	var readAfter []string
	app, spec := personalRunFixture(t, personalRunOptions{google: func(app *Server, path string) {
		if paused.Load() && path != "/token" {
			mu.Lock()
			readAfter = append(readAfter, path)
			mu.Unlock()
		}
		if path == "/gmail/v1/users/me/messages" && !paused.Load() {
			if err := app.ConfigureOutgoing("paused"); err != nil {
				t.Error(err)
			}
			paused.Store(true)
		}
	}})
	err := runPersonal(t, app, spec)
	if !paused.Load() {
		t.Fatal("collection never listed messages", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(readAfter) != 0 {
		t.Fatalf("read the person's mail after the instance paused: %v", readAfter)
	}
}

// The assistant reads a person's responsibilities with that person's own
// access. A people page in a directory notebook they cannot read reaches
// neither Jev nor the model.
func TestPersonalAssistantDoesNotReadAnUnreadableDirectory(t *testing.T) {
	const marker = "DIRECTORY_ONLY_RESPONSIBILITY"
	var mu sync.Mutex
	var requests [][]byte
	record := func(_ *Server, body []byte) {
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
	}
	app, spec := personalRunFixture(t, personalRunOptions{model: record, jev: record, setup: func(app *Server, policy *catalog.AssistantPolicy) {
		if err := app.catalog.Create(catalog.Bootstrap, "directory", "Directory"); err != nil {
			t.Fatal(err)
		}
		policy.DirectoryProject = "directory"
	}})
	account, err := app.catalog.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	directory, err := app.catalog.Access(catalog.Bootstrap, "directory", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = directory.Save(t.Context(), "people/"+account.ID+".md", []byte("# Alice\n\n"+marker+"\n"), "", catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	if _, err = app.catalog.Access("alice", "directory", "read"); err == nil {
		t.Fatal("fixture: alice can read the directory notebook")
	}
	if err = runPersonal(t, app, spec); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) < 2 {
		t.Fatalf("the run asked Jev and the model %d times, want both", len(requests))
	}
	for _, body := range requests {
		if bytes.Contains(body, []byte(marker)) {
			t.Fatal("a people page alice cannot read reached Jev or the model")
		}
	}
}

// repositorySourcesFixture opens repository sources for connected repository
// notebooks. Each one allows external processing as given.
func repositorySourcesFixture(t *testing.T, app *Server, external map[string]bool) *reposource.Manager {
	t.Helper()
	for project, allowed := range external {
		state := reposource.State{ID: "fixture", Project: project, Actor: catalog.Bootstrap, Installation: 7, Repository: reposource.Repository{ID: 42, FullName: "example/source", DefaultBranch: "main", Private: true}, Connected: true, Generation: 1, Downloaded: "synthetic", IndexConfig: reposource.IndexConfig{ExternalProcessing: allowed}}
		data, _ := json.Marshal(map[string]any{"version": 1, "active": "fixture", "sources": map[string]reposource.State{"fixture": state}})
		if _, err := app.db.WriteDocument(t.Context(), app.db.Pool, "repository-sources", project, 0, data); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := reposource.Open(app.db, filepath.Join(t.TempDir(), "sources"), nil, nil, func(p, a string) error { _, err := app.catalog.Access(a, p, "owner"); return err })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	return manager
}

// A personal assistant run reads only notebooks and data connections that the
// person may read and that allow external processing. It never reads Inboxes,
// other people's assistant notebooks or notebooks the person excluded.
func TestPersonalSpecReadsOnlyPermittedSources(t *testing.T) {
	app, _ := fixture(t)
	if _, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []struct{ id, kind string }{{"shared", "notes"}, {"excluded", "notes"}, {"code", "repository"}, {"local-code", "repository"}} {
		if err := app.catalog.CreateKind(catalog.Bootstrap, n.id, n.id, n.kind); err != nil {
			t.Fatal(err)
		}
		for _, user := range []string{"alice", "bob"} {
			if err := app.catalog.Member(catalog.Bootstrap, n.id, user, "read"); err != nil {
				t.Fatal(err)
			}
		}
	}
	app.repositories = repositorySourcesFixture(t, app, map[string]bool{"code": true, "local-code": false})
	var err error
	read := []connectors.Operation{{Name: "read", ReadOnly: true, Method: "GET", Path: "/"}}
	app.dataConnections, err = connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{
		{ID: "permitted", Kind: "http", Endpoint: "http://127.0.0.1:1", ExternalProcessing: true, Operations: read},
		{ID: "local-only", Kind: "http", Endpoint: "http://127.0.0.1:1", Operations: read},
		{ID: "ungranted", Kind: "http", Endpoint: "http://127.0.0.1:1", ExternalProcessing: true, Operations: read},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"permitted", "local-only"} {
		if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: id, Users: map[string][]string{"alice": {"read"}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{"alice", "bob"} {
		if _, err = app.catalog.EnsureInbox(user); err != nil {
			t.Fatal(err)
		}
	}
	specOf := func(user string, settings catalog.AssistantSettings) automation.Spec {
		t.Helper()
		settings.Enabled, settings.ExternalProcessing = true, true
		saved, err := app.catalog.SaveAssistant(user, settings)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := app.personalSpec(user, saved)
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}
	specOf("bob", catalog.AssistantSettings{})
	alice := specOf("alice", catalog.AssistantSettings{ExcludedProjects: []string{"excluded"}})
	if want := []string{"code", "shared"}; !slices.Equal(alice.ReadProjects, want) {
		t.Errorf("alice's assistant reads notebooks %v, want %v", alice.ReadProjects, want)
	}
	if want := []string{"permitted"}; !slices.Equal(alice.Connections, want) {
		t.Errorf("alice's assistant reads data connections %v, want %v", alice.Connections, want)
	}
	// An administrator can list everyone's notebooks and use every connection.
	admin := specOf("admin", catalog.AssistantSettings{})
	if want := []string{"code", "default", "excluded", "shared"}; !slices.Equal(admin.ReadProjects, want) {
		t.Errorf("the administrator's assistant reads notebooks %v, want %v", admin.ReadProjects, want)
	}
	if want := []string{"permitted", "ungranted"}; !slices.Equal(admin.Connections, want) {
		t.Errorf("the administrator's assistant reads data connections %v, want %v", admin.Connections, want)
	}
}

// personalAutomationContext is the context of a run of alice's assistant.
func personalAutomationContext(t *testing.T, app *Server, spec automation.Spec) context.Context {
	t.Helper()
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// disconnectedPersonalSource is a run of alice's assistant and an observation
// of her Google mail, which she has since disconnected.
func disconnectedPersonalSource(t *testing.T) (*Server, context.Context, *automation.Run, []personal.Observation) {
	t.Helper()
	app, spec := personalRunFixture(t, personalRunOptions{})
	ctx := personalAutomationContext(t, app, spec)
	run := &automation.Run{ID: automation.Hash(t.Name()), Spec: spec, Created: time.Now()}
	connections, err := app.personalConnections.List(spec.Personal.Account)
	if err != nil || len(connections) != 1 {
		t.Fatal("fixture: alice's Google connection", connections, err)
	}
	items := []personal.Observation{{Key: "email:t1", Kind: "email", Revision: "101", Source: connectors.PersonalOperation{Provider: "google", Generation: connections[0].Generation, Operation: "email.thread", Arguments: map[string]any{"thread": "t1"}}}}
	if err = app.checkPersonalSources(ctx, run, items); err != nil {
		t.Fatal("fixture: the connected source was refused", err)
	}
	if err = app.personalConnections.Disconnect(spec.Personal.Account, "google"); err != nil {
		t.Fatal(err)
	}
	return app, ctx, run, items
}

func TestCheckPersonalSourcesRefusesADisconnectedSource(t *testing.T) {
	app, ctx, run, items := disconnectedPersonalSource(t)
	if err := app.checkPersonalSources(ctx, run, items); err == nil {
		t.Fatal("accepted an observation from a disconnected Google account")
	}
}

// assistantMemoryAfterTurnOff is the context of a run of alice's assistant
// after the instance turned personal assistants off. Her assistant memory
// holds the returned text.
func assistantMemoryAfterTurnOff(t *testing.T) (*Server, context.Context, string) {
	t.Helper()
	const memory = "PRIVATE_ASSISTANT_MEMORY"
	app, spec := personalRunFixture(t, personalRunOptions{})
	store, err := app.catalog.Access("alice", spec.Project, "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "CONTEXT.md", []byte("# Current context\n\n"+memory+"\n"), "", "alice"); err != nil {
		t.Fatal(err)
	}
	ctx := personalAutomationContext(t, app, spec)
	current, err := app.personalContext(ctx)
	if err != nil || !strings.Contains(fmt.Sprint(current.Memory), memory) {
		t.Fatal("fixture: the run could not read alice's memory", err)
	}
	policy := app.catalog.AssistantPolicy()
	policy.Enabled = false
	if _, err = app.catalog.SetAssistantPolicy(catalog.Bootstrap, policy); err != nil {
		t.Fatal(err)
	}
	return app, ctx, memory
}

func TestPersonalContextRefusesARunAfterAssistantsAreTurnedOff(t *testing.T) {
	app, ctx, memory := assistantMemoryAfterTurnOff(t)
	current, err := app.personalContext(ctx)
	if err == nil || strings.Contains(fmt.Sprint(current.Memory), memory) {
		t.Fatal("a run read assistant memory after the instance turned assistants off", err)
	}
}

// automaticDraftWithoutConsent is a published draft of a reply in alice's
// assistant notebook and the context of a run of her assistant. She has not
// turned on mailbox drafts. drafts counts the mailbox drafts Google receives.
func automaticDraftWithoutConsent(t *testing.T) (app *Server, ctx context.Context, draft personal.Draft, drafts *atomic.Int32) {
	t.Helper()
	drafts = new(atomic.Int32)
	app, spec := personalRunFixture(t, personalRunOptions{google: func(_ *Server, path string) {
		if path == "/gmail/v1/users/me/drafts" {
			drafts.Add(1)
		}
	}})
	if err := runPersonal(t, app, spec); err != nil {
		t.Fatal(err)
	}
	published := publishedDrafts(t, app, spec)
	if len(published) != 1 {
		t.Fatalf("fixture: published %d drafts, want 1", len(published))
	}
	if settings, err := app.catalog.Assistant("alice"); err != nil || settings.MailboxDrafts {
		t.Fatal("fixture: mailbox drafts are on", err)
	}
	return app, personalAutomationContext(t, app, spec), published[0], drafts
}

func TestPersonalMailboxDraftRefusesAutomaticDraftsWithoutConsent(t *testing.T) {
	app, ctx, draft, drafts := automaticDraftWithoutConsent(t)
	if _, err := app.personalMailboxDraft(ctx, draft, true); !errors.Is(err, workspace.ErrForbidden) || drafts.Load() != 0 {
		t.Fatalf("created %d mailbox drafts without consent: %v", drafts.Load(), err)
	}
}

// automationAfterConsentWithdrawn is the context of a run of alice's assistant
// after she withdrew her consent to reading her mail.
func automationAfterConsentWithdrawn(t *testing.T) (*Server, context.Context) {
	t.Helper()
	app, spec := personalRunFixture(t, personalRunOptions{})
	ctx := personalAutomationContext(t, app, spec)
	if err := app.checkAutomationAuth(ctx); err != nil {
		t.Fatal("fixture: the run was refused", err)
	}
	settings, err := app.catalog.Assistant("alice")
	if err != nil {
		t.Fatal(err)
	}
	settings.Features = nil
	if _, err = app.catalog.SaveAssistant("alice", settings); err != nil {
		t.Fatal(err)
	}
	return app, ctx
}

func TestCheckAutomationAuthRefusesARunAfterConsentIsWithdrawn(t *testing.T) {
	app, ctx := automationAfterConsentWithdrawn(t)
	if err := app.checkAutomationAuth(ctx); err == nil {
		t.Fatal("a run kept its authority after alice withdrew consent")
	}
}
