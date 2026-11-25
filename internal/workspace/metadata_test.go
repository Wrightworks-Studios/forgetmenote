package workspace

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

func TestMetadataSyntaxAndMalformedValues(t *testing.T) {
	for _, content := range []string{"---\ntags: [fire, 'old tongue', \"scholar, law\"]\n---\nText", "---\r\ntags:\r\n- fire\r\n- old tongue\r\n- 'scholar, law'\r\n---\r\nText"} {
		m, err := ParseMetadata([]byte(content))
		if err != nil || len(m.Tags) != 3 || m.Tags[2] != "scholar, law" {
			t.Fatalf("parse %+v %v", m, err)
		}
	}
	for _, content := range []string{"---\ntags: [unfinished\n---\nText", "---\ntags: *alias\n---\nText", "---\ntitle: |\n  multiline\n---\nText", "---\ntags: [a]\ntags: [b]\n---\nText"} {
		if _, err := ParseMetadata([]byte(content)); err == nil {
			t.Fatalf("unsafe metadata accepted: %s", content)
		}
	}
}

func BenchmarkMetadataLargeBody(b *testing.B) {
	content := []byte("---\ntitle: Large notebook source\ntags: [reference]\n---\n" + strings.Repeat("Short paragraph.\n", 100000))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		m, err := ParseMetadata(content)
		if err != nil || m.Title != "Large notebook source" || len(m.Tags) != 1 {
			b.Fatal(m, err)
		}
	}
}

func TestMetadataFilterUsesCommittedValuesAndAppliesBeforeLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	notes := map[string]string{}
	for i := 0; i < 105; i++ {
		content := "---\ntags: [ordinary]\nstatus: draft\n---\nAstronomy\n"
		if i == 104 {
			content = "---\ntitle: Special note\ntags: [Research]\nstatus: canon\n---\nAstronomy\n"
		}
		notes[fmt.Sprintf("note-%03d.md", i)] = content
	}
	commitFiles(t, s, notes)
	found, err := s.Find(ctx, "astronomy", "research", "canon", "")
	if err != nil || len(found) != 1 || found[0].Path != "note-104.md" || found[0].Metadata.Title != "Special note" {
		t.Fatalf("filtered search: %+v %v", found, err)
	}
	doc, _ := s.Read(ctx, "note-104.md", "")
	content := strings.Replace(string(doc.Content), "tags: [Research]", "tags: [updated]", 1)
	if _, err = s.Save(ctx, doc.Path, []byte(content), doc.Revision, "writer"); err != nil {
		t.Fatal(err)
	}
	found, err = s.Find(ctx, "", "updated", "", "")
	if err != nil || len(found) != 1 {
		t.Fatalf("cache invalidation: %+v %v", found, err)
	}
}

func TestListingMetadataCannotMutateCommittedSearchCache(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	doc, err := s.Save(ctx, "metadata.md", []byte("---\ntitle: Original\ntags: [original]\n---\nneedle\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	listing, err := s.List(ctx)
	if err != nil || len(listing) != 1 || listing[0].Metadata == nil {
		t.Fatal("listing missing metadata", err)
	}
	listing[0].Metadata.Title = "caller mutation"
	listing[0].Metadata.Tags[0] = "caller mutation"
	found, err := s.Find(ctx, "needle", "original", "", "")
	if err != nil || len(found) != 1 || found[0].Metadata.Title != "Original" {
		t.Fatal("public listing mutated subsequent committed search", err)
	}
	committed, err := s.Read(ctx, doc.Path, "")
	if err != nil || committed.Revision != doc.Revision || !strings.Contains(string(committed.Content), "tags: [original]") {
		t.Fatal("listing changed stored content", err)
	}
}

// A path that a revision turns into a symbolic link, as an imported
// repository may, is never listed; when a later revision makes it a note
// again, its metadata is listed again.
func TestListingSkipsCommittedLinksAndRecoversRepairedNotes(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	doc, err := s.Save(ctx, "repair.md", []byte("---\ntags: [retained]\nstatus: ready\n---\n# Repair\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Find(ctx, "", "retained", "ready", ""); err != nil {
		t.Fatal(err)
	}
	commitFixture(t, s, notestore.FileChange{Path: doc.Path, Content: []byte("../outside"), Mode: gitobj.ModeSymlink})
	entries, err := s.List(ctx)
	if err != nil || len(entries) != 0 {
		t.Fatal("symlink exposed by listing", entries, err)
	}
	if entries, err = s.Find(ctx, "outside", "", "", ""); err != nil || len(entries) != 0 {
		t.Fatal("symlink exposed by search", entries, err)
	}
	commitFixture(t, s, notestore.FileChange{Path: doc.Path, Content: doc.Content, Mode: gitobj.ModeFile})
	entries, err = s.Find(ctx, "", "retained", "ready", "")
	if err != nil || len(entries) != 1 || entries[0].Path != doc.Path {
		t.Fatal("path repair did not restore committed metadata", entries, err)
	}
}

func TestContainerInvalidImportsRemainVisibleAndAuthoringRejectsInvalid(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	commitFiles(t, s, map[string]string{
		"bad.md":           "---\nfmn-container: future\n---\nRepair me\n",
		"archive.md":       "---\ntags: [broken\nfmn-container: archive\n---\nStill archived\n",
		"status.md":        "---\nstatus: archived\n---\nOrdinary\n",
		"Drafts/folder.md": "# Ordinary folder name\n",
	})
	notes, err := s.ListCollection(ctx, "notes")
	if err != nil || len(notes) != 3 {
		t.Fatalf("repair listing: %+v %v", notes, err)
	}
	archives, err := s.ListCollection(ctx, "archives")
	if err != nil || len(archives) != 1 || archives[0].Path != "archive.md" || archives[0].Metadata.Container != "archive" {
		t.Fatalf("unrelated metadata error lost archive: %+v %v", archives, err)
	}
	counts, err := s.Collections(ctx)
	if err != nil || counts.Invalid != 1 || counts.Archives != 1 || counts.Notes != 3 {
		t.Fatalf("counts: %+v %v", counts, err)
	}
	for _, value := range []string{"future", "", "[draft]", "draft\nfmn-container: archive", "archive\n  nested: value"} {
		if _, err := s.Save(ctx, "new.md", []byte("---\nfmn-container: "+value+"\n---\nBody"), "", "editor"); err == nil {
			t.Fatalf("invalid authoring accepted: %q", value)
		}
	}
	if _, err := s.Save(ctx, "unrelated.md", []byte("---\ntags: [unfinished\nBody discussing fmn-container"), "", "editor"); err != nil {
		t.Fatalf("unrelated legacy syntax changed: %v", err)
	}
	bad, _ := s.Read(ctx, "bad.md", "")
	if _, err := s.Save(ctx, bad.Path, []byte("# Repaired\n"), bad.Revision, "editor"); err != nil {
		t.Fatal(err)
	}
}
