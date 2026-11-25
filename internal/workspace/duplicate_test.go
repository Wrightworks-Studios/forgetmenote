package workspace

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestDuplicateFolderPreservesSourceAndCopiesProtection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for p, content := range map[string]string{
		"notes/a.md": "# Original\n\n[Sibling](b.md) [Outside](../outside.md)\n![Map](media/map.png)\n",
		"notes/b.md": "# Other\n", "notes/media/map.png": "image", "outside.md": "[Original](notes/a.md)",
	} {
		if _, err := s.Save(ctx, p, []byte(content), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	addDiscussion(t, s, "notes/a.md", Comment{ID: "c1", Author: "reader", Text: "Original discussion", Created: "2026-11-22T00:00:00Z"})
	lock := setLocks(t, s, "notes")
	copied, err := s.Duplicate(ctx, "notes", "copies/notes", lock, "owner")
	if err != nil {
		t.Fatal(err)
	}
	original, _ := s.Read(ctx, "notes/a.md", "")
	if !strings.Contains(string(original.Content), "../outside.md") {
		t.Fatal("source changed")
	}
	copy, _ := s.Read(ctx, "copies/notes/a.md", "")
	if !strings.Contains(string(copy.Content), "../../outside.md") || !strings.Contains(string(copy.Content), "[Sibling](b.md)") {
		t.Fatalf("copy links: %s", copy.Content)
	}
	outside, _ := s.Read(ctx, "outside.md", "")
	if string(outside.Content) != "[Original](notes/a.md)" {
		t.Fatal("inbound links changed")
	}
	comments, _ := s.Comments(ctx, copy.Path)
	if len(comments.Comments) != 0 {
		t.Fatal("copied discussion")
	}
	comments, _ = s.Comments(ctx, original.Path)
	if len(comments.Comments) != 1 {
		t.Fatal("lost discussion")
	}
	for _, p := range []string{"notes/a.md", "copies/notes/a.md"} {
		if locks := lockRulesOf(t, s, p); len(locks) != 1 {
			t.Fatalf("protection lost: %+v", locks)
		}
	}
	if history := entryHistory(t, s, copy.Path); len(history) != 1 {
		t.Fatalf("copy history: %+v", history)
	}
	state, _ := s.Status(ctx)
	if state.Dirty || state.Revision != copied.Revision {
		t.Fatal("unclean copy", state)
	}
}
func TestDuplicateNoteRejectsCollisionsStalePathsAndRollsBack(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, _ := s.Save(ctx, "a.md", []byte("# Original"), "", "test")
	for _, path := range []string{"a.md", "../escape.md", ".git/config", "a.md/child.md"} {
		if _, err := s.Duplicate(ctx, "a.md", path, doc.Revision, "owner"); err == nil {
			t.Fatal("invalid destination accepted", path)
		}
	}
	if _, err := s.Duplicate(ctx, "a.md", "b.md", strings.Repeat("a", 40), "owner"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale copy accepted", err)
	}
	release := failCommits(t, s)
	if _, err := s.Duplicate(ctx, "a.md", "b.md", doc.Revision, "owner"); err == nil {
		t.Fatal("expected commit failure")
	}
	release()
	state, _ := s.Status(ctx)
	if state.Dirty || state.Revision != doc.Revision {
		t.Fatal("failed copy left changes", state)
	}
	if _, err := s.Read(ctx, "b.md", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("copy survived failed commit", err)
	}
	if _, err := s.Duplicate(ctx, "a.md", "b.md", doc.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestMoveKeepsIdentityAndDuplicateMintsANewOne(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, err := NewNoteID()
	if err != nil {
		t.Fatal(err)
	}
	content, err := WithNoteID([]byte("# One\n"), id)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Save(ctx, "one.md", content, "", "author")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d.Content), "fmn-id:") {
		t.Fatalf("new note has no stable identity: %s", d.Content)
	}
	m, err := ParseMetadata(d.Content)
	if err != nil {
		t.Fatal(err)
	}
	_ = m
	moved, err := s.Move(ctx, "one.md", "renamed.md", d.Revision, "author")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(ctx, "renamed.md", "")
	if err != nil || string(got.Content) != string(d.Content) {
		t.Fatalf("rename lost identity: %v", err)
	}
	if _, err = s.Save(ctx, "collision.md", d.Content, "", "author"); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	copy, err := s.Duplicate(ctx, "renamed.md", "copy.md", moved.Revision, "author")
	if err != nil {
		t.Fatal(err)
	}
	copied, err := s.Read(ctx, "copy.md", copy.Revision)
	if err != nil || string(copied.Content) == string(d.Content) {
		t.Fatalf("copy reused identity: %v", err)
	}
}
