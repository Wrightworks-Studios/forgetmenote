package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

func waitForLiveReaders(t *testing.T, store *Store, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		got := store.liveWaiting
		store.mu.Unlock()
		if got == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("live readers did not reach expected count", count)
}

func liveKnown(v LiveDocument) string {
	return fmt.Sprintf("%s/%s/%t", v.LiveRevision, v.Version, v.Pending)
}

// An unchanged poll answers at once and a long poll waits, holding no lock,
// until a change or its cancellation.
func TestLiveWaitIdlesUntilCancelled(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	known := liveKnown(base)
	for i := 0; i < 3; i++ {
		if v, err := s.WaitLive(ctx, "note.md", known, 0, false); err != nil || !v.Unchanged {
			t.Fatalf("unchanged poll: %+v %v", v, err)
		}
	}
	waitCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := s.WaitLive(waitCtx, "note.md", known, 25000, false); done <- err }()
	waitForLiveReaders(t, s, 1)
	// An unrelated commit leaves the live note as it was.
	if _, err := s.Save(ctx, "other.md", []byte("unrelated\n"), "", ""); err != nil {
		t.Fatal("a live wait held the notebook:", err)
	}
	select {
	case err := <-done:
		t.Fatal("idle wait ended", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitForLiveReaders(t, s, 0)
}

func TestLiveWaitLeavesSaveCapacityAndReceivesUncommittedEdits(t *testing.T) {
	s, _, _ := liveTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	known := liveKnown(base)
	results := make(chan LiveDocument, 4)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { v, e := s.WaitLive(ctx, "note.md", known, 25000, false); results <- v; errs <- e }()
	}
	waitForLiveReaders(t, s, 4)
	if _, err := s.Save(ctx, "other.md", []byte("unrelated"), "", ""); err != nil {
		t.Fatal("live waits starved a save", err)
	}
	accepted, err := s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "wake", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "beta"}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		v := <-results
		err := <-errs
		if err != nil || v.Content != accepted.Content || !v.Pending {
			t.Fatalf("wait = %+v, %v", v, err)
		}
	}
	waitForLiveReaders(t, s, 0)
}

// A second gateway, with its own notes store on the same database, sees the
// first one's accepted edits, wakes its long polls for them, adds its own,
// and checkpoints them all before its next write.
func TestLiveEditsReachAnotherGateway(t *testing.T) {
	s, _, _ := liveTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	other, err := OpenDB(ctx, openTestNotes(t, testEnv(t, s)), s.notebook, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	if err = recoverLive(ctx, other); err != nil {
		t.Fatal(err)
	}
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	seen, err := other.LiveRead(ctx, "note.md")
	if err != nil || seen.LiveRevision != base.LiveRevision || seen.Content != base.Content {
		t.Fatalf("the other gateway reads another document: %+v %v", seen, err)
	}
	woken := make(chan LiveDocument, 1)
	failed := make(chan error, 1)
	go func() {
		v, err := other.WaitLive(ctx, "note.md", liveKnown(seen), 25000, false)
		woken <- v
		failed <- err
	}()
	waitForLiveReaders(t, other, 1)
	accepted, err := s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "first-gateway", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "ALPHA"}}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := <-woken, <-failed
	if err != nil || v.Unchanged || v.Content != accepted.Content || v.LiveRevision != accepted.LiveRevision || !v.Pending {
		t.Fatalf("the other gateway's long poll: %+v %v", v, err)
	}
	other.mu.Lock()
	pending := other.hasLivePending()
	other.mu.Unlock()
	if !pending {
		t.Fatal("the other gateway does not hold the pending edit")
	}
	// A browser on the other gateway goes on from what it saw.
	browser, err := browserDoc(v.Update)
	if err != nil {
		t.Fatal(err)
	}
	browser.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, 0, "# ", nil) })
	merged, err := other.LiveApply(ctx, LiveUpdate{Path: "note.md", Generation: strings.Split(v.LiveRevision, ":")[0], OperationID: "second-gateway", Update: crdt.EncodeStateAsUpdateV1(browser, nil)})
	if err != nil || merged.Content != "# "+accepted.Content {
		t.Fatalf("the other gateway's edit: %+v %v", merged, err)
	}
	eventually := func(what string, ok func(LiveDocument) bool) {
		t.Helper()
		for {
			v, err := s.LiveRead(ctx, "note.md")
			if err == nil && ok(v) {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("the first gateway never saw %s: %+v %v", what, v, err)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	eventually("the other gateway's edit", func(v LiveDocument) bool { return v.Content == merged.Content && v.Pending })
	// An ordinary write on the other gateway checkpoints both edits first.
	if _, err = other.Save(ctx, "other.md", []byte("unrelated"), "", ""); err != nil {
		t.Fatal(err)
	}
	committed, err := other.Read(ctx, "note.md", "")
	if err != nil || string(committed.Content) != merged.Content {
		t.Fatalf("the write did not checkpoint the live edits: %q %v", committed.Content, err)
	}
	eventually("the checkpoint", func(v LiveDocument) bool { return v.Content == merged.Content && !v.Pending })
	audit := revisionMessage(t, s, committed.Version)
	if strings.Count(audit, "Collaboration-Receipt:") != 2 {
		t.Fatalf("the checkpoint lacks a gateway's receipt: %s", audit)
	}
}
