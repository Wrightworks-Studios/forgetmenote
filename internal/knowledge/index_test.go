package knowledge

import (
	"context"
	"forgetmenote/internal/workspace"
	"math"
	"testing"
)

func indexFixture(t *testing.T) (*Engine, workspace.Document) {
	t.Helper()
	s := newNotebook(t, nil)
	d, err := s.Save(context.Background(), "a.md", []byte("---\ntags: [research]\n---\n# Alpha\nAn observation.\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(database(t, s), s)
	if err != nil {
		t.Fatal(err)
	}
	return e, d
}
func TestAgentIndexAndQueryWithNoEndpoint(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	status, err := e.Configure(ctx, testConfig(), false)
	if err != nil {
		t.Fatal(err)
	}
	if status.BrowserSearch {
		t.Fatal("agent vectors must not imply browser query capability")
	}
	pending, err := e.Pending(ctx, 10)
	if err != nil || len(pending.Passages) != 1 {
		t.Fatal(pending, err)
	}
	p := pending.Passages[0]
	submission := Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}}
	status, err = e.Submit(ctx, submission)
	if err != nil || status.Indexed != 1 || status.IndexedRevision != status.Revision {
		t.Fatal(status, err)
	}
	results, err := e.Search(ctx, SearchQuery{Vector: []float32{1, 0, 0}, ConfigID: status.ConfigID, Tag: "research"})
	if err != nil || len(results) != 1 || results[0].Path != "a.md" {
		t.Fatal(results, err)
	}
	none, err := e.Search(ctx, SearchQuery{Vector: []float32{1, 0, 0}, ConfigID: status.ConfigID, Tag: "other"})
	if err != nil || len(none) != 0 {
		t.Fatal(none, err)
	}
	reopened, err := Open(database(t, e.Store), e.Store)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Pending(ctx, 10)
	if err != nil || len(again.Passages) != 0 {
		t.Fatal(again, err)
	}
	changed := testConfig()
	changed.Model = "different-model"
	if _, err = e.Configure(ctx, changed, false); err == nil {
		t.Fatal("silently mixed model configurations")
	}
	if _, err = e.Configure(ctx, changed, true); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Submit(ctx, submission); err == nil {
		t.Fatal("accepted incompatible model")
	}
}
func TestIndexRejectsMalformedAndStaleVectorsAtomically(t *testing.T) {
	e, d := indexFixture(t)
	ctx := context.Background()
	e.Configure(ctx, testConfig(), false)
	pending, err := e.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	p := pending.Passages[0]
	sub := Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}}
	for _, v := range [][]float32{{1, 2}, {0, 0, 0}, {float32(math.Inf(1)), 0, 0}, {float32(math.NaN()), 0, 0}} {
		bad := sub
		bad.Vectors = []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: v}}
		if _, err = e.Submit(ctx, bad); err == nil {
			t.Fatal("accepted invalid vector", v)
		}
	}
	e.Store.Save(ctx, "a.md", []byte("# Changed\n"), d.Revision, "")
	if _, err = e.Submit(ctx, sub); err == nil {
		t.Fatal("accepted stale snapshot")
	}
	next, err := e.Pending(ctx, 10)
	if err != nil || next.Status.Indexed != 0 {
		t.Fatal(next, err)
	}
}

func TestDuplicateVectorIdentityRejected(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	e.Configure(ctx, testConfig(), false)
	pending, _ := e.Pending(ctx, 10)
	p := pending.Passages[0]
	v := VectorSubmission{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}
	_, err := e.Submit(ctx, Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []VectorSubmission{v, v}})
	if err == nil {
		t.Fatal("ambiguous duplicate passage accepted")
	}
}

func TestDisabledIndexStillRequiresExplicitModelRebuild(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	config := testConfig()
	if _, err := e.Configure(ctx, config, false); err != nil {
		t.Fatal(err)
	}
	config.Enabled = false
	if _, err := e.Configure(ctx, config, false); err != nil {
		t.Fatal(err)
	}
	config.Enabled = true
	config.Model = "replacement"
	if _, err := e.Configure(ctx, config, false); err == nil {
		t.Fatal("disabled index lost its rebuild protection")
	}
	if _, err := e.Configure(ctx, config, true); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSetupRetryPreservesIndexedVectors(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	if _, err := e.ConfigureAgent(ctx, "fixture-v1", 3, false); err != nil {
		t.Fatal(err)
	}
	pending, err := e.Pending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	p := pending.Passages[0]
	_, err = e.Submit(ctx, Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}})
	if err != nil {
		t.Fatal(err)
	}
	status, err := e.ConfigureAgent(ctx, "fixture-v1", 3, false)
	if err != nil || status.Indexed != 1 {
		t.Fatal("setup retry discarded index", status, err)
	}
	reopened, err := Open(database(t, e.Store), e.Store)
	if err != nil {
		t.Fatal(err)
	}
	status, err = reopened.ConfigureAgent(ctx, "fixture-v1", 3, false)
	if err != nil || status.Indexed != 1 {
		t.Fatal("setup not preserved after reopen", status, err)
	}
}
