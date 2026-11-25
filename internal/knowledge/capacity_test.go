package knowledge

import (
	"fmt"
	"strings"
	"testing"

	"forgetmenote/internal/workspace"
)

func TestNotebookIndexBeyondLegacyVectorCount(t *testing.T) {
	e, old := indexFixture(t)
	var text strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&text, "# Passage %d\nUnique evidence number %d.\n\n", i, i)
	}
	if _, err := e.Store.Save(t.Context(), old.Path, []byte(text.String()), old.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Dimensions = 1536
	status, err := e.Configure(t.Context(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if status.Total < 3000 {
		t.Fatalf("lost distinct passages: %d", status.Total)
	}
	pending, err := e.Pending(t.Context(), 10)
	if err != nil || len(pending.Passages) == 0 {
		t.Fatalf("index cannot make progress: %v", err)
	}
}

func TestPassagesUseIndexBudgetInsteadOfDocumentCount(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 10001; i++ {
		fmt.Fprintf(&body, "# Section %d\nEvidence %d.\n\n", i, i)
	}
	snapshot := workspace.Snapshot{Files: []workspace.SnapshotFile{{Path: "manual.md", Content: body.String()}}}
	passages, err := Split(snapshot, testConfig())
	if err != nil || len(passages) != 10001 {
		t.Fatalf("lost passages: %d, %v", len(passages), err)
	}
	t.Setenv("FMN_LIMIT_INDEX_BYTES", "1024")
	if _, err := Split(snapshot, testConfig()); err == nil || !strings.Contains(err.Error(), "FMN_LIMIT_INDEX_BYTES") {
		t.Fatalf("resource budget not enforced: %v", err)
	}
}
