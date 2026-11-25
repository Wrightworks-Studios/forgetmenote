package notestore_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
)

// A read model built while another gateway writes is one revision: its head,
// files and Versions all come from the same moment.
func TestColdReadsDuringWritesSeeOneRevision(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("nb")
	notePath := func(i int) string { return fmt.Sprintf("a%d/b%d/c%d/n%d.md", i%5, i%11, i%23, i) }
	var files []notestore.FileChange
	for i := range 300 {
		files = append(files, write(notePath(i), fmt.Sprint("note ", i)))
	}
	first := apply(t, n, notestore.ChangeSet{Files: files})
	var seqs sync.Map // commit ID to seq
	seqs.Store(first.Head.Commit, first.Head.Seq)
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			r, err := n.Apply(context.Background(), notestore.ChangeSet{Subject: "w", Author: ada, Files: []notestore.FileChange{
				write(notePath(i%300), fmt.Sprint("edit ", i)), write(fmt.Sprintf("new/%d.md", i), "x")}})
			if err != nil {
				t.Error("write:", err)
				return
			}
			seqs.Store(r.Head.Commit, r.Head.Seq)
		}
	}()
	defer func() { stop.Store(true); wg.Wait() }()
	for i := range 120 {
		other := e.open(e.db, notestore.Options{})
		nb := other.Notebook("nb")
		if i%2 == 1 {
			if _, err := nb.Head(e.ctx); err != nil {
				t.Fatalf("cold head %d: %v", i, err)
			}
		}
		files, err := nb.Files(e.ctx, gitobj.ID{})
		if err != nil {
			t.Fatalf("cold read %d: %v", i, err)
		}
		// Commit k (from seq 3) added new/k-3.md: the files hold new/0 up
		// to the newest commit any of their Versions names, and no Version
		// is newer than that.
		added, newest := 0, int64(0)
		for _, f := range files {
			seq, ok := seqs.Load(f.Version)
			if !ok { // committed, but its writer has not returned yet
				info, err := nb.Commit(e.ctx, f.Version)
				if err != nil {
					t.Fatalf("cold read %d: %s has Version %s: %v", i, f.Path, f.Version, err)
				}
				seq = info.Seq
				seqs.Store(f.Version, seq)
			}
			newest = max(newest, seq.(int64))
			if name, ok := strings.CutPrefix(f.Path, "new/"); ok {
				k, _ := strconv.Atoi(strings.TrimSuffix(name, ".md"))
				if k >= len(files) {
					t.Fatalf("cold read %d: %s", i, f.Path)
				}
				added++
			}
		}
		if len(files) != 300+added || newest != first.Head.Seq+int64(added) {
			t.Fatalf("cold read %d mixes revisions: %d files, %d added, newest Version at seq %d", i, len(files), added, newest)
		}
		other.Close()
	}
}

// A gateway whose model is of a notebook since deleted and created again
// with fewer revisions serves the new notebook, to its own writes too.
func TestARecreatedNotebookReplacesAStaleModel(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("nb")
	for i := range 4 {
		apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write(fmt.Sprintf("old%d.md", i), "old")}})
	}
	other, _ := pgtest.Gateway(t, e.db)
	remote := e.open(other, notestore.Options{}).Notebook("nb")
	if files, err := remote.Files(e.ctx, gitobj.ID{}); err != nil || len(files) != 4 {
		t.Fatal(len(files), err)
	}
	if err := e.store.Delete(e.ctx, "nb"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Create(e.ctx, "nb", ada); err != nil {
		t.Fatal(err)
	}
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("new.md", "new")}})
	r := apply(t, remote, notestore.ChangeSet{Files: []notestore.FileChange{write("remote.md", "remote")}})
	if r.Head.Seq != 3 {
		t.Fatal("write on the new notebook:", r.Head)
	}
	if got := read(t, remote, gitobj.ID{}, "remote.md"); got != "remote" {
		t.Fatal(got)
	}
	files, err := remote.Files(e.ctx, gitobj.ID{})
	if err != nil || strings.Join(paths(files), " ") != "new.md remote.md" {
		t.Fatal(paths(files), err)
	}
	waitFor(t, func() bool { h, _ := n.Head(e.ctx); return h == r.Head })
}

// Watchers hear a deleted notebook as the zero head, on every gateway.
func TestWatchHearsDeletions(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	other, _ := pgtest.Gateway(t, e.db)
	remote := e.open(other, notestore.Options{})
	watch := func(s *notestore.Store) chan notestore.Head {
		heads := make(chan notestore.Head, 64)
		s.Watch(func(notebook string, h notestore.Head) {
			if notebook == "nb" {
				heads <- h
			}
		})
		return heads
	}
	local, far := watch(e.store), watch(remote)
	pgtest.Listening(t, other, "notebook_revisions")
	n := e.create("nb")
	r := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "a")}})
	expect := func(heads chan notestore.Head, want notestore.Head) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case h := <-heads:
				if h == want {
					return
				}
			case <-deadline:
				t.Fatal("never heard", want)
			}
		}
	}
	expect(local, r.Head)
	expect(far, r.Head)
	if err := e.store.Delete(e.ctx, "nb"); err != nil {
		t.Fatal(err)
	}
	expect(local, notestore.Head{})
	expect(far, notestore.Head{})
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

// Two gateways editing one large note at once never fail: each write's
// transaction may find, every time it tries, that the other gateway has
// committed again, and it puts the blocks that head lacks until it commits.
func TestTwoGatewaysWritingOneLargeNoteAtOnceNeverFail(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("nb")
	other, _ := pgtest.Gateway(t, e.db)
	remote := e.open(other, notestore.Options{}).Notebook("nb")
	var b strings.Builder
	for i := range 300 {
		b.WriteString("Paragraph " + strconv.Itoa(i) + "." + strings.Repeat(" Call me Ishmael.", 70) + "\n\n")
	}
	book := b.String()
	first := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("book.md", book)}})
	const writes = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	wrote := map[gitobj.ID]string{}
	for g, nb := range []*notestore.Notebook{n, remote} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range writes {
				content := strings.Replace(book, fmt.Sprintf("Paragraph %d.", (i*7+g*3)%300), fmt.Sprintf("Paragraph %d-%d, edited.", g, i), 1)
				r, err := nb.Apply(context.Background(), notestore.ChangeSet{Subject: "Edit", Author: ada, Files: []notestore.FileChange{write("book.md", content)}})
				if err != nil {
					t.Errorf("gateway %d, write %d: %v", g, i, err)
					continue
				}
				mu.Lock()
				wrote[r.Head.Commit] = content
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	cold := e.open(e.db, notestore.Options{}).Notebook("nb")
	head, err := cold.Head(e.ctx)
	if err != nil || head.Seq != first.Head.Seq+2*writes {
		t.Fatal("head:", head, err)
	}
	for commit, content := range wrote {
		if got := read(t, cold, commit, "book.md"); got != content {
			t.Fatalf("book.md at %s: %d bytes, want %d", commit, len(got), len(content))
		}
	}
}
