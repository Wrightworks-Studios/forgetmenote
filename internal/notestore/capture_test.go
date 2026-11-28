package notestore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
)

// blockHold is a capture's hold on the blocks: the sweep's lock on a
// connection of its own, and the backend that holds it.
type blockHold struct {
	t       *testing.T
	conn    *pgx.Conn
	backend int32
	unlock  func(context.Context) error
}

func holdBlocks(t *testing.T, e *env) *blockHold {
	t.Helper()
	pooled, err := e.db.Pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn := pooled.Hijack()
	t.Cleanup(func() { conn.Close(context.Background()) })
	h := &blockHold{t: t, conn: conn}
	if err := conn.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&h.backend); err != nil {
		t.Fatal(err)
	}
	if h.unlock, err = notestore.LockBlocks(e.ctx, conn, e.db.Org); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *blockHold) release() {
	h.t.Helper()
	if err := h.unlock(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

// What a snapshot taken before a purge uses stays in the blob store while a
// capture holds the blocks: the purge's rewrite commits, and its removal of
// the blocks it freed waits for the capture and then removes them.
func TestAPurgeWaitsForACaptureToRemoveTheBlocksItFreed(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	secret := newSecret(t)
	commits := leakyHistory(t, n, secret)
	head := commits[len(commits)-1]
	hold := holdBlocks(t, e)
	snapshot, err := hold.conn.BeginTx(e.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Rollback(context.Background())
	before, err := notestore.ReferencedBlocks(e.ctx, snapshot, e.db.Org)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(blockBytes(t, e, before), []byte(secret)) {
		t.Fatal("the snapshot's blocks do not hold the content the purge erases")
	}
	purged := make(chan error, 1)
	var result notestore.RewriteResult
	go func() {
		var err error
		result, err = n.Rewrite(e.ctx, 3, "purge-capture", redact(secret), notestore.RewriteOptions{ExpectHead: &head, Actor: "owner", Reason: "a leaked key", Scope: map[string]any{"kind": "text"}})
		purged <- err
	}()
	pgtest.WaitingFor(t, e.db, hold.backend)(1)
	select {
	case err := <-purged:
		t.Fatal("the purge removed blocks while a capture held them:", err)
	case <-time.After(200 * time.Millisecond):
	}
	if now, err := n.Head(e.ctx); err != nil || now.Commit == head {
		t.Fatalf("the purge's rewrite waited for the capture: %v, %v", now, err)
	}
	// Every block of the snapshot is still there, the purged ones included.
	if !bytes.Contains(blockBytes(t, e, before), []byte(secret)) {
		t.Fatal("the capture's snapshot lost the blocks the purge freed")
	}
	if err := snapshot.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	hold.release()
	if err := <-purged; err != nil || result.Removed.Blocks == 0 {
		t.Fatalf("the purge after the capture: removed %+v, %v", result.Removed, err)
	}
	if bytes.Contains(fileBytes(t, e.blobs.Path()), []byte(secret)) {
		t.Fatal("the purged content is still in the blob store")
	}
	after, err := notestore.ReferencedBlocks(e.ctx, e.db.Pool, e.db.Org)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blockBytes(t, e, after), []byte(secret)) {
		t.Fatal("after the purge the file versions still use blocks with the purged content")
	}
}

// A purge that waits longer for a capture than its removal's budget keeps
// its rewrite, says the purged content stays in the blob store until the
// scheduled sweep, and that sweep removes it once the capture ends.
func TestAPurgeThatOutwaitsACaptureLeavesItsBlocksToTheSweep(t *testing.T) {
	notestore.PurgeRemovalBudgetForTest(t, 300*time.Millisecond)
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	secret := newSecret(t)
	commits := leakyHistory(t, n, secret)
	head := commits[len(commits)-1]
	hold := holdBlocks(t, e)
	result, err := n.Rewrite(e.ctx, 3, "purge-outwaited", redact(secret), notestore.RewriteOptions{ExpectHead: &head, Actor: "owner", Reason: "a leaked key", Scope: map[string]any{"kind": "text"}})
	if err == nil || result.Head == (notestore.Head{}) || !strings.Contains(err.Error(), "until the scheduled block sweep removes it") || !strings.Contains(err.Error(), "instance capture") {
		t.Fatalf("a purge that outwaited a capture: %v, %v", result.Head, err)
	}
	if !bytes.Contains(fileBytes(t, e.blobs.Path()), []byte(secret)) {
		t.Fatal("the purge removed blocks while a capture held them")
	}
	hold.release()
	e.age()
	if r, swept, err := e.store.ScheduledSweepForTest(e.ctx); err != nil || !swept || r.Blocks == 0 {
		t.Fatalf("the sweep after the capture: %+v %v %v", r, swept, err)
	}
	if bytes.Contains(fileBytes(t, e.blobs.Path()), []byte(secret)) {
		t.Fatal("the sweep left the purged content in the blob store")
	}
	if got, want := read(t, n, result.Head.Commit, "keep.md"), "Unrelated, edited.\n"; got != want {
		t.Fatalf("the purged notebook reads %q", got)
	}
}

// No sweep runs while a capture holds the blocks, and a store whose sweep
// found them held tries again soon rather than the next day.
func TestASweepWaitsForACapture(t *testing.T) {
	notestore.ScheduleBlockSweepsForTest(t, 50*time.Millisecond, time.Hour, time.Hour, 10*time.Millisecond)
	notestore.RetryLockedBlockSweepsForTest(t, 100*time.Millisecond)
	e := newEnv(t, notestore.Options{})
	hold := holdBlocks(t, e)
	// A gateway that starts during the capture: its first sweep, 50 ms on,
	// finds the blocks held.
	e.open(e.db, notestore.Options{})
	n := e.create("notes")
	first := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "A.\n")}}).Head
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "B.\n")}})
	stale := first.Commit
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, ExpectHead: &stale, Files: []notestore.FileChange{write("book.md", manuscript(100))}}); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a write over a stale head:", err)
	}
	e.age()
	staged := len(e.blockFiles())
	if staged == 0 {
		t.Fatal("the refused write put no blocks")
	}
	if r, swept, err := e.store.ScheduledSweepForTest(e.ctx); err != nil || swept || r.Blocks != 0 {
		t.Fatalf("a sweep while a capture holds the blocks: %+v %v %v", r, swept, err)
	}
	time.Sleep(400 * time.Millisecond) // the scheduled sweep and its retries
	if got := len(e.blockFiles()); got != staged {
		t.Fatalf("a sweep removed %d blocks while a capture held them", staged-got)
	}
	hold.release()
	eventually(t, "no sweep ran soon after the capture ended", func() bool { return len(e.blockFiles()) == 0 })
}

// blockBytes reads the blocks from the blob store, checking each one's
// hash and size.
func blockBytes(t *testing.T, e *env, blocks []notestore.Block) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, b := range blocks {
		raw, err := os.ReadFile(filepath.Join(e.blobs.Path(), filepath.FromSlash(b.Key)))
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(raw)) != b.Size || sha256.Sum256(raw) != b.SHA256 {
			t.Fatalf("block %s does not match its hash and size", b.Key)
		}
		out.Write(raw)
	}
	return out.Bytes()
}
