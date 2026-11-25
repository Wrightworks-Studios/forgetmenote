package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRepositorySearchSeparatesDisplayExcerptFromPinnedEvidence(t *testing.T) {
	app, _ := fixture(t)
	markdown := "---\ntitle: Hidden metadata\n---\n# Readable\n\n| Field | Value |\n| --- | --- |\n| SnippetNeedle | Visible words |\n"
	code := "package example\n// SnippetNeedle keeps the literal pipe in code.\nfunc bits(a,b int) int { return a | b }\n"
	configureSearchSource(t, app, map[string]string{"readable.md": markdown, "bits.go": code})
	request := httptest.NewRequest("POST", app.origin+"/api/retrieval", strings.NewReader(`{"projects":["default"],"corpora":["repository"],"query":"SnippetNeedle","mode":"text"}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var result struct {
		Sources struct {
			Results []struct {
				Path, Text, Excerpt, Citation, Revision string
				StartByte, EndByte                      int
			}
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, hit := range result.Sources.Results {
		original := map[string]string{"readable.md": markdown, "bits.go": code}[hit.Path]
		if original == "" {
			continue
		}
		found[hit.Path] = true
		if hit.Text != original[hit.StartByte:hit.EndByte] || hit.Revision == "" || hit.Citation == "" {
			t.Fatal("display formatting changed pinned source evidence", hit.Path)
		}
		if hit.Excerpt == "" {
			t.Fatal("missing display excerpt", hit.Path)
		}
		if hit.Path == "readable.md" && (strings.Contains(hit.Excerpt, "|") || strings.Contains(hit.Excerpt, "Hidden metadata") || !strings.Contains(hit.Excerpt, "Visible words")) {
			t.Fatal("Markdown syntax leaked into excerpt", hit.Excerpt)
		}
		if hit.Path == "bits.go" && !strings.Contains(hit.Excerpt, "a | b") {
			t.Fatal("code operators were discarded", hit.Excerpt)
		}
	}
	if !found["readable.md"] || !found["bits.go"] {
		t.Fatal("expected both source types", found)
	}
}

func TestRepositorySearchTableContinuationExcerpt(t *testing.T) {
	app, _ := fixture(t)
	markdown := "# Large table\n\n| Field | Value |\n| --- | --- |\n" + strings.Repeat("| Earlier row | Filler text long enough to create several source chunks |\n", 90) + "| UniqueContinuationNeedle | Visible final row |\n"
	configureSearchSource(t, app, map[string]string{"large.md": markdown})
	request := httptest.NewRequest("POST", app.origin+"/api/retrieval", strings.NewReader(`{"projects":["default"],"corpora":["repository"],"query":"UniqueContinuationNeedle","mode":"text"}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var result struct {
		Sources struct {
			Results []struct {
				Path, Text, Excerpt string
				StartByte, EndByte  int
			}
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, hit := range result.Sources.Results {
		if hit.Path != "large.md" || !strings.Contains(hit.Text, "UniqueContinuationNeedle") {
			continue
		}
		if hit.StartByte == 0 {
			t.Fatal("invalid fixture: query did not select a continuation chunk")
		}
		if hit.Text != markdown[hit.StartByte:hit.EndByte] {
			t.Fatal("changed pinned evidence")
		}
		if strings.Contains(hit.Excerpt, "|") {
			t.Fatalf("table formatting remains in continuation display excerpt at byte %d: %q", hit.StartByte, hit.Excerpt)
		}
		if !strings.Contains(hit.Excerpt, "UniqueContinuationNeedle") {
			t.Fatalf("missing matching text: %q", hit.Excerpt)
		}
		return
	}
	t.Fatal("no matching continuation chunk", response.Body.String())
}

func TestScopedNoteSearchFormatsTableContinuationsWithoutChangingPassages(t *testing.T) {
	for _, rows := range []int{0, 90} {
		t.Run(fmt.Sprint(rows), func(t *testing.T) {
			app, _ := fixture(t)
			if err := app.catalog.Create(catalog.Bootstrap, "excerpt-book", "Excerpt book"); err != nil {
				t.Fatal(err)
			}
			content := "# Notebook table\n\n| Field | Value |\n| --- | --- |\n" + strings.Repeat("| Earlier row | Filler context for continuation chunks |\n", rows) + "| ScopedNoteNeedle | Visible final row |\n"
			body, _ := json.Marshal(map[string]string{"path": "Table.md", "content": content, "revision": ""})
			w := apiCall(app, "POST", "/p/excerpt-book/api/note", string(body), testToken)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			w = apiCall(app, "POST", "/api/retrieval", `{"projects":["default","excerpt-book"],"corpora":["notes"],"query":"ScopedNoteNeedle","mode":"text"}`, testToken)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			var result struct {
				Results []struct {
					Excerpt string
					Note    *knowledge.Passage
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			for _, hit := range result.Results {
				if hit.Note == nil || !strings.Contains(hit.Note.Text, "ScopedNoteNeedle") {
					continue
				}
				if !strings.Contains(content, hit.Note.Text) || workspace.ContentHash([]byte(hit.Note.Text)) != hit.Note.Hash || hit.Note.Citation == "" {
					t.Fatal("display changed pinned passage")
				}
				if strings.Contains(hit.Excerpt, "|") || !strings.Contains(hit.Excerpt, "ScopedNoteNeedle") {
					t.Fatal("invalid scoped display excerpt", hit.Excerpt)
				}
				return
			}
			t.Fatal("missing note result", w.Body)
		})
	}
}

func TestEmptyExcerptRemainsExplicit(t *testing.T) {
	app, _ := fixture(t)
	configureSearchSource(t, app, map[string]string{"metadata.md": "---\ntitle: UniqueMetadataNeedle\n---\n"})
	req := httptest.NewRequest("POST", app.origin+"/api/retrieval", strings.NewReader(`{"projects":["default"],"corpora":["repository"],"query":"UniqueMetadataNeedle","mode":"text"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var result struct {
		Results []struct{ Source map[string]json.RawMessage }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, item := range result.Results {
		var path string
		json.Unmarshal(item.Source["path"], &path)
		if path != "metadata.md" {
			continue
		}
		t.Log(string(item.Source["text"]))
		if _, ok := item.Source["excerpt"]; !ok {
			t.Fatal("intentional empty excerpt omitted: current browser falls back to original front matter")
		}
		return
	}
	t.Fatal("missing fixture result", rec.Body.String())
}
