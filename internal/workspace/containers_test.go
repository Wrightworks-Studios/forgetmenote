package workspace

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

func TestContainerTransitionsPreservePropertiesDiscussionAndHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	original := "---\r\nstatus: draft\r\ncustom:\r\n  nested: keep\r\n---\r\n# Exact text\r\n[Link](other.md)\r\n"
	doc, err := s.Save(ctx, "Archives/note.md", []byte(original), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	doc.Revision = addDiscussion(t, s, doc.Path, Comment{ID: "c1", Author: "reader", Text: "Discussion stays here", Created: "2026-11-22T00:00:00Z"})
	discussion, err := s.Comments(ctx, doc.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, container := range []string{"draft", "archive", ""} {
		doc, err = s.SetContainer(ctx, doc.Path, container, doc.Revision, "editor")
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := ParseMetadata(doc.Content)
		if err != nil || metadata.Container != container || metadata.Status != "draft" {
			t.Fatalf("classification: %+v %v", metadata, err)
		}
		if !strings.Contains(string(doc.Content), "custom:\r\n  nested: keep\r\n") || !strings.HasSuffix(string(doc.Content), "# Exact text\r\n[Link](other.md)\r\n") {
			t.Fatalf("content damaged: %q", doc.Content)
		}
		current, err := s.Comments(ctx, doc.Path)
		if err != nil || current.Comments[0].Text != discussion.Comments[0].Text {
			t.Fatalf("discussion: %+v %v", current, err)
		}
	}
	if string(doc.Content) != original {
		t.Fatalf("roundtrip changed bytes: %q", doc.Content)
	}
	archive, err := s.SetContainer(ctx, doc.Path, "archive", doc.Revision, "editor")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.SetMetadata(ctx, doc.Path, Metadata{Title: "New title", Status: "canon"}, archive.Revision, "old client")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := ParseMetadata(saved.Content)
	if err != nil || metadata.Container != "archive" || metadata.Status != "canon" {
		t.Fatalf("properties lost classification: %+v %v", metadata, err)
	}
	if _, err = s.SetContainer(ctx, "../outside.md", "archive", saved.Revision, "editor"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("traversal: %v", err)
	}
	linked := commitFixture(t, s, notestore.FileChange{Path: "link.md", Content: []byte("/outside.md"), Mode: gitobj.ModeSymlink})
	if _, err = s.SetContainer(ctx, "link.md", "archive", linked, "editor"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("symlink: %v", err)
	}
}

func TestContainerCommitFailureConflictAndLocks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "note.md", []byte("# Original\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	release := failCommits(t, s)
	if _, err = s.SetContainer(ctx, doc.Path, "archive", doc.Revision, "editor"); err == nil {
		t.Fatal("accepted failed commit")
	}
	release()
	current, _ := s.Read(ctx, doc.Path, "")
	state, _ := s.Status(ctx)
	if string(current.Content) != string(doc.Content) || state.Revision != doc.Revision || state.Dirty {
		t.Fatalf("failed classification changed repository: %+v %+v", current, state)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	updates := []func() error{
		func() error { _, err := s.SetContainer(ctx, doc.Path, "archive", doc.Revision, "editor"); return err },
		func() error {
			_, err := s.Save(ctx, doc.Path, []byte("# Competing editor text\n"), doc.Revision, "editor")
			return err
		},
	}
	for _, update := range updates {
		wg.Add(1)
		go func(update func() error) { defer wg.Done(); errs <- update() }(update)
	}
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("concurrent classification: %d wins %d conflicts", wins, conflicts)
	}
	locked := setLocks(t, s, doc.Path)
	if _, err = s.SetContainer(ctx, doc.Path, "", locked, "editor"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("locked classification: %v", err)
	}
	if _, err = s.SetContainer(ctx, doc.Path, "future", locked, "editor"); err == nil {
		t.Fatal("unknown classification accepted")
	}
}

func TestContainerConcurrentDifferentNotesAndLiveConflict(t *testing.T) {
	s, ctx, doc := liveTestStore(t)
	other, err := s.Save(ctx, "other.md", []byte("# Other"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	go func() { _, err := s.SetContainer(ctx, doc.Path, "draft", doc.Revision, "editor"); errs <- err }()
	go func() { _, err := s.SetContainer(ctx, other.Path, "archive", other.Revision, "editor"); errs <- err }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	doc, _ = s.Read(ctx, doc.Path, "")
	base, err := s.LiveRead(ctx, doc.Path)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := s.LivePatch(ctx, TargetedPatch{Path: doc.Path, Base: base.LiveRevision, OperationID: "container-peer", Edits: []TextEdit{{Start: 0, End: 0, Text: "Peer's accepted text\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetContainer(ctx, doc.Path, "archive", doc.Revision, "editor"); !errors.Is(err, ErrConflict) {
		t.Fatalf("classification overwrote live edit: %v", err)
	}
	committed, err := s.Read(ctx, doc.Path, "")
	if err != nil || string(committed.Content) != accepted.Content {
		t.Fatalf("lost accepted text: %+v %v", committed, err)
	}
}

func TestContainerSourceCaptureAndRefreshPreserveMembership(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	capture := SourceCapture{Title: "Original source", Original: []byte("Original evidence"), Text: "Original evidence\n", Record: SourceRecord{MIME: "text/plain", Method: "uploaded"}}
	doc, err := s.ImportSource(ctx, capture, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	container, err := ParseContainer(doc.Content)
	if err != nil || container != "" {
		t.Fatalf("source archive auto-classified: %s %v", container, err)
	}
	original := ParseSource(doc.Content)
	doc, err = s.SetContainer(ctx, doc.Path, "archive", doc.Revision, "editor")
	if err != nil {
		t.Fatal(err)
	}
	classified := ParseSource(doc.Content)
	if *original != *classified {
		t.Fatalf("source identity changed: %+v %+v", original, classified)
	}
	capture.Text = "Updated evidence\n"
	capture.Original = []byte("Updated evidence")
	updated, err := s.ImportSource(ctx, capture, doc.Path, doc.Revision, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := ParseMetadata(updated.Content)
	if err != nil || metadata.Container != "archive" || metadata.Source.ID != original.ID {
		t.Fatalf("source refresh lost membership: %+v %v", metadata, err)
	}
}

func TestContainerGroupedWritesRejectInvalidWithoutChangingGit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "group.md", []byte("# Before"), "", "editor")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplyGroup(ctx, EditGroup{ID: "invalid-container", Summary: "Reject malformed shared classification", Edits: []NoteEdit{{Path: doc.Path, Revision: doc.Revision, Hash: ContentHash(doc.Content), Content: "---\nfmn-container: draft\nfmn-container: archive\n---\nAfter"}}})
	if err == nil {
		t.Fatal("group committed conflicting classifications")
	}
	state, _ := s.Status(ctx)
	current, _ := s.Read(ctx, doc.Path, "")
	if state.Revision != doc.Revision || state.Dirty || string(current.Content) != string(doc.Content) {
		t.Fatalf("rejected group changed notebook: %+v %+v", state, current)
	}
}

func TestContainerMalformedExtentCannotCommit(t *testing.T) {
	for name, content := range map[string]string{
		"quoted-double":       "---\n\"fmn-container\": archive\n---\nBody\n",
		"quoted-single":       "---\n'fmn-container': draft\n---\nBody\n",
		"quoted-escaped":      "---\n\"fmn\\u002dcontainer\": archive\n---\nBody\n",
		"quoted-escaped-byte": "---\n\"fmn\\x2dcontainer\": archive\n---\nBody\n",
		"quoted-duplicate":    "---\nfmn-container: archive\n\"fmn-container\": draft\n---\nBody\n",
		"quoted-unknown":      "---\n'fmn-container': future\n---\nBody\n",
		"quoted-oversized":    "---\n" + strings.Repeat("# padding\n", 6600) + "\"fmn-container\": archive\n---\nBody\n",
		"blank":               "---\nfmn-container: archive\n\n  nested: value\n---\nBody\n",
		"comment":             "---\nfmn-container: archive\n# separator\n  nested: value\n---\nBody\n",
		"oversized":           "---\n" + strings.Repeat("# padding\n", 6600) + "fmn-container: invalid\n---\nBody\n",
	} {
		t.Run(name, func(t *testing.T) {
			s, ctx, doc := liveTestStore(t)
			if _, err := s.Save(ctx, doc.Path, []byte(content), doc.Revision, "editor"); err == nil {
				t.Fatal("Save accepted malformed reserved field")
			}
			if _, err := s.ApplyGroup(ctx, EditGroup{ID: "malformed-container", Summary: "Reject malformed collection", Edits: []NoteEdit{{Path: doc.Path, Revision: doc.Revision, Hash: ContentHash(doc.Content), Content: content}}}); err == nil {
				t.Fatal("group accepted malformed reserved field")
			}
			live, err := s.LiveRead(ctx, doc.Path)
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := s.LivePatch(ctx, TargetedPatch{Path: doc.Path, Base: live.LiveRevision, OperationID: "malformed-container", Edits: []TextEdit{{Start: 0, End: 0, Text: content}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.LiveCheckpoint(ctx, doc.Path); err == nil {
				t.Fatal("checkpoint accepted malformed reserved field")
			}
			pending, err := s.LiveRead(ctx, doc.Path)
			if err != nil || !pending.Pending || pending.Content != accepted.Content {
				t.Fatalf("lost recoverable text: %+v %v", pending, err)
			}
			current, err := s.Read(ctx, doc.Path, "")
			if err != nil || string(current.Content) != string(doc.Content) {
				t.Fatalf("changed saved bytes: %+v %v", current, err)
			}
			state, err := s.Status(ctx)
			if err != nil || state.Revision != doc.Revision || state.Dirty {
				t.Fatalf("changed Git: %+v %v", state, err)
			}
			// Offline/imported malformed notes remain readable in Notes for repair.
			commitFixture(t, s, fixture("imported.md", content))
			notes, err := s.ListCollection(ctx, "notes")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, note := range notes {
				if note.Path == "imported.md" {
					found = note.Metadata != nil && note.Metadata.ContainerError != ""
				}
			}
			if !found {
				t.Fatal("malformed import not projected as repairable invalid collection")
			}
		})
	}
}

func TestMetadataEditPreservesUnknownFieldsAndBody(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	original := "---\ntitle: 'Old title'\nstatus: provisional\ntags:\n  - Fire\n  - 'old tongue'\ncustom:\n  nested: [keep, exactly]\n# keep this comment\n---\n\n# Human text\nExact body.\n"
	doc, err := s.Save(ctx, "note.md", []byte(original), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseMetadata(doc.Content)
	if err != nil || m.Title != "Old title" || len(m.Tags) != 2 {
		t.Fatalf("metadata = %+v %v", m, err)
	}
	m.Title = "New title"
	m.Tags = []string{"Fire", "research", "fire"}
	m.Status = "canon"
	saved, err := s.SetMetadata(ctx, doc.Path, m, doc.Revision, "writer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved.Content), "custom:\n  nested: [keep, exactly]\n# keep this comment\n") || !strings.HasSuffix(string(saved.Content), "---\n\n# Human text\nExact body.\n") {
		t.Fatalf("unrelated text changed: %s", saved.Content)
	}
	parsed, err := ParseMetadata(saved.Content)
	if err != nil || parsed.Title != "New title" || parsed.Status != "canon" || len(parsed.Tags) != 2 {
		t.Fatalf("roundtrip: %+v %v", parsed, err)
	}
	if _, err = s.SetMetadata(ctx, doc.Path, m, doc.Revision, "writer"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale properties accepted", err)
	}
	lock := setLocks(t, s, doc.Path)
	if _, err = s.SetMetadata(ctx, doc.Path, m, lock, "writer"); !errors.Is(err, ErrForbidden) {
		t.Fatal("properties bypassed lock", err)
	}
}
