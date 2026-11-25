package workspace

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// The notes store keeps a note's live document under its entry ID: a
// snapshot, the updates accepted after it and an epoch.

func liveEntryOf(t *testing.T, s *Store, path string) string {
	t.Helper()
	f, err := s.notes.Stat(context.Background(), gitobj.ID{}, path)
	if err != nil {
		t.Fatal(err)
	}
	return f.EntryID
}

func liveStateOf(t *testing.T, s *Store, path string) notestore.LiveState {
	t.Helper()
	st, err := s.notes.LiveState(context.Background(), liveEntryOf(t, s, path))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestLiveUpdatesAppendWithoutRewritingSnapshotAndRestore(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	before := liveStateOf(t, s, base.Path)
	if before.Snapshot == nil || len(before.Updates) != 0 || before.Base.String() != base.Version {
		t.Fatalf("a new live document: %+v", before)
	}
	patch := TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "append", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "accepted"}}}
	accepted, err := s.LivePatch(ctx, patch)
	if err != nil {
		t.Fatal(err)
	}
	after := liveStateOf(t, s, base.Path)
	if !bytes.Equal(before.Snapshot, after.Snapshot) || after.SnapshotSeq != before.SnapshotSeq || len(after.Updates) != 1 || after.Updates[0].Actor != "alice" {
		t.Fatalf("an accepted edit rewrote the snapshot or appended otherwise: %+v", after)
	}
	reopened, err := reopenLiveStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	live, err := reopened.LiveRead(ctx, base.Path)
	if err != nil || live.Content != accepted.Content || live.LiveRevision != accepted.LiveRevision || !live.Pending {
		t.Fatal("restored update lost accepted state", err)
	}
	if _, err = reopened.LivePatch(ctx, patch); err != nil {
		t.Fatal("restored operation receipt lost", err)
	}
	checkpoint, err := reopened.LiveCheckpoint(ctx, base.Path)
	if err != nil {
		t.Fatal(err)
	}
	// The checkpoint compacts the update into a snapshot of the new revision.
	compacted := liveStateOf(t, reopened, base.Path)
	if len(compacted.Updates) != 0 || compacted.Base.String() != checkpoint.Version || compacted.Epoch != before.Epoch {
		t.Fatalf("checkpoint did not compact the live document: %+v", compacted)
	}
}

func TestLiveUpdatesCompactIntoASnapshotAtTheirBound(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	first := liveStateOf(t, s, base.Path)
	var last TargetedPatch
	for i := 0; i < dbLiveLogEntries+2; i++ {
		last = TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: fmt.Sprintf("bounded-%d", i), Edits: []TextEdit{{Start: 0, End: 0, Text: "x"}}}
		if base, err = s.LivePatch(ctx, last); err != nil {
			t.Fatal(err)
		}
	}
	st := liveStateOf(t, s, base.Path)
	if len(st.Updates) >= dbLiveLogEntries || st.SnapshotSeq <= first.SnapshotSeq || !st.Base.IsZero() || bytes.Equal(st.Snapshot, first.Snapshot) {
		t.Fatalf("updates did not compact at their bound: %d updates after %d", len(st.Updates), st.SnapshotSeq)
	}
	reopened, err := reopenLiveStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := reopened.LiveRead(ctx, base.Path)
	if err != nil || actual.Content != base.Content || actual.LiveRevision != base.LiveRevision || !actual.Pending {
		t.Fatal("compaction changed current state", err)
	}
	if _, err = reopened.LivePatch(ctx, last); err != nil {
		t.Fatal("compaction lost operation receipts", err)
	}
	if _, err = reopened.LiveCheckpoint(ctx, base.Path); err != nil {
		t.Fatal(err)
	}
	receipts := strings.Count(revisionMessage(t, reopened, ""), "Collaboration-Receipt:")
	if receipts != dbLiveLogEntries+2 {
		t.Fatalf("the checkpoint has %d receipts", receipts)
	}
}

