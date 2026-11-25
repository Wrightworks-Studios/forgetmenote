package web

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
)

// Wiki navigation orders pages by their `order` front matter. Every reader of
// the notebook receives it with the ordinary note listing; editing properties
// keeps the field, and a value that is not a number is ignored, not an error.
func TestWikiPageOrderIsListedAndKept(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"wiki-reader": "read", "wiki-editor": "edit"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	save := func(path, content string) noteOutput {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"path": path, "content": content, "revision": ""})
		w := proxyCall(app, "wiki-editor", "POST", "/p/default/api/note", string(body))
		if w.Code != 200 {
			t.Fatalf("save %s: %d %s", path, w.Code, w.Body)
		}
		var out noteOutput
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	second := save("guide/second.md", "---\ntitle: Second steps\norder: 2\n---\nBody.\n")
	save("guide/unordered.md", "---\norder: soon\ntitle: Unordered\n---\nBody.\n")
	save("guide/first.md", "---\norder: -0.5\n---\n# First steps\n")

	listed := func() map[string]workspace.Entry {
		t.Helper()
		w := proxyCall(app, "wiki-reader", "GET", "/p/default/api/notes?collection=notes", "")
		if w.Code != 200 {
			t.Fatalf("list: %d %s", w.Code, w.Body)
		}
		var entries []workspace.Entry
		if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
			t.Fatal(err)
		}
		out := map[string]workspace.Entry{}
		for _, entry := range entries {
			out[entry.Path] = entry
		}
		return out
	}
	entries := listed()
	order := func(path string) any {
		entry := entries[path]
		if entry.Metadata == nil || entry.MetadataError != "" {
			t.Fatalf("%s metadata: %+v", path, entry)
		}
		if entry.Metadata.Order == nil {
			return nil
		}
		return *entry.Metadata.Order
	}
	if got := order("guide/second.md"); got != 2.0 {
		t.Fatalf("second order = %v", got)
	}
	if got := order("guide/first.md"); got != -0.5 {
		t.Fatalf("first order = %v", got)
	}
	if got := order("guide/unordered.md"); got != nil || entries["guide/unordered.md"].Metadata.Title != "Unordered" {
		t.Fatalf("unordered = %v %+v", got, entries["guide/unordered.md"].Metadata)
	}

	body, _ := json.Marshal(map[string]any{"path": second.Path, "revision": second.Revision, "metadata": map[string]any{"title": "Second steps, revised", "tags": []string{}}})
	if w := proxyCall(app, "wiki-editor", "POST", "/p/default/api/metadata", string(body)); w.Code != 200 {
		t.Fatalf("properties: %d %s", w.Code, w.Body)
	} else if !strings.Contains(w.Body.String(), `order: 2\n`) {
		t.Fatalf("properties dropped order: %s", w.Body)
	}
	entries = listed()
	if got := order("guide/second.md"); got != 2.0 || entries["guide/second.md"].Metadata.Title != "Second steps, revised" {
		t.Fatalf("after properties: %v %+v", got, entries["guide/second.md"].Metadata)
	}
}

// "Open as wiki" is a notebook setting only its owners change; every member
// sees it with the notebook so the browser can choose the default layout.
func TestOpenAsWikiSettingIsOwnerManagedAndVisibleToReaders(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"wiki-reader": "read", "wiki-editor": "edit", "wiki-owner": "owner"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.catalog.CreateKind(catalog.Bootstrap, "code", "Code", "repository"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "code", "wiki-owner", "owner"); err != nil {
		t.Fatal(err)
	}
	openAsWiki := func(user, project string) bool {
		t.Helper()
		w := proxyCall(app, user, "GET", "/api/projects", "")
		if w.Code != 200 {
			t.Fatalf("projects: %d %s", w.Code, w.Body)
		}
		var projects []catalog.Project
		if err := json.Unmarshal(w.Body.Bytes(), &projects); err != nil {
			t.Fatal(err)
		}
		for _, p := range projects {
			if p.ID == project {
				return p.OpenAsWiki
			}
		}
		t.Fatalf("%s cannot see %s", user, project)
		return false
	}
	if openAsWiki("wiki-reader", "default") {
		t.Fatal("new notebooks open as wiki")
	}
	for _, user := range []string{"wiki-reader", "wiki-editor", "stranger"} {
		if w := proxyCall(app, user, "POST", "/p/default/api/wiki", `{"openAsWiki":true}`); w.Code != 403 {
			t.Fatalf("%s changed the wiki setting: %d %s", user, w.Code, w.Body)
		}
	}
	if openAsWiki("wiki-reader", "default") {
		t.Fatal("refused change was kept")
	}
	if w := proxyCall(app, "wiki-owner", "POST", "/p/default/api/wiki", `{"openAsWiki":true}`); w.Code != 200 {
		t.Fatalf("owner: %d %s", w.Code, w.Body)
	}
	if !openAsWiki("wiki-reader", "default") || !openAsWiki("wiki-editor", "default") {
		t.Fatal("members do not see the wiki setting")
	}
	if w := proxyCall(app, "wiki-owner", "POST", "/p/default/api/wiki", `{"openAsWiki":false}`); w.Code != 200 || openAsWiki("wiki-reader", "default") {
		t.Fatalf("owner could not turn the wiki off: %d %s", w.Code, w.Body)
	}
	// Repository notebooks have no Markdown pages to arrange as a wiki.
	if w := proxyCall(app, "wiki-owner", "POST", "/p/code/api/wiki", `{"openAsWiki":true}`); w.Code < 400 || openAsWiki("wiki-owner", "code") {
		t.Fatalf("repository notebook accepted the wiki setting: %d %s", w.Code, w.Body)
	}
}

