package githubapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSourceAppCannotIssueBackupCredentials(t *testing.T) {
	g := &Client{contents: "read"}
	if _, err := g.BackupToken(t.Context(), 7, Repository{ID: 42}); err != ErrDenied {
		t.Fatal("source credential crossed write boundary", err)
	}
	if _, err := g.BackupDestination(t.Context(), "user", 7, 42, false); err != ErrDenied {
		t.Fatal(err)
	}
	if _, err := g.CreatePrivate(t.Context(), "user", "owner", "backup", strings.Repeat("x", 32)); err != ErrDenied {
		t.Fatal(err)
	}
}

func TestPrivateCreationUsesUserTokenAndReconcilesUncertainResponse(t *testing.T) {
	marker := strings.Repeat("m", 40)
	created := false
	creates := 0
	repo := Repository{ID: 42, FullName: "alice/notes", Private: true, Description: marker}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-token" {
			t.Error("creation did not use user authorization")
		}
		switch r.URL.Path {
		case "/repos/alice/notes":
			if !created {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(repo)
		case "/user":
			json.NewEncoder(w).Encode(map[string]string{"login": "alice"})
		case "/user/repos":
			creates++
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			if in["private"] != true || in["auto_init"] != false || in["description"] != marker {
				t.Error("unsafe repository initialization")
			}
			created = true
			http.Error(w, "uncertain response", 502)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	g := &Client{contents: "write", API: server.URL, HTTP: server.Client()}
	if _, err := g.CreatePrivate(t.Context(), "user-token", "alice", "notes", marker); err == nil {
		t.Fatal("unexpected success")
	}
	result, err := g.CreatePrivate(t.Context(), "user-token", "alice", "notes", marker)
	if err != nil || result.ID != 42 || creates != 1 {
		t.Fatal("creation retry did not reconcile", result, err, creates)
	}
	if _, err = g.CreatePrivate(t.Context(), "user-token", "alice", "notes", strings.Repeat("other", 10)); err == nil {
		t.Fatal("unrelated same-named repository adopted")
	}
	repo.Private = false
	if _, err = g.CreatePrivate(t.Context(), "user-token", "alice", "notes", marker); err == nil {
		t.Fatal("public destination adopted")
	}
}
