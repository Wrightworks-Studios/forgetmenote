package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/notestore"
	"forgetmenote/internal/workspace"
)

func TestContainerHistoryRepairsPreFeatureCachedMetadata(t *testing.T) {
	e, old := indexFixture(t)
	ctx := context.Background()
	if _, err := e.Store.Save(ctx, old.Path, []byte("---\nfmn-container: archive\n---\n# Archived\ncacheclassificationneedle\n"), old.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HybridSearch(ctx, historyQuery("cacheclassificationneedle")); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e.history)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err = json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["version"] = 2
	for _, value := range legacy["blobs"].(map[string]any) {
		metadata := value.(map[string]any)["metadata"].(map[string]any)
		delete(metadata, "container")
		delete(metadata, "containerError")
	}
	raw, _ = json.Marshal(legacy)
	if err = os.WriteFile(filepath.Join(e.dir, "cache", "history.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(database(t, e.Store), e.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	query := historyQuery("cacheclassificationneedle")
	query.Archives = "exclude"
	query.Drafts = "exclude"
	got, err := reopened.HybridSearch(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Matches) != 0 {
		t.Fatalf("pre-feature cache made Archive ordinary: %+v", got)
	}
}

func TestContainerBM25ScopeAndPacking(t *testing.T) {
	e := retrievalNotes(t, map[string]string{
		"a-archive.md":  "---\nfmn-container: archive\n---\n# Beacon\nBeacon archival bright exact name.\n",
		"b-archive.md":  "---\nfmn-container: archive\n---\n# Beacon second\nBeacon older distinct account.\n",
		"z-ordinary.md": "# Current\n\n## First\nBeacon current evidence one.\n\n## Second\nBeacon current evidence two.\n\n## Third\nBeacon current evidence three.\n",
		"shared.md":     "---\nfmn-container: draft\n---\n# Work\nBeacon saved shared draft.\n",
	})
	for _, scope := range []string{"current", "history"} {
		for _, archives := range []string{"exclude", "lower"} {
			t.Run(scope+archives, func(t *testing.T) {
				raw, _ := json.Marshal(map[string]any{"text": "beacon", "scope": scope, "archives": archives, "drafts": "exclude", "localOnly": true, "limit": 3})
				var query RetrievalQuery
				if err := json.Unmarshal(raw, &query); err != nil {
					t.Fatal(err)
				}
				got, err := e.Retrieve(context.Background(), query)
				if err != nil {
					t.Fatal(err)
				}
				if len(got.Passages) != 3 {
					t.Fatalf("want three current passages: %+v", got)
				}
				for _, p := range got.Passages {
					if p.Path != "z-ordinary.md" {
						t.Fatalf("excluded/lower evidence consumed ranking or packing budget: %+v", got.Passages)
					}
				}
			})
		}
	}
}

func TestContainerSemanticTiersFacetsAndProvenance(t *testing.T) {
	notes := map[string]string{"z-current.md": "---\ntags: [visible]\nstatus: active\n---\n# Ordinary\nBeacon current evidence.\n"}
	for i := 0; i < 55; i++ {
		notes[fmt.Sprintf("a%02d.md", i)] = fmt.Sprintf("---\nfmn-container: archive\ntags: [archivedonly]\nstatus: old\n---\n# Beacon\nArchive evidence %d.\n", i)
	}
	e := groupedRetrievalNotes(t, notes)
	ctx := context.Background()
	config := testConfig()
	if _, err := e.Configure(ctx, config, false); err != nil {
		t.Fatal(err)
	}
	fillIndex(t, e, "history")
	for _, scope := range []string{"current", "history"} {
		for _, mode := range []string{"text", "meaning", "all"} {
			for _, choice := range []string{"lower", "exclude"} {
				t.Run(scope+mode+choice, func(t *testing.T) {
					query := HybridQuery{SearchQuery: SearchQuery{Text: "beacon", Scope: scope, Limit: 5, Vector: []float32{1, 0, 0}, ConfigID: config.ID(), ContainerSelection: ContainerSelection{Archives: choice, Drafts: "exclude"}}, Mode: mode}
					got, err := e.HybridSearch(ctx, query)
					if err != nil {
						t.Fatal(err)
					}
					if len(got.Matches) == 0 || got.Matches[0].Path != "z-current.md" {
						t.Fatalf("lower tier crowded included evidence: %+v", got)
					}
					if choice == "exclude" {
						if len(got.Matches) != 1 {
							t.Fatalf("excluded semantic candidates: %+v", got.Matches)
						}
						if got.Filters == nil || strings.Contains(fmt.Sprint(*got.Filters), "archivedonly") {
							t.Fatalf("excluded facets: %+v", got.Filters)
						}
					}
					semantic, err := e.Search(ctx, query.SearchQuery)
					if err != nil || len(semantic) == 0 || semantic[0].Path != "z-current.md" {
						t.Fatalf("direct semantic: %+v %v", semantic, err)
					}
				})
			}
		}
	}
}

func TestContainerIdenticalHistoryDoesNotInheritClassification(t *testing.T) {
	e := retrievalNotes(t, map[string]string{"included.md": "---\ntags: [visible]\n---\n# Shared text\nIdentical passage needle.\n", "archived.md": "---\nfmn-container: archive\ntags: [archiveonly]\n---\n# Shared text\nIdentical passage needle.\n"})
	for _, choice := range []string{"exclude", "lower"} {
		q := historyQuery("needle")
		q.Archives = choice
		q.Drafts = "exclude"
		got, err := e.HybridSearch(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if choice == "lower" {
			want = 2
		}
		if len(got.Matches) != 1 || got.Matches[0].Path != "included.md" || got.Matches[0].OccurrenceCount != want || got.Matches[0].Occurrences[0].Path != "included.md" {
			t.Fatalf("classification leaked through shared passage hash: %+v", got)
		}
		if choice == "exclude" && strings.Contains(fmt.Sprint(got.Filters), "archiveonly") {
			t.Fatalf("excluded facet: %+v", got.Filters)
		}
	}
}

func TestContainerPartialHistoryRetainsCoverageEvenWithNoMatches(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	commitFixture(t, e.Store, "Synthetic oversized historical note", notestore.FileChange{Path: "too-big.md", Content: make([]byte, workspace.MaxFileBytes()+1)})
	commitFixture(t, e.Store, "Remove oversized current note", notestore.FileChange{Path: "too-big.md"})
	for _, mode := range []string{"text", "all", "meaning"} {
		q := historyQuery("absentneedle")
		q.Mode = mode
		q.Archives = "exclude"
		q.Drafts = "exclude"
		got, err := e.HybridSearch(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Matches) != 0 || got.History == nil || got.History.State != "limited" || !strings.Contains(got.Message, "FMN_LIMIT_FILE_BYTES") {
			t.Fatalf("partial history became complete empty results: %+v", got)
		}
	}
	got, err := e.Retrieve(ctx, RetrievalQuery{Text: "absentneedle", Scope: "history", LocalOnly: true, ContainerSelection: ContainerSelection{Archives: "exclude", Drafts: "exclude"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Passages) != 0 || got.History == nil || got.History.State != "limited" || !strings.Contains(got.Message, "FMN_LIMIT_FILE_BYTES") {
		t.Fatalf("partial retrieval lost coverage: %+v", got)
	}
	// The legacy semantic-only array cannot carry partial coverage. It must fail
	// explicitly and direct callers to the structured hybrid result.
	if _, err = e.Search(ctx, SearchQuery{Text: "absentneedle", Scope: "history", ContainerSelection: ContainerSelection{Archives: "exclude", Drafts: "exclude"}}); err == nil {
		t.Fatal("semantic-only array silently reported incomplete history as empty")
	}
}

func TestContainerScopeAndMembershipInvalidateRetrievalCache(t *testing.T) {
	e := retrievalNotes(t, map[string]string{"same.md": "---\nfmn-container: draft\n---\n# Evidence\nSaffron needle.\n"})
	ctx := context.Background()
	q := RetrievalQuery{Text: "saffron", LocalOnly: true, ContainerSelection: ContainerSelection{Archives: "exclude", Drafts: "include"}}
	included, err := e.Retrieve(ctx, q)
	if err != nil || len(included.Passages) != 1 {
		t.Fatal(included, err)
	}
	q.Drafts = "exclude"
	excluded, err := e.Retrieve(ctx, q)
	if err != nil || len(excluded.Passages) != 0 || excluded.ScopeID == included.ScopeID {
		t.Fatal("scope reused included evidence", excluded, err)
	}
	q.Drafts = "include"
	doc, err := e.Store.Read(ctx, "same.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.SetContainer(ctx, doc.Path, "archive", doc.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	changed, err := e.Retrieve(ctx, q)
	if err != nil || len(changed.Passages) != 0 || changed.Revision == included.Revision {
		t.Fatal("membership reused included evidence", changed, err)
	}
	history, err := e.HybridSearch(ctx, HybridQuery{SearchQuery: SearchQuery{Text: "saffron", Scope: "history", ContainerSelection: q.ContainerSelection}, Mode: "text"})
	if err != nil || len(history.Matches) != 1 || history.Matches[0].Container != "draft" {
		t.Fatal("current membership rewrote version-local policy", history, err)
	}
	original, err := e.Store.Read(ctx, included.Passages[0].Path, included.Passages[0].Revision)
	if err != nil || workspace.ContentHash(original.Content) != included.Passages[0].NoteHash {
		t.Fatal("classification changed pinned citation", original, err)
	}
}

func TestContainerHistoryTiersPrecedeOccurrenceAndRetrievalCaps(t *testing.T) {
	var body strings.Builder
	body.WriteString("# Shared\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&body, "\n## Part %d\nneedle evidence %d.\n", i, i)
	}
	e := retrievalNotes(t, map[string]string{"z-included.md": body.String()})
	var archived []notestore.FileChange
	for i := 0; i < 300; i++ {
		archived = append(archived, notestore.FileChange{Path: fmt.Sprintf("a%03d.md", i), Content: []byte("---\nfmn-container: archive\n---\n" + body.String())})
	}
	commitFixture(t, e.Store, "Synthetic classified shared passages", archived...)
	for _, choice := range []string{"lower", "exclude"} {
		q := historyQuery("needle")
		q.Archives = choice
		q.Drafts = "exclude"
		got, err := e.HybridSearch(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Matches) == 0 {
			t.Fatal("included history lost")
		}
		for _, hit := range got.Matches {
			if hit.Path != "z-included.md" || hit.Occurrences[0].Path != "z-included.md" {
				t.Fatalf("lower history won representative/occurrence cap: %+v", hit)
			}
			if choice == "lower" && (!hit.OccurrencesTruncated || hit.OccurrenceCount != 301 || len(hit.Occurrences) != 256) {
				t.Fatalf("wrong bounded provenance: %+v", hit)
			}
		}
		evidence, err := e.Retrieve(context.Background(), RetrievalQuery{Text: "needle", Scope: "history", LocalOnly: true, Limit: 3, ContainerSelection: q.ContainerSelection})
		if err != nil {
			t.Fatal(err)
		}
		if len(evidence.Passages) != 3 {
			t.Fatal("eligible history passage cap", evidence)
		}
		for _, p := range evidence.Passages {
			if p.Path != "z-included.md" {
				t.Fatalf("lower candidates consumed 10000 passage or packing cap: %+v", evidence)
			}
		}
		if choice == "lower" && (evidence.History == nil || evidence.History.State != "limited" || !strings.Contains(evidence.Message, "10000 passage")) {
			t.Fatal("candidate partial limit hidden", evidence)
		}
	}
}

func TestContainerRetrieveSemanticSaturationKeepsVectorReuse(t *testing.T) {
	notes := map[string]string{"z-current.md": "# Current\nbeacon ordinary evidence\n"}
	for i := 0; i < 105; i++ {
		notes[fmt.Sprintf("a%03d.md", i)] = fmt.Sprintf("---\nfmn-container: archive\n---\n# Beacon\nArchived evidence %d.\n", i)
	}
	e := groupedRetrievalNotes(t, notes)
	calls := syntheticRetrievalEndpoint(t, e, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{0, 1, 0}}})
	})
	for _, scope := range []string{"current", "history"} {
		for _, choice := range []string{"lower", "exclude"} {
			got, err := e.Retrieve(context.Background(), RetrievalQuery{Text: "beacon", Scope: scope, Limit: 1, ContainerSelection: ContainerSelection{Archives: choice, Drafts: "exclude"}})
			if err != nil || got.Mode != "hybrid" || len(got.Passages) != 1 || got.Passages[0].Path != "z-current.md" {
				t.Fatalf("semantic 100-candidate saturation bypassed tier: %+v %v", got, err)
			}
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("synthetic semantic branch not exercised: %d", calls.Load())
	}
	before := len(e.cache.Vectors)
	configID := e.state.Config.ID()
	settings := e.RetrievalSettings()
	settings.Settings.Search.Archives = "exclude"
	if _, err := e.SetRetrievalSettings(settings.Settings, settings.ID); err != nil {
		t.Fatal(err)
	}
	doc, err := e.Store.Read(context.Background(), "z-current.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.SetContainer(context.Background(), doc.Path, "archive", doc.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	if count := fillIndex(t, e, ""); count != 0 || len(e.cache.Vectors) != before || e.state.Config.ID() != configID {
		t.Fatalf("classification/preferences re-embedded unchanged text: %d", count)
	}
}
