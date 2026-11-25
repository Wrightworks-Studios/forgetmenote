package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHybridRanksNotesAndAppliesSharedFilters(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	contents := map[string]string{
		"a.md": "---\ntitle: Eldur\ntags: [lore]\nstatus: canon\n---\n# Fire\nHeat.",
		"b.md": "---\ntags: [lore]\nstatus: canon\n---\n# Fire\nEldur is mentioned here.\n",
		"c.md": "---\ntags: [lore]\nstatus: canon\n---\n# Flame\nWarmth and light.",
	}
	// More than the candidate limit in one note must not crowd out other notes.
	for i := 0; i < 65; i++ {
		contents["b.md"] += fmt.Sprintf("\n## Section %d\nA distinct fire observation %d.\n", i, i)
	}
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		rev := ""
		if p == "a.md" {
			rev = doc.Revision
		}
		if _, err := e.Store.Save(ctx, p, []byte(contents[p]), rev, ""); err != nil {
			t.Fatal(err)
		}
	}
	status, err := e.Configure(ctx, testConfig(), false)
	if err != nil {
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
		sub := Submission{Revision: pending.Status.Revision, ConfigID: status.ConfigID}
		for _, p := range pending.Passages {
			v := []float32{0, 1, 0}
			if p.Path == "b.md" {
				v = []float32{.8, .6, 0}
			}
			if p.Path == "c.md" {
				v = []float32{1, 0, 0}
			}
			sub.Vectors = append(sub.Vectors, VectorSubmission{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: v})
		}
		if _, err = e.Submit(ctx, sub); err != nil {
			t.Fatal(err)
		}
	}
	query := HybridQuery{SearchQuery: SearchQuery{Text: "Eldur", Vector: []float32{1, 0, 0}, ConfigID: status.ConfigID, Tag: "LORE", Status: "CANON", Limit: 3}}
	for mode, want := range map[string][]string{"all": {"a.md", "b.md", "c.md"}, "text": {"a.md", "b.md"}, "meaning": {"c.md", "b.md", "a.md"}} {
		query.Mode = mode
		got, err := e.HybridSearch(ctx, query)
		if err != nil || len(got.Matches) != len(want) {
			t.Fatalf("%s: %+v %v", mode, got, err)
		}
		for i, p := range want {
			if got.Matches[i].Path != p {
				t.Fatalf("%s: %+v", mode, got)
			}
		}
	}
	query.Mode = "all"
	query.Status = "draft"
	if got, err := e.HybridSearch(ctx, query); err != nil || len(got.Matches) != 0 {
		t.Fatal(got, err)
	}
	query.Status = ""
	query.Path = "c.md"
	if got, err := e.HybridSearch(ctx, query); err != nil || len(got.Matches) != 1 || got.Matches[0].Path != "c.md" {
		t.Fatal(got, err)
	}
	query.ConfigID = "wrong"
	if _, err := e.HybridSearch(ctx, query); err == nil {
		t.Fatal("accepted mismatched agent vector")
	}
}

func TestHybridFallbackAndInvalidInput(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	for _, q := range []HybridQuery{{}, {SearchQuery: SearchQuery{Text: "Alpha"}, Mode: "unknown"}} {
		if _, err := e.HybridSearch(ctx, q); err == nil {
			t.Fatal("invalid input accepted", q)
		}
	}
	for _, mode := range []string{"all", "meaning"} {
		got, err := e.HybridSearch(ctx, HybridQuery{SearchQuery: SearchQuery{Text: "Alpha"}, Mode: mode})
		if err != nil || got.Message == "" {
			t.Fatal(got, err)
		}
		if mode == "all" && (got.Mode != "text" || len(got.Matches) != 1) {
			t.Fatal(got)
		}
		if mode == "meaning" && len(got.Matches) != 0 {
			t.Fatal("meaning silently returned text", got)
		}
	}
	config := testConfig()
	config.Endpoint = true
	e.Configure(ctx, config, false)
	pending, _ := e.Pending(ctx, 1)
	p := pending.Passages[0]
	e.Submit(ctx, Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline", 503) }))
	defer ts.Close()
	endpoint, _ := NewEndpoint(ts.URL+"/api/embed", "")
	e.provider.Store(endpoint)
	got, err := e.HybridSearch(ctx, HybridQuery{SearchQuery: SearchQuery{Text: "Alpha"}})
	if err != nil || got.Mode != "text" || len(got.Matches) != 1 || got.Message == "" {
		t.Fatal(got, err)
	}
}

func TestHybridDoesNotBlendDifferentRevisions(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	config := testConfig()
	config.Endpoint = true
	e.Configure(ctx, config, false)
	pending, _ := e.Pending(ctx, 1)
	p := pending.Passages[0]
	e.Submit(ctx, Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A save during model inference invalidates the old vector before ranking.
		if _, err := e.Store.Save(ctx, "a.md", []byte("# Alpha\nNew evidence."), doc.Revision, ""); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 0, 0}}})
	}))
	defer ts.Close()
	endpoint, _ := NewEndpoint(ts.URL+"/api/embed", "")
	e.provider.Store(endpoint)
	got, err := e.HybridSearch(ctx, HybridQuery{SearchQuery: SearchQuery{Text: "Alpha"}})
	if err != nil || len(got.Matches) != 1 || got.Matches[0].Text != "# Alpha" {
		t.Fatal(got, err)
	}
	if got.Message == "" {
		t.Fatal("concurrent invalidation reported a complete index", got)
	}
}
