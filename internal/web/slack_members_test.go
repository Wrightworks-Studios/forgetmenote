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

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
)

func TestSlackWorkspaceMembersShareChannelRepliesWithoutPrivateHistory(t *testing.T) {
	var prompts []string
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		raw, _ := json.Marshal(input)
		prompts = append(prompts, string(raw))
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Answer."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	service, err := app.catalog.CreateService(catalog.Bootstrap, "Company assistant", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", service.ID, "read"); err != nil {
		t.Fatal(err)
	}
	policy := app.slack.policy()
	policy.Channels, policy.DirectMessages = nil, nil
	policy.Members = &messaging.MemberAccess{Actor: service.ID, Binding: messaging.Binding{Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true}}
	if _, err = app.catalog.SetChatClients(catalog.Bootstrap, catalog.ChatClients{Clients: map[string]messaging.Client{"slack": policy}}); err != nil {
		t.Fatal(err)
	}
	var deliveries []map[string]any
	var methods []string
	memberActive := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users.info":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"id": r.URL.Query().Get("user"), "team_id": "T123", "deleted": !memberActive, "is_stranger": r.URL.Query().Get("user") == "UGUEST"}})
		case "/conversations.replies":
			w.Write([]byte(`{"ok":true,"messages":[{"ts":"0.1","user":"UAUTHOR","text":"ORIGINAL_BUDGET_131578"},{"ts":"0.2","user":"UGUEST","text":"EXCLUDED_GUEST_CONTEXT"}],"has_more":false}`))
		case "/chat.postMessage", "/chat.postEphemeral":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			deliveries = append(deliveries, body)
			methods = append(methods, r.URL.Path)
			w.Write([]byte(`{"ok":true}`))
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL + "/chat.postEphemeral"
	invoke := func(id, user, channel, text string, direct bool) {
		t.Helper()
		d := messaging.Address{Client: "slack", Workspace: "T123", Channel: channel, Thread: "thread", Message: id, User: user, Direct: direct}
		if err := app.enqueueMessage(t.Context(), messaging.Event{ID: id, Text: text, Address: d}); err != nil {
			t.Fatal(err)
		}
		if ran, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil || !ran {
			t.Fatalf("run: %v %v", ran, err)
		}
	}
	invoke("private", "UNEW", "CANY", "PRIVATE_CHANNEL_HISTORY", false)
	if len(methods) != 1 || methods[0] != "/chat.postEphemeral" {
		runs, _ := app.automations.Queue.List()
		t.Fatalf("existing private policy failed: methods=%v runs=%+v", methods, runs)
	}
	config := app.catalog.ChatClients()
	client := config.Clients["slack"]
	client.Members.Binding.Shared = true
	client.Members.Binding.AudienceApproved = true
	client.Members.Binding.Actors = []string{service.ID}
	client.Members.Binding.ThreadHistory = true
	config.Clients["slack"] = client
	if _, err = app.catalog.SetChatClients(catalog.Bootstrap, config); err != nil {
		t.Fatal("workspace-wide public channel policy rejected:", err)
	}
	invoke("public", "UNEW", "CANY", "PUBLIC_CHANNEL_QUESTION", false)
	invoke("followup", "UOTHER", "CANY", "Explain that answer.", false)
	invoke("other-channel", "UOTHER", "COTHER", "OTHER_CHANNEL_QUESTION", false)
	invoke("dm", "UNEW", "DNEW", "PRIVATE_DM_QUESTION", true)
	invoke("other-dm", "UOTHER", "DOTHER", "SECOND_PRIVATE_DM", true)
	if len(deliveries) != 6 || len(prompts) != 6 {
		t.Fatal("missing replies", len(deliveries), len(prompts))
	}
	for i := 1; i < len(deliveries); i++ {
		if methods[i] != "/chat.postMessage" || deliveries[i]["user"] != nil || deliveries[i]["thread_ts"] != "thread" {
			t.Errorf("reply %d was not a normal thread message: %s %+v", i, methods[i], deliveries[i])
		}
		if strings.Contains(prompts[i], "PRIVATE_CHANNEL_HISTORY") {
			t.Errorf("reply %d reused formerly private history", i)
		}
	}
	if !strings.Contains(prompts[2], "PUBLIC_CHANNEL_QUESTION") {
		t.Error("a different workspace member could not follow the public thread")
	}
	for _, i := range []int{1, 2} {
		if !strings.Contains(prompts[i], "ORIGINAL_BUDGET_131578") || strings.Contains(prompts[i], "EXCLUDED_GUEST_CONTEXT") {
			t.Errorf("shared context %d lost a verified member's original request or included a guest", i)
		}
	}
	for _, i := range []int{3, 4, 5} {
		if strings.Contains(prompts[i], "PUBLIC_CHANNEL_QUESTION") {
			t.Errorf("public thread history crossed destinations in reply %d", i)
		}
	}
	if strings.Contains(prompts[5], "PRIVATE_DM_QUESTION") {
		t.Error("DM history crossed users")
	}
	memberActive = false
	invoke("inactive", "UNEW", "CANY", "INACTIVE_MEMBER", false)
	if len(prompts) != 6 || len(deliveries) != 6 {
		t.Fatal("inactive workspace member invoked AI or received a reply")
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", service.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = app.messageBinding(messaging.Address{Client: "slack", Workspace: "T123", Channel: "CANY", User: "UNEW", Shared: true})
	if err == nil {
		t.Fatal("public channel retained revoked service access")
	}
}

