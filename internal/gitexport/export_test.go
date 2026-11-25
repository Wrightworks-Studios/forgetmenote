package gitexport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// The export is the notebook's history as Git has it, the same bytes every
// time, and a bundle of it imports into a new notebook that exports to the
// same commits.
func TestExportIsReproducibleAndRoundTripsThroughABundle(t *testing.T) {
	e := newEnv(t)
	n := e.create("notes")
	head := history(t, n)
	got, err := e.ex.Sync(e.ctx, "notes")
	if err != nil || got != head {
		t.Fatal(got, head, err)
	}
	repo := e.ex.Path("notes")
	if ref := git(t, repo, "rev-parse", "refs/heads/main"); ref != head.Commit.String() {
		t.Fatalf("branch %s, head %s", ref, head.Commit)
	}
	if symbolic := git(t, repo, "symbolic-ref", "HEAD"); symbolic != "refs/heads/main" {
		t.Fatal(symbolic)
	}
	git(t, repo, "fsck", "--strict", "--no-dangling")
	if count := git(t, repo, "rev-list", "--count", "main"); count != "7" {
		t.Fatal("commits:", count)
	}
	// The layout and trailers are the notebook's, byte for byte.
	files, err := n.Files(e.ctx, gitobj.ID{})
	if err != nil {
		t.Fatal(err)
	}
	var listing []string
	for _, f := range files {
		listing = append(listing, f.Path)
		content, _, err := n.Read(e.ctx, gitobj.ID{}, f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if exported, _ := gitErr(repo, nil, "cat-file", "blob", "main:"+f.Path); exported != string(content) {
			t.Fatalf("%s differs in the export", f.Path)
		}
	}
	if exported := strings.Fields(git(t, repo, "ls-tree", "-r", "--name-only", "main")); !slices.Equal(exported, listing) {
		t.Fatalf("export lists %v, notebook %v", exported, listing)
	}
	if mode := git(t, repo, "ls-tree", "main", "assets/tool.sh"); !strings.HasPrefix(mode, "100755 ") {
		t.Fatal("executable:", mode)
	}
	if trailers := git(t, repo, "log", "-1", "--format=%(trailers:key=Gitnotes-Source,valueonly)", "main"); trailers != "mcp" {
		t.Fatal("trailers:", trailers)
	}

	// Another export of the same revisions writes identical files.
	again := e.exporter(t.TempDir(), Options{})
	if _, err := again.Sync(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	first, second := objectFiles(t, repo), objectFiles(t, again.Path("notes"))
	if len(first) == 0 || !maps.EqualFunc(first, second, bytes.Equal) {
		t.Fatalf("exports differ: %d and %d objects", len(first), len(second))
	}

	// Export, bundle, import, export: the same commits.
	var bundle bytes.Buffer
	if err := e.ex.Bundle(e.ctx, "notes", &bundle, 1<<30); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	upload := filepath.Join(t.TempDir(), "notes.bundle")
	if err := os.WriteFile(upload, bundle.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, ".", "clone", "--quiet", upload, clone)
	if text, _ := os.ReadFile(filepath.Join(clone, "Projects/Ideas/idea.md")); !strings.Contains(string(text), "An idea, moved.") {
		t.Fatal("a clone of the bundle:", string(text))
	}
	copied := e.store.Notebook("copy")
	imported, err := ImportBundle(e.ctx, copied, bytes.NewReader(bundle.Bytes()), testLimits(t))
	if err != nil {
		t.Fatal(err)
	}
	if imported.Commit != head.Commit || imported.Seq != head.Seq {
		t.Fatal("imported head:", imported, head)
	}
	if _, err := Verify(e.ctx, copied, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ex.Sync(e.ctx, "copy"); err != nil {
		t.Fatal(err)
	}
	if a, b := git(t, repo, "rev-list", "main"), git(t, e.ex.Path("copy"), "rev-list", "main"); a != b {
		t.Fatalf("round trip changed commits:\n%s\n%s", a, b)
	}
	idea, err := copied.Stat(e.ctx, gitobj.ID{}, "Projects/Ideas/idea.md")
	if err != nil || idea.EntryID != "0123456789abcdef0123456789abcdef" {
		t.Fatal("the imported note keeps its fmn-id:", idea, err)
	}

	// Maintenance packs the objects and checks them; exports carry on.
	if err := e.ex.Maintain(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if loose := objectFiles(t, repo); len(loose) != 0 {
		t.Fatal("loose objects after repack:", len(loose))
	}
	next := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("AGENTS.md", "Follow the rules, revised.\n")}}).Head
	if got, err := e.ex.Sync(e.ctx, "notes"); err != nil || got != next {
		t.Fatal(got, err)
	}
	git(t, repo, "fsck", "--strict", "--no-dangling")
	if err := e.ex.Maintain(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
}

// An export after a new revision writes exactly the objects Git says the
// new revision added, also when earlier objects are packed.
func TestIncrementalExportWritesOnlyNewObjects(t *testing.T) {
	e := newEnv(t)
	n := e.create("notes")
	old := history(t, n)
	if _, err := e.ex.Sync(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	repo := e.ex.Path("notes")
	for _, packed := range []bool{false, true} {
		if packed {
			if err := e.ex.Maintain(e.ctx, "notes"); err != nil {
				t.Fatal(err)
			}
		}
		before := objectFiles(t, repo)
		book, _, err := n.Read(e.ctx, gitobj.ID{}, "Projects/book.md")
		if err != nil {
			t.Fatal(err)
		}
		edited := strings.Replace(string(book), "A paragraph", "One paragraph", 1)
		next := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("Projects/book.md", edited)}}).Head
		if _, err := e.ex.Sync(e.ctx, "notes"); err != nil {
			t.Fatal(err)
		}
		var written []string
		for p := range objectFiles(t, repo) {
			if _, ok := before[p]; !ok {
				written = append(written, strings.Replace(p, "/", "", 1))
			}
		}
		slices.Sort(written)
		want := sortedFields(git(t, repo, "rev-list", "--objects", "--no-object-names", next.Commit.String(), "--not", old.Commit.String()))
		if !slices.Equal(written, want) || len(want) != 4 {
			t.Fatalf("packed %v: wrote %v, the revision added %v", packed, written, want)
		}
		old = next
	}
	// Nothing new writes nothing.
	before := objectFiles(t, repo)
	if _, err := e.ex.Sync(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if len(objectFiles(t, repo)) != len(before) {
		t.Fatal("an unchanged notebook wrote objects")
	}
}

// Started, the exporter follows every commit the store reports and catches
// up with notebooks that changed while it was not running.
func TestStartedExporterFollowsCommitsAndCatchesUp(t *testing.T) {
	e := newEnv(t)
	n := e.create("notes")
	history(t, n)
	quiet := e.create("quiet")
	dir := t.TempDir()
	ex := e.exporter(dir, Options{CatchUp: time.Hour, Maintenance: time.Millisecond,
		Notebooks: func(context.Context) ([]string, error) { return []string{"notes", "quiet"}, nil }})
	if _, err := New(e.store, dir, Options{}); !errors.Is(err, ErrExporting) {
		t.Fatal("a second exporter of one directory:", err)
	}
	ex.Start()
	headOf := func(nb *notestore.Notebook) gitobj.ID {
		h, err := nb.Head(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return h.Commit
	}
	exported := func(nb *notestore.Notebook) func() bool {
		return func() bool { return refOf(t, ex.Path(nb.ID())) == headOf(nb) }
	}
	waitFor(t, "the listed notebooks' first export", func() bool { return exported(n)() && exported(quiet)() })
	// Maintenance runs in the background after new objects.
	waitFor(t, "maintenance", func() bool { return len(objectFiles(t, ex.Path("notes"))) == 0 })
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("Inbox/new.md", "New.\n")}})
	waitFor(t, "a commit's export", exported(n))
	later := e.create("later")
	waitFor(t, "a new notebook's export", exported(later))
	git(t, ex.Path("notes"), "fsck", "--strict", "--no-dangling")

	// Commits while no exporter runs are caught up from the repositories
	// on disk.
	ex.Close()
	apply(t, later, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "a\n")}})
	restarted := e.exporter(dir, Options{CatchUp: time.Hour})
	restarted.Start()
	waitFor(t, "the catch-up", exported(later))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func refOf(t *testing.T, repo string) gitobj.ID {
	t.Helper()
	id, err := readRef(repo)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return id
}