// MCP clients change the same setting through set_notebook_wiki, with the
// browser's Owner check and the write consent of other notebook settings.
func TestOpenAsWikiThroughMCPNeedsOwnerAndWriteConsent(t *testing.T) {
	app, _ := fixture(t)
	for user, role := range map[string]string{"wiki-editor": "edit", "wiki-owner": "owner"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	args := map[string]any{"project": "default", "openAsWiki": true}
	actionCall(t, actionClient(t, app, "wiki-owner", "notes:read"), "set_notebook_wiki", args, true)
	actionCall(t, actionClient(t, app, "wiki-editor", "notes:read notes:write notes:manage"), "set_notebook_wiki", args, true)
	owner := actionClient(t, app, "wiki-owner", "notes:read notes:write")
	if raw := actionCall(t, owner, "list_projects", map[string]any{}, false); strings.Contains(string(raw), `"openAsWiki"`) {
		t.Fatalf("refused MCP change was kept: %s", raw)
	}
	actionCall(t, owner, "set_notebook_wiki", args, false)
	if raw := actionCall(t, owner, "list_projects", map[string]any{}, false); !strings.Contains(string(raw), `"openAsWiki":true`) {
		t.Fatalf("list_projects does not show the wiki setting: %s", raw)
	}
}

// [[Page title]] and [[Target|label]] links are real links: the page they name
// lists the linking note under "Referenced by" (its evidence backlinks) and in
// the note map, resolved as the wiki resolves them in the browser. A link names
// a title, not a file, so retitling a page re-points the links to its old title.
func TestPageLinksAreBacklinksAndFollowTitles(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	save := func(path, content, revision string) workspace.Document {
		t.Helper()
		doc, err := store.Save(ctx, path, []byte(content), revision, "author")
		if err != nil {
			t.Fatalf("save %s: %v", path, err)
		}
		return doc
	}
	save("Setup.md", "# Getting started\nAt the root.\n", "")
	start := save("guide/start.md", "---\ntitle: Getting started\n---\nIn the guide.\n", "")
	save("guide/draft.md", "---\nfmn-container: draft\ntitle: Getting started\n---\nNot a page yet.\n", "")
	save("guide/index.md", "# The guide\n", "")
	save("guide/linker.md", "# Linker\nSee [[getting STARTED]] and [[The guide|our guide]].\n\nNot `[[Setup]]`, nor:\n\n```\n[[Setup]]\n```\n", "")
	save("notes/paths.md", "# Paths\nRead [[/guide/start.md|the start]] and [[Missing page]].\n", "")

	backlinks := func(path string) []string {
		t.Helper()
		w := call(app, "GET", "/api/knowledge/evidence?path="+url.QueryEscape(path), "", testToken)
		if w.Code != 200 {
			t.Fatalf("evidence %s: %d %s", path, w.Code, w.Body)
		}
		var evidence knowledge.Evidence
		if err := json.Unmarshal(w.Body.Bytes(), &evidence); err != nil {
			t.Fatal(err)
		}
		notes := []string{}
		for _, ref := range evidence.Backlinks {
			notes = append(notes, ref.Note)
		}
		sort.Strings(notes)
		return notes
	}
	mapped := func() map[string]knowledge.MapEntry {
		t.Helper()
		w := call(app, "GET", "/api/map?limit=40", "", testToken)
		if w.Code != 200 {
			t.Fatalf("map: %d %s", w.Code, w.Body)
		}
		var result struct {
			Entries []knowledge.MapEntry `json:"entries"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		out := map[string]knowledge.MapEntry{}
		for _, entry := range result.Entries {
			out[entry.Path] = entry
		}
		return out
	}
	expect := func(want map[string][]string) {
		t.Helper()
		entries := mapped()
		for path, linkers := range want {
			if got := backlinks(path); !slices.Equal(got, linkers) {
				t.Errorf("Referenced by for %s = %v, want %v", path, got, linkers)
			}
			got := slices.Clone(entries[path].Backlinks)
			sort.Strings(got)
			if len(got) == 0 && len(linkers) == 0 {
				continue
			}
			if !slices.Equal(got, linkers) {
				t.Errorf("map backlinks for %s = %v, want %v", path, got, linkers)
			}
		}
	}
	// The nearest page titled "Getting started" wins, in any case; drafts are
	// not pages, and text in code is not a link.
	expect(map[string][]string{
		"guide/start.md": {"guide/linker.md", "notes/paths.md"},
		"guide/index.md": {"guide/linker.md"},
		"Setup.md":       {},
		"guide/draft.md": {},
	})
	w := call(app, "GET", "/api/knowledge/evidence?path=guide/linker.md", "", testToken)
	if !strings.Contains(w.Body.String(), `"label":"our guide"`) || !strings.Contains(w.Body.String(), `"path":"guide/index.md"`) {
		t.Fatalf("labelled page link is not a source: %s", w.Body)
	}

	save("guide/start.md", "---\ntitle: Starting out\n---\nIn the guide.\n", start.Revision)
	expect(map[string][]string{
		"guide/start.md": {"notes/paths.md"},
		"Setup.md":       {"guide/linker.md"},
		"guide/draft.md": {},
	})
}
