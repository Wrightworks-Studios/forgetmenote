package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
)

func TestGroupBatchesCurrentPreimagesAndTrash(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete=%v", deleting), func(t *testing.T) {
			s := newTestStore(t)
			var head string
			group := EditGroup{ID: "batched", Summary: "Batch twelve edits"}
			for i := 0; i < 12; i++ {
				path := fmt.Sprintf("batch-%02d.md", i)
				content := fmt.Sprintf("# Note %d\n", i)
				doc, err := s.Save(t.Context(), path, []byte(content), "", "")
				if err != nil {
					t.Fatal(err)
				}
				head = doc.Revision
				group.Edits = append(group.Edits, NoteEdit{Path: path, Content: content + "Changed\n", Hash: ContentHash([]byte(content)), Delete: deleting})
			}
			for i := range group.Edits {
				group.Edits[i].Revision = head
			}
			start := time.Now()
			result, err := s.ApplyGroup(t.Context(), group)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("12-note group: %s serialized operation", time.Since(start))
			if info, err := s.notes.Commit(t.Context(), mustID(t, result.Revision)); err != nil || commitSubject(info.Commit.Message) != group.Summary {
				t.Fatalf("the group is not one revision: %v", err)
			}
			for _, edit := range group.Edits {
				doc, err := s.Read(t.Context(), edit.Path, "")
				if deleting {
					if !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("deleted %s: %v", edit.Path, err)
					}
				} else if err != nil || string(doc.Content) != edit.Content {
					t.Fatalf("updated %s: %q %v", edit.Path, doc.Content, err)
				}
			}
			if deleting {
				trash, err := s.ListTrash(t.Context())
				if err != nil || len(trash.Entries) != len(group.Edits) {
					t.Fatalf("trash history: %d %v", len(trash.Entries), err)
				}
			}
		})
	}
}