// A first export of many revisions writes them as one pack and computes
// their deltas; the repository is the store's history all the same, and
// later exports, even of many revisions, carry on from it as loose objects.
func TestExportOfManyRevisionsAtOnce(t *testing.T) {
	defer func(n int64) { packRevisions = n }(packRevisions)
	packRevisions = 10
	e := newEnv(t)
	n := e.create("notes")
	page := strings.Repeat("A line every version of the note keeps.\n", 60)
	edit := func(revisions int) notestore.Head {
		var head notestore.Head
		for i := range revisions {
			head = apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{
				write(fmt.Sprintf("Inbox/note %d.md", i%7), fmt.Sprintf("%sRevision %d of note %d.\n", page, i, i%7)),
			}}).Head
		}
		return head
	}
	history(t, n)
	repo := e.ex.Path("notes")
	packs := func() []string {
		p, _ := filepath.Glob(filepath.Join(repo, "objects", "pack", "*.pack"))
		return p
	}
	for i, revisions := range []int{25, 30, 1} {
		head := edit(revisions)
		if got, err := e.ex.Sync(e.ctx, "notes"); err != nil || got != head {
			t.Fatal(revisions, got, err)
		}
		if refOf(t, repo) != head.Commit {
			t.Fatal("the branch is not the head")
		}
		if _, err := Verify(e.ctx, n, repo); err != nil {
			t.Fatal(revisions, err)
		}
		git(t, repo, "fsck", "--strict", "--no-dangling")
		if len(packs()) != 1 {
			t.Fatalf("after export %d: packs %v", i+1, packs())
		}
		if i == 0 && deltified(t, repo) == 0 {
			t.Fatal("the first export's pack has no deltas")
		}
	}
	if count := git(t, repo, "count-objects", "-v"); strings.HasPrefix(count, "count: 0\n") {
		t.Fatal("later exports wrote no loose objects:", count)
	}
}

