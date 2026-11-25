package notestore_test

import (
	"errors"
	"testing"
	"time"

	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
)

// liveSum is what AuthorityWrite.ExpectLive names: the sum of the
// notebook's live documents' sequence numbers.
func liveSum(t *testing.T, e *env, n *notestore.Notebook) int64 {
	t.Helper()
	docs, err := n.LiveDocs(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, d := range docs {
		sum += d.LastSeq
	}
	return sum
}

// A live update fenced on the authority record is stored only while the
// record is at the version it names, and a refused one stores nothing.
func TestLiveAppendFencedOnTheAuthorityRecord(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	const entry = "0123456789abcdef0123456789abcdef"
	if seq, err := n.LiveAppendFenced(e.ctx, entry, 0, []byte("u1"), "alice", 0); err != nil || seq != 1 {
		t.Fatal("an update fenced on no record:", seq, err)
	}
	version, err := n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.LiveAppendFenced(e.ctx, entry, 0, []byte("late"), "alice", 0); !errors.Is(err, notestore.ErrAuthorityChanged) {
		t.Fatal("an update fenced on a replaced record:", err)
	}
	if seq, err := n.LiveAppendFenced(e.ctx, entry, 0, []byte("u2"), "alice", version); err != nil || seq != 2 {
		t.Fatal("an update fenced on the record:", seq, err)
	}
	st, err := n.LiveState(e.ctx, entry)
	if err != nil || len(st.Updates) != 2 || string(st.Updates[0].Update) != "u1" || string(st.Updates[1].Update) != "u2" {
		t.Fatalf("stored updates: %+v %v", st, err)
	}
	if _, err := e.store.Notebook("missing").LiveAppendFenced(e.ctx, entry, 0, []byte("x"), "alice", 0); err == nil {
		t.Fatal("an update stored for a notebook that does not exist")
	}
}

// An authority record that expects the live documents as its writer saw
// them is not stored once an update came after, and an update stored while
// the record is written waits for it: one is ordered before the other.
func TestAuthorityRecordsAndLiveUpdatesSeeEachOther(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	const entry = "0123456789abcdef0123456789abcdef"
	if _, err := n.LiveAppendFenced(e.ctx, entry, 0, []byte("u1"), "alice", 0); err != nil {
		t.Fatal(err)
	}
	seen := liveSum(t, e, n)
	if _, err := n.LiveAppendFenced(e.ctx, entry, 0, []byte("u2"), "alice", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{}`), ExpectLive: &seen}); !errors.Is(err, notestore.ErrLiveChanged) || !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a record over an update its writer did not see:", err)
	}
	if a, err := n.Authority(e.ctx); err != nil || a.Version != 0 {
		t.Fatal("the refused record was stored:", a, err)
	}
	seen = liveSum(t, e, n)
	waiting, release := pgtest.HoldWrites(t, e.db, "live_docs")
	appended := make(chan error, 1)
	go func() {
		_, err := n.LiveAppendFenced(e.ctx, entry, 0, []byte("u3"), "alice", 0)
		appended <- err
	}()
	waiting(1)
	stored := make(chan error, 1)
	go func() {
		_, err := n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{}`), ExpectLive: &seen})
		stored <- err
	}()
	// The record waits for the update, or, unfenced, is stored before it.
	var err error
	settled := false
	for deadline := time.Now().Add(10 * time.Second); !settled && pgtest.LockWaiters(t, e.db) < 2; time.Sleep(5 * time.Millisecond) {
		select {
		case err = <-stored:
			settled = true
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the record neither waits nor is stored")
		}
	}
	release()
	if !settled {
		err = <-stored
	}
	if err := <-appended; err != nil {
		t.Fatal("the update stored first:", err)
	}
	if !errors.Is(err, notestore.ErrLiveChanged) {
		t.Fatalf("a record written while an update was stored (before it: %t): %v", settled, err)
	}
	seen = liveSum(t, e, n)
	if _, err := n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{}`), ExpectLive: &seen}); err != nil {
		t.Fatal(err)
	}
}

// A live update that waits for an authority record being written reads the
// record once it is stored, and is refused by it.
func TestLiveUpdateWaitingForAnAuthorityRecordReadsIt(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	const entry = "0123456789abcdef0123456789abcdef"
	if _, err := n.LiveAppendFenced(e.ctx, entry, 0, []byte("u1"), "alice", 0); err != nil {
		t.Fatal(err)
	}
	waiting, release := pgtest.HoldWrites(t, e.db, "notebook_authority")
	stored := make(chan error, 1)
	go func() {
		_, err := n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{}`)})
		stored <- err
	}()
	waiting(1)
	appended := make(chan error, 1)
	go func() {
		_, err := n.LiveAppendFenced(e.ctx, entry, 0, []byte("late"), "alice", 0)
		appended <- err
	}()
	// The update waits for the record, or, unfenced, is stored before it.
	var err error
	settled := false
	for deadline := time.Now().Add(10 * time.Second); !settled && pgtest.LockWaiters(t, e.db) < 2; time.Sleep(5 * time.Millisecond) {
		select {
		case err = <-appended:
			settled = true
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the update neither waits nor is stored")
		}
	}
	release()
	if !settled {
		err = <-appended
	}
	if err := <-stored; err != nil {
		t.Fatal("the record stored first:", err)
	}
	if !errors.Is(err, notestore.ErrAuthorityChanged) {
		t.Fatalf("an update stored while a record was written (before it: %t): %v", settled, err)
	}
	if st, err := n.LiveState(e.ctx, entry); err != nil || len(st.Updates) != 1 {
		t.Fatalf("stored updates: %+v %v", st, err)
	}
}
