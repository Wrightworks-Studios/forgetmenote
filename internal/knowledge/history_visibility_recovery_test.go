package knowledge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHistoricalCheckpointsWithoutBatchPolicyBindingAreRescanned(t *testing.T) {
	f := evidenceNotebook(t)
	ctx := context.Background()
	if _, err := f.engine.Configure(ctx, testConfig(), false); err != nil {
		t.Fatal(err)
	}
	fillIndex(t, f.engine, "history")
	f.engine.Close()
	path := filepath.Join(f.engine.dir, "cache", "history.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy historyCache
	if err = json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	// Version 1 checkpoints could declare completion after an A->B->A scan
	// silently skipped a hidden postimage. Their vectors remain reusable.
	legacy.Version = 1
	for id, version := range legacy.Versions {
		if version.Path == "private/hidden.md" {
			delete(legacy.Versions, id)
			break
		}
	}
	if err = persist(path, legacy); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(database(t, f.store), f.store)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := reopened.HybridSearch(ctx, historyQuery("evidence"))
	if err != nil || result.History == nil || result.History.Versions != 3 || len(result.Matches) != 3 {
		t.Fatalf("legacy checkpoint incorrectly reused incomplete coverage: %+v %v", result.History, err)
	}
	if pending, err := reopened.PendingScope(ctx, 32, "history"); err != nil || len(pending.Passages) != 0 || pending.Status.History.Indexed != 3 {
		t.Fatalf("rescanning discarded compatible vectors: %+v %v", pending, err)
	}
}
