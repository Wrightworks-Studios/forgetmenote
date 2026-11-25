package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/workspace"
)

func TestHistoricalSearchFindsRemovedPassageWithoutChangingCurrentSearch(t *testing.T) {
	e, original := indexFixture(t)
	ctx := context.Background()
	if _, err := e.Store.Save(ctx, "a.md", []byte("# Alpha\nA replacement paragraph.\n"), original.Revision, ""); err != nil {
		t.Fatal(err)
	}
	var query HybridQuery
	if err := json.Unmarshal([]byte(`{"text":"observation","mode":"text","scope":"history"}`), &query); err != nil {
		t.Fatal(err)
	}
	result, err := e.HybridSearch(ctx, query)
	if err != nil || len(result.Matches) != 1 {
		t.Fatalf("removed passage must remain searchable in history: %+v %v", result, err)
	}
	if result.Matches[0].Revision != original.Revision {
		t.Fatalf("historical result must cite its committed version: %+v", result.Matches[0])
	}
	current, err := e.HybridSearch(ctx, HybridQuery{SearchQuery: SearchQuery{Text: "observation"}, Mode: "text"})
	if err != nil || len(current.Matches) != 0 {
		t.Fatalf("current scope included a removed passage: %+v %v", current, err)
	}
}

func historyQuery(text string) HybridQuery {
	return HybridQuery{SearchQuery: SearchQuery{Text: text, Scope: "history"}, Mode: "text"}
}
func fillIndex(t *testing.T, e *Engine, scope string) int {
	t.Helper()
	count := 0
	for attempts := 0; attempts < 100; attempts++ {
		pending, err := e.PendingScope(context.Background(), 32, scope)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending.Passages) == 0 {
			return count
		}
		sub := Submission{Scope: scope, Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID}
		for _, p := range pending.Passages {
			sub.Vectors = append(sub.Vectors, VectorSubmission{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}})
		}
		if _, err = e.Submit(context.Background(), sub); err != nil {
			t.Fatal(err)
		}
		count += len(sub.Vectors)
	}
	t.Fatal("index did not finish")
	return count
}
func TestHistoryReusesVectorsAcrossEditsCopiesAndRestart(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	if _, err := e.Configure(ctx, testConfig(), false); err != nil {
		t.Fatal(err)
	}
	if got := fillIndex(t, e, ""); got != 1 {
		t.Fatal(got)
	}
	if _, err := e.Store.Save(ctx, "a.md", []byte("# Alpha\nChanged observation.\n"), doc.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if got := fillIndex(t, e, ""); got != 1 {
		t.Fatal(got)
	}
	if got := fillIndex(t, e, "history"); got != 0 {
		t.Fatalf("re-embedded known historical text: %d", got)
	}
	if _, err := e.Store.Save(ctx, "copy.md", doc.Content, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := fillIndex(t, e, "history"); got != 0 {
		t.Fatalf("copied content should share vectors: %d", got)
	}
	reopened, err := Open(database(t, e.Store), e.Store)
	if err != nil {
		t.Fatal(err)
	}
	if got := fillIndex(t, reopened, "history"); got != 0 {
		t.Fatalf("restart discarded compatible embeddings: %d", got)
	}
	q := historyQuery("An observation")
	q.Mode = "meaning"
	q.Vector = []float32{1, 0, 0}
	q.ConfigID = testConfig().ID()
	result, err := reopened.HybridSearch(ctx, q)
	if err != nil || len(result.Matches) != 2 {
		t.Fatalf("unique semantic passages: %+v %v", result, err)
	}
	for _, hit := range result.Matches {
		if strings.Contains(hit.Text, "An observation") && hit.OccurrenceCount != 2 {
			t.Fatalf("copy provenance lost: %+v", hit)
		}
	}
}
func TestHistoryCompletedSearchDoesNotRewriteCache(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	if _, err := e.HybridSearch(ctx, historyQuery("observation")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(e.dir, "cache", "history.json")
	fixed := time.Unix(123456789, 0)
	if err := os.Chtimes(file, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.HybridSearch(ctx, historyQuery("observation")); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(file)
	if err != nil || !info.ModTime().Equal(fixed) {
		t.Fatalf("completed history search rewrote the cache: %v %v", info, err)
	}
}
func TestHistoryCrossSplitMatchCitesWholeNote(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	content := "# Cross split\n" + strings.Repeat("x", 104) + " crossing boundary " + strings.Repeat("y", 130) + "\n## Footer\nUnrelated footer.\n"
	if _, err := e.Store.Save(ctx, "a.md", []byte(content), doc.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Configure(ctx, testConfig(), false); err != nil {
		t.Fatal(err)
	}
	result, err := e.HybridSearch(ctx, historyQuery("crossing boundary"))
	if err != nil || len(result.Matches) != 1 {
		t.Fatalf("full-text match missing: %+v %v", result, err)
	}
	hit := result.Matches[0]
	if hit.Hash != "" || strings.Contains(hit.Citation, "fmn-passage") || !strings.Contains(hit.Text, "crossing boundary") || hit.NoteHash != workspace.ContentHash([]byte(content)) {
		t.Fatalf("cross-split match claims unrelated passage: %+v", hit)
	}
}
func TestHistoryPartialImportedCommitResumesAfterRestart(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	var imported []notestore.FileChange
	for i := 0; i < 600; i++ {
		imported = append(imported, notestore.FileChange{Path: fmt.Sprintf("import-%04d.md", i), Content: []byte(fmt.Sprintf("# Imported\nObservation %d.\n", i))})
	}
	commitFixture(t, e.Store, "Synthetic bulk import", imported...)
	first, err := e.PendingScope(ctx, 32, "history")
	if err != nil || first.Status.History.CompletedRevision == first.Status.History.Revision || first.Status.History.Versions != 512 {
		t.Fatalf("import scan was not bounded: %+v %v", first.Status, err)
	}
	reopened, err := Open(database(t, e.Store), e.Store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reopened.PendingScope(ctx, 32, "history")
	if err != nil || second.Status.History.Versions != 601 || second.Status.History.CompletedRevision != second.Status.History.Revision {
		t.Fatalf("partial import did not resume: %+v %v", second.Status, err)
	}
	result, err := reopened.HybridSearch(ctx, historyQuery("Observation 599."))
	if err != nil || len(result.Matches) != 1 {
		t.Fatal(result, err)
	}
}

// Stored vectors that fail validation fail closed at open; see
// TestCorruptStoredVectorsFailClosedAtOpen. The history catalog is derived and
// rebuilt.
func TestHistoryRebuildsCorruptCatalog(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	if _, err := e.Configure(ctx, testConfig(), false); err != nil {
		t.Fatal(err)
	}
	fillIndex(t, e, "history")
	if err := os.WriteFile(filepath.Join(e.dir, "cache", "history.json"), []byte("broken derived cache"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(database(t, e.Store), e.Store)
	if err != nil {
		t.Fatal(err)
	}
	text, err := reopened.HybridSearch(ctx, historyQuery("observation"))
	if err != nil || len(text.Matches) != 1 {
		t.Fatal(text, err)
	}
}

func TestHistoryRetriesFailedCheckpointWithoutAnotherCommit(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	cache := filepath.Join(e.dir, "cache")
	if err := os.WriteFile(cache, []byte("synthetic obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := e.HybridSearch(ctx, historyQuery("observation"))
	if err != nil || first.History.State != "unavailable" {
		t.Fatal(first, err)
	}
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	second, err := e.HybridSearch(ctx, historyQuery("observation"))
	if err != nil || second.History.Message != "" {
		t.Fatal(second, err)
	}
	if _, err := os.Stat(filepath.Join(cache, "history.json")); err != nil {
		t.Fatalf("checkpoint was not retried: %v", err)
	}
}

func TestHistoryBoundsPassageExpansionAndReturnedProvenance(t *testing.T) {
	t.Setenv("FMN_LIMIT_HISTORY_BYTES", "6400000")
	e, _ := indexFixture(t)
	ctx := context.Background()
	content := ""
	for i := 0; i < 160; i++ {
		content += fmt.Sprintf("## Piece %d\nSharedword evidence.\n", i)
	}
	var copies []notestore.FileChange
	for i := 0; i < 700; i++ {
		copies = append(copies, notestore.FileChange{Path: fmt.Sprintf("copy-%04d.md", i), Content: []byte(content)})
	}
	commitFixture(t, e.Store, "Synthetic shared-blob import", copies...)
	for i := 0; i < 3; i++ {
		if _, err := e.PendingScope(ctx, 1, "history"); err != nil {
			t.Fatal(err)
		}
	}
	result, err := e.HybridSearch(ctx, historyQuery("Sharedword"))
	if err != nil || result.History.State != "limited" || len(result.Matches) == 0 {
		t.Fatalf("history expansion was not explicitly bounded: %+v %v", result.History, err)
	}
	for _, hit := range result.Matches {
		if len(hit.Occurrences) > 256 || !hit.OccurrencesTruncated || hit.OccurrenceCount <= 256 {
			t.Fatalf("unbounded or misleading provenance: %d/%d", len(hit.Occurrences), hit.OccurrenceCount)
		}
	}
	e.mu.Lock()
	parts, err := e.historyPassages(ctx)
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 100000 {
		t.Fatalf("passage allocation exceeds explicit bound: %d", len(parts))
	}
}

func TestHistoryFiltersUseHistoricalMetadataAndSafeVisibleNotes(t *testing.T) {
	ctx := context.Background()
	store := newNotebook(t, []string{"private"})
	old, err := store.Save(ctx, "proof.md", []byte("---\ntags: [retired]\nstatus: old\n---\n# Proof\nAncient testimony.\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(ctx, old.Path, []byte("---\ntags: [current]\nstatus: new\n---\n# Proof\nRecent testimony.\n"), old.Revision, ""); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"private/hidden.md", "not-a-note.txt"} {
		if _, err = store.Save(ctx, path, []byte("Ancient testimony."), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	commitFixture(t, store, "Synthetic symlink", notestore.FileChange{Path: "link.md", Content: []byte("proof.md"), Mode: gitobj.ModeSymlink})
	e, err := Open(database(t, store), store)
	if err != nil {
		t.Fatal(err)
	}
	query := historyQuery("Ancient testimony")
	query.Tag = "RETIRED"
	query.Status = "OLD"
	result, err := e.HybridSearch(ctx, query)
	if err != nil || len(result.Matches) != 1 || result.Matches[0].Revision != old.Revision {
		t.Fatal(result, err)
	}
	query.Tag = "current"
	if result, err = e.HybridSearch(ctx, query); err != nil || len(result.Matches) != 0 {
		t.Fatal(result, err)
	}
	query.Tag = ""
	query.Status = ""
	if result, err = e.HybridSearch(ctx, query); err != nil || len(result.Matches) != 1 {
		t.Fatalf("excluded, non-note or symlink content leaked: %+v %v", result, err)
	}
}

func TestOversizedHistoricalNoteDoesNotBreakCurrentSearch(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	commitFixture(t, e.Store, "Synthetic oversized historical blob", notestore.FileChange{Path: "oversized.md", Content: make([]byte, workspace.MaxFileBytes()+1)})
	commitFixture(t, e.Store, "Remove synthetic oversized blob", notestore.FileChange{Path: "oversized.md"})
	result, err := e.HybridSearch(ctx, historyQuery("observation"))
	if err != nil || result.History.State != "limited" || !strings.Contains(result.History.Message, "FMN_LIMIT_FILE_BYTES") {
		t.Fatalf("oversized history was not reported: %+v %v", result.History, err)
	}
	current, err := e.HybridSearch(ctx, HybridQuery{SearchQuery: SearchQuery{Text: "observation"}, Mode: "text"})
	if err != nil || len(current.Matches) != 1 {
		t.Fatal(current, err)
	}
	if _, err = e.Configure(ctx, testConfig(), false); err != nil {
		t.Fatal(err)
	}
	fillIndex(t, e, "history")
	pending, err := e.PendingScope(ctx, 32, "history")
	if err != nil || len(pending.Passages) != 0 || pending.Status.History.State != "limited" || !strings.Contains(pending.Status.History.Message, "FMN_LIMIT_FILE_BYTES") {
		t.Fatalf("completed historical embeddings erased an applicable scan limit: %+v %v", pending.Status.History, err)
	}
}

func TestHistoryRejectsCorruptNegativeResumeCursor(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	if _, err := e.HybridSearch(ctx, historyQuery("observation")); err != nil {
		t.Fatal(err)
	}
	e.history.ChangeCursor = -1
	if err := persist(filepath.Join(e.dir, "cache", "history.json"), e.history); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(database(t, e.Store), e.Store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reopened.HybridSearch(ctx, historyQuery("observation"))
	if err != nil || len(result.Matches) != 1 {
		t.Fatalf("corrupt cursor did not safely rebuild: %+v %v", result, err)
	}
}
