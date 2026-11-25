package gitexport

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"forgetmenote/internal/benchfixture"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// BenchmarkBudgetExportF2 measures the storage v2 import and export budgets
// (design §13, bake-off §4) on F2, the 2,000-note notebook with 200 images
// and 20,000 revisions. Each iteration starts from an empty store: it
// imports F2's Git history, verifies it, exports the whole history, packs
// and checks the export, exports single new revisions, purges a note from
// the 41st revision on and rebuilds the export, and imports a bundle of the
// export into a second notebook, whose export must have the same head. Run it with -benchtime=1x; FMN_BUDGET_F2=small runs a
// 2,000-revision F2. docs/design/storage-v2-results-store.md has the
// results.
func BenchmarkBudgetExportF2(b *testing.B) {
	o := benchfixture.NotebookOptions{}
	if os.Getenv("FMN_BUDGET_F2") == "small" {
		o = benchfixture.NotebookOptions{Notes: 200, Assets: 20, Revisions: 2000, MinAssetBytes: 4 << 10, MaxAssetBytes: 16 << 10}
	}
	revs, err := benchfixture.Notebook(o)
	if err != nil {
		b.Fatal(err)
	}
	source := filepath.Join(b.TempDir(), "f2.git")
	started := time.Now()
	fixture, err := benchfixture.WriteGit(b.Context(), source, revs, true)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("materialized F2: %d revisions in %v", fixture.Revisions, time.Since(started).Round(time.Millisecond))
	seconds := map[string][]float64{}
	var single []float64
	timed := func(name string, fn func()) {
		start := time.Now()
		fn()
		seconds[name] = append(seconds[name], time.Since(start).Seconds())
	}
	for b.Loop() {
		e := newEnv(b)
		n := e.store.Notebook("f2")
		var head notestore.Head
		timed("import-s", func() {
			if head, err = ImportGitDir(e.ctx, n, source); err != nil || head.Commit.String() != fixture.Head {
				b.Fatal(head, err)
			}
		})
		timed("verify-s", func() {
			if rep, err := Verify(e.ctx, n, source); err != nil || rep.Commits != fixture.Revisions {
				b.Fatal(rep, err)
			}
		})
		timed("export-s", func() {
			if exported, err := e.ex.Sync(e.ctx, "f2"); err != nil || exported != head {
				b.Fatal(exported, err)
			}
		})
		timed("maintain-s", func() {
			if err := e.ex.Maintain(e.ctx, "f2"); err != nil {
				b.Fatal(err)
			}
		})
		if got := git(b, e.ex.Path("f2"), "rev-parse", "main"); got != fixture.Head {
			b.Fatal("the export's head is", got)
		}
		// One new revision at a time, as the exporter follows each commit.
		files, err := n.Files(e.ctx, gitobj.ID{})
		if err != nil {
			b.Fatal(err)
		}
		i := slices.IndexFunc(files, func(f notestore.File) bool { return filepath.Ext(f.Path) == ".md" })
		content, _, err := n.Read(e.ctx, gitobj.ID{}, files[i].Path)
		if err != nil {
			b.Fatal(err)
		}
		for k := range 40 {
			content = fmt.Appendf(content, "One more line, number %d.\n", k)
			next := apply(b, n, notestore.ChangeSet{Files: []notestore.FileChange{{Path: files[i].Path, Content: content}}}).Head
			start := time.Now()
			if got, err := e.ex.Sync(e.ctx, "f2"); err != nil || got != next {
				b.Fatal(got, err)
			}
			single = append(single, float64(time.Since(start))/float64(time.Millisecond))
		}
		// A purge that redacts a note created early, from its first
		// revision on, rewrites nearly all of the history, the purge
		// budget's case, and the export is rebuilt.
		purge := fmt.Sprintf("budget-%d", len(seconds["import-s"]))
		var target string
		var from int64
		earlier, err := n.Revisions(e.ctx, 40, 50)
		if err != nil {
			b.Fatal(err)
		}
		for _, r := range earlier {
			for _, c := range r.Changes {
				if target == "" && filepath.Ext(c.Path) == ".md" && c.Op == "add" {
					target, from = c.EntryID, r.Seq
				}
			}
		}
		if target == "" {
			b.Fatal("no note is created in revisions 41 to 90")
		}
		redact := func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
			if f.EntryID != target {
				return nil, true, nil
			}
			data, err := content()
			return bytes.ReplaceAll(data, []byte(" the "), []byte(" [purged] ")), true, err
		}
		timed("purge-rewrite-s", func() {
			if _, err := n.Rewrite(e.ctx, from, purge, redact, notestore.RewriteOptions{Actor: "owner", Reason: "budget"}); err != nil {
				b.Fatal(err)
			}
		})
		timed("purge-rebuild-s", func() {
			if err := e.ex.Rebuild(e.ctx, "f2", purge); err != nil {
				b.Fatal(err)
			}
		})
		// The round trip: a bundle of the export, imported into another
		// notebook, exports the same history.
		bundle := filepath.Join(b.TempDir(), "f2.bundle")
		timed("bundle-s", func() {
			f, err := os.Create(bundle)
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()
			if err := e.ex.Bundle(e.ctx, "f2", f, 4<<30); err != nil {
				b.Fatal(err)
			}
		})
		final, err := n.Head(e.ctx)
		if err != nil {
			b.Fatal(err)
		}
		copied := e.store.Notebook("f2-copy")
		timed("bundle-import-s", func() {
			f, err := os.Open(bundle)
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()
			l := testLimits(b)
			l.BundleBytes, l.ObjectBytes, l.Objects = 4<<30, 16<<30, 10_000_000
			if got, err := ImportBundle(e.ctx, copied, f, l); err != nil || got.Commit != final.Commit {
				b.Fatal(got, err)
			}
		})
		if exported, err := e.ex.Sync(e.ctx, "f2-copy"); err != nil || exported.Commit != final.Commit {
			b.Fatal("the bundle's import exports", exported, err, "not", final)
		}
		b.StopTimer()
		if err := e.ex.Maintain(e.ctx, "f2-copy"); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	for name, s := range seconds {
		b.ReportMetric(slices.Max(s), name)
	}
	slices.Sort(single)
	b.ReportMetric(single[len(single)/2], "one-p50-ms")
	b.ReportMetric(single[int(0.95*float64(len(single)))], "one-p95-ms")
}
