package notestore_test

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

// The manuscript benchmarks read fixture F1, f1-moby-dick.md, from the
// directory FMN_FIXTURES names, and skip without it.
func fixtureF1(b *testing.B) []byte {
	dir := os.Getenv("FMN_FIXTURES")
	if dir == "" {
		b.Skip("FMN_FIXTURES is not set")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "f1-moby-dick.md"))
	if err != nil {
		b.Skip("fixture F1:", err)
	}
	return raw
}

type benchEnv struct {
	ctx   context.Context
	db    *pg.DB
	blobs *blob.Dir
	store *notestore.Store
}

func newBenchEnv(b *testing.B) *benchEnv {
	db := pgtest.Org(b)
	dir, err := blob.OpenDir(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { dir.Close() })
	e := &benchEnv{ctx: context.Background(), db: db, blobs: dir}
	e.store = e.open(b)
	return e
}

func (e *benchEnv) open(b *testing.B) *notestore.Store {
	s, err := notestore.Open(e.ctx, e.db, e.blobs, notestore.Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(s.Close)
	return s
}

func percentiles(b *testing.B, unit string, samples []float64) {
	if len(samples) == 0 {
		return
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	at := func(p float64) float64 { return s[min(len(s)-1, int(p*float64(len(s))))] }
	b.ReportMetric(at(0.50), "p50-"+unit)
	b.ReportMetric(at(0.95), "p95-"+unit)
	b.ReportMetric(s[len(s)-1], "max-"+unit)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// logicalBytes sums the stored size of the org's rows in the store's
// tables, and the blob directory's bytes.
func (e *benchEnv) logicalBytes(b *testing.B) (rows, blobs int64) {
	for _, table := range []string{"revisions", "revision_changes", "git_trees", "file_versions", "content_segments", "content_blocks", "entries"} {
		var n int64
		if err := e.db.Pool.QueryRow(e.ctx, `SELECT coalesce(sum(pg_column_size(t.*)), 0) FROM `+table+` t WHERE org = $1`, e.db.Org).Scan(&n); err != nil {
			b.Fatal(err)
		}
		rows += n
	}
	filepath.WalkDir(e.blobs.Path(), func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				blobs += info.Size()
			}
		}
		return nil
	})
	return rows, blobs
}

// physicalBytes is the database's size on disk, indexes and dead rows
// included.
func (e *benchEnv) physicalBytes(b *testing.B) int64 {
	var n int64
	if err := e.db.Pool.QueryRow(e.ctx, `SELECT sum(pg_total_relation_size(c)) FROM unnest(ARRAY['revisions','revision_changes','git_trees','file_versions','content_segments','content_blocks','entries','notebook_heads']::regclass[]) c`).Scan(&n); err != nil {
		b.Fatal(err)
	}
	return n
}

// Budget: a checkpoint that changes one paragraph of a 1.2 MB manuscript,
// p95 < 30 ms on the server, storage growth < 16 KiB.
func BenchmarkApplyParagraphEditOfManuscript(b *testing.B) {
	doc := fixtureF1(b)
	e := newBenchEnv(b)
	if _, err := e.store.Create(e.ctx, "book", ada); err != nil {
		b.Fatal(err)
	}
	n := e.store.Notebook("book")
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "Add the manuscript", Author: ada, Files: []notestore.FileChange{{Path: "Manuscripts/moby-dick.md", Content: doc}}}); err != nil {
		b.Fatal(err)
	}
	paragraphs := bytes.SplitAfter(doc, []byte("\n\n"))
	var latency, grown, grownBlob []float64
	rows, blobs := e.logicalBytes(b)
	physical := e.physicalBytes(b)
	edits := 0
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		k := 10 + i*37%(len(paragraphs)-20)
		paragraphs[k] = append(bytes.TrimRight(paragraphs[k], "\n"), []byte(fmt.Sprintf(" An edit, number %d.\n\n", i))...)
		content := bytes.Join(paragraphs, nil)
		b.StartTimer()
		start := time.Now()
		if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "Checkpoint", Author: ada, Files: []notestore.FileChange{{Path: "Manuscripts/moby-dick.md", Content: content}},
			Trailers: []gitobj.Trailer{{Key: "Gitnotes-Source", Value: "browser"}, {Key: "Collaboration-Receipt", Value: "r"}}}); err != nil {
			b.Fatal(err)
		}
		latency = append(latency, ms(time.Since(start)))
		b.StopTimer()
		r, bl := e.logicalBytes(b)
		grown = append(grown, float64(r-rows+bl-blobs))
		grownBlob = append(grownBlob, float64(bl-blobs))
		rows, blobs = r, bl
		edits++
		b.StartTimer()
	}
	b.StopTimer()
	percentiles(b, "ms", latency)
	percentiles(b, "bytes", grown)
	percentiles(b, "blob-bytes", grownBlob)
	b.ReportMetric(float64(e.physicalBytes(b)-physical)/float64(edits), "pg-disk-bytes/edit")
}

