package reposource

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/blob/blobtest"
)

func flatVectors(texts []string) [][]float32 {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out
}

// buyIndex has f's gateway, the lease holder, embed alpha and classify its
// map, counting every input it pays for.
func (f *sourceFixture) buyIndex(t *testing.T) (embedded, classified *int) {
	t.Helper()
	embedded, classified = new(int), new(int)
	f.m.SetEmbedder(func(_ context.Context, _ IndexConfig, texts []string, background bool) ([][]float32, error) {
		if background {
			*embedded += len(texts)
		}
		return flatVectors(texts), nil
	})
	f.m.SetMapClassifier(func() string { return "fixture-v1" }, func(_ context.Context, _ State, inputs []MapInput) ([]MapClassification, error) {
		var out []MapClassification
		for _, in := range inputs {
			*classified++
			out = append(out, MapClassification{in.Path, MapLabel{InputHash: in.InputHash, Model: "fixture-v1", Topic: "observability", Probability: .9}})
		}
		return out, nil
	})
	ctx := t.Context()
	s, _ := f.m.Status("alpha")
	if _, err := f.m.ConfigureIndex("alpha", "owner", s.Generation, IndexConfig{Embeddings: true, ExternalProcessing: true, MapJudgments: true, Model: "fixture", Dimensions: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	f.embedAll(t, "alpha")
	for {
		more, err := f.m.EnrichMapPending(ctx, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	if s, _ = f.m.Status("alpha"); *embedded == 0 || *classified == 0 || s.Index.Embedded != s.Index.Chunks || s.Index.MapClassified == 0 {
		t.Fatalf("the lease holder did not buy alpha's index: %d inputs, %d labels, %+v", *embedded, *classified, s.Index)
	}
	return embedded, classified
}

// freeloader makes m a gateway that must not pay for what is stored: its
// embedder answers only queries, and its map classifier fails the test.
func freeloader(t *testing.T, m *Manager) (queries *int) {
	queries = new(int)
	m.SetEmbedder(func(_ context.Context, _ IndexConfig, texts []string, background bool) ([][]float32, error) {
		if background {
			t.Errorf("a gateway paid again to embed %d stored inputs", len(texts))
		} else {
			*queries++
		}
		return flatVectors(texts), nil
	})
	m.SetMapClassifier(func() string { return "fixture-v1" }, func(_ context.Context, _ State, inputs []MapInput) ([]MapClassification, error) {
		t.Errorf("a gateway paid again to classify %d stored map entries", len(inputs))
		return nil, ErrUnavailable
	})
	return queries
}

// servesAlpha checks that m answers semantic search, map queries and file reads
// for alpha from the generation and evidence another gateway bought.
func servesAlpha(t *testing.T, m *Manager, s State, retained File) {
	t.Helper()
	ctx := t.Context()
	out, err := m.Search(ctx, SearchQuery{Projects: []string{"alpha"}, Query: "Health", Check: allowSource})
	if err != nil {
		t.Fatal(err)
	}
	coverage, semantic := out.Coverage["alpha"], false
	for _, r := range out.Results {
		semantic = semantic || r.Path == "health.go" && slices.Contains(r.Methods, "meaning")
	}
	if coverage.Mode != "hybrid" || coverage.Chunks == 0 || coverage.Embedded != coverage.Chunks || !semantic {
		t.Fatalf("search did not use the stored vectors: %+v %+v", coverage, out.Results)
	}
	file, err := m.File(ctx, "alpha", s.ID, s.Downloaded, "README.md")
	if err != nil || !file.Retained || file.Content != retained.Content || file.Hash != retained.Hash {
		t.Fatalf("retained evidence: %+v %v", file, err)
	}
	labelled, err := m.Map(ctx, "alpha", MapQuery{Query: "observability"})
	if err != nil || len(labelled.Entries) == 0 || labelled.Entries[0].Label == nil || labelled.Entries[0].Label.Topic != "observability" {
		t.Fatalf("the stored map labels: %+v %v", labelled, err)
	}
	code, err := m.File(ctx, "alpha", s.ID, s.Downloaded, "health.go")
	if err != nil || code.Retained || !strings.Contains(code.Content, "func Health") {
		t.Fatalf("a source file from this gateway's own mirror: %+v %v", code, err)
	}
}

// durableKeys lists what f's sources should keep in blob storage: each
// source's named generation, and alpha's retained README.
func durableKeys(t *testing.T, f *sourceFixture, retained File) []string {
	t.Helper()
	var keys []string
	for _, p := range []string{"alpha", "beta"} {
		s, err := f.m.Status(p)
		if err != nil || s.Index.Generation == "" {
			t.Errorf("%s's state names no index generation: %+v %v", p, s.Index, err)
		}
		for _, name := range []string{"chunks.jsonl", "manifest.json", "map.json", "vectors.f32", "vectors.json"} {
			keys = append(keys, sourcesDir+"/"+p+"/"+s.ID+"/index/generations/"+s.Index.Generation+"/"+name)
		}
		if p == "alpha" {
			keys = append(keys, sourcesDir+"/alpha/"+s.ID+"/evidence/blobs/"+retained.Hash, sourcesDir+"/alpha/"+s.ID+"/evidence/refs/"+evidenceID(42, s.Downloaded, "README.md")+".json")
		}
	}
	slices.Sort(keys)
	return keys
}

// Gateways that share an instance but not a disk: the lease holder pays for
// alpha's index and retains evidence, and the other gateway serves both from
// blob storage, again after losing its cache, and takes over the lease
// holder's work without buying anything again.
func TestGatewaysShareGenerationsAndEvidenceThroughBlobStorage(t *testing.T) {
	stores := map[string]func(t *testing.T) blob.Store{
		"directory": func(t *testing.T) blob.Store {
			store, err := blob.OpenDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			return store
		},
		"s3": func(t *testing.T) blob.Store { return blobtest.Start(t).Store(t, "instance") },
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			store := open(t)
			disk := t.TempDir()
			f := newSourceFixture(t, disk, store)
			f.buyIndex(t)
			s, _ := f.m.Status("alpha")
			retained, err := f.m.Capture(ctx, "alpha", s.ID, s.Downloaded, []string{"README.md"})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := store.List(ctx, sourcesDir+"/")
			if want := durableKeys(t, f, retained[0]); err != nil || !slices.Equal(stored, want) {
				t.Errorf("blob storage holds %q (%v), want %q", stored, err, want)
			}
			// The lease holder's disk is gone: the other gateway reaches only
			// what blob storage holds.
			if err = os.RemoveAll(disk); err != nil {
				t.Fatal(err)
			}
			other := t.TempDir()
			b := f.gateway(t, other, store)
			queries := freeloader(t, b)
			servesAlpha(t, b, s, retained[0])
			// Losing a cache costs a rebuild from blob storage.
			if err = os.RemoveAll(other); err != nil {
				t.Fatal(err)
			}
			servesAlpha(t, b, s, retained[0])
			if _, err = os.Stat(filepath.Join(other, "alpha", s.ID, "index", "generations", s.Index.Generation, "vectors.f32")); err != nil {
				t.Fatal("the stored generation was not fetched into the new cache:", err)
			}
			if _, err = b.Refresh(ctx, "alpha"); err != nil {
				t.Fatal(err)
			}
			if more, err := b.EmbedPending(ctx, "alpha"); err != nil || more {
				t.Fatal("embedding after a takeover:", more, err)
			}
			if more, err := b.EnrichMapPending(ctx, "alpha"); err != nil || more {
				t.Fatal("map classification after a takeover:", more, err)
			}
			servesAlpha(t, b, s, retained[0])
			if *queries != 3 {
				t.Fatalf("%d query embeddings for three searches", *queries)
			}
		})
	}
}

// Blob storage at the data directory, as a single host keeps it, is the cache
// itself: each source keeps today's files and nothing more, a map-only update
// still links the unchanged vectors rather than storing a copy, and another
// gateway over that directory serves them without paying again.
func TestDataDirectoryBlobsAreTheCache(t *testing.T) {
	ctx := t.Context()
	data := t.TempDir()
	store, err := blob.OpenDir(data)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f := newSourceFixture(t, filepath.Join(data, sourcesDir), store)
	f.buyIndex(t)
	s, _ := f.m.Status("alpha")
	vectors := func(s State) os.FileInfo {
		t.Helper()
		info, err := os.Stat(filepath.Join(data, sourcesDir, "alpha", s.ID, "index", "generations", s.Index.Generation, "vectors.f32"))
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	before := vectors(s)
	// A newer classifier relabels the map without touching the vectors.
	f.m.SetMapClassifier(func() string { return "fixture-v2" }, func(_ context.Context, _ State, inputs []MapInput) ([]MapClassification, error) {
		var out []MapClassification
		for _, in := range inputs {
			out = append(out, MapClassification{in.Path, MapLabel{InputHash: in.InputHash, Model: "fixture-v2", Topic: "observability", Probability: .8}})
		}
		return out, nil
	})
	if more, err := f.m.EnrichMapPending(ctx, "alpha"); err != nil || !more {
		t.Fatal(more, err)
	}
	relabelled, _ := f.m.Status("alpha")
	if relabelled.Index.Generation == s.Index.Generation || !os.SameFile(before, vectors(relabelled)) {
		t.Fatal("a map-only update did not link the vectors into its new generation")
	}
	retained, err := f.m.Capture(ctx, "alpha", s.ID, s.Downloaded, []string{"README.md"})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(data, sourcesDir, "alpha", s.ID)
	var files []string
	err = filepath.WalkDir(source, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(source, p)
		switch rel = filepath.ToSlash(rel); {
		case rel == "source.git":
			return fs.SkipDir
		case rel != "askpass" && !e.IsDir():
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	generation := "index/generations/" + relabelled.Index.Generation + "/"
	want := []string{"evidence/blobs/" + retained[0].Hash, "evidence/refs/" + evidenceID(42, s.Downloaded, "README.md") + ".json", "index/current.json", generation + "chunks.jsonl", generation + "manifest.json", generation + "map.json", generation + "vectors.f32", generation + "vectors.json"}
	if !slices.Equal(files, want) {
		t.Fatalf("alpha's source keeps %q, want %q", files, want)
	}
	if pointer, _ := os.ReadFile(filepath.Join(source, "index", "current.json")); string(pointer) != `{"generation":"`+relabelled.Index.Generation+`"}` {
		t.Fatalf("index/current.json is %s", pointer)
	}
	stored := map[string]bool{}
	if keys, err := store.List(ctx, sourcesDir+"/alpha/"+s.ID+"/"); err == nil {
		for _, key := range keys {
			stored[strings.TrimPrefix(key, sourcesDir+"/alpha/"+s.ID+"/")] = true
		}
	}
	if !stored[generation+"vectors.f32"] || !stored["evidence/refs/"+evidenceID(42, s.Downloaded, "README.md")+".json"] {
		t.Fatalf("the data directory's store does not hold alpha's files: %v", slices.Sorted(maps.Keys(stored)))
	}
	b := f.gateway(t, t.TempDir(), store)
	freeloader(t, b)
	b.SetMapClassifier(func() string { return "fixture-v2" }, func(_ context.Context, _ State, inputs []MapInput) ([]MapClassification, error) {
		t.Errorf("a gateway paid again to classify %d stored map entries", len(inputs))
		return nil, ErrUnavailable
	})
	servesAlpha(t, b, s, retained[0])
}

// A state saved before states named their index generation keeps the one its
// directory points at: a gateway serves it and names it at the next refresh,
// without paying for it again.
func TestUnnamedGenerationIsAdoptedWithoutPayingAgain(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	f := newSourceFixture(t, root, nil)
	f.buyIndex(t)
	s, _ := f.m.Status("alpha")
	retained, err := f.m.Capture(ctx, "alpha", s.ID, s.Downloaded, []string{"README.md"})
	if err != nil {
		t.Fatal(err)
	}
	version, body, err := f.m.db.ReadDocument(ctx, f.m.db.Pool, stateKind, "alpha", -1)
	var state diskState
	if err == nil {
		err = json.Unmarshal(body, &state)
	}
	if err != nil {
		t.Fatal(err)
	}
	for id, source := range state.Sources {
		source.Index.Generation = ""
		state.Sources[id] = source
	}
	raw, _ := json.Marshal(state)
	if _, err = f.m.db.WriteDocument(ctx, f.m.db.Pool, stateKind, "alpha", version, raw); err != nil {
		t.Fatal(err)
	}
	restarted := f.gateway(t, root, nil)
	freeloader(t, restarted)
	servesAlpha(t, restarted, s, retained[0])
	if _, err = restarted.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if more, err := restarted.EmbedPending(ctx, "alpha"); err != nil || more {
		t.Fatal("embedding an adopted generation:", more, err)
	}
	if adopted, _ := restarted.Status("alpha"); adopted.Index.Generation != s.Index.Generation {
		t.Fatalf("the refresh named %q, not the generation %q the directory held", adopted.Index.Generation, s.Index.Generation)
	}
}

// A generation blob storage lost while the lease holder still caches it is
// stored again at the next refresh, so other gateways serve it without anyone
// paying for it again.
func TestRefreshStoresAGenerationBlobStorageLost(t *testing.T) {
	ctx := t.Context()
	store := blobtest.Start(t).Store(t, "instance")
	f := newSourceFixture(t, t.TempDir(), store)
	f.buyIndex(t)
	s, _ := f.m.Status("alpha")
	retained, err := f.m.Capture(ctx, "alpha", s.ID, s.Downloaded, []string{"README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if err = blob.RemoveAll(ctx, store, sourcesDir+"/alpha/"+s.ID+"/index/"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if again, _ := f.m.Status("alpha"); again.Index.Generation != s.Index.Generation {
		t.Fatalf("the refresh replaced generation %q with %q", s.Index.Generation, again.Index.Generation)
	}
	other := f.gateway(t, t.TempDir(), store)
	freeloader(t, other)
	servesAlpha(t, other, s, retained[0])
}

// interrupting runs before once, ahead of storing the first key match accepts.
type interrupting struct {
	blob.Store
	match  func(key string) bool
	before func()
	once   sync.Once
}

func (s *interrupting) Put(ctx context.Context, key string, body io.ReadSeeker) error {
	if s.match != nil && s.match(key) {
		s.once.Do(s.before)
	}
	return s.Store.Put(ctx, key, body)
}

// Another gateway's write while the lease holder stores a paid embedding
// batch, such as a webhook delivery it records, costs neither: the state
// keeps the delivery and names the batch's generation, which nobody embeds
// again.
func TestAnotherGatewaysWriteKeepsAPaidBatch(t *testing.T) {
	ctx := t.Context()
	objects := blobtest.Start(t).Store(t, "instance")
	store := &interrupting{Store: objects}
	f := newSourceFixture(t, t.TempDir(), store)
	other := f.gateway(t, t.TempDir(), objects)
	paid := 0
	f.m.SetEmbedder(func(_ context.Context, _ IndexConfig, texts []string, background bool) ([][]float32, error) {
		if background {
			paid += len(texts)
		}
		return flatVectors(texts), nil
	})
	s, _ := f.m.Status("alpha")
	if _, err := f.m.ConfigureIndex("alpha", "owner", s.Generation, IndexConfig{Embeddings: true, ExternalProcessing: true, Model: "fixture", Dimensions: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	store.match = func(key string) bool { return strings.HasSuffix(key, "/manifest.json") }
	store.before = func() {
		if err := other.Wake(42, 7, "delivery-during-publication"); err != nil {
			t.Error(err)
		}
	}
	if more, err := f.m.EmbedPending(ctx, "alpha"); err != nil || !more {
		t.Fatal("the paid batch:", more, err)
	}
	if more, err := f.m.EmbedPending(ctx, "alpha"); err != nil || more {
		t.Fatal("embedding again:", more, err)
	}
	s, _ = f.m.Status("alpha")
	if !s.Pending || s.Index.Chunks == 0 || s.Index.Embedded != s.Index.Chunks || paid != s.Index.Chunks {
		t.Fatalf("paid for %d inputs; the state is pending %v with %+v", paid, s.Pending, s.Index)
	}
	freeloader(t, other)
	out, err := other.Search(ctx, SearchQuery{Projects: []string{"alpha"}, Query: "Health", Check: allowSource})
	if coverage := out.Coverage["alpha"]; err != nil || coverage.Mode != "hybrid" || coverage.Embedded != coverage.Chunks {
		t.Fatalf("the other gateway's search: %+v %v", coverage, err)
	}
}
