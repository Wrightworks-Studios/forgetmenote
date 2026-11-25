package search

import (
	"context"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"path/filepath"
	"testing"
	"time"
)

// vectorSetVersion is the stored version of a notebook's vector set, 0 when it
// has none. The notebook is its engine directory's base name.
func vectorSetVersion(t testing.TB, db *pg.DB, store *workspace.Store) int64 {
	t.Helper()
	var version int64
	err := db.Pool.QueryRow(context.Background(), `SELECT coalesce((SELECT version FROM knowledge_vector_sets WHERE org = $1 AND notebook = $2), 0)`, db.Org, filepath.Base(filepath.Dir(store.Root))).Scan(&version)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func TestActiveAgentLeaseDoesNotSuppressStorageRecovery(t *testing.T) {
	indexes, stores := retainedIndexes(t, 1)
	store := stores[0]
	if _, err := store.Save(t.Context(), "a.md", []byte("# Agent\nSynthetic evidence.\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	engine, release, err := indexes.Acquire(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Configure(t.Context(), knowledge.Config{Enabled: true, Model: "synthetic", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}, false); err != nil {
		release()
		t.Fatal(err)
	}
	pending, err := engine.Pending(t.Context(), 1)
	if err != nil || len(pending.Passages) != 1 {
		release()
		t.Fatalf("fixture pending: %+v %v", pending, err)
	}
	passage := pending.Passages[0]
	submission := knowledge.Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []knowledge.VectorSubmission{{ID: passage.ID, Hash: passage.Hash, NoteHash: passage.NoteHash, Vector: []float32{1, 0, 0}}}}
	if _, err = engine.Submit(t.Context(), submission); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	if err = indexes.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		indexes.mu.Lock()
		slot := indexes.slots[store]
		ready := slot.checkpoint.Load() != nil && !slot.pending.Load() && slot.references == 0
		indexes.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ready agent-only fixture did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	// Another foreground request may hold the same engine for a long operation.
	_, outerRelease, err := indexes.Acquire(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer outerRelease()
	engine, innerRelease, err := indexes.Acquire(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer innerRelease()
	before := vectorSetVersion(t, indexes.db, store)
	restore := pgtest.FailTable(t, indexes.db, "knowledge_vector_sets")
	_, submitErr := engine.Submit(t.Context(), submission)
	restore()
	if submitErr == nil {
		t.Fatal("fixture failed to reject vector persistence")
	}
	innerRelease()
	// Preserve the actual production timer. The surviving lease must not hide a
	// newly created storage-recovery requirement from the retry scheduler.
	deadline = time.Now().Add(34 * time.Second)
	for {
		if vectorSetVersion(t, indexes.db, store) != before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("active agent-only lease suppressed storage recovery on the 30-second timer")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