func TestCorruptStoredLiveUpdateIsRefusedWithoutChangingIt(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LivePatch(ctx, TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "corruption", Edits: []TextEdit{{Start: 0, End: 0, Text: "x"}}}); err != nil {
		t.Fatal(err)
	}
	st := liveStateOf(t, s, base.Path)
	var h dbLiveUpdate
	if _, err = dbLiveDecode(st.Updates[0].Update, &h); err != nil {
		t.Fatal(err)
	}
	// An update that claims a text it does not make.
	h.Key, h.After, h.ContentHash = ContentHash([]byte("another operation")), uint64(st.Updates[0].Seq), "wrong"
	corrupt, err := dbLiveEncode(h, []byte{0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.notes.LiveAppend(ctx, liveEntryOf(t, s, base.Path), st.Epoch, corrupt, "intruder"); err != nil {
		t.Fatal(err)
	}
	stored := liveStored(t, s, base.Path)
	reopened, err := reopenLiveStore(t, s)
	if err == nil {
		t.Fatal("corrupt acknowledged update was accepted")
	}
	if _, err = reopened.LiveRead(ctx, base.Path); err == nil {
		t.Fatal("corrupt acknowledged update was read")
	}
	if liveStored(t, s, base.Path) != stored {
		t.Fatal("failed recovery changed the stored evidence")
	}
}

func TestLiveSmallEditStoresABoundedDeltaForALargeNote(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	content := "alpha\n" + strings.Repeat("ordinary Markdown paragraph\n", 8000)
	if _, err := s.Save(ctx, "large.md", []byte(content), "", ""); err != nil {
		t.Fatal(err)
	}
	base, err := s.LiveRead(ctx, "large.md")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err = s.LivePatch(ctx, TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "small-in-large", Edits: []TextEdit{{Start: 0, End: 5, Old: "alpha", Text: "ALPHA"}}}); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	st := liveStateOf(t, s, base.Path)
	if len(st.Updates) != 1 || len(st.Updates[0].Update) >= len(st.Snapshot)/10 {
		t.Fatal("small edit retained whole-document serialization cost", len(st.Updates), len(st.Snapshot))
	}
	t.Logf("snapshot_bytes=%d update_bytes=%d patch_ms=%d", len(st.Snapshot), len(st.Updates[0].Update), elapsed.Milliseconds())
}

// Updates whose snapshot is gone, as after an incomplete restore, are never
// replaced by a document started again from Git.
func TestStoredLiveUpdatesWithoutTheirSnapshotAreNeverReplacedFromGit(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LivePatch(ctx, TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "missing-snapshot", Edits: []TextEdit{{Start: 0, End: 0, Text: "accepted "}}}); err != nil {
		t.Fatal(err)
	}
	entry := liveEntryOf(t, s, base.Path)
	update := liveStateOf(t, s, base.Path).Updates[0].Update
	epoch, err := s.notes.LiveReset(ctx, entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.notes.LiveAppend(ctx, entry, epoch, update, "alice"); err != nil {
		t.Fatal(err)
	}
	stored := liveStored(t, s, base.Path)
	reopened, err := reopenLiveStore(t, s)
	if err == nil {
		t.Fatal("updates without their snapshot could be discarded by a fresh start")
	}
	if _, err = reopened.LiveRead(ctx, base.Path); err == nil {
		t.Fatal("updates without their snapshot were read")
	}
	if liveStored(t, s, base.Path) != stored {
		t.Fatal("failed recovery changed accepted update evidence")
	}
}

// A document whose new epoch began but whose first snapshot was never
// stored, as after a crash between the two, starts again when it is used.
func TestLiveDocumentResetBeforeItsSnapshotStartsAgain(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	if _, err := s.notes.LiveReset(ctx, liveEntryOf(t, s, "note.md")); err != nil {
		t.Fatal(err)
	}
	reopened, err := reopenLiveStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	base, err := reopened.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal("an empty epoch stranded the first live read", err)
	}
	if _, err = reopened.LivePatch(ctx, TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "after-reset-crash", Edits: []TextEdit{{Start: 0, End: 0, Text: "recovered "}}}); err != nil {
		t.Fatal("an empty epoch stranded writes", err)
	}
}

func TestFutureLiveSnapshotFormatIsRefusedWithoutChangingIt(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LivePatch(ctx, TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "future-version", Edits: []TextEdit{{Start: 0, End: 0, Text: "pending "}}}); err != nil {
		t.Fatal(err)
	}
	st := liveStateOf(t, s, base.Path)
	future := bytes.Replace(st.Snapshot, []byte(dbLiveFormat), []byte("fmn-live 999 "), 1)
	if err = s.notes.LiveCompact(ctx, liveEntryOf(t, s, base.Path), st.Epoch, future, st.SnapshotSeq, st.Base); err != nil {
		t.Fatal(err)
	}
	stored := liveStored(t, s, base.Path)
	if _, err = reopenLiveStore(t, s); err == nil {
		t.Fatal("future live snapshot accepted")
	}
	if liveStored(t, s, base.Path) != stored {
		t.Fatal("unknown snapshot format changed stored evidence")
	}
}