func TestGroupCommitConflictAndRetry(t *testing.T) {
	s := newTestStore(t)
	ctx := attribution.With(context.Background(), attribution.Identity{ID: "alice", Name: "Alice", Email: "alice@users.gitnotes.invalid", Source: "mcp", Client: "registered"})
	a, _ := s.Save(ctx, "a.md", []byte("A"), "", "")
	b, _ := s.Save(ctx, "b.md", []byte("B"), "", "")
	g := EditGroup{ID: "review-1", Summary: "Connect related notes", Edits: []NoteEdit{{Path: "a.md", Content: "A2", Revision: a.Revision, Hash: ContentHash([]byte("A"))}, {Path: "b.md", Content: "B2", Revision: b.Revision, Hash: ContentHash([]byte("B"))}}}
	result, err := s.ApplyGroup(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.notes.Commit(ctx, mustID(t, result.Revision))
	if err != nil || len(info.Changes) != 2 || info.Commit.Parents[0].String() != b.Revision || commitRecord(info).UserID != "alice" {
		t.Fatal(info, err)
	}
	again, err := s.ApplyGroup(ctx, g)
	if err != nil || !again.Replayed || again.Revision != result.Revision {
		t.Fatal(again, err)
	}
	g.Summary = "changed retry"
	if _, err = s.ApplyGroup(ctx, g); !errors.Is(err, ErrConflict) {
		t.Fatal("accepted different retry", err)
	}
	g.ID = "stale"
	if _, err = s.ApplyGroup(ctx, g); !errors.Is(err, ErrConflict) {
		t.Fatal("accepted stale group", err)
	}
	doc, _ := s.Read(ctx, "a.md", "")
	if string(doc.Content) != "A2" {
		t.Fatal(doc)
	}
}

func TestGroupHistoricalBasePreservesChangedThenRevertedConflict(t *testing.T) {
	s := newTestStore(t)
	original, err := s.Save(t.Context(), "reverted.md", []byte("Original"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := s.Save(t.Context(), original.Path, []byte("Changed"), original.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(t.Context(), original.Path, original.Content, changed.Revision, ""); err != nil {
		t.Fatal(err)
	}
	group := EditGroup{ID: "stale-reverted", Summary: "Must preserve the historical conflict", Edits: []NoteEdit{{Path: original.Path, Revision: original.Revision, Hash: ContentHash(original.Content), Content: "Stale replacement"}}}
	if _, err := s.ApplyGroup(t.Context(), group); !errors.Is(err, ErrConflict) {
		t.Fatalf("reverted text erased its later history: %v", err)
	}
	actual, err := s.Read(t.Context(), original.Path, "")
	if err != nil || string(actual.Content) != "Original" {
		t.Fatalf("conflicting group changed content: %q %v", actual.Content, err)
	}
}
func TestGroupLockAndFailedCommitDoNotPartiallyApply(t *testing.T) {
	s := newTestStore(t)
	setLocks(t, s, "manuscript")
	ctx := context.Background()
	a, _ := s.Save(ctx, "a.md", []byte("A"), "", "")
	g := EditGroup{ID: "locked", Summary: "Two notes", Edits: []NoteEdit{{Path: "a.md", Content: "changed", Revision: a.Revision, Hash: ContentHash([]byte("A"))}, {Path: "manuscript/new.md", Content: "blocked"}}}
	if _, err := s.ApplyGroup(ctx, g); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	doc, _ := s.Read(ctx, "a.md", "")
	if string(doc.Content) != "A" {
		t.Fatal("partial locked write")
	}
	g.Edits[1].Path = "new.md"
	release := failCommits(t, s)
	if _, err := s.ApplyGroup(ctx, g); err == nil {
		t.Fatal("expected commit failure")
	}
	release()
	doc, _ = s.Read(ctx, "a.md", "")
	if string(doc.Content) != "A" {
		t.Fatal("partial failed write")
	}
	if _, err := s.Read(ctx, "new.md", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new file leaked")
	}
	status, _ := s.Status(ctx)
	if status.Dirty {
		t.Fatal("dirty after failed group")
	}
}

func TestConcurrentGroupsDoNotOverwriteSameBase(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, _ := s.Save(ctx, "shared.md", []byte("Original"), "", "")
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, text := range []string{"First draft", "Second draft"} {
		go func(text string) {
			<-start
			_, err := s.ApplyGroup(ctx, EditGroup{ID: text, Summary: text, Edits: []NoteEdit{{Path: doc.Path, Content: text, Revision: doc.Revision, Hash: ContentHash(doc.Content)}}})
			results <- err
		}(text)
	}
	close(start)
	success, conflict := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
	if history := entryHistory(t, s, doc.Path); len(history) != 2 {
		t.Fatal("unexpected partial or duplicate commits", history)
	}
}

func TestMutationScopeChecksEveryGroupAndTransferSource(t *testing.T) {
	owner := newTestStoreExcluding(t, []string{"private"})
	ctx := context.Background()
	a, err := owner.Save(ctx, "folder/active.md", []byte("# Active\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	b, err := owner.Save(ctx, "folder/archive.md", []byte("---\nfmn-container: archive\n---\n# Excluded\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	scoped := WithMutationScope(ctx, MutationScope{Version: 1, Content: ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}})
	group := EditGroup{ID: "mixed", Summary: "mixed edit", Edits: []NoteEdit{{Path: a.Path, Revision: a.Revision, Hash: ContentHash(a.Content), Content: "# Changed\n"}, {Path: b.Path, Revision: b.Revision, Hash: ContentHash(b.Content), Content: "# Promoted\n"}}}
	if _, err = owner.ApplyGroup(scoped, group); err == nil {
		t.Error("mixed group promoted excluded member")
	}
	head, _ := owner.Status(ctx)
	if _, err = owner.Duplicate(scoped, "folder", "copy", head.Revision, "agent"); err == nil {
		t.Error("folder copy included excluded source")
	}
	doc, _ := owner.Read(ctx, a.Path, "")
	if string(doc.Content) != string(a.Content) {
		t.Error("rejected group was not atomic")
	}
}

func TestChangeGroupDeleteRetainsTrash(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	d, err := s.Save(ctx, "notes/discard.md", []byte("# Recover this note\n"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplyGroup(ctx, EditGroup{ID: "trash-delete", Summary: "Discard note", Edits: []NoteEdit{{Path: d.Path, Revision: d.Revision, Hash: ContentHash(d.Content), Delete: true}}})
	if err != nil {
		t.Fatal(err)
	}
	trash, err := s.ListTrash(ctx)
	if err != nil || len(trash.Entries) != 1 || trash.Entries[0].OriginalPath != d.Path {
		t.Fatalf("deletion must retain a recoverable Trash note: %+v %v", trash, err)
	}
	kept, err := s.Read(ctx, trash.Entries[0].Path, "")
	if err != nil || !strings.Contains(string(kept.Content), "Recover this note") {
		t.Fatalf("the Trash note: %q %v", kept.Content, err)
	}
	if _, err := s.Read(ctx, d.Path, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the deleted note is still there:", err)
	}
	entries, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Path, "Trash/") {
			t.Fatal("Trash leaked into ordinary note listing")
		}
	}
}

func mustID(t *testing.T, revision string) gitobj.ID {
	t.Helper()
	id, err := gitobj.ParseID(revision)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
