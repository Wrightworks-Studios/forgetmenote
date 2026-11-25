package workspace

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestMoveFolderPreservesLinksCommentsLocksAndHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	contents := map[string]string{
		"old/a.md":          "# An original note\n\nSeveral lines of original content.\n\n[Other](../other.md#section)\n![Image](media/pic.png)\n",
		"old/locked.md":     "# Keep this text unchanged\n",
		"old/media/pic.png": "image bytes",
		"other.md":          "# References\n\n[Read](old/a.md#heading)\n\n[ref]: old/a.md \"A title\"\n\n`[code](old/a.md)`\n\n```md\n[code](old/a.md)\n```\n",
	}
	for p, text := range contents {
		if _, err := s.Save(ctx, p, []byte(text), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	addDiscussion(t, s, "old/a.md", Comment{ID: "c1", Author: "reader", Text: "Feedback", Created: "2026-11-22T00:00:00Z"})
	lock := setLocks(t, s, "old/locked.md")
	moved, err := s.Move(ctx, "old", "new/nested", lock, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if moved.UpdatedLinks != 2 {
		t.Fatalf("updated notes = %d", moved.UpdatedLinks)
	}
	if _, err := s.Read(ctx, "old/a.md", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source still exists", err)
	}
	note, err := s.Read(ctx, "new/nested/a.md", "")
	if err != nil || !strings.Contains(string(note.Content), "../../other.md#section") || !strings.Contains(string(note.Content), "media/pic.png") {
		t.Fatalf("outbound links: %s %v", note.Content, err)
	}
	other, _ := s.Read(ctx, "other.md", "")
	if !strings.Contains(string(other.Content), "[Read](new/nested/a.md#heading)") || !strings.Contains(string(other.Content), "[ref]: new/nested/a.md") || !strings.Contains(string(other.Content), "`[code](old/a.md)`") || !strings.Contains(string(other.Content), "```md\n[code](old/a.md)\n```") {
		t.Fatalf("inbound links/code: %s", other.Content)
	}
	comments, err := s.Comments(ctx, note.Path)
	if err != nil || len(comments.Comments) != 1 || comments.Comments[0].Text != "Feedback" {
		t.Fatalf("comments: %+v %v", comments, err)
	}
	locks := lockRulesOf(t, s, "new/nested/locked.md")
	if len(locks) != 1 || locks[0].Path != "new/nested/locked.md" {
		t.Fatalf("locks: %+v", locks)
	}
	history := entryHistory(t, s, note.Path)
	if len(history) < 2 {
		t.Fatalf("history: %+v", history)
	}
	for _, version := range history {
		if _, err := s.Read(ctx, version.Path, version.Revision); err != nil {
			t.Fatalf("history path %s: %v", version.Path, err)
		}
	}
	state, _ := s.Status(ctx)
	if state.Dirty || state.Revision != moved.Revision {
		t.Fatalf("unclean move: %+v", state)
	}
}
func TestMoveRejectsStaleUnsafeAndOverwritingTargets(t *testing.T) {
	for _, target := range []string{"../escape", "old/child", "existing.md", ".git/config"} {
		t.Run(target, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			s.Save(ctx, "old/a.md", []byte("original"), "", "test")
			s.Save(ctx, "existing.md", []byte("existing"), "", "test")
			state, _ := s.Status(ctx)
			if _, err := s.Move(ctx, "old", target, state.Revision, "owner"); err == nil {
				t.Fatal("unsafe move accepted")
			}
			after, _ := s.Status(ctx)
			if after.Dirty || after.Revision != state.Revision {
				t.Fatal("failed move changed repo")
			}
		})
	}
	s := newTestStore(t)
	ctx := context.Background()
	a, _ := s.Save(ctx, "a.md", []byte("A"), "", "test")
	s.Save(ctx, "b.md", []byte("B"), "", "test")
	if _, err := s.Move(ctx, "a.md", "new.md", a.Revision, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale move accepted", err)
	}
}
func TestMoveFailureRollsBackAndLockedReferencesBlock(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.Save(ctx, "a.md", []byte("A"), "", "test")
	s.Save(ctx, "ref.md", []byte("[A](a.md)"), "", "test")
	lock := setLocks(t, s, "ref.md")
	if _, err := s.Move(ctx, "a.md", "b.md", lock, "owner"); !errors.Is(err, ErrForbidden) {
		t.Fatal("locked reference changed", err)
	}
	unlocked := setLocks(t, s)
	release := failCommits(t, s)
	if _, err := s.Move(ctx, "a.md", "b.md", unlocked, "owner"); err == nil {
		t.Fatal("expected commit failure")
	}
	release()
	state, _ := s.Status(ctx)
	if state.Dirty || state.Revision != unlocked {
		t.Fatalf("rollback failed: %+v", state)
	}
	a, _ := s.Read(ctx, "a.md", "")
	if string(a.Content) != "A" {
		t.Fatal("source lost")
	}
}

func TestMovingChildOutOfLockedFolderPreservesProtection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.Save(ctx, "locked/a.md", []byte("Protected text"), "", "test")
	lock := setLocks(t, s, "locked")
	if _, err := s.Move(ctx, "locked/a.md", "a.md", lock, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "a.md", []byte("changed"), lock, "writer"); !errors.Is(err, ErrForbidden) {
		t.Fatal("move weakened inherited lock", err)
	}
}
func TestMoveHistoryFollowsShortNotesAndFolders(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.Save(ctx, "outside.md", []byte("outside"), "", "test")
	doc, _ := s.Save(ctx, "one/a.md", []byte("[x](../outside.md)"), "", "test")
	moved, err := s.Move(ctx, "one", "two/deep", doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"two/deep/a.md", "two/deep"} {
		history, err := s.History(ctx, scope)
		if err != nil || len(history) != 2 {
			t.Fatalf("%s history: %+v %v", scope, history, err)
		}
		if strings.HasSuffix(scope, ".md") {
			for _, version := range history {
				if _, err := s.Read(ctx, version.Path, version.Revision); err != nil {
					t.Fatal("old version cannot be opened", err)
				}
			}
		}
	}
	if _, err = s.Move(ctx, "two/deep/a.md", "short.md", moved.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	history, err := s.History(ctx, "short.md")
	if err != nil || len(history) != 3 {
		t.Fatalf("second rename history: %+v %v", history, err)
	}
}

func TestMovePreservesPinnedHistoricalDestination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	source, _ := s.Save(ctx, "sources/a.md", []byte("# Source"), "", "")
	text := "[Evidence](sources/a.md?fmn-rev=" + source.Revision + "#source)"
	summary, _ := s.Save(ctx, "summary.md", []byte(text), "", "")
	_, err := s.Move(ctx, "sources", "archive", summary.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := s.Read(ctx, "summary.md", "")
	if string(doc.Content) != text {
		t.Fatal("historical path rewritten to a path absent from the pinned revision", string(doc.Content))
	}
	head, _ := s.Status(ctx)
	_, err = s.Move(ctx, "summary.md", "notes/summary.md", head.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	doc, _ = s.Read(ctx, "notes/summary.md", "")
	if !strings.Contains(string(doc.Content), "../sources/a.md?fmn-rev=") {
		t.Fatal("historical relative path not rebased", string(doc.Content))
	}
}

func TestScopedHistoryFollowsMovesWithRewrittenContents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "outside.md", []byte("outside"), "", "test"); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Save(ctx, "one/a.md", []byte("[x](../outside.md)"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	moved, err := s.Move(ctx, "one", "two/deep", doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Move(ctx, "two/deep/a.md", "short.md", moved.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}
	history, err := s.HistoryScoped(ctx, "short.md", scope)
	if err != nil || len(history) != 3 {
		t.Fatalf("scoped history lost pre-move versions: %+v %v", history, err)
	}
	for _, version := range history {
		if version.Message != "Committed eligible note version" {
			t.Fatalf("scoped history exposed the unfiltered commit message: %q", version.Message)
		}
		if _, err := s.Read(ctx, version.Path, version.Revision); err != nil {
			t.Fatalf("historical version cannot be opened: %v", err)
		}
	}
}
