package notestore_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

// faultyBlobs is a blob store whose deletes fail while fail is set, and
// which calls onDelete, if set, before each delete.
type faultyBlobs struct {
	blob.Store
	fail     atomic.Bool
	onDelete func()
}

func (b *faultyBlobs) Delete(ctx context.Context, key string) error {
	if b.onDelete != nil {
		b.onDelete()
	}
	if b.fail.Load() {
		return errors.New("injected: blob store unavailable")
	}
	return b.Store.Delete(ctx, key)
}

// sweepEnv is a store over a faulty blob directory that runs the scheduled
// block sweep every 50 ms.
func sweepEnv(t *testing.T) (*env, *faultyBlobs) {
	t.Helper()
	notestore.ScheduleBlockSweepsForTest(t, 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond)
	return faultyEnv(t)
}

// faultyEnv is a store over a faulty blob directory.
func faultyEnv(t *testing.T) (*env, *faultyBlobs) {
	t.Helper()
	db := pgtest.Org(t)
	dir, err := blob.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dir.Close() })
	blobs := &faultyBlobs{Store: dir}
	s, err := notestore.Open(context.Background(), db, blobs, notestore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return &env{t: t, ctx: context.Background(), db: db, blobs: dir, store: s}, blobs
}

// age makes every block and segment older than the sweep's grace period, as
// the hours before a daily sweep do.
func (e *env) age() {
	e.t.Helper()
	for _, table := range []string{"content_blocks", "content_segments"} {
		if _, err := e.db.Pool.Exec(e.ctx, `UPDATE `+table+` SET touched = touched - interval '1 hour' WHERE org = $1`, e.db.Org); err != nil {
			e.t.Fatal(err)
		}
	}
}

