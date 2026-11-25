package reposource

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

// Opt-in evaluation only reads a caller-selected private Git cache. Receipts and
// excerpts stay beside that cache, outside the application checkout.
func TestSourcePrivatePilotEvaluation(t *testing.T) {
	file := os.Getenv("FMN_SOURCE_EVAL_CONFIG")
	if file == "" {
		t.Skip("private source evaluation is opt-in")
	}
	var cfg struct {
		Git, Revision, Output string
		Cases                 []struct {
			Name, Query string
			Paths       []string
		}
	}
	b, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		t.Fatal("invalid evaluation configuration")
	}
	if !filepath.IsAbs(cfg.Git) || !filepath.IsAbs(cfg.Output) {
		t.Fatal("absolute private paths required")
	}
	c := &cache{dir: filepath.Join(cfg.Output, "cache"), limits: Limits{History: 2, CacheBytes: 1 << 30, EvidenceBytes: 1 << 20, FileBytes: 1 << 20, Files: 10000, Seconds: 120}, allowFile: true, remoteURL: func(Repository) string { return "file://" + cfg.Git }}
	state := State{ID: "pilot", Project: "example-service", Downloaded: cfg.Revision, Repository: Repository{ID: 42, FullName: "example/private-pilot", DefaultBranch: "main"}}
	ctx := context.Background()
	if err = c.refresh(ctx, state.Repository, cfg.Revision, ""); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	idx, err := c.buildIndex(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	lex, err := newSourceLexicon(ctx, idx)
	if err != nil {
		t.Fatal(err)
	}
	build := time.Since(started)
	type evaluation struct {
		Name        string
		Query       string
		Micros      int64
		Hit5, Hit10 bool
		Results     []SourceResult
	}
	results := []evaluation{}
	durations := []int64{}
	valid := 0
	var heap runtime.MemStats
	runtime.ReadMemStats(&heap)
	peak := heap.HeapAlloc
	for _, item := range cfg.Cases {
		q := SearchQuery{Projects: []string{state.Project}, Query: item.Query, Mode: "text", Limit: 12}
		start := time.Now()
		ranked, _, err := rankSourceLexical(ctx, idx, state, q, nil, lex)
		if err != nil {
			t.Fatal(err)
		}
		micros := time.Since(start).Microseconds()
		durations = append(durations, micros)
		e := evaluation{Name: item.Name, Query: item.Query, Micros: micros}
		e.Results = ranked[:min(12, len(ranked))]
		for i, r := range e.Results {
			raw, err := c.git(ctx, filepath.Join(c.dir, "source.git"), "", int(maxBlobBytes()), "cat-file", "blob", r.BlobID)
			if err != nil || r.StartByte < 0 || r.EndByte > len(raw) || string(raw[r.StartByte:r.EndByte]) != r.Text || hash([]byte(r.Text)) != r.ContentHash {
				t.Fatal("invalid source provenance", r.Path)
			}
			valid++
			for _, p := range item.Paths {
				if r.Path == p {
					if i < 5 {
						e.Hit5 = true
					}
					if i < 10 {
						e.Hit10 = true
					}
				}
			}
		}
		results = append(results, e)
		runtime.ReadMemStats(&heap)
		peak = max(peak, heap.HeapAlloc)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	bytes, _ := diskBytes(filepath.Join(c.dir, "index"))
	report := map[string]any{"revision": idx.Commit, "files": len(idx.Files), "chunks": len(idx.Chunks), "excluded": idx.Exclusions, "indexBytes": bytes, "estimatedLexicalBytes": lex.terms.EstimatedBytes(), "indexMilliseconds": build.Milliseconds(), "sampledPeakHeap": peak, "provenanceChecked": valid, "cases": results, "providerCalls": 0, "p50Micros": durations[len(durations)/2], "p95Micros": durations[min(len(durations)-1, len(durations)*95/100)]}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err = os.WriteFile(filepath.Join(cfg.Output, "keyword-evaluation.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("files=%d chunks=%d exclusions=%d bytes=%d build=%s provenance=%d; private receipt saved", len(idx.Files), len(idx.Chunks), len(idx.Exclusions), bytes, build, valid)
}
