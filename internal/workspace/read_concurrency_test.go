package workspace

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
)

// heldBlobs is a blob store whose next content block read, once armed,
// waits until it is released, as a slow disk or object store would.
type heldBlobs struct {
	blob.Store
	armed    atomic.Bool
	entered  chan struct{}
	released chan struct{}
}

// arm holds the next block read; entered closes once it waits.
func (h *heldBlobs) arm() (entered <-chan struct{}, release func()) {
	h.entered, h.released = make(chan struct{}), make(chan struct{})
	h.armed.Store(true)
	var once atomic.Bool
	released := h.released
	return h.entered, func() {
		if once.CompareAndSwap(false, true) {
			close(released)
		}
	}
}

func (h *heldBlobs) wait(ctx context.Context, key string) error {
	if !strings.HasPrefix(key, "blocks/") || !h.armed.CompareAndSwap(true, false) {
		return nil
	}
	close(h.entered)
	select {
	case <-h.released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *heldBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := h.wait(ctx, key); err != nil {
		return nil, err
	}
	return h.Store.Get(ctx, key)
}

func (h *heldBlobs) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if err := h.wait(ctx, key); err != nil {
		return nil, err
	}
	return h.Store.GetRange(ctx, key, offset, length)
}

// newHeldTestStore is newTestStore over a blob store whose reads a test can
// hold. reopen opens the notebook again with cold caches, so that its next
// read of a note larger than the store keeps inline reaches the blob store.
func newHeldTestStore(t *testing.T) (s *Store, held *heldBlobs, reopen func() *Store) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.OpenDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	held = &heldBlobs{Store: blobs}
	db := pgtest.Org(t)
	notes := func() *notestore.Store {
		ns, err := notestore.Open(context.Background(), db, held, notestore.Options{ImportEntryID: NoteEntryID})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(ns.Close)
		return ns
	}
	state := filepath.Join(dir, "projects", "notes")
	if s, err = CreateDB(context.Background(), notes(), "notes", state, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, held, func() *Store {
		s.Close()
		again, err := OpenDB(context.Background(), notes(), "notes", state, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { again.Close() })
		return again
	}
}

// committedSnapshotText is a note too large for the store to keep inline,
// so reading it reads the blob store.
var committedSnapshotText = "committed snapshot\n\n" + strings.Repeat("Padding past the inline limit.\n", 400)

func TestCommittedReadDoesNotHoldNotebookWriteLock(t *testing.T) {
	assertHeldReadAllowsSave(t, func(s *Store, doc Document) error {
		actual, err := s.Read(t.Context(), doc.Path, "")
		if err == nil && (string(actual.Content) != string(doc.Content) || actual.Revision != doc.Revision) {
			return ErrConflict
		}
		return err
	})
}
func TestCommittedListDoesNotHoldNotebookWriteLock(t *testing.T) {
	assertHeldReadAllowsSave(t, func(s *Store, doc Document) error {
		actual, err := s.List(t.Context())
		if err == nil && (len(actual) != 1 || actual[0].Path != doc.Path) {
			return ErrConflict
		}
		return err
	})
}
func TestCommittedFindDoesNotHoldNotebookWriteLock(t *testing.T) {
	assertHeldReadAllowsSave(t, func(s *Store, doc Document) error {
		actual, err := s.Find(t.Context(), "snapshot", "", "", "")
		if err == nil && (len(actual) != 1 || actual[0].Path != doc.Path) {
			return ErrConflict
		}
		return err
	})
}

// Hold the first content read; later reads by the writer continue. This
// proves lock concurrency and fixed-revision results together.
func assertHeldReadAllowsSave(t *testing.T, read func(*Store, Document) error) {
	t.Helper()
	s, held, reopen := newHeldTestStore(t)
	ctx := t.Context()
	doc, err := s.Save(ctx, "read.md", []byte(committedSnapshotText), "", "")
	if err != nil {
		t.Fatal(err)
	}
	s = reopen()
	entered, release := held.arm()
	t.Cleanup(release)
	readDone := make(chan error, 1)
	go func() { readDone <- read(s, doc) }()
	select {
	case <-entered:
	case err := <-readDone:
		t.Fatal("immutable read never reached the blob store", err)
	case <-time.After(3 * time.Second):
		release()
		<-readDone
		t.Fatal("immutable read never reached the blob store")
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := s.Save(ctx, "parallel.md", []byte("committed snapshot parallel save\n"), "", "")
		writeDone <- err
	}()
	blocked := false
	select {
	case err := <-writeDone:
		if err != nil {
			release()
			<-readDone
			t.Fatal(err)
		}
	case <-time.After(750 * time.Millisecond):
		blocked = true
	}
	release()
	if blocked {
		if err := <-writeDone; err != nil {
			t.Error(err)
		}
	}
	if err := <-readDone; err != nil {
		t.Fatal("read did not preserve its captured revision", err)
	}
	if blocked {
		t.Fatal("immutable content read blocked an unrelated notebook save")
	}
	// The older read must not have left its listing in place of the newer.
	if entries, err := s.List(ctx); err != nil || len(entries) != 2 {
		t.Fatal("listing after the parallel save:", entries, err)
	}
}
