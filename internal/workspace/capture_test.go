package workspace

import (
	"context"
	"errors"
	"forgetmenote/internal/attribution"
	"os"
	"testing"
)

func captureContext() context.Context {
	return attribution.With(context.Background(), attribution.Identity{ID: "capture-user", Name: "Synthetic user", Email: "capture@example.test", Source: "browser"})
}
func captureMove(doc Document) CaptureMove {
	return CaptureMove{ID: "filing-one", From: doc.Path, Version: doc.Version, Hash: ContentHash(doc.Content), To: "notes/filed.md", SourceProject: "inbox", TargetProject: "target"}
}
func TestCaptureTransferReservesExactVersionAndReplays(t *testing.T) {
	ctx := captureContext()
	source := newTestStore(t)
	target := newTestStore(t)
	doc, err := source.Save(ctx, "capture.md", []byte("# A capture\n"), "", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = target.Save(ctx, "notes/existing.md", []byte("existing"), "", "user"); err != nil {
		t.Fatal(err)
	}
	in := captureMove(doc)
	frozen, err := source.CapturePrepare(ctx, in)
	if err != nil {
		t.Fatalf("prepare exact capture: %v", err)
	}
	if string(frozen.Content) != string(doc.Content) {
		t.Fatal("capture bytes changed")
	}
	if _, err = source.Save(ctx, doc.Path, []byte("newer"), doc.Revision, "user"); !errors.Is(err, ErrConflict) {
		t.Fatalf("reserved source mutated: %v", err)
	}
	if _, err = source.Save(ctx, "independent.md", []byte("independent"), "", "user"); err != nil {
		t.Fatal("reservation blocked unrelated note", err)
	}
	filed, err := target.CaptureReceive(ctx, in, frozen.Content)
	if err != nil {
		t.Fatal(err)
	}
	again, err := target.CaptureReceive(ctx, in, frozen.Content)
	if err != nil || again.Version != filed.Version {
		t.Fatal("receive did not replay", again, err)
	}
	receipt, err := source.CaptureFinish(ctx, in)
	if err != nil || receipt.State != "done" {
		t.Fatal(receipt, err)
	}
	if _, err = source.Read(ctx, doc.Path, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source still active", err)
	}
	if _, err = source.Read(ctx, doc.Path, doc.Revision); err != nil {
		t.Fatal("source history lost", err)
	}
	if _, err = source.CaptureFinish(ctx, in); err != nil {
		t.Fatal("finish retry", err)
	}
	changed := in
	changed.To = "notes/other.md"
	if _, err = target.CaptureReceive(ctx, changed, frozen.Content); !errors.Is(err, ErrConflict) {
		t.Fatal("changed operation payload accepted", err)
	}
}
func TestCaptureRejectsStaleAndAbsentDestination(t *testing.T) {
	ctx := captureContext()
	s := newTestStore(t)
	doc, err := s.Save(ctx, "capture.md", []byte("capture"), "", "user")
	if err != nil {
		t.Fatal(err)
	}
	in := captureMove(doc)
	if _, err = s.Save(ctx, doc.Path, []byte("newer"), doc.Revision, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CapturePrepare(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale capture did not conflict: %v", err)
	}
	target := newTestStore(t)
	empty := headOf(t, target)
	if _, err = target.CaptureReceive(ctx, in, doc.Content); !errors.Is(err, ErrCaptureInput) {
		t.Fatal("created generated destination folder", err)
	}
	if headOf(t, target) != empty {
		t.Fatal("generated folder committed")
	}
	receipt, err := target.CaptureAbortReceive(ctx, in)
	if err != nil || receipt.State != "canceled" {
		t.Fatal(receipt, err)
	}
	if _, err = target.Save(ctx, "notes/existing.md", []byte("existing"), "", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err = target.CaptureReceive(ctx, in, doc.Content); !errors.Is(err, ErrConflict) {
		t.Fatal("late receive escaped cancellation fence", err)
	}
}

func TestCapturePrepareRejectsAcceptedPendingEdit(t *testing.T) {
	ctx := captureContext()
	s := newTestStore(t)
	doc, err := s.Save(ctx, "capture.md", []byte("capture"), "", "user")
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.LiveRead(ctx, doc.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LivePatch(ctx, TargetedPatch{Path: doc.Path, Base: live.LiveRevision, OperationID: "edit-before-filing", Edits: []TextEdit{{Start: 0, End: 0, Text: "new "}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CapturePrepare(ctx, captureMove(doc)); !errors.Is(err, ErrConflict) {
		t.Fatal("pending edit was filed from stale snapshot", err)
	}
	now, err := s.Read(ctx, doc.Path, "")
	if err != nil || string(now.Content) != "new capture" {
		t.Fatal("accepted edit lost", now, err)
	}
}
func TestCaptureCreateRecoveryDoesNotOverwriteNewerEdit(t *testing.T) {
	ctx := captureContext()
	s := newTestStore(t)
	first, err := s.CaptureCreate(ctx, "capture-one", "quicknote-one.md", []byte("capture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, first.Path, []byte("newer"), first.Revision, "user"); err != nil {
		t.Fatal(err)
	}
	replay, err := s.CaptureCreate(ctx, "capture-one", first.Path, []byte("capture"))
	if err != nil || replay.Version != first.Version {
		t.Fatal("creation receipt not replayed", replay, err)
	}
	saved, err := s.CaptureSaved(ctx, "capture-one", first.Path, ContentHash(first.Content))
	if err != nil || saved.Version != first.Version {
		t.Fatal("capture acknowledgement not recovered", saved, err)
	}
	now, err := s.Read(ctx, first.Path, "")
	if err != nil || string(now.Content) != "newer" {
		t.Fatal("newer content overwritten", now, err)
	}
}
func TestCaptureFailedCommitAndDependencyRefusalPreserveSource(t *testing.T) {
	ctx := captureContext()
	source := newTestStore(t)
	target := newTestStore(t)
	doc, err := source.Save(ctx, "capture.md", []byte("capture"), "", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = target.Save(ctx, "notes/existing.md", []byte("existing"), "", "user"); err != nil {
		t.Fatal(err)
	}
	in := captureMove(doc)
	frozen, err := source.CapturePrepare(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	restore := failCommits(t, target)
	if _, err = target.CaptureReceive(ctx, in, frozen.Content); err == nil {
		t.Fatal("blocked write succeeded")
	}
	if _, err = target.Read(ctx, in.To, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed target write remained", err)
	}
	restore()
	outcome, err := target.CaptureAbortReceive(ctx, in)
	if err != nil || outcome.State != "canceled" {
		t.Fatal(outcome, err)
	}
	if _, err = source.CaptureCancel(ctx, in); err != nil {
		t.Fatal(err)
	}
	unchanged, err := source.Read(ctx, doc.Path, "")
	if err != nil || unchanged.Version != doc.Version {
		t.Fatal("failed filing changed source", unchanged, err)
	}
	linked, err := source.Save(ctx, "linked.md", []byte("[relative](capture.md)"), "", "user")
	if err != nil {
		t.Fatal(err)
	}
	linkedMove := captureMove(linked)
	linkedMove.ID = "linked-filing"
	if _, err = source.CapturePrepare(ctx, linkedMove); err == nil {
		t.Fatal("relative dependency silently moved")
	}
	in.ID = "backlink-filing"
	if _, err = source.CapturePrepare(ctx, in); err == nil {
		t.Fatal("backlink silently broken")
	}
}
