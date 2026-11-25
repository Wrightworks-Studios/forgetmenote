package notestore_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

func (e *env) blockFiles() map[string]bool {
	e.t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(e.blobs.Path(), "blocks"))
	if errors.Is(err, fs.ErrNotExist) {
		return out
	}
	if err != nil {
		e.t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			out[entry.Name()] = true
		}
	}
	return out
}

func manuscript(paragraphs int) string {
	rng := rand.New(rand.NewPCG(7, 8))
	var b strings.Builder
	b.WriteString("---\nfmn-id: 6d6f62792d6469636b2d6e6f76656c31\n---\n\n# Book\n\n")
	for i := range paragraphs {
		fmt.Fprintf(&b, "Paragraph %d. %s\n\n", i, strings.Repeat("Call me Ishmael, some years ago. ", 5+rng.IntN(40)))
	}
	return b.String()
}

func TestLargeNotesShareBlocksAcrossVersions(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	book := manuscript(600)
	r1 := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("book.md", book)}})
	first := e.blockFiles()
	if len(first) < 100 {
		t.Fatalf("a %d-byte note is in %d blocks", len(book), len(first))
	}
	edited := strings.Replace(book, "Paragraph 300.", "Paragraph 300, revised.", 1)
	r2 := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("book.md", edited)}})
	added := 0
	for name := range e.blockFiles() {
		if !first[name] {
			added++
		}
	}
	if added < 1 || added > 3 {
		t.Fatalf("a one-paragraph edit added %d blocks", added)
	}
	if got := read(t, n, zero, "book.md"); got != edited {
		t.Fatal("edited note differs")
	}
	if got := read(t, n, r1.Head.Commit, "book.md"); got != book {
		t.Fatal("first version differs")
	}
	// A store with cold caches reads the same bytes, and ranges of them.
	cold := e.open(e.db, notestore.Options{})
	cn := cold.Notebook("notes")
	f, err := cn.Stat(e.ctx, zero, "book.md")
	if err != nil || f.Version != r2.Head.Commit || f.Size != int64(len(edited)) {
		t.Fatal("cold stat:", f, err)
	}
	rng := rand.New(rand.NewPCG(1, 1))
	for range 50 {
		off := rng.Int64N(int64(len(edited)))
		length := rng.Int64N(int64(len(edited)) - off + 1)
		got, err := cn.ReadRange(e.ctx, f.Blob, off, length)
		if err != nil || !bytes.Equal(got, []byte(edited[off:off+length])) {
			t.Fatalf("range %d+%d: %v", off, length, err)
		}
	}
	if _, err := cn.ReadRange(e.ctx, f.Blob, int64(len(edited)), 1); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("a range past the end:", err)
	}
	if got, err := cn.ReadBlob(e.ctx, f.Blob); err != nil || string(got) != edited {
		t.Fatal("cold read:", err)
	}
	// Large other files are cut every 4 MiB and read by range without the
	// whole block.
	asset := make([]byte, 9<<20)
	for i := range asset {
		asset[i] = byte(i * 7)
	}
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{{Path: "assets/scan.tiff", Content: asset}}})
	af, _ := n.Stat(e.ctx, zero, "assets/scan.tiff")
	cold2 := e.open(e.db, notestore.Options{}).Notebook("notes")
	for _, r := range [][2]int64{{0, 10}, {4<<20 - 5, 10}, {9<<20 - 3, 3}, {100, 6 << 20}} {
		got, err := cold2.ReadRange(e.ctx, af.Blob, r[0], r[1])
		if err != nil || !bytes.Equal(got, asset[r[0]:r[0]+r[1]]) {
			t.Fatalf("asset range %v: %v", r, err)
		}
	}
	// A corrupt block is noticed, not served.
	for name := range e.blockFiles() {
		os.Chmod(filepath.Join(e.blobs.Path(), "blocks", name), 0o600)
		if err := os.WriteFile(filepath.Join(e.blobs.Path(), "blocks", name), []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.open(e.db, notestore.Options{}).Notebook("notes").ReadBlob(e.ctx, f.Blob); err == nil {
		t.Fatal("corrupt blocks were read")
	}
}

func TestOtherGatewaysSeeWritesAndWriteOnTop(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "one")}})
	other, _ := pgtest.Gateway(t, e.db)
	remote := e.open(other, notestore.Options{})
	heads := make(chan notestore.Head, 16)
	remote.Watch(func(notebook string, h notestore.Head) {
		if notebook == "notes" {
			heads <- h
		}
	})
	rn := remote.Notebook("notes")
	if got := read(t, rn, zero, "a.md"); got != "one" {
		t.Fatal(got)
	}
	pgtest.Listening(t, other, "notebook_revisions")
	r := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "two")}})
	deadline := time.After(10 * time.Second)
	for {
		select {
		case h := <-heads:
			if h != r.Head {
				continue
			}
		case <-deadline:
			t.Fatal("the other gateway never heard the new head")
		}
		break
	}
	if h, _ := rn.Head(e.ctx); h != r.Head {
		t.Fatal("other gateway's head:", h)
	}
	if got := read(t, rn, zero, "a.md"); got != "two" {
		t.Fatal("other gateway's read:", got)
	}
	// A write through a gateway whose model is behind applies to the stored
	// head, never to its stale copy.
	behind := e.open(e.db, notestore.Options{}).Notebook("notes")
	behind.Head(e.ctx)
	r3 := apply(t, rn, notestore.ChangeSet{Files: []notestore.FileChange{write("b.md", "b")}})
	r4 := apply(t, behind, notestore.ChangeSet{Files: []notestore.FileChange{write("c.md", "c")}})
	info, _ := behind.Commit(e.ctx, r4.Head.Commit)
	if r4.Head.Seq != r3.Head.Seq+1 || info.Commit.Parents[0] != r3.Head.Commit {
		t.Fatal("write on a stale model:", r4.Head, info.Commit.Parents)
	}
	files, _ := behind.Files(e.ctx, zero)
	if strings.Join(paths(files), " ") != "a.md b.md c.md" {
		t.Fatal(paths(files))
	}
}

