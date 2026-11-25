package workspace

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/benchfixture"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"

	"github.com/reearth/ygo/crdt"
)

// The storage v2 budgets (design §13, bake-off §4) measured through the
// workspace, the path a request takes to the notes store, on the bake-off
// fixtures: F1, the 1.2 MB manuscript, after its 5,000-edit F1h history,
// and F2, the 2,000-note notebook with 20,000 revisions. Each latency
// sub-benchmark reports the p50, p95 and maximum of its iterations, so run
// them with a fixed iteration count, as
// docs/design/storage-v2-results-store.md does.

// latencies collects one sample per timed iteration.
type latencies []time.Duration

func (l *latencies) time(fn func()) {
	start := time.Now()
	fn()
	*l = append(*l, time.Since(start))
}

func (l latencies) report(b *testing.B) {
	if len(l) == 0 {
		return
	}
	s := slices.Clone(l)
	slices.Sort(s)
	at := func(p float64) float64 {
		return float64(s[min(len(s)-1, int(p*float64(len(s))))]) / float64(time.Millisecond)
	}
	b.ReportMetric(at(0.50), "p50-ms")
	b.ReportMetric(at(0.95), "p95-ms")
	b.ReportMetric(float64(s[len(s)-1])/float64(time.Millisecond), "max-ms")
}

// replayFixture commits revs to s's notebook, each with its author, time,
// message, renames and the entry IDs the workspace gives new notes, and
// returns how many it committed and the path the last one wrote. It skips
// the init commit, which the notebook already has.
func replayFixture(tb testing.TB, s *Store, revs iter.Seq[benchfixture.Revision]) (count int, last string) {
	tb.Helper()
	ctx := context.Background()
	exists := map[string]bool{}
	for rev := range revs {
		if rev.Kind == benchfixture.KindInit {
			continue
		}
		cs := notestore.ChangeSet{Author: gitobj.Signature{Name: rev.AuthorName, Email: rev.AuthorEmail, When: rev.Time.Unix(), TZ: rev.Time.Format("-0700")},
			Subject: rev.Subject, AllowEmpty: true}
		for _, t := range rev.Trailers {
			cs.Trailers = append(cs.Trailers, gitobj.Trailer{Key: t.Key, Value: t.Value})
		}
		for _, c := range rev.Changes {
			fc := notestore.FileChange{Path: c.Path, Content: c.Content, From: c.OldPath}
			if c.Content != nil && c.OldPath == "" && !exists[c.Path] {
				fc.EntryID = NoteEntryID(c.Path, c.Content)
			}
			if c.OldPath != "" {
				delete(exists, c.OldPath)
			}
			exists[c.Path] = c.Content != nil
			cs.Files = append(cs.Files, fc)
			last = c.Path
		}
		if _, err := s.notes.Apply(ctx, cs); err != nil {
			tb.Fatalf("revision %d (%s): %v", rev.Seq, rev.Kind, err)
		}
		count++
	}
	// Production's autovacuum analyzes tables after a large import; until
	// it does, Postgres plans path queries without statistics.
	if _, err := testEnv(tb, s).db.Pool.Exec(ctx, "ANALYZE"); err != nil {
		tb.Fatal(err)
	}
	return count, last
}

// storeSize is the size of the rows s's org keeps for revisions (with
// the blob directory) and for live documents.
func storeSize(tb testing.TB, s *Store) (revisions, live int64) {
	tb.Helper()
	env := testEnv(tb, s)
	sum := func(tables ...string) (total int64) {
		for _, table := range tables {
			var n int64
			if err := env.db.Pool.QueryRow(context.Background(), `SELECT coalesce(sum(pg_column_size(t.*)), 0) FROM `+table+` t WHERE org = $1`, env.db.Org).Scan(&n); err != nil {
				tb.Fatal(err)
			}
			total += n
		}
		return total
	}
	revisions = sum("revisions", "revision_changes", "git_trees", "file_versions", "content_segments", "content_blocks", "entries", "notebook_heads")
	filepath.WalkDir(env.blobs.Path(), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				revisions += info.Size()
			}
		}
		return nil
	})
	return revisions, sum("live_docs", "live_updates")
}

