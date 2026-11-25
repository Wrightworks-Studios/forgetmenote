package reposource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mapFixture(t *testing.T) *sourceFixture {
	t.Helper()
	f := searchFixture(t)
	dir := f.dirs["alpha"]
	for name, text := range map[string]string{
		"Procfile":            "web: bin/serve-app\n",
		"bin/serve-app":       "#!/bin/sh\nif [ \"${MAKE_GUIDE:-yes}\" = yes ]; then make -C manual html; fi\nexec app --port 8080\n",
		"manual/README.md":    "# Guide\nBuild HTML with make.\n",
		"internal/storage.py": "def open_store():\n    return connect()\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "application entry points")
	f.heads["alpha"].Store(strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD")))
	if _, err := f.m.Refresh(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRepositoryMapClassificationReusesUnchangedInputs(t *testing.T) {
	f := mapFixture(t)
	ctx := context.Background()
	s, _ := f.m.Status("alpha")
	cfg := s.IndexConfig
	cfg.ExternalProcessing, cfg.MapJev = true, true
	if _, err := f.m.ConfigureIndex("alpha", "owner", s.Generation, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	var called []string
	f.m.SetMapClassifier(func() string { return "fixture-v1" }, func(_ context.Context, _ State, inputs []MapInput) ([]MapClassification, error) {
		var labels []MapClassification
		for _, in := range inputs {
			called = append(called, in.Path)
			labels = append(labels, MapClassification{in.Path, MapLabel{InputHash: in.InputHash, Model: "fixture-v1", Topic: "persistence", Probability: .9}})
		}
		return labels, nil
	})
	for {
		more, err := f.m.EnrichMapPending(ctx, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	if len(called) == 0 {
		t.Fatal("enabled incremental map classification never ran")
	}
	before := len(called)
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if more, err := f.m.EnrichMapPending(ctx, "alpha"); err != nil || more || len(called) != before {
		t.Fatal("unchanged map called provider", more, err, called)
	}
	oldManager := f.m
	oldManager.Close()
	reopened, err := Open(oldManager.db, oldManager.root, oldManager.blobs, oldManager.github, oldManager.check)
	if err != nil {
		t.Fatal(err)
	}
	f.m = reopened
	t.Cleanup(f.m.Close)
	f.m.cacheFor = oldManager.cacheFor
	f.m.SetMapClassifier(oldManager.mapModel, oldManager.classify)
	if more, err := f.m.EnrichMapPending(ctx, "alpha"); err != nil || more || len(called) != before {
		t.Fatal("restart lost classification cache", more, err)
	}
	dir := f.dirs["alpha"]
	if err := os.WriteFile(filepath.Join(dir, "internal/storage.py"), []byte("def open_store():\n    return connect(timeout=5)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "change one implementation")
	f.heads["alpha"].Store(strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD")))
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if more, err := f.m.EnrichMapPending(ctx, "alpha"); err != nil || !more {
		t.Fatal(more, err)
	}
	if !reflect.DeepEqual(called[before:], []string{"internal/storage.py"}) {
		t.Fatal("unchanged files were reclassified", called[before:])
	}
	result, err := f.m.Map(ctx, "alpha", MapQuery{Query: "persistence"})
	if err != nil || len(result.Entries) == 0 || result.Revision != f.heads["alpha"].Load() {
		t.Fatal(result, err)
	}
}

func TestRepositoryMapTracksRenamesManifestChangesAndExclusions(t *testing.T) {
	f := mapFixture(t)
	ctx := context.Background()
	initial, err := f.m.Map(ctx, "alpha", MapQuery{Limit: 1})
	if err != nil || len(initial.Entries) != 1 || initial.Next == "" {
		t.Fatal(initial, err)
	}
	seen := []string{initial.Entries[0].Path}
	page := initial
	for page.Next != "" {
		page, err = f.m.Map(ctx, "alpha", MapQuery{Revision: initial.Revision, After: page.Next, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.Entries {
			seen = append(seen, entry.Path)
		}
	}
	if len(seen) != initial.Total {
		t.Fatal("pagination lost entries", seen, initial.Total)
	}
	dir := f.dirs["alpha"]
	gitRun(t, dir, "mv", "bin/serve-app", "bin/launch")
	os.WriteFile(filepath.Join(dir, "Procfile"), []byte("web: bin/launch\n"), 0600)
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "rename entry point")
	f.heads["alpha"].Store(strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD")))
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Map(ctx, "alpha", MapQuery{Revision: initial.Revision}); !errors.Is(err, ErrConflict) {
		t.Fatal("old map silently read HEAD", err)
	}
	result, err := f.m.Map(ctx, "alpha", MapQuery{Query: "runtime"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range result.Entries {
		if entry.Path == "bin/serve-app" {
			t.Fatal("map retained deleted file")
		}
		found = found || entry.Path == "bin/launch" && entry.Role == "startup"
	}
	if !found {
		t.Fatal("manifest change did not route renamed entry", result)
	}
	s, _ := f.m.Status("alpha")
	cfg := s.IndexConfig
	cfg.Exclude = []string{"bin/"}
	if _, err := f.m.ConfigureIndex("alpha", "owner", s.Generation, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Map(ctx, "alpha", MapQuery{}); err == nil {
		t.Fatal("served map under stale path policy")
	}
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	result, err = f.m.Map(ctx, "alpha", MapQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Entries {
		if strings.HasPrefix(entry.Path, "bin/") || strings.Contains(strings.Join(entry.Links, "\n"), "bin/") {
			t.Fatal("excluded path leaked through map", entry)
		}
	}
	if _, err := f.m.Map(ctx, "alpha", MapQuery{PathPrefix: "../"}); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}

func TestRepositoryMapRejectsCancelledClassification(t *testing.T) {
	f := mapFixture(t)
	ctx := context.Background()
	s, _ := f.m.Status("alpha")
	cfg := s.IndexConfig
	cfg.ExternalProcessing, cfg.MapJev = true, true
	if _, err := f.m.ConfigureIndex("alpha", "owner", s.Generation, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	f.m.SetMapClassifier(func() string { return "fixture" }, func(ctx context.Context, _ State, _ []MapInput) ([]MapClassification, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	done := make(chan error, 1)
	go func() { _, err := f.m.EnrichMapPending(ctx, "alpha"); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("classification did not start")
	}
	s, _ = f.m.Status("alpha")
	cfg.MapJev = false
	if _, err := f.m.ConfigureIndex("alpha", "owner", s.Generation, cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted cancelled provider work")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("settings did not cancel classification")
	}
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	result, err := f.m.Map(ctx, "alpha", MapQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Entries {
		if entry.Label != nil {
			t.Fatal("stale classification published")
		}
	}
}

func TestRepositoryMapRoutesStartupToOriginalScript(t *testing.T) {
	f := mapFixture(t)
	result, err := f.m.Search(context.Background(), SearchQuery{Projects: []string{"alpha"}, Query: "runtime startup", Mode: "text", Check: allowSource})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range result.Results {
		if hit.Path == "bin/serve-app" {
			if !strings.Contains(hit.Text, "MAKE_GUIDE") || hit.ContentHash != hash([]byte(hit.Text)) || hit.Revision != f.heads["alpha"].Load() {
				t.Fatal("navigation replaced original evidence", hit)
			}
			return
		}
	}
	t.Fatalf("runtime navigation did not retrieve the manifest's entry script: %+v", result.Results)
}

func TestRepositoryMapUpgradeReusesExistingVectors(t *testing.T) {
	f := mapFixture(t)
	f.enable(t, "alpha")
	inputs := 0
	f.m.SetEmbedder(func(_ context.Context, _ IndexConfig, texts []string, _ bool) ([][]float32, error) {
		inputs += len(texts)
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	})
	f.embedAll(t, "alpha")
	before := inputs
	ctx := context.Background()
	if err := f.m.read(ctx, "alpha", "", func(c *cache, s State) error {
		idx, err := c.loadIndex(ctx, s.Index.Generation)
		if err != nil {
			return err
		}
		// Reproduce the supported on-disk generation from before maps existed.
		idx.MapVersion, idx.MapHash = "", ""
		raw, err := json.Marshal(idx)
		if err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(idx.dir, "map.json")); err != nil {
			return err
		}
		return atomicFile(filepath.Join(idx.dir, "manifest.json"), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	f.embedAll(t, "alpha")
	result, err := f.m.Map(ctx, "alpha", MapQuery{Query: "startup"})
	if err != nil || len(result.Entries) == 0 || before == 0 || inputs != before {
		t.Fatal("map migration replaced compatible vectors", result, err, before, inputs)
	}
}

func TestRepositoryMapRejectsDamagedStorage(t *testing.T) {
	for _, kind := range []string{"corrupt", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := mapFixture(t)
			s, _ := f.m.Status("alpha")
			c := f.m.cacheFor(filepath.Join(f.m.root, "alpha", s.ID))
			idx, err := c.loadIndex(t.Context(), s.Index.Generation)
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(idx.dir, "map.json")
			if kind == "symlink" {
				if err = os.Rename(p, p+".saved"); err == nil {
					err = os.Symlink(p+".saved", p)
				}
			} else {
				err = os.WriteFile(p, []byte("[]"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if _, err := f.m.Map(ctx, "alpha", MapQuery{}); !errors.Is(err, ErrUnavailable) {
				t.Fatal("accepted damaged map", err)
			}
			if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
				t.Fatal(err)
			}
			result, err := f.m.Map(ctx, "alpha", MapQuery{})
			if err != nil || len(result.Entries) == 0 {
				t.Fatal("failed to rebuild derived map", result, err)
			}
		})
	}
}

func TestRepositoryMapProviderFailureKeepsLocalNavigation(t *testing.T) {
	f := mapFixture(t)
	s, _ := f.m.Status("alpha")
	cfg := s.IndexConfig
	cfg.ExternalProcessing, cfg.MapJev = true, true
	if _, err := f.m.ConfigureIndex("alpha", "owner", s.Generation, cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.m.SetMapClassifier(func() string { return "fixture" }, func(context.Context, State, []MapInput) ([]MapClassification, error) {
		calls++
		return nil, errors.New("provider unavailable")
	})
	if _, err := f.m.EnrichMapPending(ctx, "alpha"); err == nil {
		t.Fatal("provider failure was hidden")
	}
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if more, err := f.m.EnrichMapPending(ctx, "alpha"); err != nil || more || calls != 1 {
		t.Fatal("unchanged refresh retried failed purchase", more, err, calls)
	}
	result, err := f.m.Map(ctx, "alpha", MapQuery{Query: "startup"})
	s, _ = f.m.Status("alpha")
	if err != nil || len(result.Entries) == 0 || s.Index.MapError == "" {
		t.Fatal("provider failure broke local map", result, s.Index, err)
	}
}

func TestRepositoryMapPrioritizesExplicitRoleOverIncidentalDocumentation(t *testing.T) {
	f := mapFixture(t)
	dir := f.dirs["alpha"]
	if err := os.MkdirAll(filepath.Join(dir, ".agent-notes"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf(".agent-notes/guide-%02d.md", i)), []byte("# Documentation\nSphinx docs conventions\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "add documentation navigation noise")
	f.heads["alpha"].Store(strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD")))
	ctx := context.Background()
	if _, err := f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	query := MapQuery{Query: "sphinx docs startup entrypoint", Limit: 3}
	first, err := f.m.Map(ctx, "alpha", query)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range first.Entries {
		seen[entry.Path] = true
	}
	if !seen["bin/serve-app"] {
		t.Fatal("incidental documentation hid the requested entry point", first)
	}
	page := first
	for page.Next != "" {
		query.Revision, query.After = first.Revision, page.Next
		page, err = f.m.Map(ctx, "alpha", query)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.Entries {
			if seen[entry.Path] {
				t.Fatal("ranked pagination repeated a path", entry.Path)
			}
			seen[entry.Path] = true
		}
	}
	if len(seen) != first.Total {
		t.Fatal("ranked pagination lost entries", len(seen), first.Total)
	}
}
