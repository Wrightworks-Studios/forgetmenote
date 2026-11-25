package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"strings"
	"testing"
)

func TestWritingOutlineAndTermsUseCommittedDocuments(t *testing.T) {
	app, store := fixture(t)
	doc, err := store.Save(t.Context(), "chapter.md", []byte("# Chapter one\n\nElizabeth sees Lizzy.\n\n## Later\n\nElizabeth returns.\n"), "", "tester")
	if err != nil {
		t.Fatal(err)
	}
	w := apiCall(app, "POST", "/api/v1/actions/document_outline", `{"project":"default","path":"chapter.md"}`, testToken)
	if w.Code != 200 {
		t.Fatalf("outline: %d %s", w.Code, w.Body)
	}
	var outline struct {
		Revision string `json:"revision"`
		Stats    struct {
			Words int `json:"words"`
		} `json:"stats"`
		Sections []struct {
			Title        string `json:"title"`
			Words        int    `json:"words"`
			SubtreeWords int    `json:"subtreeWords"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &outline); err != nil {
		t.Fatal(err)
	}
	if outline.Revision != doc.Revision || outline.Stats.Words != 8 || len(outline.Sections) != 2 || outline.Sections[0].Words != 5 || outline.Sections[0].SubtreeWords != 8 {
		t.Fatalf("outline: %+v", outline)
	}
	w = apiCall(app, "POST", "/api/v1/actions/find_term_mentions", `{"project":"default","terms":[{"name":"Elizabeth","aliases":["Lizzy"]}]}`, testToken)
	if w.Code != 200 {
		t.Fatalf("terms: %d %s", w.Code, w.Body)
	}
	var mentions struct {
		Revision string `json:"revision"`
		Matches  []struct {
			Text string `json:"text"`
			Path string `json:"path"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &mentions); err != nil {
		t.Fatal(err)
	}
	if mentions.Revision != doc.Revision || len(mentions.Matches) != 3 || mentions.Matches[0].Path != "chapter.md" {
		t.Fatalf("mentions: %+v", mentions)
	}
}

func TestWritingGlossarySaveNeverCheckpointsPeerDraft(t *testing.T) {
	app, store := fixture(t)
	doc, err := store.Save(t.Context(), "glossary.md", []byte("# Writing glossary\n\n- Lizzy\n"), "", "tester")
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveRead(t.Context(), doc.Path)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(live.Content, "Lizzy")
	_, err = store.LivePatch(t.Context(), workspace.TargetedPatch{Path: doc.Path, Base: live.LiveRevision, OperationID: "glossary-peer", Edits: []workspace.TextEdit{{Start: start, End: start + 5, Old: "Lizzy", Text: "Elizabeth"}}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"path": doc.Path, "revision": doc.Revision, "content": "# Writing glossary\n\n- Darcy\n", "requireCheckpoint": true})
	w := call(app, "POST", "/api/note", string(body), testToken)
	current, err := store.Read(t.Context(), doc.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != 409 || current.Revision != doc.Revision || string(current.Content) != string(doc.Content) {
		t.Fatalf("glossary save checkpointed or replaced a peer draft: %d %s; content %s", w.Code, w.Body, current.Content)
	}
}

func TestWritingPagingPermissionsAndCommittedScope(t *testing.T) {
	app, store := fixture(t)
	for i := 0; i < 11; i++ {
		status, _ := store.Status(t.Context())
		if _, err := store.Save(t.Context(), fmt.Sprintf("chapter-%02d.md", i), []byte("# Chapter\n\nElizabeth sees Lizzy and Elizabeth.\n"), status.Revision, "tester"); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	client := actionClient(t, app, "reader", "notes:read")
	args := map[string]any{"project": "default", "terms": []map[string]any{{"name": "Elizabeth", "aliases": []string{"Lizzy"}}}, "limit": 2}
	count, pages := 0, 0
	for {
		raw := actionCall(t, client, "find_term_mentions", args, false)
		var out writingTermsResult
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		count += len(out.Matches)
		pages++
		if pages > 30 {
			t.Fatal("pagination did not finish")
		}
		for _, match := range out.Matches {
			if match.Revision != out.Revision || match.Hash == "" || !strings.Contains(match.Citation, out.Revision) {
				t.Fatalf("unpinned citation %+v", match)
			}
		}
		if out.Complete {
			break
		}
		if out.Next == "" {
			t.Fatal("incomplete without continuation")
		}
		args["after"], args["revision"] = out.Next, out.Revision
	}
	if count != 33 {
		t.Fatalf("found %d matches over %d pages", count, pages)
	}
	args["terms"] = []map[string]any{{"name": "Different"}}
	actionCall(t, client, "find_term_mentions", args, true)
	outline := actionCall(t, client, "document_outline", map[string]any{"project": "default", "path": "chapter-00.md"}, false)
	if !strings.Contains(string(outline), `"words":6`) {
		t.Fatal(string(outline))
	}
	actionCall(t, client, "document_outline", map[string]any{"project": "default", "path": "../escape.md"}, true)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "reader", ""); err != nil {
		t.Fatal(err)
	}
	actionCall(t, client, "document_outline", map[string]any{"project": "default", "path": "chapter-00.md"}, true)
	for _, asset := range []string{"app.mjs", "dist/writing-worker.js"} {
		w := apiCall(app, "GET", "/static/"+asset, "", "")
		if w.Code != 200 {
			t.Fatalf("asset %s: %d", asset, w.Code)
		}
	}
}

func TestWritingRepositoryMarkdownAndOutlineContinuation(t *testing.T) {
	app, _ := fixture(t)
	configureSearchSource(t, app, map[string]string{"README.md": "# Guide\n\nElizabeth returns.\n\n## Second\n\nLizzy leaves.\n"})
	client := actionClient(t, app, catalog.Bootstrap, "notes:read")
	args := map[string]any{"project": "default", "origin": "repository", "path": "README.md", "limit": 1}
	raw := actionCall(t, client, "document_outline", args, false)
	var first writingOutlineResult
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	if first.Next != 1 || first.Total != 2 || first.Stats.Words != 6 || first.Source == "" {
		t.Fatalf("outline %+v", first)
	}
	args["after"] = first.Next
	actionCall(t, client, "document_outline", args, true)
	args["revision"], args["source"] = first.Revision, first.Source
	raw = actionCall(t, client, "document_outline", args, false)
	var last writingOutlineResult
	if err := json.Unmarshal(raw, &last); err != nil {
		t.Fatal(err)
	}
	if last.Next != 0 || len(last.Sections) != 1 || last.Sections[0].Title != "Second" {
		t.Fatalf("continuation %+v", last)
	}
	raw = actionCall(t, client, "find_term_mentions", map[string]any{"project": "default", "origin": "repository", "terms": []map[string]any{{"name": "Elizabeth", "aliases": []string{"Lizzy"}}}}, false)
	var terms writingTermsResult
	if err := json.Unmarshal(raw, &terms); err != nil {
		t.Fatal(err)
	}
	if len(terms.Matches) != 2 || terms.Matches[0].Path != "README.md" || !strings.Contains(terms.Matches[0].Citation, "#L3-L3") {
		t.Fatalf("repository mentions %+v", terms)
	}
}
