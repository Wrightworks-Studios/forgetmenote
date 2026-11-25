package workspace

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"forgetmenote/internal/notestore"
)

// observedHeadOf is the head s's watchers were last told of.
func observedHeadOf(s *Store) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observedHead
}

// waitObserved waits until s's watchers heard of head.
func waitObserved(t *testing.T, s *Store, head string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); observedHeadOf(s) != head; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("watchers heard of %q, not %q", observedHeadOf(s), head)
		}
	}
}

// While an operation holds one notebook's store, other notebooks' stores
// and the notes store's other watchers still hear new heads promptly; the
// busy store hears its own once it is free.
func TestOneBusyStoreDelaysNoOtherNotebooksHints(t *testing.T) {
	x := newTestStore(t)
	ctx := context.Background()
	y, err := CreateDB(ctx, x.noteStore, "other", filepath.Join(t.TempDir(), "other"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer y.Close()
	hints, cancel, err := y.Watch()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	raw := make(chan notestore.Head, 64)
	x.noteStore.Watch(func(notebook string, head notestore.Head) {
		if notebook == "other" {
			raw <- head
		}
	})
	x.mu.Lock()
	held := true
	defer func() {
		if held {
			x.mu.Unlock()
		}
	}()
	busy := commitFixture(t, x, fixture("x.md", "x"))
	time.Sleep(50 * time.Millisecond) // x's head reaches the dispatch first
	start := time.Now()
	head := commitFixture(t, y, fixture("y.md", "y"))
	select {
	case <-hints:
	case <-time.After(2 * time.Second):
		t.Fatal("y's watchers heard nothing while x's store was busy")
	}
	for got := (notestore.Head{}); got.Commit.String() != head; {
		select {
		case got = <-raw:
		case <-time.After(2 * time.Second):
			t.Fatal("the notes store's own watcher heard nothing of y while x's store was busy")
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("y's hints took %v while x's store was busy", elapsed)
	}
	held = false
	x.mu.Unlock()
	waitObserved(t, x, busy)
}

// A notebook deleted and created again while its store is busy is heard of
// as deleted before its new head, though the new head's sequence is lower
// than the heads the store heard of before; later heads reach its watchers.
func TestStoreHearsANotebookDeletedAndCreatedAgainWhileBusy(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var head string
	for i := range 4 {
		head = commitFixture(t, s, fixture("n.md", string(rune('a'+i))))
	}
	waitObserved(t, s, head)
	deleted := make(chan struct{}, 1)
	s.noteStore.Watch(func(notebook string, h notestore.Head) {
		if notebook == "notes" && h == (notestore.Head{}) {
			deleted <- struct{}{}
		}
	})
	s.mu.Lock()
	if err := DeleteDB(ctx, s.noteStore, "notes"); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	select {
	case <-deleted:
	case <-time.After(2 * time.Second):
		s.mu.Unlock()
		t.Fatal("the deletion was never dispatched while the store was busy")
	}
	if _, err := s.noteStore.Create(ctx, "notes", signature(systemIdentity, time.Now())); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	again := commitFixture(t, s, fixture("n.md", "again"))
	waitObserved(t, s, again)
}
