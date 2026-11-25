package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/attribution"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestNotebookPeoplePresentationPreservesOriginalAttribution(t *testing.T) {
	app, _ := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	for _, person := range []catalog.User{{ID: "alice", DisplayName: "Alice Example"}, {ID: "bob", DisplayName: "Bob Example"}} {
		if err := app.catalog.SetUser(catalog.Bootstrap, person); err != nil {
			t.Fatal(err)
		}
		if err := app.catalog.Member(catalog.Bootstrap, "default", person.ID, "comment"); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/note", `{"path":"People.md","content":"# People\n","revision":""}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = proxyCall(app, "bob", "GET", "/p/default/api/comments?path=People.md", "")
	var discussion workspace.Discussion
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &discussion) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	first, _ := json.Marshal(map[string]string{"path": "People.md", "text": "First comment", "revision": discussion.Revision})
	w = proxyCall(app, "alice", "POST", "/p/default/api/comments", string(first))
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &discussion) != nil || len(discussion.Comments) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	input, _ := json.Marshal(map[string]string{"path": "People.md", "text": "Reply", "replyTo": discussion.Comments[0].ID, "revision": discussion.Revision})
	w = proxyCall(app, "bob", "POST", "/p/default/api/comments", string(input))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = proxyCall(app, "bob", "GET", "/p/default/api/comments?path=People.md", "")
	var output struct {
		workspace.Discussion
		People map[string]string `json:"people"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &output) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if output.People["alice"] != "Alice Example" || output.People["bob"] != "Bob Example" {
		t.Fatalf("missing participant names: %s", w.Body.String())
	}
	if output.Comments[0].Author != "alice" || output.Comments[0].Replies[0].Author != "bob" {
		t.Fatal("authorship identity changed", output)
	}
	w = proxyCall(app, "bob", "GET", "/p/default/api/history?path=People.md", "")
	var history []map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &history) != nil || len(history) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	account, err := app.catalog.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	if history[0]["authorDisplayName"] != "Alice Example" || history[0]["userId"] != account.ID || history[0]["authorEmail"] != account.Identity().Email {
		t.Fatal("history presentation lost original attribution", history)
	}
	denied := proxyCall(app, "unrelated", "GET", "/p/default/api/comments?path=People.md", "")
	if denied.Code != 403 {
		t.Fatal("participant metadata escaped notebook permission", denied.Code)
	}
}

func TestExternalGitNamesRemainReadableWithoutInventingIdentity(t *testing.T) {
	app, store := fixture(t)
	ctx := attribution.With(context.Background(), attribution.Identity{Name: "User Friendly", Email: "public@example.test"})
	if _, err := store.Save(ctx, "External.md", []byte("# External\n"), "", "external"); err != nil {
		t.Fatal(err)
	}
	w := call(app, "GET", "/p/default/api/history?path=External.md", "", testToken)
	var rows []map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if rows[0]["authorDisplayName"] != "User Friendly" || rows[0]["authorName"] != "User Friendly" || rows[0]["userId"] != nil {
		t.Fatal("external author presentation changed or invented identity", rows)
	}
}
