package reposource

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type sourceFixture struct {
	m      *Manager
	dirs   map[string]string
	heads  map[string]*atomic.Value
	denied atomic.Bool
}

func searchFixture(t *testing.T) *sourceFixture {
	t.Helper()
	return newSourceFixture(t, t.TempDir(), nil)
}

// newSourceFixture connects and refreshes alpha and beta on a gateway whose
// caches are under root and whose durable files go to blobs.
func newSourceFixture(t *testing.T, root string, blobs blob.Store) *sourceFixture {
	t.Helper()
	f := &sourceFixture{dirs: map[string]string{}, heads: map[string]*atomic.Value{}}
	repos := map[string]Repository{"alpha": {ID: 42, FullName: "example/alpha", DefaultBranch: "main"}, "beta": {ID: 43, FullName: "example/beta", DefaultBranch: "main"}}
	for p := range repos {
		dir, _ := upstream(t)
		os.WriteFile(filepath.Join(dir, "health.go"), []byte("package sample\n// Health checks database readiness.\nfunc Health() bool { return true }\n"), 0600)
		gitRun(t, dir, "add", ".")
		gitRun(t, dir, "commit", "-m", "health")
		f.dirs[p] = dir
		v := &atomic.Value{}
		v.Store(strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD")))
		f.heads[p] = v
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/installations":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "installations": []map[string]int{{"id": 7}}})
		case "/user/installations/7/repositories":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "repositories": []Repository{repos["alpha"], repos["beta"]}})
		case "/app/installations/7/access_tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-token", "expires_at": time.Now().Add(time.Hour)})
		case "/repositories/42":
			json.NewEncoder(w).Encode(repos["alpha"])
		case "/repositories/43":
			json.NewEncoder(w).Encode(repos["beta"])
		default:
			for p := range repos {
				if r.URL.Path == "/repos/example/"+p+"/commits/main" {
					json.NewEncoder(w).Encode(map[string]any{"sha": f.heads[p].Load()})
					return
				}
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := testGitHub(t, key, gh, Config{AppID: "123", Limits: Limits{History: 2, CacheBytes: 32 << 20, EvidenceBytes: 1 << 20, FileBytes: 1 << 20, Files: 100, Seconds: 10}})
	f.m, err = Open(pgtest.Org(t), root, blobs, g, func(project, actor string) error {
		if f.denied.Load() || actor != "owner" {
			return ErrDenied
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.m.Close)
	f.m.cacheFor = func(dir string) *cache {
		return &cache{dir: dir, limits: g.Config.Limits, allowFile: true, remoteURL: func(repo Repository) string { return "file://" + f.dirs[strings.TrimPrefix(repo.FullName, "example/")] }}
	}
	for p, r := range repos {
		if _, err = f.m.Connect(context.Background(), p, "owner", "grant", 7, r.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = f.m.Refresh(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func allowSource(context.Context, string) error { return nil }

// gateway opens another gateway of the fixture's instance, with its caches
// under root and its durable files in blobs.
func (f *sourceFixture) gateway(t *testing.T, root string, blobs blob.Store) *Manager {
	t.Helper()
	db, _ := pgtest.Gateway(t, f.m.db)
	m, err := Open(db, root, blobs, f.m.github, f.m.check)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	m.cacheFor = f.m.cacheFor
	return m
}

func (f *sourceFixture) addFiles(t *testing.T, project string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		file := filepath.Join(f.dirs[project], name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, f.dirs[project], "add", ".")
	gitRun(t, f.dirs[project], "commit", "-m", "search fixtures")
	f.heads[project].Store(strings.TrimSpace(gitRun(t, f.dirs[project], "rev-parse", "HEAD")))
	if _, err := f.m.Refresh(t.Context(), project); err != nil {
		t.Fatal(err)
	}
}

func TestSourceSearchPreservesIdentifiersInQuestionsAndScriptSymbols(t *testing.T) {
	f := searchFixture(t)
	files := map[string]string{
		"models.py":      "class Location:\n" + strings.Repeat("    # Generic location metadata.\n", 100) + "    nearby_match = models.BooleanField(help_text='Whether the nearest observation was used.')\n\n    def compute(self):\n        local_value = 1\n        return local_value\n",
		"bin/prepare-db": "#!/usr/bin/env python3\n" + strings.Repeat("# Database operations\n", 100) + "def prepare_database():\n    return clone_template()\n",
	}
	for i := range 20 {
		files[fmt.Sprintf("tests/test_%02d.py", i)] = "# Nearby location database templates cloning neighboring observation.\nassert location.nearby_match\n"
	}
	f.addFiles(t, "alpha", files)
	f.m.SetEmbedder(func(_ context.Context, _ IndexConfig, texts []string, _ bool) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	})
	f.enable(t, "alpha")
	f.embedAll(t, "alpha")
	for _, tc := range []struct{ query, path, symbol string }{
		{"What does nearby_match mean when using a neighboring location?", "models.py", "nearby_match"},
		{"How does prepare_database clone database templates?", "bin/prepare-db", "prepare_database"},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			out, err := f.m.Search(t.Context(), SearchQuery{Projects: []string{"alpha"}, Query: tc.query, Limit: 1, Check: allowSource})
			if err != nil || len(out.Results) != 1 {
				t.Fatal(out, err)
			}
			r := out.Results[0]
			if r.Path != tc.path || !r.Exact || !strings.Contains(r.Symbol, tc.symbol) || r.Language != "py" || r.Text != files[tc.path][r.StartByte:r.EndByte] {
				t.Fatalf("definition not retained in hybrid result: %+v", r)
			}
		})
	}
}

func TestSourceSearchIncludesCalledHelperEvidence(t *testing.T) {
	f := searchFixture(t)
	f.addFiles(t, "alpha", map[string]string{"probe.py": "def worker_readiness():\n    # Bluewidget readiness is checked here.\n    return inspect_local(hostname())\n\n" + strings.Repeat("# General background\n", 100) + "def inspect_local(node):\n    return ['inspect', 'ping', '--destination', node]\n"})
	out, err := f.m.Search(t.Context(), SearchQuery{Projects: []string{"alpha"}, Query: "bluewidget readiness", Mode: "text", Check: allowSource})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Results {
		if strings.Contains(r.Text, "--destination") && strings.Contains(r.Symbol, "inspect_local") {
			return
		}
	}
	t.Fatalf("caller was found without its referenced helper: %+v", out)
}

func TestSourceSearchDoesNotFanOutAmbiguousHelperNames(t *testing.T) {
	f := searchFixture(t)
	padding := strings.Repeat("# General background\n", 120)
	f.addFiles(t, "alpha", map[string]string{"probe.py": "def ambervector_startup():\n    return shared_handler()\n\n" + padding +
		"def first_case():\n    def shared_handler():\n        return 'UNRELATED_FIRST_HANDLER'\n    return shared_handler()\n\n" + padding +
		"def second_case():\n    def shared_handler():\n        return 'UNRELATED_SECOND_HANDLER'\n    return shared_handler()\n"})
	out, err := f.m.Search(t.Context(), SearchQuery{Projects: []string{"alpha"}, Query: "ambervector_startup", Mode: "text", Check: allowSource})
	if err != nil || len(out.Results) == 0 {
		t.Fatal(out, err)
	}
	for _, r := range out.Results {
		if strings.Contains(r.Text, "UNRELATED_") {
			t.Fatalf("ambiguous name promoted unrelated nested definitions: %+v", r)
		}
	}
}

func TestSourceSearchDoesNotPromotePythonArgumentsOrExamples(t *testing.T) {
	f := searchFixture(t)
	f.addFiles(t, "alpha", map[string]string{"model.py": "class Location:\n    nearby_match = models.BooleanField(\n        help_text='Whether a neighbor was used',\n        default=False,\n    )\n    def compute(self):\n        local_value = 1\n        return local_value\n\n\"\"\"Example:\ndocumented_flag = True\n\"\"\"\n"})
	for _, name := range []string{"help_text", "local_value", "documented_flag"} {
		out, err := f.m.Search(t.Context(), SearchQuery{Projects: []string{"alpha"}, Query: "What does " + name + " do?", Mode: "text", Check: allowSource})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Results) == 0 {
			t.Fatalf("mention of %s disappeared", name)
		}
		for _, r := range out.Results {
			if r.Exact {
				t.Errorf("%s was promoted as a definition: %s", name, r.Symbol)
			}
		}
	}
}
func (f *sourceFixture) enable(t *testing.T, project string) {
	t.Helper()
	s, _ := f.m.Status(project)
	_, err := f.m.ConfigureIndex(project, "owner", s.Generation, IndexConfig{Embeddings: true, ExternalProcessing: true, Model: "fixture", Dimensions: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.m.Refresh(context.Background(), project); err != nil {
		t.Fatal(err)
	}
}
func (f *sourceFixture) embedAll(t *testing.T, project string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		more, err := f.m.EmbedPending(context.Background(), project)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("embedding loop never completed")
}
func TestSourceSearchIncrementalAndSharedQueryEmbedding(t *testing.T) {
	f := searchFixture(t)
	ctx := context.Background()
	documents, queries := 0, 0
	f.m.SetEmbedder(func(ctx context.Context, c IndexConfig, texts []string, background bool) ([][]float32, error) {
		if background {
			documents += len(texts)
		} else {
			queries++
		}
		out := [][]float32{}
		for range texts {
			out = append(out, []float32{1, 0})
		}
		return out, nil
	})
	for _, p := range []string{"alpha", "beta"} {
		f.enable(t, p)
		f.embedAll(t, p)
	}
	before := documents
	for _, p := range []string{"alpha", "beta"} {
		if _, err := f.m.Refresh(ctx, p); err != nil {
			t.Fatal(err)
		}
		f.embedAll(t, p)
	}
	if documents != before {
		t.Fatal("unchanged tree embedded again")
	}
	q := SearchQuery{Projects: []string{"alpha", "beta"}, Query: "Health", Check: allowSource}
	result, err := f.m.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if queries != 1 {
		t.Fatalf("query embedded %d times for compatible repositories", queries)
	}
	found := map[string]bool{}
	for _, r := range result.Results {
		if r.Symbol == "Health" {
			found[r.Project] = true
			if r.ContentHash != hash([]byte(r.Text)) || r.Revision != f.heads[r.Project].Load() {
				t.Fatal("unbound source evidence")
			}
		}
	}
	if len(found) != 2 {
		t.Fatalf("cross repository results missing: %+v", result)
	}
	q.Mode = "text"
	if _, err = f.m.Search(ctx, q); err != nil {
		t.Fatal(err)
	}
	if queries != 1 {
		t.Fatal("text search made a provider call")
	}
	// Changing a different file preserves Health's embedding payload exactly.
	os.WriteFile(filepath.Join(f.dirs["alpha"], "README.md"), []byte("# Sample\nUpdated startup documentation.\n"), 0600)
	gitRun(t, f.dirs["alpha"], "add", ".")
	gitRun(t, f.dirs["alpha"], "commit", "-m", "docs")
	f.heads["alpha"].Store(strings.TrimSpace(gitRun(t, f.dirs["alpha"], "rev-parse", "HEAD")))
	if _, err = f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	f.embedAll(t, "alpha")
	if documents != before+1 {
		t.Fatalf("changed one page sent %d new inputs", documents-before)
	}
	q.Mode = "auto"
	q.Check = func(_ context.Context, p string) error {
		if p == "beta" {
			return ErrDenied
		}
		return nil
	}
	if out, err := f.m.Search(ctx, q); !errors.Is(err, ErrDenied) || len(out.Results) > 0 {
		t.Fatal("denied scope exposed results", err)
	}
	if queries != 1 {
		t.Fatal("authorization checked after provider call")
	}
}
func TestSourceRevocationCannotPublishEmbeddingBatch(t *testing.T) {
	f := searchFixture(t)
	f.enable(t, "alpha")
	f.m.SetEmbedder(func(context.Context, IndexConfig, []string, bool) ([][]float32, error) {
		f.denied.Store(true)
		return [][]float32{{1, 0}, {1, 0}, {1, 0}}, nil
	})
	if _, err := f.m.EmbedPending(context.Background(), "alpha"); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked batch accepted", err)
	}
	s, _ := f.m.Status("alpha")
	if s.Index.Embedded != 0 {
		t.Fatal("revoked vectors were published")
	}
}
func TestSourceIndexCorruptionRebuildAndRename(t *testing.T) {
	f := searchFixture(t)
	ctx := context.Background()
	s, _ := f.m.Status("alpha")
	c := f.m.cacheFor(filepath.Join(f.m.root, "alpha", s.ID))
	idx, err := c.loadIndex(ctx, s.Index.Generation)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(idx.dir, "chunks.jsonl"), []byte("tampered"), 0600)
	out, err := f.m.Search(ctx, SearchQuery{Projects: []string{"alpha"}, Query: "Health", Mode: "text", Check: allowSource})
	if err != nil || len(out.Results) != 0 || out.Coverage["alpha"].Message == "" {
		t.Fatal("corruption exposed stale data", out, err)
	}
	if _, err = f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.dirs["alpha"], "mv", "health.go", "ready.go")
	gitRun(t, f.dirs["alpha"], "commit", "-m", "rename")
	f.heads["alpha"].Store(strings.TrimSpace(gitRun(t, f.dirs["alpha"], "rev-parse", "HEAD")))
	if _, err = f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	out, err = f.m.Search(ctx, SearchQuery{Projects: []string{"alpha"}, Query: "Health", Mode: "text", Check: allowSource})
	if err != nil || len(out.Results) == 0 {
		t.Fatal(err)
	}
	if out.Results[0].Path != "ready.go" {
		t.Fatal("rename returned old path", out.Results[0])
	}
	restarted, err := Open(f.m.db, f.m.root, f.m.blobs, f.m.github, f.m.check)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.cacheFor = f.m.cacheFor
	out, err = restarted.Search(ctx, SearchQuery{Projects: []string{"alpha"}, Query: "Health", Mode: "text", Check: allowSource})
	if err != nil || len(out.Results) == 0 {
		t.Fatal("restart lost derived index", err)
	}
}

func TestSourceEmbeddingFailurePreservesKeywordCoverage(t *testing.T) {
	for _, failure := range []string{"provider", "missing vectors", "wrong dimensions"} {
		t.Run(failure, func(t *testing.T) {
			f := searchFixture(t)
			f.enable(t, "alpha")
			calls := 0
			f.m.SetEmbedder(func(context.Context, IndexConfig, []string, bool) ([][]float32, error) {
				calls++
				switch failure {
				case "missing vectors":
					return nil, nil
				case "wrong dimensions":
					return [][]float32{{1}}, nil
				default:
					return nil, errors.New("fixture outage")
				}
			})
			if _, err := f.m.EmbedPending(context.Background(), "alpha"); err == nil {
				t.Fatal("failure ignored")
			}
			if _, err := f.m.EmbedPending(context.Background(), "alpha"); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("failed embedding automatically retried")
			}
			out, err := f.m.Search(context.Background(), SearchQuery{Projects: []string{"alpha"}, Query: "Health", Mode: "text", Check: allowSource})
			if err != nil || len(out.Results) == 0 || out.Coverage["alpha"].Embedded != 0 {
				t.Fatal("keyword fallback lost", err)
			}
			if _, err = f.m.Control("alpha", "owner", "disconnect"); err != nil {
				t.Fatal(err)
			}
			if err = f.m.ValidateResult(context.Background(), out); !errors.Is(err, ErrDenied) {
				t.Fatal("disconnected evidence delivered", err)
			}
		})
	}
}

