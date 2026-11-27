package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Administration lists the mounted Slack app's channels and people by name and
// ID, for choosing them in the chat client settings. Only administrators can
// list them, the answer carries no token or address, and a list Slack refuses
// for a missing scope is left out with the scopes it needs, so the page takes
// typed IDs for it.
func TestChatClientDirectoryListsSlackChannelsAndPeople(t *testing.T) {
	app, _ := fixture(t)
	configureSlackFixture(t, app)
	admin := recoverySession(t, app)
	addLocal(t, app, admin, "directory-member")
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"directory-member"}, "password": {"test-password-long"}}.Encode())
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	member := login.Result().Cookies()[0]
	channelScope := true
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-bot" {
			t.Errorf("Slack was called without the bot token: %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/conversations.list":
			if !channelScope {
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "missing_scope", "needed": "channels:read", "provided": "users:read"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "channels": []any{map[string]any{"id": "C123", "name": "general"}}})
		case "/users.list":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "members": []any{map[string]any{"id": "U123", "team_id": "T123", "tz": "UTC", "profile": map[string]any{"real_name": "Ada Lovelace", "email": "ada@example.com"}}}})
		default:
			t.Errorf("unexpected Slack method %s", r.URL.Path)
		}
	}))
	t.Cleanup(slack.Close)
	app.slack.endpoint = slack.URL + "/chat.postEphemeral"
	list := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		return localRequest(app, "/api/organization/action/chat_client_directory", `{}`, cookie)
	}
	if w := list(member); w.Code != http.StatusForbidden {
		t.Fatalf("a member listed the Slack directory: %d %s", w.Code, w.Body)
	}
	w := list(admin)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"channels":[{"id":"C123","name":"general"}],"users":[{"id":"U123","name":"Ada Lovelace"}]}` {
		t.Fatalf("directory: %d %s", w.Code, w.Body)
	}
	channelScope = false
	w = list(admin)
	var refused struct {
		Channels []any            `json:"channels"`
		Users    []map[string]any `json:"users"`
		Unlisted map[string]string
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &refused) != nil || len(refused.Channels) != 0 || len(refused.Users) != 1 || !strings.Contains(refused.Unlisted["channels"], "channels:read") {
		t.Fatalf("directory without channels:read: %d %s", w.Code, w.Body)
	}
	if body := w.Body.String(); strings.Contains(body, "fixture-bot") || strings.Contains(body, "ada@example.com") {
		t.Fatalf("the directory carries the token or an address: %s", body)
	}

	bare, _ := fixture(t)
	if w = localRequest(bare, "/api/organization/action/chat_client_directory", `{}`, recoverySession(t, bare)); w.Code != 200 || !strings.Contains(w.Body.String(), `"unlisted":{"channels":"no Slack app is set up","users":"no Slack app is set up"}`) {
		t.Fatalf("directory without a Slack app: %d %s", w.Code, w.Body)
	}
}
