package knowledge

import (
	"context"
	"testing"
)

func TestStorageRecoveryDoesNotRequireEmbeddingEndpoint(t *testing.T) {
	engine, doc := indexFixture(t)
	if _, err := engine.Configure(t.Context(), testConfig(), false); err != nil {
		t.Fatal(err)
	}
	fillIndex(t, engine, "")
	before := vectorSetVersion(t, engine)
	// A copy shares every passage's content hash, so the index stays complete
	// and only its revision must be published again.
	if _, err := engine.Store.Save(context.Background(), "copy.md", doc.Content, "", ""); err != nil {
		t.Fatal(err)
	}
	restore := failVectors(t, engine)
	if _, err := engine.Pending(t.Context(), 1); err == nil {
		t.Fatal("fixture failed to report publication error")
	}
	processing := engine.Processing
	engine.Close()
	reopened, err := OpenWithProcessing(database(t, engine.Store), engine.Store, processing)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Config().Endpoint {
		t.Fatal("fixture unexpectedly has endpoint")
	}
	if !reopened.BackgroundRequired() {
		t.Fatal("storage failure is not scheduled without endpoint")
	}
	restore()
	if reopened.ProcessBatch(t.Context(), true) {
		t.Fatal("storage-only recovery unexpectedly requests another work batch")
	}
	if vectorSetVersion(t, reopened) == before {
		t.Fatal("background recovery did not republish the cache")
	}
	if reopened.RecoveryRequired() {
		t.Fatal("successful storage-only recovery kept failure")
	}
	if reopened.BackgroundRequired() {
		t.Fatal("fully repaired agent index retained background work")
	}
	result, err := reopened.Pending(t.Context(), 1)
	if err != nil || result.Status.State != "ready" {
		t.Fatalf("repaired index is not ready: %+v %v", result.Status, err)
	}
}