// eventually fails unless done holds within 15 seconds.
func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !done(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

func count(t *testing.T, db *pg.DB, table string) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE org = $1`, db.Org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A purge whose removal of the blocks it freed failed keeps the rewrite and
// says the purged content is still stored until the scheduled sweep, which
// then removes it, and nothing the notebook still uses.
func TestTheScheduledSweepRemovesTheBlocksAFailedPurgeLeft(t *testing.T) {
	e, blobs := sweepEnv(t)
	n := e.create("notes")
	secret := newSecret(t)
	commits := leakyHistory(t, n, secret)
	head := commits[len(commits)-1]
	book := read(t, n, head, "big.md")
	blobs.fail.Store(true)
	result, err := n.Rewrite(e.ctx, 3, "purge-sweep", redact(secret), notestore.RewriteOptions{ExpectHead: &head, Actor: "owner", Reason: "a leaked key", Scope: map[string]any{"kind": "text"}})
	if err == nil || result.Head == (notestore.Head{}) || !strings.Contains(err.Error(), "still in the blob store until the scheduled block sweep removes it") {
		t.Fatalf("a purge whose block removal failed: %v, %v", result.Head, err)
	}
	if !bytes.Contains(fileBytes(t, e.blobs.Path()), []byte(secret)) {
		t.Fatal("the failed removal removed the purged blocks")
	}
	blobs.fail.Store(false)
	e.age()
	eventually(t, "the scheduled sweep left the purged content in the blob store", func() bool {
		return !bytes.Contains(fileBytes(t, e.blobs.Path()), []byte(secret))
	})
	cold := e.open(e.db, notestore.Options{}).Notebook("notes")
	if got := read(t, cold, result.Head.Commit, "big.md"); got != book {
		t.Fatal("the sweep removed blocks the purged notebook uses")
	}
	if bytes.Contains(orgBytes(t, e.db), []byte(secret)) {
		t.Fatal("the database still holds the purged content")
	}
}

// A purge whose request ends once its rewrite committed still removes the
// blocks it freed, and a removal that fails once is tried again.
func TestAPurgeRemovesItsBlocksAfterItsRequestEnds(t *testing.T) {
	e, blobs := sweepEnv(t)
	n := e.create("notes")
	secret := newSecret(t)
	commits := leakyHistory(t, n, secret)
	head := commits[len(commits)-1]
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	var once sync.Once
	blobs.onDelete = func() {
		once.Do(func() {
			cancel() // the client disconnects
			blobs.fail.Store(true)
			time.AfterFunc(5*time.Millisecond, func() { blobs.fail.Store(false) })
		})
	}
	result, err := n.Rewrite(ctx, 3, "purge-ended", redact(secret), notestore.RewriteOptions{ExpectHead: &head, Actor: "owner", Reason: "a leaked key", Scope: map[string]any{"kind": "text"}})
	blobs.onDelete = nil
	if err != nil || result.Removed.Blocks == 0 {
		t.Fatalf("a purge whose request ended after its rewrite: removed %+v, %v", result.Removed, err)
	}
	if bytes.Contains(fileBytes(t, e.blobs.Path()), []byte(secret)) {
		t.Fatal("the purged content is still in the blob store")
	}
}

// A purge's rewrite holds the notebook's write lock, and its removal of the
// blocks it freed, after the rewrite committed, does not: a blob store that
// hangs then keeps no write to the notebook waiting.
func TestAPurgesBlockRemovalKeepsNoWriteWaiting(t *testing.T) {
	e, blobs := faultyEnv(t)
	n := e.create("notes")
	secret := newSecret(t)
	commits := leakyHistory(t, n, secret)
	head := commits[len(commits)-1]
	held, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	blobs.onDelete = func() { once.Do(func() { close(held); <-resume }) }
	var resumed sync.Once
	defer resumed.Do(func() { close(resume) })
	purged := make(chan error, 1)
	var result notestore.RewriteResult
	go func() {
		var err error
		result, err = n.Rewrite(e.ctx, 3, "purge-hangs", redact(secret), notestore.RewriteOptions{ExpectHead: &head, Actor: "owner", Reason: "a leaked key", Scope: map[string]any{"kind": "text"}})
		purged <- err
	}()
	select {
	case <-held:
	case err := <-purged:
		t.Fatal("the purge removed no block:", err)
	}
	wrote := make(chan error, 1)
	go func() {
		_, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "Update after.md", Author: ada, Files: []notestore.FileChange{write("after.md", "After the purge.\n")}})
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal("a write while the purge removed its blocks:", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a write waited for the purge's removal of the blocks it freed")
	}
	resumed.Do(func() { close(resume) })
	if err := <-purged; err != nil || result.Removed.Blocks == 0 {
		t.Fatalf("the purge: removed %+v, %v", result.Removed, err)
	}
	if bytes.Contains(fileBytes(t, e.blobs.Path()), []byte(secret)) {
		t.Fatal("the purged content is still in the blob store")
	}
}

// Deleting a notebook asks for a block sweep soon after it, rather than at
// the next daily one, which removes the blocks only the notebook used and
// keeps those another notebook uses.
func TestADeleteIsSweptSoonAfter(t *testing.T) {
	notestore.ScheduleBlockSweepsForTest(t, time.Hour, time.Hour, 300*time.Millisecond, 10*time.Millisecond)
	e := newEnv(t, notestore.Options{})
	kept, doomed := e.create("kept"), e.create("doomed")
	shared := manuscript(200)
	apply(t, kept, notestore.ChangeSet{Files: []notestore.FileChange{write("shared.md", shared)}})
	before := len(e.blockFiles())
	apply(t, doomed, notestore.ChangeSet{Files: []notestore.FileChange{write("shared.md", shared), write("own.md", manuscript(300))}})
	if len(e.blockFiles()) == before {
		t.Fatal("the notebook to delete put no blocks of its own")
	}
	if err := e.store.Delete(e.ctx, "doomed"); err != nil {
		t.Fatal(err)
	}
	e.age() // past the grace period by the time the sweep it asked for runs
	eventually(t, "no sweep removed the deleted notebook's blocks", func() bool { return len(e.blockFiles()) == before })
	if got := read(t, e.open(e.db, notestore.Options{}).Notebook("kept"), zero, "shared.md"); got != shared {
		t.Fatal("the kept notebook lost its content")
	}
	if rows := count(t, e.db, "content_blocks"); rows != before {
		t.Fatalf("%d block rows for %d blocks", rows, before)
	}
}

// The blocks a write put before it was refused are swept once no write can
// still use them, and never while one may.
func TestTheScheduledSweepRemovesARefusedWritesBlocksOnlyOnceOld(t *testing.T) {
	e, _ := sweepEnv(t)
	n := e.create("notes")
	first := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "A.\n")}}).Head
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "B.\n")}})
	stale := first.Commit
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, ExpectHead: &stale, Files: []notestore.FileChange{write("book.md", manuscript(300))}}); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a write over a stale head:", err)
	}
	staged := len(e.blockFiles())
	if staged == 0 {
		t.Fatal("the refused write put no blocks")
	}
	time.Sleep(300 * time.Millisecond) // several scheduled sweeps
	if got := len(e.blockFiles()); got != staged {
		t.Fatalf("the sweep removed %d recent blocks a write may still use", staged-got)
	}
	e.age()
	eventually(t, "the scheduled sweep left a refused write's blocks", func() bool { return len(e.blockFiles()) == 0 })
}

// Of two gateways that sweep at once, one sweeps and the other leaves it.
func TestOneGatewayAtATimeSweeps(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	first := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "A.\n")}}).Head
	stale := first.Commit
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "B.\n")}})
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, ExpectHead: &stale, Files: []notestore.FileChange{write("book.md", manuscript(100))}}); err == nil {
		t.Fatal("a write over a stale head succeeded")
	}
	e.age()
	gateway, _ := pgtest.Gateway(t, e.db)
	held := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	slow := &faultyBlobs{Store: e.blobs, onDelete: func() {
		once.Do(func() { close(held); <-resume })
	}}
	a, err := notestore.Open(e.ctx, e.db, slow, notestore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	b := e.open(gateway, notestore.Options{})
	type outcome struct {
		result notestore.SweepResult
		swept  bool
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, swept, err := a.ScheduledSweepForTest(e.ctx)
		done <- outcome{r, swept, err}
	}()
	<-held
	if r, swept, err := b.ScheduledSweepForTest(e.ctx); err != nil || swept || r.Blocks != 0 {
		t.Fatalf("the second gateway swept while the first did: %+v %v %v", r, swept, err)
	}
	close(resume)
	if got := <-done; got.err != nil || !got.swept || got.result.Blocks == 0 {
		t.Fatalf("the first gateway's sweep: %+v", got)
	}
	if left := len(e.blockFiles()); left != 0 {
		t.Fatalf("%d blocks left", left)
	}
}
