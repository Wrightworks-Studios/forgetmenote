package workspace

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A discussion reads its threads, anchors and replies at the note's
// revision, none for a note without one, and an invalid discussion file is
// refused; the same after a restart, and as another gateway commits it.
func TestDatabaseCommentsReadTheCommittedDiscussion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	note := saveNote(t, s, "notes/a.md", "---\ntitle: Private metadata\n---\n# Heading\n\nA **bright star** shines.\n")
	saveNote(t, s, "archive/a.md", "Same title")
	d, err := s.Comments(ctx, note.Path)
	if err != nil {
		t.Fatal(err)
	}
	anchored := d.Revision
	if d, err = s.AddComment(ctx, note.Path, "Check this passage", d.Revision, "reviewer", CommentOptions{Anchor: &CommentAnchor{Quote: "bright star", Prefix: "Heading A ", Suffix: " shines."}}); err != nil {
		t.Fatal(err)
	}
	if d, err = s.AddComment(ctx, note.Path, "Reply without editing", d.Revision, "second", CommentOptions{ReplyTo: d.Comments[0].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddComment(ctx, note.Path, "A second thread", d.Revision, "reviewer"); err != nil {
		t.Fatal(err)
	}
	saveNote(t, s, note.Path, "# Heading\n\nPassage removed.\n")
	saveNote(t, s, "broken.md", "# Broken\n")
	saveNote(t, s, "null.md", "# Null\n")
	commitFixture(t, s, fixture(commentPath("broken.md"), "not a discussion"), fixture(commentPath("null.md"), "null"))

	check := func(s *Store) {
		t.Helper()
		got, err := s.Comments(ctx, note.Path)
		if err != nil || got.Revision != headOf(t, s) || len(got.Comments) != 2 || got.Comments[0].Anchor == nil || got.Comments[0].Anchor.Quote != "bright star" || got.Comments[0].Anchor.Revision != anchored || len(got.Comments[0].Replies) != 1 {
			t.Errorf("the note's discussion: %+v %v", got, err)
		}
		if other, err := s.Comments(ctx, "archive/a.md"); err != nil || other.Comments == nil || len(other.Comments) != 0 || other.Revision != headOf(t, s) {
			t.Errorf("a note with the same basename: %+v %v", other, err)
		}
		if _, err := s.Comments(ctx, "broken.md"); errorText(err) != "invalid discussion file" {
			t.Error("an invalid discussion:", err)
		}
		for _, path := range []string{"notes", "../a.md"} {
			if _, err := s.Comments(ctx, path); err == nil {
				t.Errorf("the discussion of %s", path)
			}
		}
	}
	check(s)
	s = reopenTestStore(t, s)
	check(s)

	committed := commitFixture(t, s, fixture(commentPath("archive/a.md"), `[{"id":"x","author":"other gateway","text":"Committed elsewhere","created":"2026-11-22T00:00:00Z","resolved":false}]`))
	other, err := s.Comments(ctx, "archive/a.md")
	if err != nil || other.Revision != committed || len(other.Comments) != 1 || other.Comments[0].Author != "other gateway" {
		t.Errorf("a discussion another gateway committed: %+v %v", other, err)
	}
}

// Two gateways commenting on one discussion from the same revision have
// one winner, even before the loser hears of the winner's commit.
func TestDatabaseCommentsOnTwoGatewaysHaveOneWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	other, err := OpenDB(ctx, openTestNotes(t, testEnv(t, s)), "notes", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	note, err := s.Save(ctx, "note.md", []byte("Shared note"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	// The other gateway reads the discussion, so its read model is loaded.
	for {
		if seen, err := other.Comments(ctx, note.Path); err == nil && seen.Revision == note.Revision {
			break
		}
		time.Sleep(time.Millisecond)
	}
	first, err := s.AddComment(ctx, note.Path, "First", note.Revision, "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.AddComment(ctx, note.Path, "Second", note.Revision, "two"); !errors.Is(err, ErrConflict) {
		t.Fatal("a lost comment:", err)
	}
	if d, err := other.Comments(ctx, note.Path); err == nil && d.Revision == first.Revision && (len(d.Comments) != 1 || d.Comments[0].Text != "First") {
		t.Fatal("the discussion:", d)
	}
}