func TestConcurrentWritersSerializeIntoOneLine(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	other, _ := pgtest.Gateway(t, e.db)
	stores := []*notestore.Notebook{n, e.open(other, notestore.Options{}).Notebook("notes")}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := stores[i%2].Apply(e.ctx, notestore.ChangeSet{Subject: "w", Author: ada, Files: []notestore.FileChange{write(fmt.Sprintf("f%02d.md", i), "x")}})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	revs, err := n.Revisions(e.ctx, 0, 100)
	if err != nil || len(revs) != 25 {
		t.Fatal(len(revs), err)
	}
	for i, r := range revs[1:] {
		if r.Seq != int64(i+2) || r.Commit.Parents[0] != revs[i].ID {
			t.Fatalf("revision %d is not on one line", r.Seq)
		}
	}
	// The other gateway learns of writes it did not make through NOTIFY (or
	// its fallback poll), so its head converges rather than being current at
	// once; every write must still appear there.
	deadline := time.Now().Add(10 * time.Second)
	for {
		files, err := stores[1].Files(e.ctx, zero)
		if err == nil && len(files) == 24 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(len(files), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAFailedWriteLeavesNoTrace(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	r1 := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "one")}})
	restore := pgtest.FailTable(t, e.db, "revisions")
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, Files: []notestore.FileChange{write("a.md", "two"), write("b.md", manuscript(100))}}); err == nil {
		t.Fatal("the write succeeded")
	}
	restore()
	if h, _ := n.Head(e.ctx); h != r1.Head {
		t.Fatal("head moved:", h)
	}
	if got := read(t, n, zero, "a.md"); got != "one" {
		t.Fatal(got)
	}
	cold := e.open(e.db, notestore.Options{}).Notebook("notes")
	if files, _ := cold.Files(e.ctx, zero); len(files) != 1 {
		t.Fatal("stored files:", paths(files))
	}
	r2 := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "two")}})
	if r2.Head.Seq != r1.Head.Seq+1 {
		t.Fatal(r2.Head)
	}
}

func TestReadModelsStayWithinTheirBudget(t *testing.T) {
	e := newEnv(t, notestore.Options{ReadModelBytes: 1})
	var notebooks []*notestore.Notebook
	for i := range 4 {
		n := e.create(fmt.Sprintf("nb%d", i))
		var files []notestore.FileChange
		for j := range 50 {
			files = append(files, write(fmt.Sprintf("dir%d/file%d.md", j%5, j), fmt.Sprintf("%d/%d", i, j)))
		}
		apply(t, n, notestore.ChangeSet{Files: files})
		notebooks = append(notebooks, n)
	}
	for round := range 3 {
		for i, n := range notebooks {
			if got := read(t, n, zero, "dir3/file13.md"); got != fmt.Sprintf("%d/13", i) {
				t.Fatalf("round %d notebook %d: %q", round, i, got)
			}
			apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write(fmt.Sprintf("round%d.md", round), "r")}})
		}
	}
	files, err := notebooks[2].Files(e.ctx, zero)
	if err != nil || len(files) != 53 {
		t.Fatal(len(files), err)
	}
}