// editParagraph appends a sentence to the paragraph about a third of the
// way through content after skipping i paragraphs, so successive edits
// touch different paragraphs as a writer's checkpoints do.
func editParagraph(content []byte, i int) []byte {
	paragraphs := bytes.SplitAfter(content, []byte("\n\n"))
	k := len(paragraphs)/3 + i*37%(len(paragraphs)/3)
	for !bytes.HasSuffix(paragraphs[k], []byte(".\n\n")) {
		k++
	}
	edited := slices.Clone(paragraphs)
	edited[k] = append(bytes.TrimSuffix(slices.Clip(paragraphs[k]), []byte("\n\n")), fmt.Sprintf(" A revision, number %d.\n\n", i)...)
	return bytes.Join(edited, nil)
}

// BenchmarkBudgetManuscript measures the F1 budgets after F1h: opening the
// manuscript (warm and in a restarted process), its history through 250
// renames, an explicit save of one changed paragraph, and live editing:
// the first live read, a browser's typing update, and the checkpoint of a
// changed paragraph. FMN_FIXTURE_CACHE must hold F1 (see benchfixture).
func BenchmarkBudgetManuscript(b *testing.B) {
	novel := benchfixture.NovelOrSkip(b)
	revs, err := benchfixture.NovelHistory(novel, benchfixture.NovelHistoryOptions{})
	if err != nil {
		b.Fatal(err)
	}
	s := newTestStore(b)
	ctx := attribution.With(context.Background(), attribution.Identity{ID: "ada", Name: "Ada Lovelace", Email: "ada@example.test", Source: "browser"})
	started := time.Now()
	count, path := replayFixture(b, s, revs)
	head, err := s.notes.Head(ctx)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("replayed F1h: %d revisions in %v; the manuscript is %s", count, time.Since(started).Round(time.Millisecond), path)

	b.Run("open/warm", func(b *testing.B) {
		var l latencies
		for b.Loop() {
			l.time(func() {
				if doc, err := s.Read(ctx, path, ""); err != nil || len(doc.Content) < len(novel)/2 {
					b.Fatal(err)
				}
			})
		}
		l.report(b)
	})
	b.Run("open/cold", func(b *testing.B) {
		var l latencies
		for b.Loop() {
			b.StopTimer()
			s = reopenTestStore(b, s)
			b.StartTimer()
			l.time(func() {
				if _, err := s.Read(ctx, path, ""); err != nil {
					b.Fatal(err)
				}
			})
		}
		l.report(b)
	})
	// The manuscript's history: a page as the browser and MCP list it, and
	// every revision from the notes store.
	b.Run("history/page", func(b *testing.B) {
		var l latencies
		for b.Loop() {
			l.time(func() {
				if h, err := s.History(ctx, path); err != nil || len(h) != historyLimit {
					b.Fatal(len(h), err)
				}
			})
		}
		l.report(b)
	})
	// A few edits leave the text as it was (a word replaced by itself), so
	// their revisions are not the manuscript's.
	all, err := s.notes.History(ctx, notestore.HistoryQuery{Path: path})
	if err != nil || len(all) < count*99/100 {
		b.Fatal(len(all), err)
	}
	b.Logf("the manuscript has %d revisions", len(all))
	b.Run("history/all", func(b *testing.B) {
		var l latencies
		for b.Loop() {
			l.time(func() {
				if h, err := s.notes.History(ctx, notestore.HistoryQuery{Path: path}); err != nil || len(h) != len(all) {
					b.Fatal(len(h), err)
				}
			})
		}
		l.report(b)
	})

	doc, err := s.Read(ctx, path, "")
	if err != nil {
		b.Fatal(err)
	}
	b.Run("save/paragraph", func(b *testing.B) {
		var l latencies
		var grown []float64
		for i := 0; b.Loop(); i++ {
			b.StopTimer()
			content := editParagraph(doc.Content, i)
			before, _ := storeSize(b, s)
			b.StartTimer()
			l.time(func() {
				if doc, err = s.Save(ctx, path, content, doc.Revision, ""); err != nil {
					b.Fatal(err)
				}
			})
			b.StopTimer()
			after, _ := storeSize(b, s)
			grown = append(grown, float64(after-before))
			b.StartTimer()
		}
		l.report(b)
		percentilesOf(b, "bytes", grown)
	})

	// The first live read of a note without a live document builds it from
	// the note; the first in a restarted process loads its stored state.
	entry := liveEntryOfBench(b, s, path)
	b.Run("live/start", func(b *testing.B) {
		var l latencies
		for b.Loop() {
			b.StopTimer()
			if _, err := s.notes.LiveReset(ctx, entry); err != nil {
				b.Fatal(err)
			}
			s = reopenTestStore(b, s)
			b.StartTimer()
			l.time(func() {
				if v, err := s.LiveRead(ctx, path); err != nil || v.Pending {
					b.Fatal(err)
				}
			})
		}
		l.report(b)
	})
	b.Run("live/load", func(b *testing.B) {
		var l latencies
		for b.Loop() {
			b.StopTimer()
			s = reopenTestStore(b, s)
			b.StartTimer()
			l.time(func() {
				if _, err := s.LiveRead(ctx, path); err != nil {
					b.Fatal(err)
				}
			})
		}
		l.report(b)
	})

	// A browser types into the middle of the manuscript and sends what the
	// server lacks after each keystroke, as frontend/collaboration.mjs does.
	live, err := s.LiveRead(ctx, path)
	if err != nil {
		b.Fatal(err)
	}
	browser, err := browserDoc(live.Update)
	if err != nil {
		b.Fatal(err)
	}
	generation := strings.Split(live.LiveRevision, ":")[0]
	text := live.Content
	middle := len(text) / 2
	middle = utf16Index(text, middle+strings.Index(text[middle:], "\n\n"))
	keystrokes := 0
	send := func(b *testing.B, edit func(tx *crdt.Transaction, text *crdt.YText)) LiveDocument {
		before := browser.StateVector()
		browser.Transact(func(tx *crdt.Transaction) { edit(tx, tx.GetText("markdown")) })
		keystrokes++
		v, err := s.LiveApply(ctx, LiveUpdate{Path: path, Generation: generation, OperationID: fmt.Sprintf("budget-%d", keystrokes), Update: crdt.EncodeStateAsUpdateV1(browser, before)})
		if err != nil {
			b.Fatal(err)
		}
		text = v.Content
		return v
	}
	b.Run("live/keystroke", func(b *testing.B) {
		var l latencies
		for i := 0; b.Loop(); i++ {
			l.time(func() { send(b, func(tx *crdt.Transaction, text *crdt.YText) { text.Insert(tx, middle+i, "w", nil) }) })
		}
		l.report(b)
	})
	b.Run("live/checkpoint", func(b *testing.B) {
		var l latencies
		var grown, grownLive []float64
		for i := 0; b.Loop(); i++ {
			b.StopTimer()
			// End one more sentence of a paragraph in the middle third.
			from := len(text)/3 + i*997%(len(text)/3)
			at := utf16Index(text, from+strings.Index(text[from:], ".\n\n")+1)
			if v := send(b, func(tx *crdt.Transaction, t *crdt.YText) {
				t.Insert(tx, at, fmt.Sprintf(" A live revision, number %d.", i), nil)
			}); !v.Pending {
				b.Fatal("the live edit is not pending")
			}
			revisions, live := storeSize(b, s)
			b.StartTimer()
			var v LiveDocument
			l.time(func() {
				if v, err = s.LiveCheckpoint(ctx, path); err != nil || v.Pending || v.Content != text {
					b.Fatal(err)
				}
			})
			b.StopTimer()
			r, lv := storeSize(b, s)
			grown = append(grown, float64(r-revisions))
			grownLive = append(grownLive, float64(lv-live))
			if g := strings.Split(v.LiveRevision, ":")[0]; g != generation {
				// The checkpoint started a new generation: the browser
				// reloads, as it does on a generation change.
				if browser, err = browserDoc(v.Update); err != nil {
					b.Fatal(err)
				}
				generation = g
			}
			b.StartTimer()
		}
		l.report(b)
		percentilesOf(b, "bytes", grown)
		percentilesOf(b, "live-bytes", grownLive)
	})
	if final, err := s.notes.Head(ctx); err == nil {
		b.Logf("benchmarks added %d revisions to %d", final.Seq-head.Seq, head.Seq)
	}
}

