package knowledge

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/workspace"
)

func TestFullProjectRestoreAndDerivedRebuild(t *testing.T) {
	ctx := context.Background()
	e, doc := indexFixture(t)
	e.Configure(ctx, testConfig(), false)
	pending, _ := e.Pending(ctx, 10)
	p := pending.Passages[0]
	e.Submit(ctx, Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}})
	e.Complete(ctx, "agent", "review", "", doc.Revision, "Review reported", false)
	proposal, err := e.Propose(ctx, "agent", workspace.EditGroup{ID: "restorable", Summary: "Proposed change", Edits: []workspace.NoteEdit{{Path: doc.Path, Content: "Changed", Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content)}}})
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	e.Store.Close()
	// A full-instance restore brings back the database, which keeps the
	// notebook and its engine's state, and the notebook's directory under its
	// own ID, which keeps its content blocks and derived caches.
	restored := filepath.Join(t.TempDir(), filepath.Base(e.dir))
	err = filepath.WalkDir(e.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(e.dir, path)
		target := filepath.Join(restored, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.OpenDir(filepath.Join(restored, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	notes, err := notestore.Open(ctx, e.db, blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		t.Fatal(err)
	}
	defer notes.Close()
	store, err := workspace.OpenDB(ctx, notes, e.notebook, restored, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := Open(e.db, store)
	if err != nil {
		t.Fatal(err)
	}
	cp, _ := engine.Checkpoint(ctx, "agent", "review")
	if cp.Revision != doc.Revision {
		t.Fatal("checkpoint lost", cp)
	}
	proposals, _ := engine.Proposals(ctx)
	if len(proposals) != 1 || proposals[0].ID != proposal.ID {
		t.Fatal("proposal lost", proposals)
	}
	indexed, err := engine.Pending(ctx, 10)
	if err != nil || indexed.Status.Indexed != 1 {
		t.Fatal("restored index", indexed, err)
	}
	engine.Close()
	// Vectors and caches are derived; dropping them rebuilds the index.
	if err = os.RemoveAll(filepath.Join(restored, "cache")); err != nil {
		t.Fatal(err)
	}
	if _, err = e.db.Pool.Exec(ctx, `DELETE FROM knowledge_vector_sets WHERE org = $1 AND notebook = $2`, e.db.Org, e.notebook); err != nil {
		t.Fatal(err)
	}
	engine, err = Open(e.db, store)
	if err != nil {
		t.Fatal(err)
	}
	rebuild, err := engine.Pending(ctx, 10)
	if err != nil || len(rebuild.Passages) != 1 {
		t.Fatal("cache rebuild", rebuild, err)
	}
	if cp, _ = engine.Checkpoint(ctx, "agent", "review"); cp.Revision != doc.Revision {
		t.Fatal("rebuild changed review checkpoint")
	}
	original, err := store.Read(ctx, doc.Path, doc.Revision)
	if err != nil || string(original.Content) != string(doc.Content) {
		t.Fatal("restore content", err)
	}
}