// deltified counts the objects of repo stored as deltas.
func deltified(t testing.TB, repo string) int {
	t.Helper()
	n := 0
	for _, base := range strings.Fields(git(t, repo, "cat-file", "--batch-all-objects", "--batch-check=%(deltabase)")) {
		if strings.Trim(base, "0") != "" {
			n++
		}
	}
	return n
}

// Maintenance deletes what an export, rebuild or repack that stopped halfway
// left in the repository, such as a first export's staged pack, which only a
// purge's rebuild removed before; the repository keeps the notebook's
// history.
func TestMaintenanceRemovesWhatAStoppedExportLeft(t *testing.T) {
	e := newEnv(t)
	n := e.create("notes")
	head := history(t, n)
	if _, err := e.ex.Sync(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	repo := e.ex.Path("notes")
	stale := []string{
		filepath.Join(repo, "objects", "pack", "tmp_rebuild_1234567"),   // writePack's staged pack
		filepath.Join(repo, "objects", "pack", "tmp_pack_AbCdEf"),       // git index-pack's
		filepath.Join(repo, "objects", "pack", ".tmp-4242-pack-x.pack"), // git repack's
		filepath.Join(repo, "objects", "ab", "tmp_obj_0123456789"),      // a loose object's
	}
	for _, p := range stale {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("PACK half written"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.ex.Maintain(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	for _, p := range stale {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Error("maintenance left", filepath.Base(p), err)
		}
	}
	if refOf(t, repo) != head.Commit {
		t.Fatal("the branch moved")
	}
	if _, err := Verify(e.ctx, n, repo); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "fsck", "--strict", "--no-dangling")
}

// Maintenance that starts while an export is writing its pack waits for it:
// the export's staged pack stays until the export is done with it, and the
// export completes as if it had run alone.
func TestMaintenanceLeavesAnExportInProgressAlone(t *testing.T) {
	defer func(n int64) { packRevisions = n }(packRevisions)
	packRevisions = 3
	e := newEnv(t)
	n := e.create("notes")
	head := history(t, n)
	repo := e.ex.Path("notes")
	// Git's index-pack, which reads the staged pack, waits until released.
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim, signals := t.TempDir(), t.TempDir()
	started, release := filepath.Join(signals, "started"), filepath.Join(signals, "release")
	script := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = index-pack ]; then : >" + started + "; while [ ! -e " + release + " ]; do sleep 0.05; done; fi; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	released := func() { os.WriteFile(release, nil, 0o644) }
	t.Cleanup(released)

	synced := make(chan error, 1)
	go func() {
		got, err := e.ex.Sync(e.ctx, "notes")
		if err == nil && got != head {
			err = fmt.Errorf("exported %v, not %v", got, head)
		}
		synced <- err
	}()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the export never indexed its pack")
		}
	}
	staged, _ := filepath.Glob(filepath.Join(repo, "objects", "pack", "tmp_rebuild_*"))
	if len(staged) != 1 {
		t.Fatal("the export in progress stages one pack:", staged)
	}
	maintained := make(chan error, 1)
	go func() { maintained <- e.ex.Maintain(e.ctx, "notes") }()
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-maintained:
		t.Fatal("maintenance ran during the export:", err)
	default:
	}
	if _, err := os.Stat(staged[0]); err != nil {
		t.Fatal("maintenance removed the staged pack of an export in progress:", err)
	}
	released()
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	if err := <-maintained; err != nil {
		t.Fatal(err)
	}
	if refOf(t, repo) != head.Commit {
		t.Fatal("the branch is not the head")
	}
	if _, err := Verify(e.ctx, n, repo); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "fsck", "--strict", "--no-dangling")
	if left, _ := filepath.Glob(filepath.Join(repo, "objects", "pack", "tmp_*")); len(left) != 0 {
		t.Fatal("temporary files remain:", left)
	}
}
