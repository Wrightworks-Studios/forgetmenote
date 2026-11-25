package knowledge

import (
	"testing"
)

func TestCompleteIndexReopenDoesNotRewriteUnchangedVectorCache(t *testing.T) {
	engine, _ := indexFixture(t)
	if _, err := engine.Configure(t.Context(), testConfig(), false); err != nil {
		t.Fatal(err)
	}
	fillIndex(t, engine, "")
	before := vectorSetVersion(t, engine)
	if before == 0 {
		t.Fatal("complete index stored no vectors")
	}
	engine.Close()
	reopened, err := Open(database(t, engine.Store), engine.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	pending, err := reopened.Pending(t.Context(), 1)
	if err != nil || pending.Status.State != "ready" {
		t.Fatalf("complete index did not survive reopening: %+v %v", pending.Status, err)
	}
	if after := vectorSetVersion(t, reopened); after != before {
		t.Fatal("reopening unchanged complete index republished its vector cache")
	}
}

func TestCurrentVectorWriteFailureSurvivesReopen(t *testing.T) {
	engine, _ := indexFixture(t)
	if _, err := engine.Configure(t.Context(), testConfig(), false); err != nil {
		t.Fatal(err)
	}
	pending, err := engine.Pending(t.Context(), 1)
	if err != nil || len(pending.Passages) != 1 {
		t.Fatalf("fixture pending: %+v %v", pending, err)
	}
	restore := failVectors(t, engine)
	p := pending.Passages[0]
	submission := Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}}
	if _, err = engine.Submit(t.Context(), submission); err == nil {
		t.Fatal("a refused vector write was accepted")
	}
	processing := engine.Processing
	engine.Close()
	reopened, err := OpenWithProcessing(database(t, engine.Store), engine.Store, processing)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	pending, err = reopened.Pending(t.Context(), 1)
	if err != nil || pending.Status.State != "unavailable" || len(pending.Passages) != 1 {
		t.Fatalf("reopened engine discarded the unresolved write failure: %+v %v", pending.Status, err)
	}
	restore()
	if _, err = reopened.Submit(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	pending, err = reopened.Pending(t.Context(), 1)
	if err != nil || pending.Status.State != "ready" {
		t.Fatalf("repaired storage did not accept the vectors: %+v %v", pending, err)
	}
}