func TestDeleteAndSweepRemoveOnlyUnusedContent(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	a, b := e.create("alpha"), e.create("beta")
	shared, own := manuscript(200), manuscript(300)
	apply(t, a, notestore.ChangeSet{Files: []notestore.FileChange{write("shared.md", shared), write("own.md", own), write("small.md", "s")}})
	apply(t, b, notestore.ChangeSet{Files: []notestore.FileChange{write("copy.md", shared)}})
	apply(t, a, notestore.ChangeSet{Files: []notestore.FileChange{write("own.md", own+"More.\n\n")}})
	before := len(e.blockFiles())
	age := func() {
		if _, err := e.db.Pool.Exec(e.ctx, `UPDATE content_blocks SET touched = touched - interval '2 hours' WHERE org = $1`, e.db.Org); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Pool.Exec(e.ctx, `UPDATE content_segments SET touched = touched - interval '2 hours' WHERE org = $1`, e.db.Org); err != nil {
			t.Fatal(err)
		}
	}
	age()
	if r, err := e.store.SweepBlocks(e.ctx, time.Hour); err != nil || r.Blocks != 0 || r.Segments != 0 {
		t.Fatal("a sweep of used content removed:", r, err)
	}
	if err := e.store.Delete(e.ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Head(e.ctx); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("deleted notebook:", err)
	}
	if err := e.store.Delete(e.ctx, "alpha"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("deleting twice:", err)
	}
	// The deleted file versions touched their blocks: an immediate sweep
	// keeps them for any write that reuses them.
	if r, err := e.store.SweepBlocks(e.ctx, time.Hour); err != nil || r.Blocks != 0 {
		t.Fatal("a sweep right after the delete:", r, err)
	}
	age()
	r, err := e.store.SweepBlocks(e.ctx, time.Hour)
	if err != nil || r.Blocks == 0 || r.Segments == 0 {
		t.Fatal("sweep:", r, err)
	}
	if got := len(e.blockFiles()); got != before-r.Blocks || got == 0 {
		t.Fatalf("%d blocks before, %d swept, %d left", before, r.Blocks, got)
	}
	cold := e.open(e.db, notestore.Options{}).Notebook("beta")
	if got := read(t, cold, zero, "copy.md"); got != shared {
		t.Fatal("the other notebook's copy was damaged")
	}
	var rows int
	e.db.Pool.QueryRow(e.ctx, `SELECT count(*) FROM file_versions WHERE org = $1`, e.db.Org).Scan(&rows)
	if rows != 1 {
		t.Fatalf("%d file versions remain", rows)
	}
	// Writing the deleted content again stores it again.
	again := e.create("alpha")
	apply(t, again, notestore.ChangeSet{Files: []notestore.FileChange{write("own.md", own)}})
	if got := read(t, e.open(e.db, notestore.Options{}).Notebook("alpha"), zero, "own.md"); got != own {
		t.Fatal("rewritten content differs")
	}
	if err := e.store.Delete(e.ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(e.ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	age()
	if _, err := e.store.SweepBlocks(e.ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if left := e.blockFiles(); len(left) != 0 {
		t.Fatalf("%d blocks remain after every notebook went", len(left))
	}
	for _, table := range []string{"revisions", "revision_changes", "git_trees", "entries", "file_versions", "content_segments", "content_blocks", "notebook_heads"} {
		var count int
		e.db.Pool.QueryRow(e.ctx, `SELECT count(*) FROM `+table+` WHERE org = $1`, e.db.Org).Scan(&count)
		if count != 0 {
			t.Errorf("%d rows remain in %s", count, table)
		}
	}
}

func TestSweepKeepsRecentBlocksOfUnfinishedWrites(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	restore := pgtest.FailTable(t, e.db, "revisions")
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, Files: []notestore.FileChange{write("book.md", manuscript(100))}}); err == nil {
		t.Fatal("the write succeeded")
	}
	restore()
	orphans := len(e.blockFiles())
	if orphans == 0 {
		t.Fatal("the failed write put no blocks")
	}
	if r, err := e.store.SweepBlocks(e.ctx, 0); err != nil || r.Blocks != 0 {
		t.Fatal("blocks of a write that may still commit were swept:", r, err)
	}
	if _, err := e.db.Pool.Exec(e.ctx, `UPDATE content_blocks SET touched = now() - interval '1 hour' WHERE org = $1`, e.db.Org); err != nil {
		t.Fatal(err)
	}
	if r, err := e.store.SweepBlocks(e.ctx, 0); err != nil || r.Blocks != orphans {
		t.Fatal("orphaned blocks:", r, orphans, err)
	}
	if left := e.blockFiles(); len(left) != 0 {
		t.Fatal(len(left))
	}
}

func TestWatchHearsLocalCommits(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	heads := make(chan notestore.Head, 8)
	e.store.Watch(func(notebook string, h notestore.Head) { heads <- h })
	n := e.create("notes")
	r := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "a")}})
	timeout := time.After(5 * time.Second)
	for {
		select {
		case h := <-heads:
			if h == r.Head {
				return
			}
		case <-timeout:
			t.Fatal("no head delivered")
		}
	}
}

