//go:build !fmn_omit_slack

package messaging

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestSlackMembersListsActiveHumansAcrossPages(t *testing.T) {
	member := func(id, team, email string, flags map[string]any) map[string]any {
		u := map[string]any{"id": id, "team_id": team, "profile": map[string]any{"email": email}}
		for k, v := range flags {
			u[k] = v
		}
		return u
	}
	pages := map[string][]map[string]any{
		"": {
			member("UALICE", "T1", "alice@example.com", map[string]any{"is_email_confirmed": true}),
			member("UGUEST", "T1", "guest@example.com", map[string]any{"is_restricted": true}),
			member("USINGLE", "T1", "single@example.com", map[string]any{"is_restricted": true, "is_ultra_restricted": true}),
			member("UBOT", "T1", "", map[string]any{"is_bot": true}),
			member("UAPP", "T1", "", map[string]any{"is_app_user": true}),
		},
		"page-2": {
			member("UGONE", "T1", "gone@example.com", map[string]any{"deleted": true}),
			member("UFOREIGN", "T2", "foreign@example.com", nil),
			member("USTRANGER", "T1", "stranger@example.com", map[string]any{"is_stranger": true}),
			member("USLACKBOT", "T1", "", nil),
			member("UPENDING", "T1", "pending@example.com", map[string]any{"is_email_confirmed": false}),
			member("UQUIET", "T1", "quiet@example.com", nil),
		},
	}
	cursors := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users.list" || r.Header.Get("Authorization") != "Bearer token" || r.URL.Query().Get("limit") != "200" {
			t.Errorf("unexpected request %s %v", r.URL, r.Header)
		}
		cursor := r.URL.Query().Get("cursor")
		cursors = append(cursors, cursor)
		next := ""
		if cursor == "" {
			next = "page-2"
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "members": pages[cursor], "response_metadata": map[string]any{"next_cursor": next}})
	}))
	defer server.Close()
	s := &Slack{Team: "T1", Endpoint: server.URL + "/chat.postEphemeral", Client: server.Client(), Token: func() (string, error) { return "token", nil }}
	got, err := s.Members(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []Member{{User: "UALICE", Email: "alice@example.com"}, {User: "UPENDING"}, {User: "UQUIET", Email: "quiet@example.com"}}
	if !slices.Equal(got, want) || !slices.Equal(cursors, []string{"", "page-2"}) {
		t.Fatalf("members %+v after cursors %q", got, cursors)
	}
}

// The directory lists unarchived channels and active human members, by ID and
// name only, across pages, in name order.
func TestSlackDirectoryListsChannelsAndPeopleByName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Header.Get("Authorization") != "Bearer token" || q.Get("limit") != "200" {
			t.Errorf("unexpected request %s", r.URL)
		}
		next := ""
		if q.Get("cursor") == "" {
			next = "page-2"
		}
		switch r.URL.Path {
		case "/conversations.list":
			if q.Get("types") != "public_channel,private_channel" || q.Get("exclude_archived") != "true" {
				t.Errorf("unexpected channel query %s", r.URL.RawQuery)
			}
			channels := map[string][]map[string]any{
				"":       {{"id": "CZED", "name": "zed", "is_private": false}, {"id": "COLD", "name": "old", "is_archived": true}},
				"page-2": {{"id": "GPLAN", "name": "planning", "is_private": true}, {"id": "bad id", "name": "broken"}},
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "channels": channels[q.Get("cursor")], "response_metadata": map[string]any{"next_cursor": next}})
		case "/users.list":
			members := map[string][]map[string]any{
				"":       {{"id": "UBEA", "team_id": "T1", "profile": map[string]any{"display_name": "bea", "email": "bea@example.com"}}, {"id": "UGUEST", "team_id": "T1", "is_restricted": true, "profile": map[string]any{"real_name": "Guest"}}},
				"page-2": {{"id": "UADA", "team_id": "T1", "profile": map[string]any{"real_name": "Ada Lovelace"}}, {"id": "UBOT", "team_id": "T1", "is_bot": true}},
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "members": members[q.Get("cursor")], "response_metadata": map[string]any{"next_cursor": next}})
		default:
			t.Errorf("unexpected method %s", r.URL.Path)
		}
	}))
	defer server.Close()
	s := &Slack{Team: "T1", Endpoint: server.URL + "/chat.postEphemeral", Client: server.Client(), Token: func() (string, error) { return "token", nil }}
	got := s.Directory(t.Context())
	raw, _ := json.Marshal(got)
	want := `{"channels":[{"id":"GPLAN","name":"planning"},{"id":"CZED","name":"zed"}],"users":[{"id":"UADA","name":"Ada Lovelace"},{"id":"UBEA","name":"bea"}]}`
	if string(raw) != want {
		t.Fatalf("directory %s, want %s", raw, want)
	}
}

// A list Slack refuses for a missing scope is left out with the scopes it
// needs, and one too long to page through is left out too; the other list
// still comes back.
func TestSlackDirectoryLeavesOutWhatItCannotList(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations.list":
			pages++
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "channels": []any{map[string]any{"id": "C1", "name": "general"}}, "response_metadata": map[string]any{"next_cursor": "more"}})
		case "/users.list":
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "missing_scope", "needed": "users:read"})
		}
	}))
	defer server.Close()
	s := &Slack{Team: "T1", Endpoint: server.URL + "/chat.postEphemeral", Client: server.Client(), Token: func() (string, error) { return "token", nil }}
	got := s.Directory(t.Context())
	if len(got.Channels) != 0 || !strings.Contains(got.Unlisted["channels"], "more than") || pages != directoryPages {
		t.Fatalf("an unbounded channel list after %d pages: %+v", pages, got)
	}
	if len(got.Users) != 0 || !strings.Contains(got.Unlisted["users"], "users:read") {
		t.Fatalf("users listed without users:read: %+v", got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "token") {
		t.Fatalf("the directory carries the token: %s", raw)
	}
}
