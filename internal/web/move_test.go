package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"testing"
)

func TestMoveHTTPRequiresOwner(t *testing.T) {
	app, store := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	for user, role := range map[string]string{"reader": "read", "editor": "edit", "owner": "owner"} {
		app.catalog.Member(catalog.Bootstrap, "default", user, role)
	}
	doc, err := store.Save(context.Background(), "notes/a.md", []byte("Original note"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"from": "notes/a.md", "to": "renamed.md", "revision": doc.Revision})
	for _, user := range []string{"reader", "editor", "stranger"} {
		w := proxyCall(app, user, "POST", "/p/default/api/move", string(body))
		if w.Code != 403 {
			t.Fatalf("%s move: %d %s", user, w.Code, w.Body)
		}
	}
	w := proxyCall(app, "owner", "POST", "/p/default/api/move", string(body))
	if w.Code != 200 {
		t.Fatalf("owner move: %d %s", w.Code, w.Body)
	}
}

func TestDuplicateHTTPRequiresOwner(t *testing.T) {
	app, store := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	for user, role := range map[string]string{"reader": "read", "editor": "edit", "owner": "owner"} {
		app.catalog.Member(catalog.Bootstrap, "default", user, role)
	}
	doc, err := store.Save(context.Background(), "a.md", []byte("Original"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"from": "a.md", "to": "copy.md", "revision": doc.Revision})
	for _, user := range []string{"reader", "editor", "stranger"} {
		w := proxyCall(app, user, "POST", "/p/default/api/duplicate", string(body))
		if w.Code != 403 {
			t.Fatalf("%s: %d", user, w.Code)
		}
	}
	w := proxyCall(app, "owner", "POST", "/p/default/api/duplicate", string(body))
	if w.Code != 200 {
		t.Fatalf("owner duplicate: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"a.md", "copy.md"} {
		doc, err := store.Read(context.Background(), path, "")
		if err != nil || string(doc.Content) != "Original" {
			t.Fatalf("%s: %+v %v", path, doc, err)
		}
	}
}
