package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
)

func TestAuthenticatedGitAuthors(t *testing.T) {
	app, store := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	for _, user := range []string{"alice@example.com", "bob"} {
		if err := app.catalog.Member("@local-admin", "default", user, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	save := func(user, path, content, revision string) []string {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"path": path, "content": content, "revision": revision})
		w := proxyCall(app, user, "POST", "/p/default/api/note", string(body))
		if w.Code != 200 {
			t.Fatalf("save %d %s", w.Code, w.Body)
		}
		return gitIdentity(t, app, store)
	}
	alice := save("alice@example.com", "a.md", "A", "")
	if alice[0] == "ForgetMeNote" || !strings.HasSuffix(alice[1], "@users.gitnotes.invalid") {
		t.Fatalf("not a local user author: %v", alice)
	}
	if alice[2] != "ForgetMeNote" || alice[3] != "forgetmenote@localhost" {
		t.Fatal("wrong committer", alice)
	}
	if strings.Contains(strings.Join(alice, "\n"), "alice@example.com") || !strings.Contains(alice[4], "Gitnotes-Source: browser") {
		t.Fatal("missing provenance or leaked login", alice)
	}
	bob := save("bob", "b.md", "B", "")
	if bob[1] == alice[1] {
		t.Fatal("users share Git identity")
	}
	a, _ := store.Read(context.Background(), "a.md", "")
	again := save("alice@example.com", "a.md", "A again", a.Revision)
	if again[0] != alice[0] || again[1] != alice[1] {
		t.Fatal("unstable identity")
	}
	w := proxyCall(app, "bob", "POST", "/p/default/api/note", `{"path":"forged.md","content":"No","author":"alice@example.com"}`)
	if w.Code == 200 {
		t.Fatal("accepted caller-supplied author")
	}
	w = proxyCall(app, "bob", "GET", "/p/default/api/history?path=a.md", "")
	var history []map[string]any
	if json.Unmarshal(w.Body.Bytes(), &history) != nil || len(history) != 2 || history[0]["authorEmail"] != alice[1] || history[0]["source"] != "browser" {
		t.Fatalf("history missing authorship: %s", w.Body)
	}
}

func TestMCPAttributionUsesTokenOwnerAndRegisteredClient(t *testing.T) {
	app, store := fixture(t)
	app.catalog.Member("@local-admin", "default", "alice", "owner")
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	app.oauth.Clients["registered-client"] = oauthClient{ID: "registered-client", Name: "Approved client"}
	app.oauth.Access[digest("alice-access")] = withOAuthFamily(accessGrant{Client: "registered-client", Scope: "notes:read notes:write", Resource: ts.URL + "/mcp", Subject: "alice", Expires: time.Now().Add(time.Hour).Unix()})
	client := mcp.NewClient(&mcp.Implementation{Name: "forged-name-not-an-identity", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: "alice-access"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "save_note", Arguments: map[string]any{"project": "default", "path": "mcp.md", "content": "From MCP", "revision": ""}})
	if err != nil || result.IsError {
		t.Fatalf("MCP save: %+v %v", result, err)
	}
	a, err := app.catalog.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	git := gitIdentity(t, app, store)
	if git[1] != a.Identity().Email || !strings.Contains(git[4], "Gitnotes-Source: mcp") || !strings.Contains(git[4], "Gitnotes-Client: registered-client") || strings.Contains(git[4], "forged-name") {
		t.Fatal("MCP attribution is not bound to token", git)
	}
	discussion, _ := store.Comments(context.Background(), "mcp.md")
	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "add_comment", Arguments: map[string]any{"project": "default", "path": "mcp.md", "text": "MCP comment", "revision": discussion.Revision}})
	if err != nil || result.IsError {
		t.Fatal("MCP comment failed", err, result)
	}
	if got := gitIdentity(t, app, store); got[1] != a.Identity().Email || !strings.Contains(got[4], "Gitnotes-Source: mcp") {
		t.Fatal("MCP comment lost attribution", got)
	}
}

type attributionTransport struct{ token string }

func (tr attributionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	req.Header.Set("Authorization", "Bearer "+tr.token)
	req.Header.Set("Remote-User", "imposter")
	req.Header.Set("User-Agent", "forged-client")
	return http.DefaultTransport.RoundTrip(req)
}