func TestSlackWorkspaceMembersUseScopedIdentityAndPrivateHistory(t *testing.T) {
	prompts := []string{}
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input any
		json.NewDecoder(r.Body).Decode(&input)
		raw, _ := json.Marshal(input)
		prompts = append(prompts, string(raw))
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Answer."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	service, err := app.catalog.CreateService(catalog.Bootstrap, "Workspace assistant", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", service.ID, "read"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(app.slack.config)
	var cfg map[string]any
	json.Unmarshal(raw, &cfg)
	cfg["members"] = map[string]any{"actor": service.ID, "binding": map[string]any{"project": "default", "aiConnection": "local", "model": "synthetic", "externalProcessing": true}}
	raw, _ = json.Marshal(cfg)
	file := filepath.Join(t.TempDir(), "slack.json")
	os.WriteFile(file, raw, 0600)
	if err = app.ConfigureSlack(file); err != nil {
		t.Fatal(err)
	}
	memberTeam, deleted, bot := "T123", false, false
	guest, singleChannelGuest := false, false
	sent, checked := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users.info":
			checked++
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"id": r.URL.Query().Get("user"), "team_id": memberTeam, "deleted": deleted, "is_bot": bot, "is_restricted": guest || singleChannelGuest, "is_ultra_restricted": singleChannelGuest}})
		case "/chat.postMessage", "/chat.postEphemeral":
			sent++
			w.Write([]byte(`{"ok":true}`))
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL
	for i, user := range []string{"UNEW", "UOTHER", "UNEW"} {
		text := "PRIVATE_FIRST_USER_QUESTION"
		if i == 1 {
			text = "SECOND_USER_QUESTION"
		}
		if i == 2 {
			text = "Continue."
		}
		body, _ := json.Marshal(map[string]any{"type": "event_callback", "team_id": "T123", "event_id": text, "event": map[string]string{"type": "message", "channel_type": "im", "channel": "D" + user, "user": user, "text": text, "ts": strconv.Itoa(i+1) + ".123"}})
		stamp := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte("fixture-signing"))
		mac.Write(append([]byte("v0:"+stamp+":"), body...))
		request := httptest.NewRequest("POST", "http://localhost/integrations/slack/events", strings.NewReader(string(body)))
		request.Header.Set("X-Slack-Request-Timestamp", stamp)
		request.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		response := httptest.NewRecorder()
		app.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("Slack event: HTTP %d: %s", response.Code, response.Body.String())
		}
		if _, err = app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil {
			t.Fatal(err)
		}
	}
	if sent != 3 || checked < 3 || len(prompts) != 3 {
		t.Fatalf("sent=%d memberChecks=%d models=%d", sent, checked, len(prompts))
	}
	if strings.Contains(prompts[1], "PRIVATE_FIRST_USER_QUESTION") || !strings.Contains(prompts[2], "PRIVATE_FIRST_USER_QUESTION") {
		t.Fatal("workspace members lost their own history or saw another user's conversation")
	}
	// Slack guests belong to the workspace team but are not members; only an
	// explicit account mapping admits one.
	for _, kind := range []string{"external", "deleted", "bot", "guest", "single-channel guest"} {
		memberTeam, deleted, bot = "T123", false, false
		guest, singleChannelGuest = false, false
		switch kind {
		case "external":
			memberTeam = "TOTHER"
		case "deleted":
			deleted = true
		case "bot":
			bot = true
		case "guest":
			guest = true
		case "single-channel guest":
			singleChannelGuest = true
		}
		event := messaging.Event{ID: kind, Text: "Do not run " + kind, Address: messaging.Address{Client: "slack", Workspace: "T123", Channel: "DNEW", User: "UNEW", Direct: true}}
		if err = app.enqueueMessage(t.Context(), event); err != nil {
			continue
		}
		ran, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation)
		if err != nil || !ran {
			t.Fatalf("membership check did not run: %v", err)
		}
		runs, err := app.automations.Queue.List()
		found := false
		for _, run := range runs {
			if run.Spec.Instruction == "Do not run "+kind && run.State == "failed" {
				found = true
			}
		}
		if err != nil || !found {
			t.Fatalf("missing rejected membership record: %v", err)
		}
		if len(prompts) != 3 || sent != 3 {
			t.Fatalf("%s member received data or invoked AI", kind)
		}
	}
	memberTeam, deleted, bot = "T123", false, false
	guest, singleChannelGuest = false, false
	event := messaging.Event{ID: "revoked", Text: "Permissions revoked after enqueue", Address: messaging.Address{Client: "slack", Workspace: "T123", Channel: "DNEW", User: "UNEW", Direct: true}}
	if err = app.enqueueMessage(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", service.ID, ""); err != nil {
		t.Fatal(err)
	}
	if ran, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil || !ran {
		t.Fatalf("revoked request did not run: %v", err)
	}
	if len(prompts) != 3 || sent != 3 {
		t.Fatal("revoked service invoked AI or delivered data")
	}
}
