package workspace

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestCommentDeletionFailurePreservesDiscussionAndGit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	note, err := store.Save(ctx, "note.md", []byte("Keep this note"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.AddComment(ctx, note.Path, "Keep unless deletion commits", note.Revision, "author")
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.Read(ctx, commentPath(note.Path), "")
	if err != nil {
		t.Fatal(err)
	}
	restore := failCommits(t, store)
	if _, err := store.DeleteComment(ctx, note.Path, d.Comments[0].ID, "", d.Revision, "author", false); err == nil {
		t.Fatal("deletion succeeded despite a failed commit")
	}
	restore()
	latest, err := store.Comments(ctx, note.Path)
	if err != nil || !reflect.DeepEqual(latest, d) {
		t.Fatalf("failed deletion changed discussion: %+v %v", latest, err)
	}
	state, err := store.Status(ctx)
	if err != nil || state.Dirty || state.Revision != d.Revision {
		t.Fatalf("failed deletion changed history: %+v %v", state, err)
	}
	if again, err := reopenTestStore(t, store).Comments(ctx, note.Path); err != nil || !reflect.DeepEqual(again, d) {
		t.Fatalf("the discussion after a restart: %+v %v", again, err)
	}
	latest, err = store.DeleteComment(ctx, note.Path, d.Comments[0].ID, "", d.Revision, "author", false)
	if err != nil || len(latest.Comments) != 0 || latest.Revision == d.Revision {
		t.Fatalf("delete retry: %+v %v", latest, err)
	}
	historical, err := store.Read(ctx, commentPath(note.Path), d.Revision)
	if err != nil || !bytes.Equal(historical.Content, original.Content) {
		t.Fatalf("deletion rewrote history: %q %v", historical.Content, err)
	}
}

func TestCommentDeletionConcurrentReplyHasOneWinner(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	note, err := store.Save(ctx, "note.md", []byte("Shared note"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.AddComment(ctx, note.Path, "Parent", note.Revision, "author")
	if err != nil {
		t.Fatal(err)
	}
	id := d.Comments[0].ID
	d, err = store.AddComment(ctx, note.Path, "Existing reply", d.Revision, "other", CommentOptions{ReplyTo: id})
	if err != nil {
		t.Fatal(err)
	}
	kept := d.Comments[0].Replies[0]
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := store.DeleteComment(ctx, note.Path, id, "", d.Revision, "author", false)
		results <- err
	}()
	go func() {
		<-start
		_, err := store.AddComment(ctx, note.Path, "Concurrent reply", d.Revision, "second", CommentOptions{ReplyTo: id})
		results <- err
	}()
	close(start)
	wins, conflicts := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("concurrent results: %d successes, %d conflicts", wins, conflicts)
	}
	latest, err := store.Comments(ctx, note.Path)
	if err != nil || len(latest.Comments) != 1 || len(latest.Comments[0].Replies) < 1 {
		t.Fatalf("lost thread: %+v %v", latest, err)
	}
	c := latest.Comments[0]
	if !reflect.DeepEqual(c.Replies[0], kept) {
		t.Fatalf("lost another author's existing reply: %+v", c)
	}
	if c.Deleted {
		if c.Text != "" || len(c.Replies) != 1 {
			t.Fatalf("delete winner lost or accepted stale content: %+v", c)
		}
	} else if c.Text != "Parent" || len(c.Replies) != 2 || c.Replies[1].Text != "Concurrent reply" {
		t.Fatalf("reply winner lost content: %+v", c)
	}
	historical, err := store.Read(ctx, commentPath(note.Path), d.Revision)
	if err != nil || !bytes.Contains(historical.Content, []byte("Parent")) || !bytes.Contains(historical.Content, []byte("Existing reply")) {
		t.Fatalf("concurrent write lost prior history: %q %v", historical.Content, err)
	}
}

// Deleting a finding removes its current content and its place in the
// findings inbox, and keeps its replies and its history.
func TestDeleteFindingRemovesCurrentContentWithoutLosingRepliesOrHistory(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	note, err := store.Save(ctx, "note.md", []byte("Review this note"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	finding, err := store.AddFinding(ctx, FindingInput{Key: "synthetic-finding", Path: note.Path, Title: "Finding title", Text: "Finding details", Revision: note.Revision, Hash: ContentHash(note.Content)}, "author")
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.AddComment(ctx, note.Path, "Keep this reply", finding.Revision, "other", CommentOptions{ReplyTo: finding.Comment.ID})
	if err != nil {
		t.Fatal(err)
	}
	before := d.Revision
	d, err = store.DeleteComment(ctx, note.Path, finding.Comment.ID, "", d.Revision, "author", false)
	if err != nil || len(d.Comments) != 1 || !d.Comments[0].Deleted || d.Comments[0].Finding != nil || d.Comments[0].Text != "" || len(d.Comments[0].Replies) != 1 || d.Comments[0].Replies[0].Text != "Keep this reply" {
		t.Fatalf("finding delete: %+v %v", d, err)
	}
	inbox, err := store.Findings(ctx)
	if err != nil || len(inbox) != 0 {
		t.Fatalf("deleted finding remains in inbox: %+v %v", inbox, err)
	}
	current, err := store.Read(ctx, commentPath(note.Path), "")
	if err != nil || bytes.Contains(current.Content, []byte("Finding details")) || bytes.Contains(current.Content, []byte("Finding title")) {
		t.Fatalf("current finding content retained: %q %v", current.Content, err)
	}
	historical, err := store.Read(ctx, commentPath(note.Path), before)
	if err != nil || !bytes.Contains(historical.Content, []byte("Finding title")) || !bytes.Contains(historical.Content, []byte("Finding details")) {
		t.Fatalf("finding history lost: %q %v", historical.Content, err)
	}
}