func TestAttributionAcrossMutationPathsAndLegacyHistory(t *testing.T) {
	app, store := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	app.catalog.Member("@local-admin", "default", "owner", "owner")
	legacy, _ := store.Save(context.Background(), "a.md", []byte("Before authorship"), "", "legacy-user")
	a, err := app.catalog.Account("owner")
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(route string, body map[string]any) {
		t.Helper()
		state, _ := store.Status(context.Background())
		body["revision"] = state.Revision
		raw, _ := json.Marshal(body)
		w := proxyCall(app, "owner", "POST", "/p/default/api/"+route, string(raw))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", route, w.Code, w.Body)
		}
		if got := gitIdentity(t, app, store); got[1] != a.Identity().Email || got[2] != "ForgetMeNote" || !strings.Contains(got[4], "Gitnotes-Source: browser") {
			t.Fatalf("%s lost attribution: %v", route, got)
		}
	}
	mutate("note", map[string]any{"path": "a.md", "content": "New version"})
	mutate("metadata", map[string]any{"path": "a.md", "metadata": map[string]any{"title": "Title", "kind": "note", "tags": []string{}}})
	mutate("comments", map[string]any{"path": "a.md", "text": "Review"})
	d, _ := store.Comments(context.Background(), "a.md")
	mutate("comments/resolve", map[string]any{"path": "a.md", "id": d.Comments[0].ID})
	mutate("locks", map[string]any{"path": "a.md", "locked": true})
	mutate("locks", map[string]any{"path": "a.md", "locked": false})
	mutate("move", map[string]any{"from": "a.md", "to": "folder/renamed.md"})
	mutate("duplicate", map[string]any{"from": "folder/renamed.md", "to": "copy.md"})
	// The browser restores by reading a historical version and saving that content.
	old, _ := store.Read(context.Background(), "a.md", legacy.Revision)
	mutate("note", map[string]any{"path": "folder/renamed.md", "content": string(old.Content)})
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, _ := writer.CreateFormFile("file", "test.png")
	part.Write([]byte("synthetic-image"))
	writer.Close()
	req := httptest.NewRequest("POST", app.origin+"/p/default/api/upload", &upload)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Origin", app.origin)
	req.Header.Set("Remote-User", "owner")
	req.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)
	if w.Code != 200 || gitIdentity(t, app, store)[1] != a.Identity().Email {
		t.Fatalf("attachment attribution: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"", "folder", "folder/renamed.md"} {
		history, err := store.History(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, commit := range history {
			if commit.Revision == legacy.Revision {
				found = true
				if commit.AuthorName != "ForgetMeNote" || commit.Source != "system" {
					t.Fatal("old author changed", commit)
				}
			}
		}
		if path != "folder" && !found {
			t.Fatalf("legacy commit lost in %q history", path)
		}
	}
	// Preserve genuinely old commits without provenance trailers, including
	// names in messages: Git history from before attribution, which reaches
	// a notebook by import.
	repository := filepath.Join(t.TempDir(), "legacy")
	git(t, filepath.Dir(repository), "init", "-q", "-b", "main", repository)
	if err := os.WriteFile(filepath.Join(repository, "legacy.md"), []byte("Before trailers"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, repository, "add", "legacy.md")
	git(t, repository, "-c", "user.name=ForgetMeNote", "-c", "user.email=forgetmenote@localhost", "commit", "-q", "-m", "Update legacy.md (historical-user)")
	bundle := filepath.Join(t.TempDir(), "legacy.bundle")
	git(t, repository, "bundle", "create", bundle, "--all")
	raw, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if w := importRequest(t, app, "legacy", raw, catalog.Bootstrap, app.origin); w.Code != 200 {
		t.Fatalf("import legacy history: %d %s", w.Code, w.Body)
	}
	imported, err := app.catalog.Access(catalog.Bootstrap, "legacy", "read")
	if err != nil {
		t.Fatal(err)
	}
	history, err := imported.History(context.Background(), "")
	if err != nil || history[0].Source != "" || history[0].UserID != "" || history[0].AuthorName != "ForgetMeNote" || history[0].Message != "Update legacy.md (historical-user)" {
		t.Fatal("legacy metadata invented", history, err)
	}
}

func TestConcurrentUsersDoNotShareProcessGitIdentity(t *testing.T) {
	app, store := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	t.Setenv("GIT_AUTHOR_NAME", "forged environment")
	t.Setenv("GIT_AUTHOR_EMAIL", "forged@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "forged committer")
	identities := map[string]string{}
	for _, user := range []string{"alice", "bob"} {
		app.catalog.Member("@local-admin", "default", user, "edit")
		a, err := app.catalog.Account(user)
		if err != nil {
			t.Fatal(err)
		}
		identities[user] = a.Identity().Email
	}
	type result struct {
		user, revision string
		code           int
	}
	results := make(chan result, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			user := []string{"alice", "bob"}[i%2]
			raw, _ := json.Marshal(map[string]string{"path": fmt.Sprintf("parallel-%d.md", i), "content": user})
			w := proxyCall(app, user, "POST", "/p/default/api/note", string(raw))
			var note noteOutput
			json.Unmarshal(w.Body.Bytes(), &note)
			results <- result{user, note.Revision, w.Code}
		}(i)
	}
	for i := 0; i < 8; i++ {
		r := <-results
		if r.code != 200 {
			t.Fatalf("concurrent save: %d", r.code)
		}
		commit := commitOf(t, app, store, r.revision).Commit
		if commit.Author.Email != identities[r.user] || commit.Committer.Name != "ForgetMeNote" || commit.Committer.Email != "forgetmenote@localhost" {
			t.Fatalf("cross-user identity: %+v %+v", commit.Author, commit.Committer)
		}
	}
}