func TestSourceSettingsCancelInFlightEmbeddings(t *testing.T) {
	f := searchFixture(t)
	f.enable(t, "alpha")
	entered := make(chan struct{})
	f.m.SetEmbedder(func(ctx context.Context, _ IndexConfig, _ []string, _ bool) ([][]float32, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	done := make(chan error, 1)
	go func() { _, err := f.m.EmbedPending(context.Background(), "alpha"); done <- err }()
	<-entered
	s, _ := f.m.Status("alpha")
	if _, err := f.m.ConfigureIndex("alpha", "owner", s.Generation, IndexConfig{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled job published")
		}
	case <-time.After(time.Second):
		t.Fatal("settings did not cancel provider work")
	}
}

func TestSourceEmbeddingGemmaRebuildsUnprefixedVectorCache(t *testing.T) {
	f := searchFixture(t)
	ctx := context.Background()
	s, _ := f.m.Status("alpha")
	config := IndexConfig{Embeddings: true, ExternalProcessing: true, Model: "embeddinggemma:latest", Dimensions: 2}
	if _, err := f.m.ConfigureIndex("alpha", "owner", s.Generation, config); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	inputs := 0
	f.m.SetEmbedder(func(_ context.Context, _ IndexConfig, texts []string, background bool) ([][]float32, error) {
		if background {
			inputs += len(texts)
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	})
	f.embedAll(t, "alpha")
	before := inputs
	// Restore the pre-prefix cache identities without changing its source bytes.
	if err := f.m.read(ctx, "alpha", "", func(c *cache, s State) error {
		idx, err := c.loadIndex(t.Context(), s.Index.Generation)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(config)
		idx.ConfigID = hash(raw)
		legacy := hash([]byte("embeddinggemma:latest\n2"))
		idx.EmbeddingID = legacy
		raw, _ = json.Marshal(idx)
		if err = atomicFile(filepath.Join(idx.dir, "manifest.json"), raw); err != nil {
			return err
		}
		raw, err = os.ReadFile(filepath.Join(idx.dir, "vectors.json"))
		if err != nil {
			return err
		}
		var manifest vectorManifest
		if err = json.Unmarshal(raw, &manifest); err != nil {
			return err
		}
		manifest.Config = legacy
		raw, _ = json.Marshal(manifest)
		return atomicFile(filepath.Join(idx.dir, "vectors.json"), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	s, _ = f.m.Status("alpha")
	if s.Index.Embedded != 0 {
		t.Fatal("reused unprefixed source vectors")
	}
	f.embedAll(t, "alpha")
	if before == 0 || inputs != 2*before {
		t.Fatalf("source passages were not re-embedded: before=%d after=%d", before, inputs)
	}
	result, err := f.m.Search(ctx, SearchQuery{Projects: []string{"alpha"}, Query: "Health", Check: allowSource})
	if err != nil || len(result.Results) == 0 || result.Coverage["alpha"].Mode != "hybrid" {
		t.Fatal(result, err)
	}
}
