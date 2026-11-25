package gitexport

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/benchfixture"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
)

// BenchmarkBudgetSweepF2 measures the scheduled block sweep (SweepBlocks) of
// an instance that holds F2, the 2,000-note notebook with 200 images and
// 20,000 revisions: once with every block in use, the daily sweep's usual
// case, and once after the notebook is deleted, when the sweep removes every
// block. It reports each sweep's time and how much more heap the process
// held during it than before it. Run it with -benchtime=1x; FMN_BUDGET_F2=small runs a
// 2,000-revision F2.
func BenchmarkBudgetSweepF2(b *testing.B) {
	o := benchfixture.NotebookOptions{}
	if os.Getenv("FMN_BUDGET_F2") == "small" {
		o = benchfixture.NotebookOptions{Notes: 200, Assets: 20, Revisions: 2000, MinAssetBytes: 4 << 10, MaxAssetBytes: 16 << 10}
	}
	revs, err := benchfixture.Notebook(o)
	if err != nil {
		b.Fatal(err)
	}
	source := filepath.Join(b.TempDir(), "f2.git")
	fixture, err := benchfixture.WriteGit(b.Context(), source, revs, true)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		dir, err := blob.OpenDir(b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		defer dir.Close()
		db := pgtest.Org(b)
		store, err := notestore.Open(b.Context(), db, dir, notestore.Options{ImportEntryID: declaredID})
		if err != nil {
			b.Fatal(err)
		}
		defer store.Close()
		started := time.Now()
		if head, err := ImportGitDir(b.Context(), store.Notebook("f2"), source); err != nil || head.Commit.String() != fixture.Head {
			b.Fatal(head, err)
		}
		b.Logf("imported F2 (%d revisions) in %v", fixture.Revisions, time.Since(started).Round(time.Millisecond))
		blocks := func() int {
			n := 0
			filepath.WalkDir(filepath.Join(dir.Path(), "blocks"), func(_ string, d os.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() {
					n++
				}
				return nil
			})
			return n
		}
		sweep := func(what string) {
			// Every row past the sweep's grace period, as the hours before a
			// daily sweep make them.
			for _, table := range []string{"content_blocks", "content_segments"} {
				if _, err := db.Pool.Exec(b.Context(), `UPDATE `+table+` SET touched = touched - interval '1 hour' WHERE org = $1`, db.Org); err != nil {
					b.Fatal(err)
				}
			}
			before := blocks()
			base, heap := peakHeap()
			start := time.Now()
			result, err := store.SweepBlocks(b.Context(), 0)
			took := time.Since(start)
			peak := heap()
			if err != nil {
				b.Fatal(what, err)
			}
			b.Logf("sweep %s: %v, removed %d segments and %d blocks of %d; heap in use %.1f MiB before, at most %.1f MiB during it", what, took.Round(time.Millisecond), result.Segments, result.Blocks, before, float64(base)/(1<<20), float64(peak)/(1<<20))
			b.ReportMetric(took.Seconds(), what+"-s")
			b.ReportMetric(float64(peak-min(base, peak))/(1<<20), what+"-heap-MiB")
		}
		sweep("in-use")
		if err := store.Delete(b.Context(), "f2"); err != nil {
			b.Fatal(err)
		}
		sweep("deleted")
		if left := blocks(); left != 0 {
			b.Fatalf("%d blocks left after the notebook was deleted and swept", left)
		}
	}
}

// peakHeap returns the heap in use now, after a collection, and samples it
// until the returned function is called, which returns the most it saw.
func peakHeap() (uint64, func() uint64) {
	runtime.GC()
	var now runtime.MemStats
	runtime.ReadMemStats(&now)
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	return now.HeapInuse, func() uint64 {
		close(stop)
		wg.Wait()
		return peak.Load()
	}
}
