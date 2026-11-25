package workspace

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateCommentText = flag.Bool("update-comment-text", false, "rewrite testdata/comment-text goldens from commentText, for an intended change of behaviour only")

func TestAnchoredCommentsAndReplies(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	source := []byte("---\ntitle: Private metadata\n---\n# Heading\n\nA **bright star** shines.\n")
	note, err := s.Save(ctx, "notes/a.md", source, "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.AddComment(ctx, note.Path, "Check this passage", note.Revision, "reviewer", CommentOptions{Anchor: &CommentAnchor{Quote: "bright star", Prefix: "Heading A ", Suffix: " shines."}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Comments[0].Anchor == nil || d.Comments[0].Anchor.Revision != note.Revision {
		t.Fatal("anchor lost its original revision")
	}
	d, err = s.AddComment(ctx, note.Path, "Reply without editing", d.Revision, "second", CommentOptions{ReplyTo: d.Comments[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Comments) != 1 || len(d.Comments[0].Replies) != 1 {
		t.Fatal("reply became a separate thread")
	}
	if _, err = s.AddComment(ctx, note.Path, "Bad", d.Revision, "reviewer", CommentOptions{Anchor: &CommentAnchor{Quote: "not in this note"}}); err == nil {
		t.Fatal("accepted nonexistent passage")
	}
	if _, err = s.AddComment(ctx, note.Path, "Bad", d.Revision, "reviewer", CommentOptions{ReplyTo: "missing"}); err == nil {
		t.Fatal("accepted nonexistent thread")
	}
	got, err := s.Read(ctx, note.Path, "")
	if err != nil || string(got.Content) != string(source) {
		t.Fatal("annotations changed note text")
	}
	changed, err := s.Save(ctx, note.Path, []byte("# Heading\n\nPassage removed.\n"), got.Revision, "writer")
	if err != nil {
		t.Fatal(err)
	}
	d, err = s.Comments(ctx, note.Path)
	if err != nil || d.Comments[0].Anchor.Quote != "bright star" || d.Revision != changed.Revision {
		t.Fatal("lost original anchor after external edit")
	}
}
func TestCommentTextProjection(t *testing.T) {
	content := []byte("---\ntitle: Hidden\n---\n# H\n\nA **bold** [link](a.md).\n\n| One | Two |\n| --- | --- |\n| A | B |\n\n```\nx < y\n```\n")
	text := commentText(content)
	if text != "H A bold link. One Two A B x < y" {
		t.Fatalf("unexpected projection: %q", text)
	}
	if strings.Contains(text, "Hidden") {
		t.Fatal("frontmatter leaked into passage text")
	}
}

// The note editor finds and makes passage anchors in its own rendering of
// the note (frontend/rendered-text.mjs), which must read every corpus note
// as commentText does or the anchors it makes are refused here. Its test
// (internal/web/ui/rendered-text.test.mjs) reads these same goldens.
func TestCommentTextCorpus(t *testing.T) {
	notes, err := filepath.Glob(filepath.Join("testdata", "comment-text", "*.md"))
	if err != nil || len(notes) == 0 {
		t.Fatalf("no comment-text corpus: %v", err)
	}
	for _, note := range notes {
		content, err := os.ReadFile(note)
		if err != nil {
			t.Fatal(err)
		}
		golden := strings.TrimSuffix(note, ".md") + ".txt"
		got := commentText(content)
		if *updateCommentText {
			if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s: commentText = %q, golden %q", note, got, want)
		}
	}
}