func percentilesOf(b *testing.B, unit string, samples []float64) {
	if len(samples) == 0 {
		return
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	b.ReportMetric(s[min(len(s)-1, len(s)/2)], "p50-"+unit)
	b.ReportMetric(s[min(len(s)-1, int(0.95*float64(len(s))))], "p95-"+unit)
}

func liveEntryOfBench(tb testing.TB, s *Store, path string) string {
	tb.Helper()
	f, err := s.notes.Stat(context.Background(), gitobj.ID{}, path)
	if err != nil {
		tb.Fatal(err)
	}
	return f.EntryID
}

// BenchmarkBudgetNotebook measures the F2 budgets after replaying its
// 20,000 revisions: status, listing, browsing and finding (Find, and
// FindScoped as agents and the notes API search) warm and in a restarted
// process, reading and saving a note, note and notebook history,
// the changes of the last 1,000 revisions, and moving a folder of notes.
// FMN_BUDGET_F2=small replays a 2,000-revision F2 instead.
func BenchmarkBudgetNotebook(b *testing.B) {
	o := benchfixture.NotebookOptions{}
	if os.Getenv("FMN_BUDGET_F2") == "small" {
		o = benchfixture.NotebookOptions{Notes: 200, Assets: 20, Revisions: 2000, MinAssetBytes: 4 << 10, MaxAssetBytes: 16 << 10}
	}
	revs, err := benchfixture.Notebook(o)
	if err != nil {
		b.Fatal(err)
	}
	s := newTestStore(b)
	ctx := attribution.With(context.Background(), attribution.Identity{ID: "ada", Name: "Ada Lovelace", Email: "ada@example.test", Source: "browser"})
	started := time.Now()
	count, _ := replayFixture(b, s, revs)
	replayed := time.Since(started)
	listed, err := s.List(ctx)
	if err != nil {
		b.Fatal(err)
	}
	// The note with the most revisions, and the folder of notes closest to
	// the bake-off's 50 for the move.
	env := testEnv(b, s)
	var busiest string
	if err := env.db.Pool.QueryRow(ctx, `SELECT e.path FROM entries e JOIN (SELECT entry_id, count(*) AS n FROM revision_changes WHERE org = $1 GROUP BY entry_id) c USING (entry_id)
		WHERE e.org = $1 AND e.path LIKE '%.md' AND e.path NOT LIKE 'Trash/%' AND e.path NOT LIKE 'comments/%' ORDER BY c.n DESC, e.path LIMIT 1`, env.db.Org).Scan(&busiest); err != nil {
		b.Fatal(err)
	}
	// The folder whose notes, its subfolders' included, are closest to the
	// bake-off's 50 for the move.
	folders := map[string]int{}
	notes := 0
	for _, e := range listed {
		if !strings.HasSuffix(e.Path, ".md") || IsTrash(e.Path) {
			continue
		}
		notes++
		for dir := filepath.Dir(e.Path); dir != "."; dir = filepath.Dir(dir) {
			folders[dir]++
		}
	}
	busiestHistory, err := s.notes.History(ctx, notestore.HistoryQuery{Path: busiest})
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("replayed F2: %d revisions in %v; %d listed, %d notes; busiest note %s with %d revisions",
		count, replayed.Round(time.Millisecond), len(listed), notes, busiest, len(busiestHistory))

	warm := func(name string, read func() error) {
		b.Run(name, func(b *testing.B) {
			var l latencies
			for b.Loop() {
				l.time(func() {
					if err := read(); err != nil {
						b.Fatal(err)
					}
				})
			}
			l.report(b)
		})
	}
	cold := func(name string, read func() error) {
		b.Run(name, func(b *testing.B) {
			var l latencies
			for b.Loop() {
				b.StopTimer()
				s = reopenTestStore(b, s)
				b.StartTimer()
				l.time(func() {
					if err := read(); err != nil {
						b.Fatal(err)
					}
				})
			}
			l.report(b)
		})
	}
	expect := func(n, want int, err error) error {
		if err == nil && n != want {
			err = fmt.Errorf("%d results, want %d", n, want)
		}
		return err
	}
	cold("status/cold", func() error { _, err := s.Status(ctx); return err })
	cold("list/cold", func() error { e, err := s.List(ctx); return expect(len(e), len(listed), err) })
	// find_notes and /api/notes?q= search through FindScoped, not Find, in
	// the default search scope (Archives left out, Drafts included) for at
	// most 100 results: it reads every note of the head and its searchable
	// text on each call, where Find filters the cached listing.
	search := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "include"}, Collection: "all"}
	findScoped := func(query, tag string) func() error {
		return func() error {
			found, err := s.FindScoped(ctx, query, tag, "", "", false, 100, search)
			if err == nil && len(found.Entries) == 0 {
				err = fmt.Errorf("no notes found for %q, tag %q", query, tag)
			}
			return err
		}
	}
	cold("find/cold", func() error { _, err := s.Find(ctx, "Harbor", "", "", ""); return err })
	cold("find-scoped/cold", findScoped("harbor", ""))
	if _, err := s.Find(ctx, "Harbor", "", "", ""); err != nil {
		b.Fatal(err)
	}
	warm("status/warm", func() error { _, err := s.Status(ctx); return err })
	warm("list/warm", func() error { e, err := s.List(ctx); return expect(len(e), len(listed), err) })
	warm("browse/warm", func() error { _, err := s.BrowseCollection(ctx, "", ""); return err })
	warm("collections/warm", func() error { _, err := s.Collections(ctx); return err })
	warm("find/rare", func() error { _, err := s.Find(ctx, "Harbor", "", "", ""); return err })
	warm("find/common", func() error { _, err := s.Find(ctx, "the", "", "", ""); return err })
	warm("find/tag", func() error { _, err := s.Find(ctx, "", "research", "", ""); return err })
	warm("find-scoped/rare", findScoped("harbor", ""))
	warm("find-scoped/common", findScoped("the", ""))
	warm("find-scoped/tag", findScoped("", "research"))
	warm("read/note", func() error { _, err := s.Read(ctx, busiest, ""); return err })
	warm("history/note", func() error {
		h, err := s.History(ctx, busiest)
		return expect(len(h), min(len(busiestHistory), historyLimit), err)
	})
	warm("history/note-all", func() error {
		h, err := s.notes.History(ctx, notestore.HistoryQuery{Path: busiest})
		return expect(len(h), len(busiestHistory), err)
	})
	warm("history/notebook", func() error { h, err := s.History(ctx, ""); return expect(len(h), historyLimit, err) })
	head, err := s.notes.Head(ctx)
	if err != nil {
		b.Fatal(err)
	}
	earlier, err := s.notes.Revisions(ctx, head.Seq-1001, 1)
	if err != nil || len(earlier) != 1 {
		b.Fatal(len(earlier), err)
	}
	warm("changes-since/1000", func() error {
		c, err := s.ChangesSince(ctx, earlier[0].ID.String(), head.Commit.String())
		if err == nil && (c.FreshReview || len(c.Files) == 0) {
			err = fmt.Errorf("fresh review %v, %d files", c.FreshReview, len(c.Files))
		}
		return err
	})

	doc, err := s.Read(ctx, busiest, "")
	if err != nil {
		b.Fatal(err)
	}
	b.Run("save/note", func(b *testing.B) {
		var l latencies
		for i := 0; b.Loop(); i++ {
			content := slices.Concat(doc.Content, []byte(fmt.Sprintf("\nAn edit, number %d.\n", i)))
			l.time(func() {
				if doc, err = s.Save(ctx, busiest, content, doc.Revision, ""); err != nil {
					b.Fatal(err)
				}
			})
		}
		l.report(b)
	})
	// A move rewrites the links to the moved notes in every other note,
	// those in the Trash included, and refuses to when one of those is
	// locked. F2's locked notes link to every folder, so the moves run
	// without its locks, on the folder closest to 50 notes that can move.
	if _, err := s.notes.Stat(ctx, gitobj.ID{}, locksPath); err == nil {
		commitFixture(b, s, notestore.FileChange{Path: locksPath})
	}
	candidates := slices.SortedFunc(maps.Keys(folders), func(a, b string) int {
		if d := abs(folders[a]-50) - abs(folders[b]-50); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	folder := ""
	for _, f := range candidates {
		status, err := s.Status(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := s.Move(ctx, f, f+" moved", status.Revision, ""); err == nil {
			folder = f
			break
		}
	}
	if folder == "" {
		b.Fatal("no folder can move")
	}
	b.Logf("moving %s with %d notes", folder, folders[folder])
	b.Run("move/folder", func(b *testing.B) {
		var l latencies
		from, to := folder+" moved", folder
		for b.Loop() {
			status, err := s.Status(ctx)
			if err != nil {
				b.Fatal(err)
			}
			l.time(func() {
				if _, err := s.Move(ctx, from, to, status.Revision, ""); err != nil {
					b.Fatal(err)
				}
			})
			from, to = to, from
		}
		l.report(b)
	})
}

func abs(n int) int { return max(n, -n) }
