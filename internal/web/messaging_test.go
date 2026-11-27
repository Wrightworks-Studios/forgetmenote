package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/messaging"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMessagingRecoversCommittedAnswerWithoutRepeatingGeneration(t *testing.T) {
	calls := 0
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Saved answer."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	r := automation.Run{ID: automation.Hash("recover-answer"), Spec: automation.Spec{ID: "recover-answer", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Instruction: "Answer once.", Delivery: &automation.Delivery{Channel: "C123", Thread: "thread", User: "U123"}}}
	var checkpoint []byte
	err := app.automationrun.RunAutomation(t.Context(), &r, func() error {
		if r.Answer != "" {
			return errors.New("fixture checkpoint failure after conversation commit")
		}
		checkpoint, _ = json.Marshal(r)
		return nil
	})
	if err == nil || calls != 1 || len(transport.replies) != 0 {
		t.Fatal("missing interrupted delivery", err, calls)
	}
	if err := json.Unmarshal(checkpoint, &r); err != nil {
		t.Fatal(err)
	}
	if !r.GenerationStarted {
		t.Fatal("model work was not fenced before generation")
	}
	if err := app.automationrun.RunAutomation(t.Context(), &r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(transport.replies) != 1 || transport.replies[0].Text != "Saved answer." {
		t.Fatal("recovery repeated generation or lost answer", calls, transport.replies)
	}
	// A started generation with no committed conversation has an unknown outcome.
	r.ID = automation.Hash("unknown-answer")
	r.Answer = ""
	r.DeliveryState = ""
	r.Spec.Delivery.Thread = "unknown"
	if err := app.automationrun.RunAutomation(t.Context(), &r, func() error { return nil }); err == nil || calls != 1 {
		t.Fatal("unknown model call repeated", err, calls)
	}
}

func TestMessagingScheduledDMUsesOnlyItsApprovedNotebookScope(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	if err := app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "other", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	app.slack.config.DirectMessages = map[string]SlackChannel{"U123": app.slack.config.Channels["C123"]}
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	r := automation.Run{Spec: automation.Spec{Kind: "digest", Actor: "alice", Project: "default", ReadProjects: []string{"other"}, NotifySlackUser: "U123"}, Answer: "Includes another notebook."}
	if err := app.automationrun.Notify(t.Context(), &r, func() error { return nil }); err == nil || len(transport.replies) != 0 {
		t.Fatal("notification exceeded DM notebook scope", err)
	}
	b := app.slack.config.DirectMessages["U123"]
	b.ReadProjects = []string{"other"}
	app.slack.config.DirectMessages["U123"] = b
	if err := app.automationrun.Notify(t.Context(), &r, func() error { return nil }); err != nil || len(transport.replies) != 1 {
		t.Fatal("approved notification rejected", err)
	}
}

func TestSlackSignedFollowupsDeduplicateMessageAndMentionEvents(t *testing.T) {
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Ready."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	b := app.slack.config.Channels["C123"]
	b.Attachments = true
	app.slack.config.Channels["C123"] = b
	app.messageClients["slack"] = &fixtureChatTransport{}
	send := func(id, kind, thread, ts, subtype string, files []map[string]string) {
		event := map[string]any{"type": "event_callback", "team_id": "T123", "event_id": id, "event": map[string]any{"type": kind, "channel": "C123", "user": "U123", "text": "Continue using the evidence.", "ts": ts, "thread_ts": thread, "subtype": subtype, "files": files}}
		raw, _ := json.Marshal(event)
		stamp := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte("fixture-signing"))
		mac.Write([]byte("v0:" + stamp + ":" + string(raw)))
		r := httptest.NewRequest("POST", "http://localhost/integrations/slack/events", strings.NewReader(string(raw)))
		r.Header.Set("X-Slack-Request-Timestamp", stamp)
		r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: HTTP %d %s", id, w.Code, w.Body)
		}
	}
	send("unrelated", "message", "unknown", "1.1", "", nil)
	runs, _ := app.automations.Queue.List()
	if len(runs) != 0 {
		t.Fatal("answered an unrelated channel thread")
	}
	send("start", "app_mention", "", "2.0", "", nil)
	if _, err := app.automations.Queue.RunNext(t.Context(), app.automationrun.RunAutomation); err != nil {
		t.Fatal(err)
	}
	send("reply-message", "message", "2.0", "2.1", "", nil)
	send("reply-mention", "app_mention", "2.0", "2.1", "", nil)
	runs, _ = app.automations.Queue.List()
	if len(runs) != 2 {
		t.Fatalf("same Slack message scheduled more than once: %d", len(runs))
	}
	send("attachment", "app_mention", "2.0", "2.2", "", []map[string]string{{"id": "F123"}})
	runs, _ = app.automations.Queue.List()
	found := false
	for _, r := range runs {
		if r.Spec.Delivery.Message == "2.2" && len(r.Spec.Delivery.Files) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit file-share mention was ignored")
	}
	legacy := runs[0].Spec
	legacy.ID = "slack-" + automation.Hash("legacy-event")[:32]
	if _, err := app.automations.Queue.Enqueue(legacy, "legacy-event"); err != nil {
		t.Fatal(err)
	}
	send("legacy-event", "app_mention", "2.0", "2.3", "", nil)
	runs, _ = app.automations.Queue.List()
	if len(runs) != 4 {
		t.Fatal("upgrade repeated an already queued Slack event", len(runs))
	}
}

