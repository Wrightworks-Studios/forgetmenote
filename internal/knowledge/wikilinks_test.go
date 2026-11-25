package knowledge

import (
	"slices"
	"testing"

	"forgetmenote/internal/workspace"
)

// The cases of "[[links]] find a page by title, then by path" in
// internal/web/ui/wiki.test.mjs: the server resolves page links as the browser
// renders them.
func TestPageLinkTargetsMatchTheBrowser(t *testing.T) {
	page := func(path, title string) workspace.SnapshotFile {
		return workspace.SnapshotFile{Path: path, Metadata: workspace.Metadata{Title: title}}
	}
	pages := snapshotPages([]workspace.SnapshotFile{
		page("index.md", "Home"),
		page("Setup.md", "Getting started"),
		page("guide/index.md", "The guide"),
		page("guide/start.md", "Getting started"),
		page("guide/Glossary.md", ""),
		page("guide/deep/start.md", "Getting started"),
		page("notes/Glossary.md", "Terms"),
		page("a/twin.md", "Twin"),
		page("B/twin.md", "Twin"),
		page("AGENTS.md", ""),
	})
	for _, c := range []struct{ source, text, want string }{
		{"index.md", "getting STARTED", "Setup.md"},
		{"guide/other.md", "Getting started", "guide/start.md"},
		{"guide/deep/x.md", " Getting started ", "guide/deep/start.md"},
		{"notes/a.md", "The guide", "guide/index.md"},
		{"index.md", "Home", "index.md"},
		{"index.md", "Twin", "B/twin.md"},
		{"notes/a.md", "Glossary", "guide/Glossary.md"},
		{"index.md", "Terms", "notes/Glossary.md"},
		{"index.md", "notes/Glossary", "notes/Glossary.md"},
		{"index.md", "/notes/Glossary.md", "notes/Glossary.md"},
		{"guide/x.md", "deep/start", "guide/deep/start.md"},
		{"index.md", "guide", "guide/index.md"},
		{"guide/x.md", "Missing page", ""},
		{"guide/x.md", "AGENTS", ""},
		{"guide/x.md", "", ""},
		{"guide/x.md", "../index", ""},
		{"guide/x.md", "notes", ""},
	} {
		if got := pages.target(c.source, c.text); got != c.want {
			t.Errorf("[[%s]] from %s = %q, want %q", c.text, c.source, got, c.want)
		}
	}
}

// Page links are read from the text a reader sees: not from code, link text,
// image descriptions or raw HTML, nor across emphasis, line breaks or GFM
// table cells; escapes are read as rendered.
func TestPageLinksAreReadFromRenderedProse(t *testing.T) {
	note := func(path, content string) workspace.SnapshotFile {
		metadata, err := workspace.ParseMetadata([]byte(content))
		if err != nil {
			t.Fatal(err)
		}
		return workspace.SnapshotFile{Path: path, Content: content, Metadata: metadata}
	}
	pages := snapshotPages([]workspace.SnapshotFile{
		note("a.md", "# Alpha\n"),
		note("b.md", "# Beta\n"),
		note("c.md", "# Gamma\n"),
		note("drafts/d.md", "---\nfmn-container: draft\n---\n# Delta\n"),
	})
	content := "---\ntitle: Links\n---\n" +
		"See [[Alpha]], [[beta|the second]] and [[Delta]], then \\[\\[Beta\\]\\].\n\n" +
		"`[[Gamma]]` [link [[Gamma]]](https://example.org) ![[[Gamma]]](x.png) <span title=\"[[Gamma]]\">x</span>\n\n" +
		"[[Gam*ma*]] [[Gam\nma]]\n\n" +
		"```\n[[Gamma]]\n```\n\n" +
		"```view\n<p>[[Gamma]]</p>\n```\n\n" +
		"| One | Two |\n| --- | --- |\n| [[Gamma|split]] |\n| [[c\\|Third]] | Two |\n"
	type link struct{ path, label string }
	got := []link{}
	for _, ref := range noteLinks(note("notes/links.md", content), pages) {
		if !ref.External {
			got = append(got, link{ref.Path, ref.Label})
		}
	}
	want := []link{{"a.md", "Alpha"}, {"b.md", "the second"}, {"b.md", "Beta"}, {"c.md", "Third"}}
	if !slices.Equal(got, want) {
		t.Fatalf("page links = %v, want %v", got, want)
	}
	if refs := noteLinks(workspace.SnapshotFile{Path: "x.md", Content: "[[Alpha]]"}, nil); len(refs) != 0 {
		t.Fatalf("page links without pages: %+v", refs)
	}
}
