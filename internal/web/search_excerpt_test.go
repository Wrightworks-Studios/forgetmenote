package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"forgetmenote/internal/workspace"
)

func TestSearchDisplayExcerptsPreserveEvidence(t *testing.T) {
	app, store := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	if err := app.catalog.Member("@local-admin", "default", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	content := "---\ntitle: Autumn launch plan\nstatus: approved\n---\n# Launch plan\n\nRead the [release guide](https://example.invalid/guide) for **Autumn** dates.\n"
	doc, err := store.Save(context.Background(), "launch.md", []byte(content), "", "")
	if err != nil {
		t.Fatal(err)
	}
	request := func(scope string) []struct{ Path, Text, Excerpt, Hash, NoteHash, Revision string } {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"text": "Autumn", "mode": "text", "scope": scope})
		response := proxyCall(app, "reader", "POST", "/p/default/api/knowledge/hybrid-search", string(body))
		if response.Code != http.StatusOK {
			t.Fatalf("search %s: %d %s", scope, response.Code, response.Body)
		}
		var result struct {
			Matches []struct{ Path, Text, Excerpt, Hash, NoteHash, Revision string }
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Matches
	}
	for _, scope := range []string{"current", "history"} {
		hits := request(scope)
		found := false
		for _, hit := range hits {
			if hit.Path != doc.Path {
				continue
			}
			found = true
			if strings.Contains(hit.Excerpt, "title:") || strings.Contains(hit.Excerpt, "](") || strings.Contains(hit.Excerpt, "**") || !strings.Contains(hit.Excerpt, "Autumn dates") || !strings.Contains(hit.Excerpt, "release guide") {
				t.Fatalf("%s unreadable excerpt: %+v", scope, hit)
			}
			if scope == "current" && hit.Text != "title: Autumn launch plan" {
				t.Fatalf("changed raw evidence: %+v", hit)
			}
			if scope == "history" && hit.Hash != "" && workspace.ContentHash([]byte(hit.Text)) != hit.Hash {
				t.Fatalf("changed pinned passage evidence: %+v", hit)
			}
		}
		if !found {
			t.Fatalf("%s omitted matching note", scope)
		}
	}
	read, err := store.Read(context.Background(), doc.Path, doc.Revision)
	if err != nil || string(read.Content) != content {
		t.Fatalf("source changed: %v", err)
	}
}

func TestSearchDisplayExcerptsKeepLateMatchesAcrossUnicodeWidths(t *testing.T) {
	for _, c := range []struct{ name, padding string }{
		{"single long paragraph", strings.Repeat("a", 100000)},
		{"lowercase contracts bytes", strings.Repeat("K", 30000)},
		{"lowercase expands bytes", strings.Repeat("Ⱥ", 40000)},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, store := fixture(t)
			app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
			if err := app.catalog.Member("@local-admin", "default", "reader", "read"); err != nil {
				t.Fatal(err)
			}
			content := "# Preview boundary\n\n" + c.padding + " needle: [match](https://example.invalid) remains **readable**.\n"
			doc, err := store.Save(context.Background(), "boundary.md", []byte(content), "", "")
			if err != nil {
				t.Fatal(err)
			}
			for _, scope := range []string{"current", "history"} {
				t.Run(scope, func(t *testing.T) {
					body, _ := json.Marshal(map[string]string{"text": "needle", "mode": "text", "scope": scope})
					response := proxyCall(app, "reader", "POST", "/p/default/api/knowledge/hybrid-search", string(body))
					if response.Code != http.StatusOK {
						t.Fatalf("search: %d %s", response.Code, response.Body)
					}
					var result struct {
						Matches []struct{ Path, Text, Excerpt, Hash, NoteHash, Revision, Citation string }
					}
					if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if len(result.Matches) != 1 || result.Matches[0].Path != doc.Path {
						t.Fatalf("expected matching note, got %+v", result.Matches)
					}
					hit := result.Matches[0]
					if !utf8.ValidString(hit.Excerpt) || strings.ContainsRune(hit.Excerpt, '�') || len([]rune(hit.Excerpt)) > 242 || !strings.Contains(hit.Excerpt, "needle: match remains readable.") || strings.Contains(hit.Excerpt, "](") || strings.Contains(hit.Excerpt, "**") {
						t.Fatalf("invalid bounded late-match preview: %q", hit.Excerpt)
					}
					if scope == "current" && hit.Text != string([]rune(c.padding)[:240])+"…" {
						t.Fatalf("raw search evidence changed: %q", hit.Text)
					}
					if scope == "history" {
						citation, err := url.Parse(hit.Citation)
						if err != nil || citation.Path != "/"+doc.Path || citation.Query().Get("fmn-rev") != doc.Revision || hit.Revision != doc.Revision || hit.NoteHash != workspace.ContentHash([]byte(content)) || hit.Hash == "" || hit.Hash != workspace.ContentHash([]byte(hit.Text)) {
							t.Fatalf("changed pinned citation or evidence: %+v (%v)", hit, err)
						}
						if !strings.Contains(hit.Text, "needle: [match](https://example.invalid) remains **readable**.") {
							t.Fatalf("historical Markdown evidence changed: %q", hit.Text)
						}
					}
				})
			}
			for _, revision := range []string{"", doc.Revision} {
				read, err := store.Read(context.Background(), doc.Path, revision)
				if err != nil || string(read.Content) != content {
					t.Fatalf("source Markdown changed at %q: %v", revision, err)
				}
			}
		})
	}
}
