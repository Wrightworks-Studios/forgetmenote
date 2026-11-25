package workspace

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// A note that outgrows the live limit, the ordinary read limit of a note, is
// not live; it does not prevent a restart, rejoins live editing in a fresh
// generation once it fits again, and earlier operations stay replays.
func TestOrdinaryOversizedSaveRestartsAndPreservesLiveReceipts(t *testing.T) {
	ctx := context.Background()
	t.Setenv("FMN_LIMIT_FILE_BYTES", strconv.Itoa(256<<10))
	s := newTestStore(t)
	small, err := s.Save(ctx, "reference.md", []byte("# Reference\nsmall note\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.LiveRead(ctx, small.Path)
	if err != nil {
		t.Fatal(err)
	}
	patch := TargetedPatch{Path: small.Path, Base: base.LiveRevision, OperationID: "receipt-before-oversized", Edits: []TextEdit{{Start: len(base.Content), End: len(base.Content), Text: "accepted\n"}}}
	if _, err = s.LivePatch(ctx, patch); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := s.LiveCheckpoint(ctx, small.Path)
	if err != nil {
		t.Fatal(err)
	}
	large := "# Reference\n" + strings.Repeat("ordinary reference text\n", (192<<10)/len("ordinary reference text\n"))
	saved, err := s.Save(ctx, small.Path, []byte(large), checkpoint.Revision, "")
	if err != nil {
		t.Fatalf("guarded ordinary large save failed: %v", err)
	}
	if live, err := s.LiveRead(ctx, small.Path); err != nil || live.Content != large || live.Pending {
		t.Fatalf("a note within the read limit is not live: %v", err)
	}
	// The operator lowers the limit below the note.
	t.Setenv("FMN_LIMIT_FILE_BYTES", strconv.Itoa(64<<10))
	for i := 0; i < 2; i++ {
		if s, err = reopenLiveStore(t, s); err != nil {
			t.Fatalf("successful ordinary large save prevents restart: %v", err)
		}
		if _, err = s.LiveRead(ctx, small.Path); err == nil || !strings.Contains(err.Error(), "byte budget") {
			t.Fatalf("oversized note unexpectedly rejoined live editing: %v", err)
		}
	}
	// Writes read every note's identity, so the operator restores the limit.
	t.Setenv("FMN_LIMIT_FILE_BYTES", strconv.Itoa(256<<10))
	shrunk, err := s.Save(ctx, small.Path, []byte("# Small again\n"), saved.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.LiveRead(ctx, small.Path)
	if err != nil || current.Content != string(shrunk.Content) || strings.Split(current.LiveRevision, ":")[0] == strings.Split(base.LiveRevision, ":")[0] {
		t.Fatalf("shrunk note did not join a fresh generation: %+v %v", current, err)
	}
	replay, err := s.LivePatch(ctx, patch)
	if err != nil || replay.Content != current.Content || replay.LiveRevision != current.LiveRevision {
		t.Fatalf("retired record lost its permanent retry receipt: %+v %v", replay, err)
	}
	patch.Edits[0].Text = "changed retry"
	if _, err = s.LivePatch(ctx, patch); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed receipt retry accepted: %v", err)
	}
}

// Another writer's change of a note with pending live edits, larger than
// the Git backend's 1 MiB live limit, is a conflict at restart; the accepted
// edits stay as they were.
func TestOversizedGitChangeDoesNotDiscardPendingLiveConflict(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "pending-before-external-large", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "accepted"}}}); err != nil {
		t.Fatal(err)
	}
	before := liveStored(t, s, "note.md")
	commitFixture(t, s, fixture("note.md", strings.Repeat("x", MaxLiveBytes+1)))
	if _, err = reopenLiveStore(t, s); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending oversized conflict was skipped: %v", err)
	}
	if liveStored(t, s, "note.md") != before {
		t.Fatal("failed recovery rewrote accepted pending record")
	}
}