func TestSlackLegacyPrivateHistorySurvivesDefaultModel(t *testing.T) {
	seen := false
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input any
		json.NewDecoder(r.Body).Decode(&input)
		raw, _ := json.Marshal(input)
		seen = strings.Contains(string(raw), "Legacy private question")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Continued."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	b := app.slack.config.Channels["C123"]
	b.Model = ""
	app.slack.config.Channels["C123"] = b
	// This is the persisted identity from the old Slack-only implementation.
	oldBinding := struct {
		Project            string   `json:"project"`
		AIConnection       string   `json:"aiConnection"`
		Model              string   `json:"model"`
		Connections        []string `json:"connections,omitempty"`
		ExternalProcessing bool     `json:"externalProcessing"`
	}{"default", "local", "", nil, true}
	key := automation.Hash([]any{"slack", "T123", "C123", "1.2", "U123", oldBinding})
	c, err := app.chat.Store.OpenBound("alice", "default", key, chat.Options{Mode: "ask", Connection: "local", Model: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), RunID: "old", Role: "user", Content: "Legacy private question"}, chat.Turn{ID: chat.NewID(), RunID: "old", Role: "assistant", Content: "Prior answer."})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app.messageClients["slack"] = &fixtureChatTransport{}
	r := automation.Run{ID: automation.Hash("legacy-followup"), Spec: automation.Spec{ID: "legacy-followup", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Instruction: "Continue", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", Thread: "1.2", SlackUser: "U123"}}}
	if err := app.automationrun.RunAutomation(t.Context(), &r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("default-model configuration lost persisted private history")
	}
}

func TestSlackSharedConfigurationAcceptsNotebookCorpus(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	raw, _ := json.Marshal(app.slack.config)
	var config map[string]any
	json.Unmarshal(raw, &config)
	config["users"].(map[string]any)["U456"] = "bob"
	binding := config["channels"].(map[string]any)["C123"].(map[string]any)
	binding["readProjects"] = []string{"default"}
	binding["shared"] = true
	binding["audienceApproved"] = true
	binding["actors"] = []string{"alice", "bob"}
	binding["threadHistory"] = true
	raw, _ = json.Marshal(config)
	file := filepath.Join(t.TempDir(), "slack.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureSlack(file); err != nil {
		t.Fatalf("shared, scoped Slack conversation rejected: %v", err)
	}
}

type fixtureChatTransport struct {
	input     messaging.Context
	replies   []messaging.Reply
	addresses []messaging.Address
}

func (f *fixtureChatTransport) Context(context.Context, messaging.Address, messaging.Binding) (messaging.Context, error) {
	return f.input, nil
}
func (f *fixtureChatTransport) Send(_ context.Context, d messaging.Address, r messaging.Reply) error {
	f.replies = append(f.replies, r)
	f.addresses = append(f.addresses, d)
	return nil
}

func TestMessagingOtherClientSharesScopedThreadAcrossUsersAndRestart(t *testing.T) {
	var prompts []string
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		json.NewDecoder(r.Body).Decode(&input)
		raw, _ := json.Marshal(input)
		prompts = append(prompts, string(raw))
		if len(prompts) == 1 {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"source","type":"function","function":{"name":"read_document","arguments":"{\"project\":\"related\",\"path\":\"guide.md\"}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Evidence from the related notebook: seven."},"finish_reason":"stop"}]}`))
	})
	if err := app.catalog.Create(catalog.Bootstrap, "related", "Related"); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access(catalog.Bootstrap, "related", "edit")
	if _, err := store.Save(t.Context(), "guide.md", []byte("# Verified related source\nThe result is seven.\n"), "", "fixture"); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"alice", "bob"} {
		if err := app.catalog.Member(catalog.Bootstrap, "related", actor, "read"); err != nil {
			t.Fatal(err)
		}
	}
	transport := &fixtureChatTransport{input: messaging.Context{Messages: []messaging.ContextMessage{{ID: "earlier", User: "external-alice", Text: "Visible shared context"}, {ID: "unknown", User: "outsider", Text: "DO NOT INCLUDE UNMAPPED AUTHOR"}}}}
	app.messageClients = map[string]messaging.Transport{"other-chat": transport}
	policy := messaging.Client{Workspace: "workspace", Users: map[string]string{"external-alice": "alice", "external-bob": "bob"}, Channels: map[string]messaging.Binding{"room": {Project: "default", ReadProjects: []string{"related"}, AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Shared: true, AudienceApproved: true, Actors: []string{"alice", "bob"}}}}
	if _, err := app.catalog.SetChatClients(catalog.Bootstrap, catalog.ChatClients{Clients: map[string]messaging.Client{"other-chat": policy}}); err != nil {
		t.Fatal(err)
	}
	if err := app.initAutomations(); err != nil {
		t.Fatal(err)
	}
	for i, user := range []string{"external-alice", "external-bob"} {
		event := messaging.Event{ID: []string{"event-one", "event-two"}[i], Text: []string{"Read the related notebook.", "Explain your previous answer."}[i], Address: messaging.Address{Client: "other-chat", Workspace: "workspace", Channel: "room", Thread: "thread-one", User: user, Message: user}}
		if err := app.messaging.EnqueueMessage(t.Context(), event); err != nil {
			t.Fatal(err)
		}
		if err := app.messaging.EnqueueMessage(t.Context(), event); err != nil {
			t.Fatal("duplicate event", err)
		}
		if _, err := app.automations.Queue.RunNext(t.Context(), app.automationrun.RunAutomation); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			app.chat.Store.Close()
			reopened, err := chat.OpenStore(app.db)
			if err != nil {
				t.Fatal(err)
			}
			app.chat.Store = reopened
		}
	}
	if len(prompts) != 3 || !strings.Contains(prompts[1], "Verified related source") || !strings.Contains(prompts[2], "Read the related notebook.") {
		t.Fatalf("missing source or shared history: %v", prompts)
	}
	for _, p := range prompts {
		if strings.Contains(p, "DO NOT INCLUDE") {
			t.Fatal("unmapped thread author reached provider")
		}
	}
	if len(transport.replies) != 2 || !transport.addresses[1].Shared {
		t.Fatal("duplicate delivery or private/shared mixup")
	}
	for _, actor := range []string{"alice", "bob"} {
		conversations, _ := app.chat.Store.List(actor, "default")
		for _, c := range conversations {
			if c.Title == "Chat conversation" {
				t.Fatal("shared thread leaked into a private account store")
			}
		}
	}
	if err := app.catalog.Member(catalog.Bootstrap, "related", "bob", ""); err != nil {
		t.Fatal(err)
	}
	if err := app.messaging.EnqueueMessage(t.Context(), messaging.Event{ID: "revoked", Text: "Another answer", Address: messaging.Address{Client: "other-chat", Workspace: "workspace", Channel: "room", Thread: "thread-one", User: "external-alice"}}); err == nil {
		t.Fatal("shared answer allowed after audience lost notebook access")
	}
}

func TestMessagingAdminUpdateFencesQueuedEvents(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	event := messaging.Event{ID: "before-disable", Text: "Do not run after disabling", Address: messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", Thread: "thread", User: "U123"}}
	if err := app.messaging.EnqueueMessage(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	cookie := recoverySession(t, app)
	w := localRequest(app, "/api/organization/action/chat_clients", `{}`, cookie)
	var response struct{ Config catalog.ChatClients }
	if json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal(w.Body)
	}
	c := response.Config.Clients["slack"]
	c.Disabled = true
	response.Config.Clients["slack"] = c
	raw, _ := json.Marshal(response)
	w = localRequest(app, "/api/organization/action/set_chat_clients", string(raw), cookie)
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	w = localRequest(app, "/api/organization/action/set_chat_clients", string(raw), cookie)
	if w.Code != 409 {
		t.Fatalf("stale save: %d %s", w.Code, w.Body)
	}
	runs, _ := app.automations.Queue.List()
	if len(runs) != 1 {
		t.Fatal(runs)
	}
	if err := app.automationrun.RunAutomation(t.Context(), &runs[0], func() error { return nil }); err == nil {
		t.Fatal("disabled client resumed queued work")
	}
}

func TestMessagingRunDiagnosticsDoNotExposePrivatePrompts(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	if err := app.messaging.EnqueueMessage(t.Context(), messaging.Event{ID: "private-event", Text: "private-chat-content-marker", Address: messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", User: "U123"}}); err != nil {
		t.Fatal(err)
	}
	runs, _ := app.automations.Queue.List()
	raw, _ := json.Marshal(map[string]string{"id": runs[0].ID})
	w := localRequest(app, "/api/organization/action/automation_run", string(raw), recoverySession(t, app))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "private-chat-content-marker") {
		t.Fatal("automation diagnostics expose a private chat prompt")
	}
}

func TestMessagingAdministrationDoesNotExposeCredentials(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	w := localRequest(app, "/api/organization/action/chat_clients", `{}`, recoverySession(t, app))
	if w.Code != http.StatusOK {
		t.Fatalf("chat administration unavailable: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "fixture-bot") || strings.Contains(w.Body.String(), "signingSecretFile") {
		t.Fatal("runtime credentials exposed")
	}
	w = proxyCall(app, "alice", "POST", "/api/organization/action/chat_clients", `{}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin inspected settings: %d %s", w.Code, w.Body)
	}
}

