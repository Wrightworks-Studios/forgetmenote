package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/pg/pgtest"

	"github.com/reearth/ygo/crdt"
)

// liveFenceGateways has gateway a own note.md and gateway b hold it live
// for a browser tab, as two gateways of one instance do, with nothing
// pending.
func liveFenceGateways(t *testing.T) (a, b *Store, tab *liveTab, ctx context.Context) {
	t.Helper()
	a, ctx, _ = liveTestStore(t)
	b = openGateway(t, a)
	first, err := b.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	return a, b, newLiveTab(t, "tab", first), ctx
}

// typed is tab's next key, typed at the end of its text, as the tab sends
// it; the tab learns of the server's answer only through receive.
func (b *liveTab) typed(c string) LiveUpdate {
	text := b.doc.GetText("markdown")
	b.doc.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, text.Len(), c, nil) })
	b.keys++
	return LiveUpdate{Path: b.held.Path, Generation: strings.Split(b.held.LiveRevision, ":")[0], OperationID: fmt.Sprintf("%s-%d", b.name, b.keys), Update: crdt.EncodeStateAsUpdateV1(b.doc, b.server.StateVector())}
}

// running runs fn on its own goroutine, as another request would.
func running(fn func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	return done
}

// settledOrWaiting waits until done has fn's result, which it returns, or
// until n statements of s's database wait for a lock.
func settledOrWaiting(t *testing.T, s *Store, done <-chan error, n int) (err error, settled bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		select {
		case err := <-done:
			return err, true
		default:
		}
		if pgtest.LockWaiters(t, testEnv(t, s).db) >= n {
			return nil, false
		}
	}
	t.Fatalf("neither settled nor %d statements waiting for a lock", n)
	return nil, false
}

// livePendingIn reports whether s's notes store holds live edits waiting for
// a checkpoint, whichever gateway accepted them.
func livePendingIn(t *testing.T, s *Store) bool {
	t.Helper()
	docs, err := liveDB(s).notes.LiveDocs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.LastSeq != d.SnapshotSeq || d.Snapshot && d.Base.IsZero() {
			return true
		}
	}
	return false
}

// A live update another gateway checked before a handoff was prepared, and
// stores while the handoff's record is being written, is refused as any
// write is while a handoff is prepared; the handoff completes without it,
// and nothing is left that could never be checkpointed.
func TestLiveUpdateCheckedBeforeAHandoffIsRefusedByItsPreparation(t *testing.T) {
	a, b, tab, ctx := liveFenceGateways(t)
	head := headOf(t, a)
	waiting, release := pgtest.HoldWrites(t, testEnv(t, a).db, "notebook_authority")
	prepared := running(func() error {
		_, err := a.PrepareAuthority(ctx, AuthorityRequest{ID: filingHandoff, Generation: 1, From: "forgetmenote", To: "github", Revision: head})
		return err
	})
	waiting(1)
	in := tab.typed("!")
	typed := running(func() error {
		_, err := b.LiveApply(tab.answer(ctx), in)
		return err
	})
	err, settled := settledOrWaiting(t, a, typed, 2)
	release()
	if !settled {
		err = <-typed
	}
	if perr := <-prepared; perr != nil {
		t.Fatal("prepare:", perr)
	}
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("a live update checked before the handoff was stored after its record (settled while it was written: %t): %v", settled, err)
	}
	if livePendingIn(t, a) {
		t.Fatal("a prepared handoff leaves live edits pending")
	}
	state, err := a.FinishAuthority(ctx, filingHandoff, true)
	if err != nil || state.Type != "github" || state.Committed != head {
		t.Fatalf("handoff: %+v %v", state, err)
	}
	if doc, err := reopenTestStore(t, a).Read(ctx, "note.md", ""); err != nil || strings.Contains(string(doc.Content), "!") {
		t.Fatalf("the GitHub notebook holds %q, %v", doc.Content, err)
	}
}

