package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/pg/pgtest"
)

func completedHistorySearch(t *testing.T, e *Engine, text string) HybridResult {
	t.Helper()
	ctx := context.Background()
	head, err := e.Store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result HybridResult
	for range 100 {
		result, err = e.HybridSearch(ctx, HybridQuery{SearchQuery: SearchQuery{Text: text, Scope: "history"}, Mode: "text"})
		if err != nil {
			t.Fatal(err)
		}
		if result.History != nil && result.History.CompletedRevision == head.Revision {
			return result
		}
	}
	t.Fatalf("history never completed: %+v", result.History)
	return result
}

// A notebook imported from Git keeps its merges: history covers every
// version HEAD reaches, side branches and merge resolutions included, and
// nothing HEAD does not reach.
func TestHistoricalSearchIncludesMergedAncestorsAndMergeResolution(t *testing.T) {
	var side, merged string
	store := importedNotebook(t, func(git func(args ...string) string, repo string) {
		write := func(path, content string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", path)
		}
		write("a.md", "---\ntags: [research]\n---\n# Alpha\nAn observation.\n")
		git("commit", "--quiet", "-m", "Synthetic note")
		git("checkout", "--quiet", "-b", "side")
		write("a.md", "# Side\nSidebranchuniquewitness.\n")
		git("commit", "--quiet", "-m", "Synthetic side edit")
		side = git("rev-parse", "HEAD")
		git("checkout", "--quiet", "main")
		write("a.md", "# Main\nMainbranchuniquewitness.\n")
		git("commit", "--quiet", "-m", "Synthetic main edit")
		// The merge's own tree resolves both edits with new text.
		git("merge", "--quiet", "--no-ff", "--no-commit", "-s", "ours", "side")
		write("a.md", "# Resolution\nMergeresolutionuniquewitness.\n")
		git("commit", "--quiet", "-m", "Resolve synthetic merge")
		merged = git("rev-parse", "HEAD")
		git("checkout", "--quiet", "-b", "unmerged")
		write("private-branch.md", "Unmergeduniquewitness.\n")
		git("commit", "--quiet", "-m", "Synthetic unmerged edit")
		git("checkout", "--quiet", "main")
	})
	e, err := Open(database(t, store), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	ctx := context.Background()
	for _, tc := range []struct{ text, revision string }{
		{"Sidebranchuniquewitness", side},
		{"Mergeresolutionuniquewitness", merged},
	} {
		t.Run(tc.text, func(t *testing.T) {
			result := completedHistorySearch(t, e, tc.text)
			if len(result.Matches) != 1 || result.Matches[0].Revision != tc.revision {
				t.Fatalf("missing or misattributed history: %+v", result.Matches)
			}
			match := result.Matches[0]
			doc, err := e.Store.Read(ctx, match.Path, match.Revision)
			if err != nil || !strings.Contains(string(doc.Content), tc.text) {
				t.Fatalf("historical citation did not resolve: %v", err)
			}
		})
	}
	if matches := completedHistorySearch(t, e, "Unmergeduniquewitness").Matches; len(matches) != 0 {
		t.Fatalf("unmerged branch leaked into history scope: %+v", matches)
	}
	if status, err := e.Store.Status(ctx); err != nil || status.Revision != merged {
		t.Fatal("search changed the head", status, err)
	}
}

func TestHistoricalSearchRetainsTransientVersionsBetweenScans(t *testing.T) {
	e, initial := indexFixture(t)
	ctx := context.Background()
	completedHistorySearch(t, e, "observation")
	transient, err := e.Store.Save(ctx, "a.md", []byte("# Transient\nTransientuniquewitness.\n"), initial.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := e.Store.Save(ctx, "a.md", initial.Content, transient.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	result := completedHistorySearch(t, e, "Transientuniquewitness")
	if len(result.Matches) != 1 || result.Matches[0].Revision != transient.Revision {
		t.Fatalf("lost intermediate version: %+v", result.Matches)
	}
	current, err := e.HybridSearch(ctx, HybridQuery{SearchQuery: SearchQuery{Text: "Transientuniquewitness"}, Mode: "text"})
	if err != nil || len(current.Matches) != 0 {
		t.Fatalf("current search included transient version: %+v %v", current, err)
	}
	if matches := completedHistorySearch(t, e, "observation").Matches; len(matches) != 1 {
		t.Fatalf("restored identical passage duplicated: %+v", matches)
	}
	if status, err := e.Store.Status(ctx); err != nil || status.Revision != restored.Revision {
		t.Fatal("search changed the head", status, err)
	}
}

func TestHistorySubmissionRejectsModelChangedDuringHistoryRead(t *testing.T) {
	e, _ := indexFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := testConfig()
	if _, err := e.Configure(ctx, config, false); err != nil {
		t.Fatal(err)
	}
	pending, err := e.PendingScope(ctx, 32, "history")
	if err != nil || len(pending.Passages) == 0 {
		t.Fatalf("missing initial passages: %v", err)
	}
	p := pending.Passages[0]
	submission := Submission{Scope: "history", Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID,
		Vectors: []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}}
	if _, err = e.Store.Save(ctx, "new.md", []byte("New committed content\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	// Without its derived history checkpoint, a restarted engine scans the
	// notebook's revisions from the start, without the index lock, before it
	// checks anything else of the history under that lock.
	e.Close()
	if err = os.Remove(filepath.Join(e.dir, "cache", "history.json")); err != nil {
		t.Fatal(err)
	}
	if e, err = Open(database(t, e.Store), e.Store); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	_, release := holdRevisionReads(t, e.db)
	submitted := make(chan error, 1)
	go func() { _, err := e.Submit(ctx, submission); submitted <- err }()
	for pgtest.LockWaiters(t, e.db) == 0 {
		select {
		case err := <-submitted:
			t.Fatalf("submission failed before model-change boundary: %v", err)
		case <-ctx.Done():
			t.Fatal("submission did not reach unlocked history IO")
		case <-time.After(10 * time.Millisecond):
		}
	}
	config.Model = "different-model-same-dimensions"
	configured := make(chan error, 1)
	go func() { _, err := e.Configure(ctx, config, true); configured <- err }()
	select {
	case err := <-configured:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("configuration blocked on historical IO")
	}
	release()
	select {
	case err := <-submitted:
		if err == nil {
			t.Fatal("old-model vectors accepted into new model cache")
		}
	case <-ctx.Done():
		t.Fatal("submission failed to finish")
	}
	next, err := e.Pending(ctx, 32)
	if err != nil || next.Status.ConfigID != config.ID() || next.Status.Indexed != 0 {
		t.Fatalf("new model cache contaminated: %+v %v", next.Status, err)
	}
}
