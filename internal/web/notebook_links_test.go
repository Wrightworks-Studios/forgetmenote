package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"net/http"
	"strings"
	"testing"
)

func TestNotebookLinkResolvesIdentityAfterRename(t *testing.T) {
	app, _ := fixture(t)
	if err := app.catalog.Create(catalog.Bootstrap, "second", "Second notebook"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access(catalog.Bootstrap, "second", "edit")
	if err != nil {
		t.Fatal(err)
	}
	id, err := workspace.NewNoteID()
	if err != nil {
		t.Fatal(err)
	}
	content, err := workspace.WithNoteID([]byte("# Target\n"), id)
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.Save(context.Background(), "same.md", content, "", "author")
	if err != nil {
		t.Fatal(err)
	}
	w := call(app, "POST", "/api/notebook-links/resolve", `{"project":"second","path":"same.md"}`, testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
	}
	var target struct {
		NoteID   string `json:"noteId"`
		Path     string `json:"path"`
		Revision string `json:"revision"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &target); err != nil || target.NoteID == "" {
		t.Fatalf("target: %s %v", w.Body.String(), err)
	}
	if _, err = store.Move(context.Background(), "same.md", "renamed.md", d.Revision, "author"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"project": "second", "path": "same.md", "noteId": target.NoteID})
	w = call(app, "POST", "/api/notebook-links/resolve", string(raw), testToken)
	if w.Code != 200 {
		t.Fatalf("rename resolution: %d %s", w.Code, w.Body.String())
	}
	if err = json.Unmarshal(w.Body.Bytes(), &target); err != nil || target.Path != "renamed.md" {
		t.Fatalf("renamed: %s", w.Body.String())
	}
}

func TestNotebookLinksNeverEnrichInaccessibleTargetsOrBacklinks(t *testing.T) {
	app, public := fixture(t)
	ctx := context.Background()
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Create(catalog.Bootstrap, "secret", "Unshared notebook name"); err != nil {
		t.Fatal(err)
	}
	private, err := app.catalog.Access(catalog.Bootstrap, "secret", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = private.Save(ctx, "secret-name.md", []byte("# Confidential title\n[Public](/?project=default&note=same.md)"), "", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = public.Save(ctx, "same.md", []byte("[User-written label](/?project=secret&note=secret-name.md)"), "", "owner"); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		w := proxyCall(app, "reader", "GET", "/p/default/api/knowledge/evidence?path=same.md", "")
		if w.Code != 200 {
			t.Fatalf("evidence: %s", w.Body.String())
		}
		if strings.Contains(w.Body.String(), "Confidential title") || strings.Contains(w.Body.String(), "Unshared notebook name") {
			t.Fatalf("private metadata disclosed: %s", w.Body.String())
		}
		var got struct {
			Backlinks []any `json:"backlinks"`
		}
		if json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Backlinks) != 0 {
			t.Fatalf("inaccessible backlinks: %s", w.Body.String())
		}
	}
	check()
	if err = app.catalog.Member(catalog.Bootstrap, "secret", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "reader", "POST", "/p/default/api/notebook-links/resolve", `{"project":"secret","path":"secret-name.md"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Confidential title") {
		t.Fatalf("permitted target: %s", w.Body.String())
	}
	w = proxyCall(app, "reader", "GET", "/p/default/api/knowledge/evidence?path=same.md", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Unshared notebook name") {
		t.Fatalf("authorized backlink not indexed: %s", w.Body.String())
	}
	if err = app.catalog.Member(catalog.Bootstrap, "secret", "reader", ""); err != nil {
		t.Fatal(err)
	}
	check()
	w = proxyCall(app, "reader", "POST", "/p/default/api/notebook-links/resolve", `{"project":"secret","path":"secret-name.md"}`)
	if w.Code != 403 {
		t.Fatalf("revoked target: %d", w.Code)
	}
}
