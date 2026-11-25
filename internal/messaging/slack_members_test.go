package messaging

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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

func TestPairByEmailNeedsWorkspaceDomains(t *testing.T) {
	client := Client{Workspace: "T1", Members: &MemberAccess{Actor: "service-1", Binding: Binding{Project: "default", ExternalProcessing: true}}}
	for _, domains := range [][]string{{"example.com"}, {"example.com", "corp.example.org"}} {
		client.Members.PairByEmail = domains
		if err := client.Validate(); err != nil {
			t.Fatalf("%q: %v", domains, err)
		}
	}
	for _, domains := range [][]string{{"Example.com"}, {"example"}, {"@example.com"}, {""}, {"example.com/x"}} {
		client.Members.PairByEmail = domains
		if client.Validate() == nil {
			t.Fatalf("%q was accepted as a pairing domain", domains)
		}
	}
}