func TestContextCancellationStopsAWrite(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	if _, err := n.Apply(ctx, notestore.ChangeSet{Subject: "x", Author: ada, Files: []notestore.FileChange{write("a.md", "a")}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := n.Stat(e.ctx, gitobj.ID{}, "a.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

// A write never waits for a second connection while its transaction holds
// one, so writers cannot exhaust the pool between them.
func TestWritesNeedOnlyOnePoolConnection(t *testing.T) {
	address, err := url.Parse(pgtest.URL(t, true))
	if err != nil {
		t.Fatal(err)
	}
	q := address.Query()
	q.Set("pool_max_conns", "1")
	address.RawQuery = q.Encode()
	db, err := pg.Open(context.Background(), address.String(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	e := newEnv(t, notestore.Options{})
	s := e.open(db, notestore.Options{ImportEntryID: declaredID})
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	defer cancel()
	if _, err := s.Create(ctx, "notes", ada); err != nil {
		t.Fatal(err)
	}
	n := s.Notebook("notes")
	book := manuscript(300)
	for _, content := range []string{book, strings.Replace(book, "Paragraph 150.", "Paragraph 150, edited.", 1)} {
		if _, err := n.Apply(ctx, notestore.ChangeSet{Subject: "Write the book", Author: ada, Files: []notestore.FileChange{write("book.md", content)}}); err != nil {
			t.Fatal("write:", err)
		}
	}
	g := newGitRepo(t, false)
	g.write("Inbox/idea.md", "---\nfmn-id: 0123456789abcdef0123456789abcdef\n---\n\nIdea.\n", 0o644)
	g.write("book.md", book+"An imported ending.\n", 0o644)
	g.commit(ada, "Root", nil)
	g.write("book.md", book+"An imported ending, revised.\n", 0o644)
	g.commit(ada, "Revise", nil)
	for _, raw := range strings.Fields(g.run(nil, "rev-list", "--reverse", "HEAD")) {
		commit := []byte(g.run(nil, "cat-file", "commit", raw))
		c, _ := gitobj.ParseCommit(commit)
		trees := map[gitobj.ID][]byte{}
		for _, line := range append([]string{"040000 tree " + c.Tree.String()}, strings.Split(strings.TrimSpace(g.run(nil, "ls-tree", "-r", "-t", raw)), "\n")...) {
			if fields := strings.Fields(line); len(fields) >= 3 && fields[1] == "tree" {
				trees[g.id(fields[2])] = []byte(g.run(nil, "cat-file", "tree", fields[2]))
			}
		}
		if _, err := s.Notebook("imported").ImportCommit(ctx, commit, trees, func(id gitobj.ID) ([]byte, error) {
			return []byte(g.run(nil, "cat-file", "blob", id.String())), nil
		}); err != nil {
			t.Fatal("import:", err)
		}
	}
	if got, _, err := s.Notebook("imported").Read(ctx, gitobj.ID{}, "book.md"); err != nil || string(got) != book+"An imported ending, revised.\n" {
		t.Fatal("imported book:", len(got), err)
	}
}

// The same content written under a name that cuts it into other blocks reads
// back, from the cache and cold.
func TestContentStoredOnceReadsUnderEveryName(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	content := manuscript(100)
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("data.txt", content)}})
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("copy.md", content)}})
	for _, nb := range []*notestore.Notebook{n, e.open(e.db, notestore.Options{}).Notebook("notes")} {
		if got := read(t, nb, zero, "copy.md"); got != content {
			t.Fatal("copy differs")
		}
	}
}