func TestMessagingEmptyFileConfigScopesSurviveQueueSerialization(t *testing.T) {
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Ready."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	b := app.slack.config.Channels["C123"]
	b.ReadProjects = []string{}
	b.Connections = []string{}
	app.slack.config.Channels["C123"] = b
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	if err := app.messaging.EnqueueMessage(t.Context(), messaging.Event{ID: "empty-lists", Text: "Answer.", Address: messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", User: "U123"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.automations.Queue.RunNext(t.Context(), app.automationrun.RunAutomation); err != nil {
		t.Fatal(err)
	}
	if len(transport.replies) != 1 {
		runs, _ := app.automations.Queue.List()
		t.Fatalf("empty scope failed after restart: %+v", runs)
	}
}

func TestMessagingSharedQueryRequiresEveryAudienceMembersGrant(t *testing.T) {
	step := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) {
		step++
		if step == 1 {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"query","type":"function","function":{"name":"call_data_operation","arguments":"{\"project\":\"default\",\"connection\":\"db\",\"operation\":\"query\",\"arguments\":{\"sql\":\"select 7\"}}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		var input struct {
			Messages []struct{ Role, Content string }
		}
		json.NewDecoder(r.Body).Decode(&input)
		if !strings.Contains(input.Messages[len(input.Messages)-1].Content, `"error"`) {
			t.Error("shared query ignored an audience member's missing grant")
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"The shared audience lacks data access."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	app.slack.config.Users["U456"] = "bob"
	b := app.slack.config.Channels["C123"]
	b.Connections = []string{"db"}
	b.Shared = true
	b.AudienceApproved = true
	b.Actors = []string{"alice", "bob"}
	app.slack.config.Channels["C123"] = b
	app.messageClients["slack"] = &fixtureChatTransport{}
	r := automation.Run{ID: automation.Hash("shared-query"), Spec: automation.Spec{ID: "shared-query", Kind: "assistant", Project: "default", Actor: "alice", AIConnection: "local", Model: "synthetic", Connections: []string{"db"}, ExternalProcessing: true, Instruction: "Show the data to this channel", Delivery: &automation.Delivery{Channel: "C123", User: "U123", Shared: true}}}
	if err := app.automationrun.RunAutomation(t.Context(), &r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(r.Receipts) != 0 || step != 2 {
		t.Fatal("unapproved shared data operation executed", r.Receipts, step)
	}
}
