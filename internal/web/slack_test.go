package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

func configureSlackFixture(t *testing.T, app *Server) {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "signing")
	bot := filepath.Join(dir, "bot")
	os.WriteFile(secret, []byte("fixture-signing"), 0600)
	os.WriteFile(bot, []byte("fixture-bot"), 0600)
	file := filepath.Join(dir, "config.json")
	b, _ := json.Marshal(SlackConfig{Team: "T123", SigningSecretFile: secret, BotTokenFile: bot, Users: map[string]string{"U123": "alice"}, Channels: map[string]SlackChannel{"C123": {Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true}}})
	os.WriteFile(file, b, 0600)
	if e := app.ConfigureSlack(file); e != nil {
		t.Fatal(e)
	}
}

func TestSlackThreadRetainsOnlyItsRequestersConversation(t *testing.T) {
	var prompts []string
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body["messages"])
		prompts = append(prompts, string(raw))
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"The answer is seven."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	doc, err := store.Save(t.Context(), "evidence.md", []byte("Seven records"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true}`)) }))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL
	for i, thread := range []string{"123.456", "123.456", "123.456", "987.654"} {
		instruction := "How many records?"
		if i > 0 {
			instruction = "Explain that answer."
		}
		run := automation.Run{ID: automation.Hash(i), Spec: automation.Spec{ID: "slack-" + strconv.Itoa(i), Kind: "assistant", Actor: "alice", Project: "default", Instruction: instruction, AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", Thread: thread, SlackUser: "U123"}}}
		if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			conversations, err := app.chat.Store.List("alice", "default")
			if err != nil {
				t.Fatal(err)
			}
			for _, conversation := range conversations {
				if conversation.Title == "Slack conversation" {
					if err = app.chat.Store.Update("alice", "default", conversation.ID, func(c *chat.Conversation) error {
						c.Turns[1].Citations = []chat.Citation{{Kind: "notebook", Project: "default", Path: doc.Path, Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content), URL: "/evidence"}}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			app.chat.Store.Close()
			reopened, err := chat.OpenStore(app.db)
			if err != nil {
				t.Fatal(err)
			}
			app.chat.Store = reopened
		}
		if i == 2 && len(run.Citations) != 1 {
			t.Fatalf("follow-up history multiplied source dependencies: %d", len(run.Citations))
		}
	}
	if len(prompts) != 4 || !strings.Contains(prompts[1], "How many records?") || !strings.Contains(prompts[1], "The answer is seven.") || strings.Contains(prompts[3], "How many records?") {
		t.Fatalf("missing or leaked Slack thread history: %#v", prompts)
	}
}

func TestSlackSignedEventsDeduplicateAndBindIdentity(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	body := `{"type":"event_callback","team_id":"T123","event_id":"Ev1","event":{"type":"app_mention","channel":"C123","user":"U123","text":"What changed?","ts":"123.456"}}`
	request := func(body, key string, when int64) *httptest.ResponseRecorder {
		stamp := strconv.FormatInt(when, 10)
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte("v0:" + stamp + ":" + body))
		r := httptest.NewRequest("POST", "http://localhost/integrations/slack/events", strings.NewReader(body))
		r.Header.Set("X-Slack-Request-Timestamp", stamp)
		r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 2; i++ {
		if w := request(body, "fixture-signing", time.Now().Unix()); w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	runs, e := app.automations.Queue.List()
	if e != nil || len(runs) != 1 || runs[0].Spec.Actor != "alice" || runs[0].Spec.Delivery.Channel != "C123" {
		t.Fatal(runs, e)
	}
	if w := request(body, "wrong", time.Now().Unix()); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(body, "fixture-signing", time.Now().Add(-10*time.Minute).Unix()); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(strings.Replace(body, "U123", "U999", 1), "fixture-signing", time.Now().Unix()); w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestSlackDirectMessagesUseMappedPrivateContext(t *testing.T) {
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Private DM answer"},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	app.slack.config.DirectMessages = map[string]SlackChannel{"U123": app.slack.config.Channels["C123"]}
	posts := 0
	var progress []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		progress = append(progress, r.URL.Path)
		if r.URL.Path == "/reactions.add" || r.URL.Path == "/reactions.remove" {
			if body["channel"] != "D123" || body["timestamp"] != "123.456" || body["name"] != "eyes" {
				t.Error("progress escaped its private message", body)
			}
			w.Write([]byte(`{"ok":true}`))
			return
		}
		posts++
		if r.URL.Path != "/chat.postMessage" || body["channel"] != "D123" || body["thread_ts"] != nil || body["user"] != nil {
			t.Error("DM escaped its private channel", body)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL + "/chat.postEphemeral"
	body := `{"type":"event_callback","team_id":"T123","event_id":"EvDM","event":{"type":"message","channel_type":"im","channel":"D123","user":"U123","text":"Private question","ts":"123.456"}}`
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("fixture-signing"))
	mac.Write([]byte("v0:" + stamp + ":" + body))
	r := httptest.NewRequest("POST", "http://localhost/integrations/slack/events", strings.NewReader(body))
	r.Header.Set("X-Slack-Request-Timestamp", stamp)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 1 || !runs[0].Spec.Delivery.Direct {
		t.Fatal("DM was not queued", runs, err)
	}
	if _, err = app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil || posts != 1 {
		t.Fatal("DM was not delivered", err, posts)
	}
	if strings.Join(progress, ",") != "/reactions.add,/chat.postMessage,/reactions.remove" {
		t.Fatal("progress must remain until the answer is delivered, then clear", progress)
	}
}

// Recording a direct exchange for the personal assistant's memory is best
// effort, as in notebook chat: a DM bound to an ordinary notebook cannot read
// the assistant notebook, and the answer must still arrive.
func TestSlackDirectAnswerArrivesWhenMemoryCannotRecordIt(t *testing.T) {
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Private DM answer"},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	app.slack.config.DirectMessages = map[string]SlackChannel{"U123": app.slack.config.Channels["C123"]}
	if _, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, MaintainMemory: true, TimeZone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	posts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			posts++
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL + "/chat.postEphemeral"
	body := `{"type":"event_callback","team_id":"T123","event_id":"EvMemoryDM","event":{"type":"message","channel_type":"im","channel":"D123","user":"U123","text":"Private question","ts":"123.456"}}`
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("fixture-signing"))
	mac.Write([]byte("v0:" + stamp + ":" + body))
	r := httptest.NewRequest("POST", "http://localhost/integrations/slack/events", strings.NewReader(body))
	r.Header.Set("X-Slack-Request-Timestamp", stamp)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if _, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil || posts != 1 {
		t.Fatal("the DM answer was lost to memory recording", err, posts)
	}
}

func TestSlackPrivateDeliveryReplayAndRevocation(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	posts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["user"] != "U123" || body["channel"] != "C123" || body["thread_ts"] != "123.456" || body["text"] != "Private answer" || r.Header.Get("Authorization") != "Bearer fixture-bot" {
			t.Error("invalid private delivery", body)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL
	run := automation.Run{Spec: automation.Spec{ID: "slack-fixture", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", SlackUser: "U123", Thread: "123.456"}}, Answer: "Private answer"}
	var checkpoints []string
	save := func() error { checkpoints = append(checkpoints, run.DeliveryState); return nil }
	if err := app.runAutomation(t.Context(), &run, save); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || run.DeliveryState != "sent" || len(checkpoints) != 2 || checkpoints[0] != "sending" || checkpoints[1] != "sent" {
		t.Fatal(posts, checkpoints)
	}
	if err := app.runAutomation(t.Context(), &run, save); err != nil || posts != 1 {
		t.Fatal("resent acknowledged delivery", err, posts)
	}
	run.DeliveryState = "sending"
	if err := app.runAutomation(t.Context(), &run, save); err == nil || posts != 1 {
		t.Fatal("resent ambiguous delivery", err, posts)
	}
	run.DeliveryState = ""
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := app.runAutomation(t.Context(), &run, save); err == nil || posts != 1 {
		t.Fatal("delivered after account revocation", err, posts)
	}
}
