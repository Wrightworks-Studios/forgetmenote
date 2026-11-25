package web

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
)

func TestNoteReadExposesDisplayTitleWithoutRenaming(t *testing.T) {
	app, store := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	if err := app.catalog.Member("@local-admin", "default", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, content, title string }{
		{"filename.md", "---\ntitle: Autumn launch plan\n---\n# Launch plan\nBody.", "Autumn launch plan"},
		{"quicknote-012345.md", "# **Recognizable** capture\nBody.", "Recognizable capture"},
		{"plain.md", "A **plain** capture with a [schedule](https://example.invalid).", "A plain capture with a schedule."},
	} {
		saved, err := store.Save(context.Background(), c.path, []byte(c.content), "", "")
		if err != nil {
			t.Fatal(err)
		}
		response := proxyCall(app, "reader", "GET", "/p/default/api/note?path="+url.QueryEscape(c.path), "")
		var doc struct{ Title, Path, Content, Revision string }
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &doc) != nil {
			t.Fatalf("read: %d %s", response.Code, response.Body)
		}
		if doc.Title != c.title || doc.Path != c.path || doc.Content != c.content || doc.Revision != saved.Revision {
			t.Fatalf("inconsistent display title or changed content: %+v", doc)
		}
	}
}
