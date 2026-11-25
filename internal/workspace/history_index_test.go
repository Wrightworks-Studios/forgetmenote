package workspace

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Hold a historical note's content at the blob store, an external IO
// boundary. A normal save must finish while that read is still blocked, not
// merely run quickly on a small fixture after indexing has already finished.
func TestHistoryImmutableReadDoesNotBlockSave(t *testing.T) {
	s, blobs, reopen := newHistoryHeldStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Large enough to be kept in blocks, which a save of the next version
	// never reads.
	original := strings.Repeat("Original idea. ", 1024) + "\n"
	var initial Document
	keys := blobs.record(func() {
		var err error
		if initial, err = s.Save(ctx, "idea.md", []byte(original), "", "test"); err != nil {
			t.Fatal(err)
		}
	})
	if len(keys) == 0 {
		t.Fatal("the original idea is not kept in the blob store")
	}
	current, err := s.Save(ctx, "idea.md", []byte(strings.Repeat("Current idea. ", 1024)+"\n"), initial.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	s = reopen(s)
	entered, release := blobs.hold(t, keys)
	type result struct {
		batch HistoryBatch
		err   error
	}
	historyDone := make(chan result, 1)
	go func() { batch, err := s.HistoryNotes(ctx, initial.Revision, 0, nil); historyDone <- result{batch, err} }()
	select {
	case <-entered:
	case r := <-historyDone:
		t.Fatalf("history did not reach the blob store: %v", r.err)
	case <-ctx.Done():
		t.Fatal("history did not enter blob store IO")
	}
	saveDone := make(chan error, 1)
	go func() {
		_, err := s.Save(ctx, "idea.md", []byte("Updated idea\n"), current.Revision, "test")
		saveDone <- err
	}()
	select {
	case err = <-saveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("save waited on immutable history IO")
	}
	release()
	select {
	case r := <-historyDone:
		if r.err != nil || len(r.batch.Files) != 1 || r.batch.Files[0].Content != original {
			t.Fatalf("historical snapshot changed during save: %+v %v", r.batch, r.err)
		}
	case <-ctx.Done():
		t.Fatal("historical read failed to finish")
	}
	doc, err := s.Read(ctx, "idea.md", "")
	if err != nil || string(doc.Content) != "Updated idea\n" {
		t.Fatalf("save lost: %v", err)
	}
}

// A commit that adds many notes gives all of them in one batch, in path
// order: the history index visits every commit when a notebook is first
// searched.
func TestHistoryNotesSizesCommitPostimagesTogether(t *testing.T) {
	s := newTestStore(t)
	changes := map[string][]byte{}
	for n := 0; n < 5; n++ {
		changes[fmt.Sprintf("note-%d.md", n)] = []byte(fmt.Sprintf("Synthetic note %d\n", n))
	}
	added := historyCommit(t, t.Context(), s, "Add synthetic notes", nil, changes)
	batch, err := s.HistoryNotes(t.Context(), added, 0, nil)
	if err != nil || !batch.Done || len(batch.Files) != 5 {
		t.Fatalf("history batch: %+v %v", batch, err)
	}
	for n, file := range batch.Files {
		if file.Path != fmt.Sprintf("note-%d.md", n) || file.Content != fmt.Sprintf("Synthetic note %d\n", n) {
			t.Fatalf("historical note %d: %+v", n, file)
		}
	}
}

func TestHistoryScopes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, path := range []string{"one/a.md", "one/b.md", "two/a.md"} {
		if _, err := s.Save(ctx, path, []byte("note"), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	note, err := s.History(ctx, "one/a.md")
	if err != nil || len(note) != 1 {
		t.Fatalf("note history: %d %v", len(note), err)
	}
	folder, err := s.History(ctx, "one")
	if err != nil || len(folder) != 2 {
		t.Fatalf("folder history: %d %v", len(folder), err)
	}
	project, err := s.History(ctx, "")
	if err != nil || len(project) < 3 {
		t.Fatalf("project history: %d %v", len(project), err)
	}
}

// A scoped history reads each version's content outside the notebook's write
// lock: a save finishes while that read is held at the blob store, and the
// history still lists the versions up to the head it saw.
func TestCommittedHistoryDoesNotHoldNotebookWriteLock(t *testing.T) {
	s, blobs, reopen := newHistoryHeldStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var first Document
	keys := blobs.record(func() {
		var err error
		if first, err = s.Save(ctx, "read.md", []byte(strings.Repeat("committed snapshot ", 1024)), "", ""); err != nil {
			t.Fatal(err)
		}
	})
	second, err := s.Save(ctx, "read.md", []byte(strings.Repeat("second snapshot ", 1024)), first.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	s = reopen(s)
	entered, release := blobs.hold(t, keys)
	type result struct {
		history []Commit
		err     error
	}
	readDone := make(chan result, 1)
	go func() {
		history, err := s.HistoryScoped(ctx, "read.md", historyScope)
		readDone <- result{history, err}
	}()
	select {
	case <-entered:
	case r := <-readDone:
		t.Fatalf("history did not reach the blob store: %v", r.err)
	case <-ctx.Done():
		t.Fatal("history did not enter blob store IO")
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := s.Save(ctx, "parallel.md", []byte("committed snapshot parallel save\n"), "", "")
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("save waited on immutable history IO")
	}
	release()
	r := <-readDone
	if r.err != nil || len(r.history) != 2 || r.history[0].Revision != second.Revision || r.history[1].Revision != first.Revision {
		t.Fatal("read did not preserve its captured revision", r.history, r.err)
	}
}
