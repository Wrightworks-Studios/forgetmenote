package workspace

import (
	"context"
	"reflect"
	"testing"
)

func TestSearchFindsContentAndLiteralPaths(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, p := range []string{"notes/a[1].md", "notes/other.md", "media/text.txt"} {
		if _, err := s.Save(ctx, p, []byte("Unique astronomy phrase"), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.Search(ctx, "ASTRONOMY phrase")
	if err != nil || len(entries) != 2 {
		t.Fatalf("content search = %v, %v", entries, err)
	}
	entries, err = s.Search(ctx, "a[1]")
	if err != nil || len(entries) != 1 || entries[0].Path != "notes/a[1].md" {
		t.Fatalf("literal path search = %v, %v", entries, err)
	}
}

func TestBrowseExclusionsKeepArchiveOutOfListsAndSearch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, path := range []string{"archive/old.md", "notes/current.md", "archive-notes.md"} {
		if _, err := s.Save(ctx, path, []byte("sharedword"), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	// A browsing exclusion must not remove history or prevent deliberate reads.
	s.browseExcluded = []string{"archive/"}
	entries, err := s.List(ctx)
	if err != nil || len(entries) != 2 {
		t.Fatalf("list: %+v %v", entries, err)
	}
	for _, query := range []string{"sharedword", "old", "archive"} {
		entries, err = s.Search(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Path == "archive/old.md" {
				t.Fatalf("archive leaked through search %q", query)
			}
		}
	}
	doc, err := s.Read(ctx, "archive/old.md", "")
	if err != nil || string(doc.Content) != "sharedword" {
		t.Fatal("archive content was removed")
	}
}

func TestSearchFindsNoteTextButNotViewCode(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for path, content := range map[string]string{
		"facts.md":     "# Wombat facts\n\nThey dig.\n",
		"dashboard.md": "# Dashboard\n\nCounts notes.\n\n```view\n<script>fmn.notes.search('Wombat')</script>\n```\n",
	} {
		if _, err := store.Save(ctx, path, []byte(content), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "include", Drafts: "include"}, Collection: "all"}
	for query, want := range map[string][]string{"wombat": {"facts.md"}, "counts notes": {"dashboard.md"}, "fmn.notes": nil} {
		found, err := store.FindScoped(ctx, query, "", "", "", false, 100, scope)
		if err != nil {
			t.Fatal(err)
		}
		got := []string(nil)
		for _, entry := range found.Entries {
			got = append(got, entry.Path)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("search %q: got %v, want %v", query, got, want)
		}
	}
}
