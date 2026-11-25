package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/workspace"
)

func TestRetrieveBM25WordOrderAndExactNames(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	notes := []struct{ path, text string }{
		{"a.md", "# Cycling\nBicycle maintenance repair the chain and brakes."},
		{"z-guide.md", "---\ntitle: Bicycle maintenance\n---\n# Guide\nChains should be oiled."},
		{"café.md", "# Café\nCAFÉ naïve Reykjavík."},
	}
	for _, note := range notes {
		revision := ""
		if note.path == doc.Path {
			revision = doc.Revision
		}
		if _, err := e.Store.Save(ctx, note.path, []byte(note.text), revision, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ query, first string }{
		{"brakes chain", "a.md"}, {"Bicycle maintenance", "z-guide.md"}, {"café", "café.md"}, {"naïve CAFÉ", "café.md"},
	} {
		got, err := e.Retrieve(ctx, RetrievalQuery{Text: tc.query, LocalOnly: true})
		if err != nil || len(got.Passages) == 0 || got.Passages[0].Path != tc.first {
			t.Fatalf("query %q: %+v %v", tc.query, got, err)
		}
		if got.Mode != "bm25" || !strings.Contains(got.Message, "BM25") {
			t.Fatalf("unlabeled lexical retrieval: %+v", got)
		}
	}
	// The sidebar's explicit Text mode retains exact substring semantics.
	sidebar, err := e.HybridSearch(ctx, HybridQuery{SearchQuery: SearchQuery{Text: "brakes chain"}, Mode: "text"})
	if err != nil || len(sidebar.Matches) != 0 {
		t.Fatal("sidebar phrase semantics changed", sidebar, err)
	}
}

func retrievalNotes(t *testing.T, notes map[string]string) *Engine {
	t.Helper()
	return seededRetrieval(t, notes, false)
}

// groupedRetrievalNotes commits up to 50 notes at a time, the edit group limit,
// for tests that need many notes rather than a commit for each one.
func groupedRetrievalNotes(t *testing.T, notes map[string]string) *Engine {
	t.Helper()
	return seededRetrieval(t, notes, true)
}

func seededRetrieval(t *testing.T, notes map[string]string, grouped bool) *Engine {
	t.Helper()
	store := newNotebook(t, nil)
	// Stable commit and path ordering also makes tie tests reproducible.
	paths := []string{}
	for path := range notes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var err error
	for grouped && len(paths) > 0 {
		group := workspace.EditGroup{ID: fmt.Sprintf("seed-%d", len(paths)), Summary: "Seed retrieval notes"}
		for _, path := range paths[:min(50, len(paths))] {
			group.Edits = append(group.Edits, workspace.NoteEdit{Path: path, Content: notes[path]})
		}
		paths = paths[len(group.Edits):]
		if _, err = store.ApplyGroup(context.Background(), group); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths {
		if _, err = store.Save(context.Background(), path, []byte(notes[path]), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := Open(database(t, store), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	return engine
}
func TestRetrieveBM25TermFrequencyLengthAndIDF(t *testing.T) {
	for _, tc := range []struct {
		name, query, first string
		notes              map[string]string
	}{
		{"frequency", "saffron", "z.md", map[string]string{"a.md": "# Entry\nsaffron mild mild mild mild mild", "z.md": "# Entry\nsaffron saffron saffron mild mild mild"}},
		{"length", "saffron", "z.md", map[string]string{"a.md": "# Entry\nsaffron " + strings.Repeat("mild ", 150), "z.md": "# Entry\nsaffron mild"}},
		{"idf", "common zircon", "z.md", map[string]string{"a.md": "# Entry\ncommon common common common", "b.md": "# Entry\ncommon plain plain plain", "c.md": "# Entry\ncommon plain plain plain", "z.md": "# Entry\nzircon plain plain plain"}},
		{"title_boost", "saffron", "z.md", map[string]string{"a.md": "# Entry\nsaffron mild", "z.md": "# Saffron notes\nmild mild"}},
		{"path_boost", "saffron", "saffron/entry.md", map[string]string{"a.md": "# Entry\nsaffron mild", "saffron/entry.md": "# Entry\nmild mild"}},
		{"ties", "saffron", "a.md", map[string]string{"a.md": "# Entry\nsaffron mild", "z.md": "# Entry\nsaffron mild"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := retrievalNotes(t, tc.notes)
			for i := 0; i < 2; i++ {
				r, err := e.Retrieve(context.Background(), RetrievalQuery{Text: tc.query, LocalOnly: true, Limit: 1})
				if err != nil || len(r.Passages) != 1 || r.Passages[0].Path != tc.first {
					t.Fatalf("%+v %v", r, err)
				}
			}
		})
	}
}
func TestRetrieveCompleteCitationsNeighborsDedupAndBudgets(t *testing.T) {
	content := "# Expedition\nThe observatory measures pulsars carefully. " + strings.Repeat("Unique neighboring context explains instrument calibration and uncertainty. ", 4) + "\n## Other\nPulsars emit radiation with precise timing.\n## Third\nPulsars provide another useful observation.\n"
	e := retrievalNotes(t, map[string]string{"a.md": content, "copy.md": content, "comparison.md": "# Comparison\nPulsars differ from quasars in rotation and mass."})
	if _, err := e.Configure(context.Background(), testConfig(), false); err != nil {
		t.Fatal(err)
	}
	r, err := e.Retrieve(context.Background(), RetrievalQuery{Text: "pulsars", LocalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]int{}
	hashes := map[string]bool{}
	neighbor := false
	for _, p := range r.Passages {
		byPath[p.Path]++
		if hashes[p.Hash] {
			t.Fatal("duplicate passage", p)
		}
		hashes[p.Hash] = true
		if !strings.Contains(strings.ToLower(p.Text), "pulsars") {
			neighbor = true
		}
		if workspace.ContentHash([]byte(p.Text)) != p.Hash || p.ID == "" || p.NoteHash == "" || p.SectionHash == "" {
			t.Fatal("incomplete evidence", p)
		}
		u, err := url.Parse(p.Citation)
		if err != nil || u.Query().Get("fmn-rev") != p.Revision || u.Query().Get("fmn-passage") != p.Hash || u.Query().Get("fmn-chars") != "128" {
			t.Fatal("unpinned citation", p, err)
		}
		doc, err := e.Store.Read(context.Background(), p.Path, p.Revision)
		if err != nil || !strings.Contains(string(doc.Content), p.Text) || workspace.ContentHash(doc.Content) != p.NoteHash {
			t.Fatal("citation cannot reproduce quote", p, err)
		}
	}
	if byPath["a.md"] < 3 || byPath["comparison.md"] == 0 || !neighbor {
		t.Fatalf("missing useful passages, comparison or neighbor: %+v", r)
	}
	bounded, err := e.Retrieve(context.Background(), RetrievalQuery{Text: "pulsars", LocalOnly: true, Limit: 2, TokenBudget: 700})
	if err != nil || len(bounded.Passages) > 2 || bounded.EstimatedTokens > 700 || len(bounded.Passages) == 0 {
		t.Fatal(bounded, err)
	}
	tiny, err := e.Retrieve(context.Background(), RetrievalQuery{Text: "pulsars", LocalOnly: true, TokenBudget: 1})
	if err != nil || len(tiny.Passages) != 0 || !strings.Contains(tiny.Message, "budget") {
		t.Fatal(tiny, err)
	}
}
func TestRetrieveFreshCommittedContentTrashAndLiveExclusion(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	first, err := e.Retrieve(ctx, RetrievalQuery{Text: "observation", LocalOnly: true})
	if err != nil || len(first.Passages) != 1 {
		t.Fatal(first, err)
	}
	changed, err := e.Store.Save(ctx, doc.Path, []byte("# Alpha\nReplacement evidence."), doc.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := e.Retrieve(ctx, RetrievalQuery{Text: "observation", LocalOnly: true})
	if err != nil || len(old.Passages) != 0 {
		t.Fatal("stale cache", old, err)
	}
	fresh, err := e.Retrieve(ctx, RetrievalQuery{Text: "replacement", LocalOnly: true})
	if err != nil || len(fresh.Passages) != 1 || fresh.Revision != changed.Revision || fresh.Passages[0].Hash == first.Passages[0].Hash {
		t.Fatal(fresh, err)
	}
	live, err := e.Store.LiveRead(ctx, doc.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.LivePatch(ctx, workspace.TargetedPatch{Path: doc.Path, Base: live.LiveRevision, OperationID: "retrieval-private-live", Edits: []workspace.TextEdit{{Start: 0, End: 0, Old: "", Text: "LiveOnlySecret\n"}}}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"LiveOnlySecret", "PrivateBrowserDraftSecret"} {
		result, err := e.Retrieve(ctx, RetrievalQuery{Text: text, LocalOnly: true})
		if err != nil || len(result.Passages) != 0 {
			t.Fatal("implicit draft retrieval", result, err)
		}
	}
	// Trash refuses unsaved live work, so use a separate committed fixture.
	trashEngine, original := indexFixture(t)
	if _, err = trashEngine.Retrieve(ctx, RetrievalQuery{Text: "observation", LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	tr, err := trashEngine.Store.TrashNotes(ctx, []string{original.Path}, original.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"current", "history"} {
		q := RetrievalQuery{Text: "observation", Scope: scope, LocalOnly: true}
		r, err := trashEngine.Retrieve(ctx, q)
		if err != nil || len(r.Passages) != 0 {
			t.Fatal("trash leak", r, err)
		}
		q.IncludeTrash = true
		r, err = trashEngine.Retrieve(ctx, q)
		if err != nil || len(r.Passages) == 0 {
			t.Fatal("missing explicit trash", r, err)
		}
	}
	listing, err := trashEngine.Store.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = trashEngine.Store.EmptyTrash(ctx, listing.Revision, listing.Confirmation); err != nil {
		t.Fatal(err)
	}
	r, err := trashEngine.Retrieve(ctx, RetrievalQuery{Text: "observation", Scope: "history", IncludeTrash: true, LocalOnly: true})
	if err != nil || len(r.Passages) != 0 {
		t.Fatal("emptied history resurrected", r, err)
	}
	_ = tr
}
func TestRetrieveVisibilityAfterRestart(t *testing.T) {
	f := evidenceNotebook(t)
	ctx := context.Background()
	for _, scope := range []string{"current", "history"} {
		q := RetrievalQuery{Text: "evidence", Scope: scope, LocalOnly: true}
		first, err := f.engine.Retrieve(ctx, q)
		if err != nil || len(first.Passages) < 2 {
			t.Fatal(first, err)
		}
		f.restart(t, []string{"private"})
		hidden, err := f.engine.Retrieve(ctx, q)
		if err != nil || len(hidden.Passages) != 1 || hidden.Passages[0].Path != "public.md" {
			t.Fatal("visibility leak", hidden, err)
		}
		f.restart(t, nil)
	}
}
func TestRetrieveRejectsInvalidAndCancelledRequests(t *testing.T) {
	e, _ := indexFixture(t)
	for _, q := range []RetrievalQuery{{}, {Text: "!!!"}, {Text: strings.Repeat("x", 2001)}, {Text: "x", Scope: "private"}, {Text: "x", Limit: 51}, {Text: "x", TokenBudget: 24001}, {Text: "x", Limit: -1}} {
		if _, err := e.Retrieve(context.Background(), q); err == nil {
			t.Fatal("unbounded request accepted", q)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Retrieve(ctx, RetrievalQuery{Text: "Alpha"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type retrievalQualityFixture struct {
	Notes   []struct{ Path, Content string } `json:"notes"`
	Queries []struct {
		ID, Text, Kind                string
		ExpectedPaths, CandidatePaths []string
	} `json:"queries"`
}

func loadRetrievalQuality(t *testing.T) (retrievalQualityFixture, *Engine) {
	t.Helper()
	raw, err := os.ReadFile("testdata/chat_retrieval_quality.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture retrievalQualityFixture
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	notes := map[string]string{}
	for _, note := range fixture.Notes {
		notes[note.Path] = note.Content
	}
	return fixture, retrievalNotes(t, notes)
}
func syntheticRetrievalEndpoint(t *testing.T, e *Engine, handler http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	ctx := context.Background()
	config := testConfig()
	config.Endpoint = true
	if _, err := e.Configure(ctx, config, false); err != nil {
		t.Fatal(err)
	}
	for {
		pending, err := e.Pending(ctx, 32)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending.Passages) == 0 {
			break
		}
		submission := Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID}
		seen := map[string]bool{}
		for _, p := range pending.Passages {
			if seen[p.Hash] {
				continue
			}
			seen[p.Hash] = true
			vector := []float32{0, 1, 0}
			if p.Path == "bicycle.md" {
				vector = []float32{1, 0, 0}
			}
			submission.Vectors = append(submission.Vectors, VectorSubmission{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: vector})
		}
		if _, err = e.Submit(ctx, submission); err != nil {
			t.Fatal(err)
		}
	}
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); handler(w, r) }))
	t.Cleanup(server.Close)
	endpoint, err := NewEndpoint(server.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	e.provider.Store(endpoint)
	return calls
}
func TestRetrieveSyntheticSemanticFusionAndLocalOnly(t *testing.T) {
	_, e := loadRetrievalQuality(t)
	calls := syntheticRetrievalEndpoint(t, e, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 0, 0}}})
	})
	local, err := e.Retrieve(context.Background(), RetrievalQuery{Text: "two-wheeler upkeep", LocalOnly: true})
	if err != nil || len(local.Passages) != 0 || calls.Load() != 0 || local.Mode != "bm25" {
		t.Fatal("local-only made a provider request", local, err, calls.Load())
	}
	hybrid, err := e.Retrieve(context.Background(), RetrievalQuery{Text: "two-wheeler upkeep"})
	if err != nil || len(hybrid.Passages) < 2 || hybrid.Mode != "hybrid" || calls.Load() != 1 {
		t.Fatal(hybrid, err, calls.Load())
	}
	for _, p := range hybrid.Passages {
		if p.Path != "bicycle.md" {
			t.Fatal("orthogonal semantic evidence selected", p)
		}
	}
}
func TestRetrieveRevalidatesInferenceAndCancellation(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		_, e := loadRetrievalQuality(t)
		var newRevision string
		syntheticRetrievalEndpoint(t, e, func(w http.ResponseWriter, r *http.Request) {
			doc, err := e.Store.Read(context.Background(), "bicycle.md", "")
			if err != nil {
				t.Error(err)
			}
			doc, err = e.Store.Save(context.Background(), doc.Path, []byte("# Bicycle workshop\nBrand new repair evidence."), doc.Revision, "")
			if err != nil {
				t.Error(err)
			}
			newRevision = doc.Revision
			json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 0, 0}}})
		})
		result, err := e.Retrieve(context.Background(), RetrievalQuery{Text: "Bicycle workshop"})
		if err != nil || result.Mode != "bm25" || result.Revision != newRevision || len(result.Passages) != 1 || !strings.Contains(result.Passages[0].Text, "Brand new") || !strings.Contains(result.Message, "changed") {
			t.Fatal("mixed inference revisions", result, err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		_, e := loadRetrievalQuality(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		syntheticRetrievalEndpoint(t, e, func(w http.ResponseWriter, r *http.Request) { cancel() })
		result, err := e.Retrieve(ctx, RetrievalQuery{Text: "two-wheeler upkeep"})
		if !errors.Is(err, context.Canceled) || len(result.Passages) != 0 {
			t.Fatal("cancelled evidence returned", result, err)
		}
	})
}
func TestRetrieveQualityMeasurements(t *testing.T) {
	fixture, e := loadRetrievalQuality(t)
	syntheticRetrievalEndpoint(t, e, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		vector := []float32{0, 0, 1}
		if len(request.Input) > 0 && request.Input[0] == "two-wheeler upkeep" {
			vector = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{vector}})
	})
	expected, baselineFound, bm25Found, hybridFound := 0, 0, 0, 0
	baselineTokens, bm25Tokens, hybridTokens := 0, 0, 0
	for _, q := range fixture.Queries {
		baseline, err := e.HybridSearch(context.Background(), HybridQuery{SearchQuery: SearchQuery{Text: q.Text, Limit: 12}, Mode: "text"})
		if err != nil {
			t.Fatal(err)
		}
		lexical, err := e.Retrieve(context.Background(), RetrievalQuery{Text: q.Text, LocalOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		fused, err := e.Retrieve(context.Background(), RetrievalQuery{Text: q.Text})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(baseline.Matches)
		baselineTokens += (len(raw) + 2) / 3
		bm25Tokens += lexical.EstimatedTokens
		hybridTokens += fused.EstimatedTokens
		paths := func(passages []Passage) map[string]bool {
			r := map[string]bool{}
			for _, p := range passages {
				r[p.Path] = true
			}
			return r
		}
		lexicalPaths, fusedPaths := paths(lexical.Passages), paths(fused.Passages)
		literalPaths := map[string]bool{}
		for _, p := range baseline.Matches {
			literalPaths[p.Path] = true
		}
		for _, path := range q.ExpectedPaths {
			expected++
			if literalPaths[path] {
				baselineFound++
			}
			if lexicalPaths[path] {
				bm25Found++
			}
			if fusedPaths[path] {
				hybridFound++
			}
			if !fusedPaths[path] {
				t.Errorf("%s: required source %s absent: %+v", q.ID, path, fused)
			}
		}
		if q.Kind == "missing" && (len(lexical.Passages) > 0 || len(fused.Passages) > 0) {
			t.Errorf("irrelevant evidence: %+v %+v", lexical, fused)
		}
		if q.Kind == "weak" {
			if len(fused.Passages) == 0 || !strings.Contains(fused.Message, "does not establish relevance") {
				t.Errorf("missing evidence limitation: %+v", fused)
			}
			for _, p := range fused.Passages {
				if strings.Contains(strings.ToLower(p.Text), "vaccin") {
					t.Fatal("fabricated evidence", p)
				}
			}
		}
		t.Logf("query=%s substring_paths=%d bm25_passages=%d hybrid_passages=%d bm25_estimated_tokens=%d hybrid_estimated_tokens=%d", q.ID, len(baseline.Matches), len(lexical.Passages), len(fused.Passages), lexical.EstimatedTokens, fused.EstimatedTokens)
	}
	t.Logf("quality expected_source_recall substring=%d/%d bm25=%d/%d hybrid=%d/%d; evidence_json_token_estimates substring=%d bm25=%d hybrid=%d across %d queries", baselineFound, expected, bm25Found, expected, hybridFound, expected, baselineTokens, bm25Tokens, hybridTokens, len(fixture.Queries))
	if baselineFound >= bm25Found || bm25Found >= hybridFound || hybridFound != expected {
		t.Fatal("quality improvement missing")
	}
}

func TestRetrieveLocalOnlyHistoryCannotScheduleEndpointIndexing(t *testing.T) {
	_, e := loadRetrievalQuality(t)
	calls := syntheticRetrievalEndpoint(t, e, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 0, 0}}})
	})
	if err := e.Start(e.provider.Load()); err != nil {
		t.Fatal(err)
	}
	result, err := e.Retrieve(context.Background(), RetrievalQuery{Text: "bicycle", Scope: "history", LocalOnly: true})
	if err == nil || !strings.Contains(err.Error(), "local-only history") || len(result.Passages) != 0 {
		t.Fatal(result, err)
	}
	e.Close()
	e.mu.Lock()
	scheduled := e.historyLoaded || e.historyResume
	e.mu.Unlock()
	if calls.Load() != 0 || scheduled {
		t.Fatal("local-only history triggered endpoint indexing", calls.Load(), scheduled)
	}
}
func TestRetrieveHistoricalCoverageAndPinnedOldEvidence(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		var err error
		doc, err = e.Store.Save(ctx, doc.Path, []byte(fmt.Sprintf("# Alpha\nRevision number %d contains distinct evidence.", i)), doc.Revision, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := e.Retrieve(ctx, RetrievalQuery{Text: "evidence", Scope: "history", LocalOnly: true})
	if err != nil || result.History == nil || result.History.CompletedRevision == result.History.Revision || !strings.Contains(result.Message, "incomplete") || len(result.Passages) == 0 {
		t.Fatal("incomplete history reported complete", result, err)
	}
	old, err := e.Retrieve(ctx, RetrievalQuery{Text: "observation", Scope: "history", LocalOnly: true})
	if err != nil || old.History == nil || old.History.CompletedRevision != old.History.Revision || len(old.Passages) != 1 || old.Passages[0].Revision == doc.Revision {
		t.Fatal("old evidence unavailable", old, err)
	}
	p := old.Passages[0]
	quote, err := e.Store.Read(ctx, p.Path, p.Revision)
	if err != nil || !strings.Contains(string(quote.Content), p.Text) || p.Hash != workspace.ContentHash([]byte(p.Text)) {
		t.Fatal("historical citation mismatch", p, err)
	}
}
func TestRetrieveComparisonRetainsBothSourcesAtSmallLimit(t *testing.T) {
	_, e := loadRetrievalQuality(t)
	result, err := e.Retrieve(context.Background(), RetrievalQuery{Text: "solar wind energy output", Limit: 2, LocalOnly: true})
	if err != nil || len(result.Passages) != 2 || result.Passages[0].Path == result.Passages[1].Path {
		t.Fatal("comparison crowded out", result, err)
	}
}
