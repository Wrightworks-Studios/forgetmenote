package knowledge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/decision"
	"forgetmenote/internal/workspace"
)

func hybridPaths(result HybridResult) []string {
	paths := []string{}
	for _, hit := range result.Matches {
		paths = append(paths, hit.Path)
	}
	return paths
}

// Automatic search ranks independent keywords with the chat BM25 index; Text
// keeps its literal-phrase contract.
func TestHybridAutomaticRanksKeywordsWhileTextStaysLiteral(t *testing.T) {
	engine := retrievalNotes(t, map[string]string{
		"garden.md":  "# Garden\nThe drip irrigation runs on a weekly schedule for the tomatoes.",
		"produce.md": "# Produce\nTomatoes ripen in August.",
		"other.md":   "# Other\nNothing relevant here.",
	})
	ctx := context.Background()
	query := HybridQuery{SearchQuery: SearchQuery{Text: "tomatoes irrigation schedule", Limit: 10}}
	got, err := engine.HybridSearch(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if paths := hybridPaths(got); len(paths) != 2 || paths[0] != "garden.md" || paths[1] != "produce.md" {
		t.Fatalf("automatic keyword ranking: %v %+v", paths, got)
	}
	if got.Matches[0].Excerpt == "" {
		t.Fatalf("keyword hit has no excerpt: %+v", got.Matches[0])
	}
	query.Mode = "text"
	literal, err := engine.HybridSearch(ctx, query)
	if err != nil || len(literal.Matches) != 0 {
		t.Fatalf("text mode stopped being a literal phrase match: %+v %v", literal, err)
	}
	query.Text = "weekly schedule"
	literal, err = engine.HybridSearch(ctx, query)
	if err != nil || len(literal.Matches) != 1 || literal.Matches[0].Path != "garden.md" {
		t.Fatalf("text mode phrase: %+v %v", literal, err)
	}
}

func TestHybridAutomaticKeepsExactTitlesFirstAndFilters(t *testing.T) {
	engine := retrievalNotes(t, map[string]string{
		"a.md":        "---\ntags: [lore]\n---\n# Compost\nSee the other note.",
		"b.md":        "---\ntags: [lore]\n---\n# Heap\nCompost compost compost needs turning.",
		"untagged.md": "# Bin\nCompost compost compost compost.",
	})
	got, err := engine.HybridSearch(context.Background(), HybridQuery{SearchQuery: SearchQuery{Text: "compost", Tag: "lore", Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if paths := hybridPaths(got); len(paths) != 2 || paths[0] != "a.md" || paths[1] != "b.md" {
		t.Fatalf("exact title or tag filter lost: %v", paths)
	}
}

func hybridJev(service *decision.Service, check func(context.Context) error) *JevRanking {
	return &JevRanking{Service: service, Scope: decision.Scope{Actor: "alice", Notebook: "book", Feature: "rerank", Run: "search", Operation: "search"}, Check: check}
}

func TestHybridJevRanksOnlyExplicitRequests(t *testing.T) {
	engine := retrievalNotes(t, map[string]string{
		"a.md": "# Record\nBicycle workshop bicycle workshop bicycle workshop attendance.",
		"z.md": "# Maintenance\nBicycle workshop chains require CERULEAN lubricant.",
	})
	var calls atomic.Int32
	service := rankingService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		scoreRelevance(w, r)
	})
	allow := func(context.Context) error { return nil }
	query := HybridQuery{SearchQuery: SearchQuery{Text: "bicycle workshop", Limit: 10}, Jev: hybridJev(service, allow)}
	typed, err := engine.HybridSearch(context.Background(), query)
	if err != nil || typed.Reranked || calls.Load() != 0 || hybridPaths(typed)[0] != "a.md" {
		t.Fatalf("search without an explicit request used Jev: %+v %v calls=%d", typed, err, calls.Load())
	}
	query.Rerank = true
	query.Mode = "text"
	literal, err := engine.HybridSearch(context.Background(), query)
	if err != nil || literal.Reranked || calls.Load() != 0 {
		t.Fatalf("text mode used Jev: %+v %v", literal, err)
	}
	query.Mode = "all"
	ranked, err := engine.HybridSearch(context.Background(), query)
	if err != nil || !ranked.Reranked || calls.Load() == 0 {
		t.Fatalf("explicit request did not rank: %+v %v", ranked, err)
	}
	if paths := hybridPaths(ranked); len(paths) != 2 || paths[0] != "z.md" || paths[1] != "a.md" {
		t.Fatalf("Jev order not applied: %v", paths)
	}
	query.Jev = nil
	unbound, err := engine.HybridSearch(context.Background(), query)
	if err != nil || unbound.Reranked || hybridPaths(unbound)[0] != "a.md" {
		t.Fatalf("request without an authorized binding: %+v %v", unbound, err)
	}
}

func TestHybridJevFailureKeepsLocalOrderAndRevocationFails(t *testing.T) {
	notes := map[string]string{
		"a.md": "# Record\nBicycle workshop bicycle workshop bicycle workshop attendance.",
		"z.md": "# Maintenance\nBicycle workshop chains require CERULEAN lubricant.",
	}
	t.Run("provider failure", func(t *testing.T) {
		engine := retrievalNotes(t, notes)
		service := rankingService(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) })
		got, err := engine.HybridSearch(context.Background(), HybridQuery{SearchQuery: SearchQuery{Text: "bicycle workshop"}, Rerank: true, Jev: hybridJev(service, func(context.Context) error { return nil })})
		if err != nil || got.Reranked || hybridPaths(got)[0] != "a.md" || !strings.Contains(got.Message, "Jev ranking is unavailable") {
			t.Fatalf("provider failure did not keep local order with a message: %+v %v", got, err)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		engine := retrievalNotes(t, notes)
		var revoked atomic.Bool
		service := rankingService(t, func(w http.ResponseWriter, r *http.Request) {
			revoked.Store(true)
			scoreRelevance(w, r)
		})
		_, err := engine.HybridSearch(context.Background(), HybridQuery{SearchQuery: SearchQuery{Text: "bicycle workshop"}, Rerank: true, Jev: hybridJev(service, func(context.Context) error {
			if revoked.Load() {
				return workspace.ErrForbidden
			}
			return nil
		})})
		if !errors.Is(err, workspace.ErrForbidden) {
			t.Fatalf("revocation during ranking was not reported: %v", err)
		}
	})
	t.Run("changed", func(t *testing.T) {
		engine := retrievalNotes(t, notes)
		service := rankingService(t, func(w http.ResponseWriter, r *http.Request) {
			doc, err := engine.Store.Read(context.Background(), "a.md", "")
			if err != nil {
				t.Error(err)
			} else if _, err = engine.Store.Save(context.Background(), doc.Path, []byte("Bicycle workshop changed during ranking."), doc.Revision, "operator"); err != nil {
				t.Error(err)
			}
			scoreRelevance(w, r)
		})
		_, err := engine.HybridSearch(context.Background(), HybridQuery{SearchQuery: SearchQuery{Text: "bicycle workshop"}, Rerank: true, Jev: hybridJev(service, func(context.Context) error { return nil })})
		if !errors.Is(err, workspace.ErrConflict) {
			t.Fatalf("stale ranking published: %v", err)
		}
	})
}

// Search runs as you type, so a partial word must keep its substring matches
// after the whole-word BM25 results.
func TestHybridAutomaticKeepsPartialWordMatches(t *testing.T) {
	engine := retrievalNotes(t, map[string]string{
		"guide.md": "# Guide\nEvery sample we take should be repeatable.",
		"week.md":  "# Week 40\nDuplicate samples taken at Mill Creek.",
	})
	got, err := engine.HybridSearch(context.Background(), HybridQuery{SearchQuery: SearchQuery{Text: "sample", Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if paths := hybridPaths(got); len(paths) != 2 || paths[0] != "guide.md" || paths[1] != "week.md" {
		t.Fatalf("whole word first, then substring: %v", paths)
	}
	got, err = engine.HybridSearch(context.Background(), HybridQuery{SearchQuery: SearchQuery{Text: "repeat", Limit: 10}})
	if err != nil || len(got.Matches) != 1 || got.Matches[0].Path != "guide.md" {
		t.Fatalf("partial word while typing: %+v %v", got, err)
	}
}

// Archived and trashed notes stay out of ordinary search; a search can still
// ask for Archives explicitly.
func TestHybridSearchExcludesArchivesUnlessRequested(t *testing.T) {
	engine := retrievalNotes(t, map[string]string{
		"current.md": "# Current\nHeron survey results.",
		"old.md":     "---\nfmn-container: archive\n---\n# Old\nHeron survey results from last year.",
	})
	got, err := engine.HybridSearch(context.Background(), HybridQuery{SearchQuery: SearchQuery{Text: "heron survey", Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if paths := hybridPaths(got); len(paths) != 1 || paths[0] != "current.md" {
		t.Fatalf("default search included Archives: %v", paths)
	}
	got, err = engine.HybridSearch(context.Background(), HybridQuery{SearchQuery: SearchQuery{Text: "heron survey", Limit: 10, ContainerSelection: ContainerSelection{Archives: "include"}}})
	if err != nil {
		t.Fatal(err)
	}
	if paths := hybridPaths(got); len(paths) != 2 {
		t.Fatalf("explicit Archives request: %v", paths)
	}
}