// Budget: open a 1.2 MB manuscript on the server, warm p95 < 50 ms and
// cold p95 < 300 ms. Cold is a store with empty caches; the operating
// system's file cache stays warm.
func BenchmarkReadManuscript(b *testing.B) {
	doc := fixtureF1(b)
	e := newBenchEnv(b)
	e.store.Create(e.ctx, "book", ada)
	if _, err := e.store.Notebook("book").Apply(e.ctx, notestore.ChangeSet{Subject: "Add", Author: ada, Files: []notestore.FileChange{{Path: "moby-dick.md", Content: doc}}}); err != nil {
		b.Fatal(err)
	}
	b.Run("warm", func(b *testing.B) {
		n := e.store.Notebook("book")
		var latency []float64
		for b.Loop() {
			start := time.Now()
			content, _, err := n.Read(e.ctx, gitobj.ID{}, "moby-dick.md")
			if err != nil || len(content) != len(doc) {
				b.Fatal(err)
			}
			latency = append(latency, ms(time.Since(start)))
		}
		percentiles(b, "ms", latency)
	})
	b.Run("cold", func(b *testing.B) {
		var latency []float64
		for b.Loop() {
			b.StopTimer()
			s, err := notestore.Open(e.ctx, e.db, e.blobs, notestore.Options{})
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			start := time.Now()
			content, _, err := s.Notebook("book").Read(e.ctx, gitobj.ID{}, "moby-dick.md")
			if err != nil || len(content) != len(doc) {
				b.Fatal(err)
			}
			latency = append(latency, ms(time.Since(start)))
			b.StopTimer()
			s.Close()
			b.StartTimer()
		}
		percentiles(b, "ms", latency)
	})
}

// Budget: per-note history over 5,000 revisions, p95 < 50 ms.
func BenchmarkHistoryOf5000Revisions(b *testing.B) {
	e := newBenchEnv(b)
	e.store.Create(e.ctx, "notes", ada)
	n := e.store.Notebook("notes")
	for i := range 5000 {
		files := []notestore.FileChange{{Path: "journal/daily.md", Content: []byte(fmt.Sprintf("# Daily\n\nEntry %d\n", i))}}
		if i%3 == 0 {
			files = append(files, notestore.FileChange{Path: fmt.Sprintf("other/%d.md", i%50), Content: []byte(fmt.Sprint(i))})
		}
		if i == 2500 {
			files = []notestore.FileChange{{Path: "journal/renamed.md", From: "journal/daily.md"}}
		}
		path := "journal/daily.md"
		if i > 2500 {
			path = "journal/renamed.md"
			files[0].Path = path
		}
		if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "Write", Author: ada, Files: files}); err != nil {
			b.Fatal(err)
		}
	}
	for _, limit := range []int{50, 0} {
		b.Run(fmt.Sprintf("limit=%d", limit), func(b *testing.B) {
			var latency []float64
			for b.Loop() {
				start := time.Now()
				h, err := n.History(e.ctx, notestore.HistoryQuery{Path: "journal/renamed.md", Limit: limit})
				if err != nil || limit > 0 && len(h) != limit || limit == 0 && len(h) != 5000 {
					b.Fatal(len(h), err)
				}
				latency = append(latency, ms(time.Since(start)))
			}
			percentiles(b, "ms", latency)
		})
	}
}

// Budget: notebook status and listing with 2,000 notes, p95 < 10 ms warm.
func BenchmarkListingOf2000Files(b *testing.B) {
	e := newBenchEnv(b)
	e.store.Create(e.ctx, "notes", ada)
	n := e.store.Notebook("notes")
	var files []notestore.FileChange
	for i := range 2000 {
		files = append(files, notestore.FileChange{Path: fmt.Sprintf("Area %d/Topic %d/note %d.md", i%10, i%40, i), Content: []byte(strings.Repeat("text ", 20+i%50))})
	}
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "Add notes", Author: ada, Files: files}); err != nil {
		b.Fatal(err)
	}
	b.Run("files-warm", func(b *testing.B) {
		var latency []float64
		for b.Loop() {
			start := time.Now()
			if f, err := n.Files(e.ctx, gitobj.ID{}); err != nil || len(f) != 2000 {
				b.Fatal(err)
			}
			latency = append(latency, ms(time.Since(start)))
		}
		percentiles(b, "ms", latency)
	})
	b.Run("head-warm", func(b *testing.B) {
		var latency []float64
		for b.Loop() {
			start := time.Now()
			if _, err := n.Head(e.ctx); err != nil {
				b.Fatal(err)
			}
			latency = append(latency, ms(time.Since(start)))
		}
		percentiles(b, "ms", latency)
	})
	b.Run("files-cold", func(b *testing.B) {
		var latency []float64
		for b.Loop() {
			b.StopTimer()
			s, err := notestore.Open(e.ctx, e.db, e.blobs, notestore.Options{})
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			start := time.Now()
			if f, err := s.Notebook("notes").Files(e.ctx, gitobj.ID{}); err != nil || len(f) != 2000 {
				b.Fatal(err)
			}
			latency = append(latency, ms(time.Since(start)))
			b.StopTimer()
			s.Close()
			b.StartTimer()
		}
		percentiles(b, "ms", latency)
	})
	b.Run("small-edit", func(b *testing.B) {
		var latency []float64
		for i := 0; b.Loop(); i++ {
			start := time.Now()
			if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "Edit", Author: ada, Files: []notestore.FileChange{{Path: "Area 3/Topic 3/note 3.md", Content: []byte(fmt.Sprint("edit ", i))}}}); err != nil {
				b.Fatal(err)
			}
			latency = append(latency, ms(time.Since(start)))
		}
		percentiles(b, "ms", latency)
	})
}
