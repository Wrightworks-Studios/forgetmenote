package reposource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceIndexTracksOriginalBytesAndRewrites(t *testing.T) {
	dir, _ := upstream(t)
	source := "package sample\n\n// Health reports database readiness.\nfunc Health() bool { return true }\n"
	if err := os.WriteFile(filepath.Join(dir, "health.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".env"), []byte("PASSWORD=never-index"), 0600)
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "health")
	sha := gitRun(t, dir, "rev-parse", "HEAD")
	c := &cache{dir: t.TempDir(), limits: Limits{Files: 100, FileBytes: 1 << 20, Seconds: 10}}
	if err := os.Rename(filepath.Join(dir, ".git"), filepath.Join(c.dir, "source.git")); err != nil {
		t.Fatal(err)
	}
	s := State{ID: "source", Project: "alpha", Repository: Repository{ID: 42, FullName: "example/source"}, Downloaded: strings.TrimSpace(sha)}
	index, err := c.buildIndex(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Chunks) == 0 || index.Commit != s.Downloaded {
		t.Fatalf("empty or wrong snapshot: %+v", index)
	}
	found := false
	for _, ch := range index.Chunks {
		if ch.Path == ".env" {
			t.Fatal("credential file indexed")
		}
		if ch.Path == "health.go" {
			if ch.Text != source[ch.StartByte:ch.EndByte] || ch.ContentHash != hash([]byte(ch.Text)) {
				t.Fatal("invented source bytes")
			}
			if strings.Contains(ch.Text, "func Health") {
				found = true
				if ch.Symbol != "Health" || ch.StartLine < 1 {
					t.Fatalf("missing symbol: %+v", ch)
				}
			}
		}
	}
	if !found {
		t.Fatal("health declaration missing")
	}
	if len(index.Exclusions) != 1 || index.Exclusions[0].Path != ".env" {
		t.Fatalf("exclusions: %+v", index.Exclusions)
	}
	// The source state names the generation its refresh published.
	s.Index.Generation = filepath.Base(index.dir)
	again, err := c.buildIndex(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if again.dir != index.dir {
		t.Fatal("unchanged commit rebuilt")
	}
	// Move the bare cache back solely to modify the real fixture repository.
	os.Rename(filepath.Join(c.dir, "source.git"), filepath.Join(dir, ".git"))
	gitRun(t, dir, "rm", "health.go")
	gitRun(t, dir, "commit", "-m", "delete health")
	s.Downloaded = strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
	os.Rename(filepath.Join(dir, ".git"), filepath.Join(c.dir, "source.git"))
	index, err = c.buildIndex(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range index.Chunks {
		if ch.Path == "health.go" {
			t.Fatal("deleted file survived")
		}
	}
}

func TestSourceIndexUpgradesChunkerAndReusesUnchangedVectors(t *testing.T) {
	dir, _ := upstream(t)
	os.WriteFile(filepath.Join(dir, "health.go"), []byte("package sample\nfunc Health() bool { return true }\n"), 0600)
	os.WriteFile(filepath.Join(dir, "prepare"), []byte("#!/usr/bin/env python3\ndef prepare_database():\n    return clone_template()\n"), 0600)
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "scripts")
	sha := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
	c := &cache{dir: t.TempDir(), limits: Limits{Files: 100, Seconds: 10}}
	if err := os.Rename(filepath.Join(dir, ".git"), filepath.Join(c.dir, "source.git")); err != nil {
		t.Fatal(err)
	}
	s := State{ID: "source", Project: "alpha", Repository: Repository{ID: 42, FullName: "example/source"}, Downloaded: sha, IndexConfig: IndexConfig{Model: "fixture", Dimensions: 2}}
	idx, err := c.buildIndex(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	s.Index.Generation = filepath.Base(idx.dir)
	legacy := idx
	legacy.Chunker, legacy.ChunkHash = "source-v1", ""
	legacy.Chunks = append([]SourceChunk(nil), idx.Chunks...)
	oldVectors := map[string][]float32{}
	healthHash := ""
	for i := range legacy.Chunks {
		ch := &legacy.Chunks[i]
		if ch.Path == "prepare" {
			ch.Language, ch.Symbol = "", ""
		}
		ch.EmbeddingHash = hash([]byte(s.IndexConfig.embeddingID() + "\nsource-v1\n" + payloadVersion + "\n" + ch.payload()))
		oldVectors[ch.EmbeddingHash] = []float32{1, 0}
		if ch.Path == "health.go" {
			healthHash = ch.EmbeddingHash
		}
	}
	legacy.Map = nil
	buildMap(&legacy, sourceIndex{})
	if err := c.publishIndex(t.Context(), &legacy, idx, oldVectors, s.IndexConfig); err != nil {
		t.Fatal(err)
	}
	s.Index.Generation = filepath.Base(legacy.dir)
	upgraded, err := c.buildIndex(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ch := range upgraded.Chunks {
		if ch.Path == "prepare" && ch.Language == "py" && strings.Contains(ch.Symbol, "prepare_database") {
			found = true
		}
	}
	if !found {
		t.Fatal("unchanged upstream snapshot kept the old script index")
	}
	vm, file, err := vectors(upgraded, s.IndexConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, ok := vm.Rows[healthHash]; !ok {
		t.Fatal("chunker upgrade discarded unchanged embedding payload")
	}
}

func TestSourceRangeStreamsLargeUTF8Line(t *testing.T) {
	dir, _ := upstream(t)
	body := strings.Repeat("é", 600000) + "\nsecond line\n"
	os.WriteFile(filepath.Join(dir, "large.txt"), []byte(body), 0600)
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "large")
	sha := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
	c := &cache{dir: t.TempDir(), limits: Limits{Files: 100, FileBytes: 1 << 20, Seconds: 10}}
	os.Rename(filepath.Join(dir, ".git"), filepath.Join(c.dir, "source.git"))
	s := State{ID: "source", Repository: Repository{ID: 42, FullName: "example/source"}}
	r := LineRange{StartLine: 1, EndLine: 2}
	var got strings.Builder
	for calls := 0; calls < 30; calls++ {
		f, err := c.fileRange(context.Background(), s, sha, "large.txt", r)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.Content) > 65536 || f.Content != body[f.StartByte:f.EndByte] {
			t.Fatal("range lost original bytes")
		}
		if f.More != (f.EndByte < len(body)) {
			t.Fatal("range continuation state disagrees with remaining original bytes")
		}
		got.WriteString(f.Content)
		if f.Cursor == "" {
			break
		}
		r.Cursor = f.Cursor
	}
	if got.String() != body {
		t.Fatalf("range continuation lost bytes: %d/%d", got.Len(), len(body))
	}
	r.StartLine = 2
	if _, err := c.fileRange(context.Background(), s, sha, "large.txt", r); err == nil {
		t.Fatal("cursor widened its bound range")
	}
}