// A live update another gateway stores while a handoff is being prepared
// lands before the handoff's record or is refused by it; one that lands
// first refuses the handoff as pending live edits do, so it is never handed
// over unwritten, and it is checkpointed as usual.
func TestLiveUpdateStoredWhileAHandoffIsPreparedRefusesTheHandoff(t *testing.T) {
	a, b, tab, ctx := liveFenceGateways(t)
	head := headOf(t, a)
	waiting, release := pgtest.HoldWrites(t, testEnv(t, a).db, "live_docs")
	in := tab.typed("!")
	var accepted LiveDocument
	typed := running(func() (err error) {
		accepted, err = b.LiveApply(tab.answer(ctx), in)
		return err
	})
	waiting(1)
	prepared := running(func() error {
		_, err := a.PrepareAuthority(ctx, AuthorityRequest{ID: filingHandoff, Generation: 1, From: "forgetmenote", To: "github", Revision: head})
		return err
	})
	perr, settled := settledOrWaiting(t, a, prepared, 2)
	release()
	if !settled {
		perr = <-prepared
	}
	err := <-typed
	switch {
	case err == nil && perr == nil:
		t.Fatalf("the handoff was prepared over a live update it never saw (prepared while the update was stored: %t)", settled)
	case err == nil && !errors.Is(perr, ErrLivePending):
		t.Fatal("the handoff after a stored live update:", perr)
	case err != nil:
		t.Fatal("the live update before the handoff's record:", err)
	}
	if !accepted.Pending {
		t.Fatalf("the accepted update: %+v", accepted)
	}
	checkpoint, err := b.LiveCheckpoint(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	waitHead(t, a, checkpoint.Revision)
	if _, err = a.PrepareAuthority(ctx, AuthorityRequest{ID: filingHandoff, Generation: 1, From: "forgetmenote", To: "github", Revision: checkpoint.Revision}); err != nil {
		t.Fatal("prepare after the checkpoint:", err)
	}
	state, err := a.FinishAuthority(ctx, filingHandoff, true)
	if err != nil || state.Committed != checkpoint.Revision {
		t.Fatalf("handoff: %+v %v", state, err)
	}
	if doc, err := reopenTestStore(t, a).Read(ctx, "note.md", ""); err != nil || !strings.HasSuffix(string(doc.Content), "!") {
		t.Fatalf("the GitHub notebook holds %q, %v", doc.Content, err)
	}
}

// A live update another gateway stored refuses a handoff here as one this
// gateway accepted would, also while this gateway holds the note's live
// document as it was before the update.
func TestLiveUpdateStoredOnAnotherGatewayRefusesAHandoff(t *testing.T) {
	a, b, tab, ctx := liveFenceGateways(t)
	if _, err := a.LiveRead(ctx, "note.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.LiveApply(tab.answer(ctx), tab.typed("!")); err != nil {
		t.Fatal(err)
	}
	_, err := a.PrepareAuthority(ctx, AuthorityRequest{ID: filingHandoff, Generation: 1, From: "forgetmenote", To: "github", Revision: headOf(t, a)})
	if !errors.Is(err, ErrLivePending) {
		t.Fatal("a handoff prepared over another gateway's pending live edit:", err)
	}
}

// A gateway that heard of another gateway's pending live edit holds it, and
// once the other gateway checkpointed it, a handoff here is previewed and
// prepared rather than refused for an edit that no longer waits.
func TestHandoffAfterAnotherGatewayCheckpointedItsLiveEdit(t *testing.T) {
	a, b, tab, ctx := liveFenceGateways(t)
	if _, err := b.LiveApply(tab.answer(ctx), tab.typed("!")); err != nil {
		t.Fatal(err)
	}
	n := liveDB(a)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		n.mu.Lock()
		held := n.live["note.md"] != nil && len(n.live["note.md"].Uncommitted) > 0
		n.mu.Unlock()
		if held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the gateway never held the other gateway's pending edit")
		}
	}
	checkpoint, err := b.LiveCheckpoint(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	waitHead(t, a, checkpoint.Revision)
	snapshot, err := a.NotebookSnapshot(ctx)
	if err != nil || snapshot.Revision != checkpoint.Revision {
		t.Fatalf("preview after the checkpoint: %s %v", snapshot.Revision, err)
	}
	if _, err = a.PrepareAuthority(ctx, AuthorityRequest{ID: filingHandoff, Generation: 1, From: "forgetmenote", To: "github", Revision: checkpoint.Revision}); err != nil {
		t.Fatal("prepare after the checkpoint:", err)
	}
}
