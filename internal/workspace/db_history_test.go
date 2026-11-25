package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
)

// historyCommit commits one revision to s, kept in the notes store, as its
// mutations do: through applyChanges under mu, signed with ctx's attribution.
// renames maps each destination to its source, which keeps its entry and,
// unless changes write the destination, its content; changes write content,
// or delete a path with nil.
func historyCommit(t testing.TB, ctx context.Context, s *Store, summary string, renames map[string]string, changes map[string][]byte) string {
	t.Helper()
	db := s.backend
	ctx = s.mutationContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	head, err := db.head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.applyChanges(ctx, head, summary, orderedChanges(changes, renames))
	if err != nil {
		t.Fatal(err)
	}
	return result.Head.Commit.String()
}

// historySave saves content at path over whatever is there.
func historySave(t testing.TB, ctx context.Context, s *Store, path, content string) string {
	t.Helper()
	expected := ""
	if doc, err := s.Read(ctx, path, ""); err == nil {
		expected = doc.Revision
	}
	doc, err := s.Save(ctx, path, []byte(content), expected, "test")
	if err != nil {
		t.Fatal(err)
	}
	return doc.Revision
}

var historyTimeFormat = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d[+-]\d\d:\d\d$`)

// historyScope admits ordinary notes only.
var historyScope = ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}

// historyNotes reads every batch of one revision's history notes as the
// knowledge index does, sharing known blobs between batches.
func readHistoryNotes(t *testing.T, s *Store, revision string, known map[string]SnapshotFile) ([]HistoryBatch, error) {
	t.Helper()
	var batches []HistoryBatch
	for offset := 0; ; {
		batch, err := s.HistoryNotes(context.Background(), revision, offset, known)
		if err != nil {
			return batches, err
		}
		batches = append(batches, batch)
		for _, f := range batch.Files {
			known[f.Blob] = f.SnapshotFile
		}
		if batch.Done {
			return batches, nil
		}
		if batch.Next <= offset {
			t.Fatalf("%s: batch at %d made no progress", revision, offset)
		}
		offset = batch.Next
	}
}

// The history of a note FMN moved, renamed, edited, deleted and created
// again, and of the folders it lived in, follows the renames each move
// records, a move whose content changed beyond recognition by its subject
// alone, and a rename without a move's subject as a rename.
func TestMovedNoteAndFolderHistoryFollowsRenames(t *testing.T) {
	ctx := attribution.With(context.Background(), attribution.Identity{ID: "u-1", Name: "Ada Lovelace", Email: "ada@example.com", Source: "browser", Client: "  Tester  "})
	excluded := []string{"private"}
	db := newTestStoreExcluding(t, excluded)
	commit := func(summary string, renames map[string]string, changes map[string][]byte) {
		historyCommit(t, ctx, db, summary, renames, changes)
	}
	move := func(from, to string, files ...string) {
		renames := map[string]string{}
		for _, f := range files {
			renames[movedPath(f, from, to)] = f
		}
		historyCommit(t, ctx, db, fmt.Sprintf("Move %q to %q", from, to), renames, nil)
	}
	save := func(path, content string) { historySave(t, ctx, db, path, content) }
	save("drafts/a.md", "# A\n\nfirst\n")
	save("drafts/a.md", "# A\n\nsecond\n")
	save("drafts/b.md", "# B\n")
	save("other.md", "# Other\n\nThe original text of the other note.\n")
	save("private/p.md", "# Private\n")
	move("drafts/a.md", "final/a.md", "drafts/a.md")
	save("final/a.md", "# A\n\nthird\n")
	move("drafts", "archive/drafts", "drafts/b.md")
	save("archive/drafts/b.md", "# B\n\nedited\n")
	commit(`Move "other.md" to "elsewhere.md"`, nil, map[string][]byte{"other.md": nil, "elsewhere.md": []byte("# Elsewhere\n\nRewritten from scratch; nothing like it was.\n")})
	commit("Rename by hand", map[string]string{"final/renamed.md": "final/a.md"}, nil)
	commit("Update archive/drafts/b.md", nil, map[string][]byte{"archive/drafts/b.md": nil})
	save("archive/drafts/b.md", "# B, a new note at an old path\n")
	save("final/renamed.md", "---\nfmn-container: archive\n---\n# A\n\nfourth\n")
	save("final/renamed.md", "# A\n\nfifth\n")

	if h, _ := db.History(ctx, "final/renamed.md"); len(h) != 7 || h[len(h)-1].Path != "drafts/a.md" {
		t.Fatalf("the moved note's history does not reach its first path: %+v", h)
	}
	if h, _ := db.History(ctx, "archive/drafts"); len(h) != 8 || h[len(h)-1].Message != "Update drafts/a.md" {
		t.Fatalf("the moved folder's history does not reach its first path: %+v", h)
	}
	if h, _ := db.History(ctx, "elsewhere.md"); len(h) != 2 || h[1].Path != "other.md" {
		t.Fatalf("a move only its subject records was not followed: %+v", h)
	}
	for _, c := range mustHistory(t, db, ctx, "final/renamed.md") {
		if !historyTimeFormat.MatchString(c.AuthoredAt) || c.AuthorName != "Ada Lovelace" {
			t.Fatalf("history entry %+v", c)
		}
		if _, err := db.Read(ctx, c.Path, c.Revision); err != nil {
			t.Fatalf("history entry %s does not open at %s: %v", c.Path, c.Revision, err)
		}
	}
	if h, err := db.HistoryScoped(ctx, "private/p.md", historyScope); err == nil && len(h) != 0 {
		t.Fatalf("scoped history of an excluded note: %+v", h)
	}
}

func mustHistory(t *testing.T, s *Store, ctx context.Context, path string) []Commit {
	t.Helper()
	h, err := s.History(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// historyRepository is a Git repository a notebook is imported from: commit
// writes, removes and renames files through Git at a fixed time, as an
// earlier release or someone editing the repository by hand did.
type historyRepository struct {
	t    *testing.T
	root string
	when time.Time
}

func newHistoryRepository(t *testing.T) *historyRepository {
	r := &historyRepository{t: t, root: t.TempDir(), when: time.Unix(1764000000, 0)}
	r.git("init", "-q", "--initial-branch=main")
	r.git("commit", "-q", "--allow-empty", "-m", "Initialize notes notebook")
	return r
}

func (r *historyRepository) git(args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.root, "-c", "user.name=Grace Hopper", "-c", "user.email=grace@example.com", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	stamp := fmt.Sprintf("@%d +0000", r.when.Unix())
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// commit renames each source of renames to its destination with git mv,
// then writes changes, deleting a path with nil, and commits them with
// message, a minute after the commit before.
func (r *historyRepository) commit(message string, renames map[string]string, changes map[string][]byte) {
	r.t.Helper()
	for to, from := range renames {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(r.root, to)), 0700); err != nil {
			r.t.Fatal(err)
		}
		r.git("mv", from, to)
	}
	for path, content := range changes {
		full := filepath.Join(r.root, path)
		if content == nil {
			r.git("rm", "-q", path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0600); err != nil {
			r.t.Fatal(err)
		}
		r.git("add", path)
	}
	r.when = r.when.Add(time.Minute)
	r.git("commit", "-q", "--allow-empty", "-m", message)
}

// A notebook imported from Git, merges, executable notes, symbolic links,
// renames Git infers and moves only a subject records included, follows a
// note's renames back to its first path, a move only its subject records,
// and lists no merge in a folder's history.
func TestImportedHistoryFollowsGitRenamesAndMoves(t *testing.T) {
	ctx := attribution.With(context.Background(), attribution.Identity{ID: "u-1", Name: "Ada Lovelace", Email: "ada@example.com", Source: "mcp", Client: "  Claude  "})
	excluded := []string{"private"}
	git := newHistoryRepository(t)
	save := func(path, content string) {
		git.commit("Update "+path, nil, map[string][]byte{path: []byte(content)})
	}
	move := func(from, to string) {
		git.commit(fmt.Sprintf("Move %q to %q", from, to), map[string]string{to: from}, nil)
	}
	save("drafts/a.md", "# A\n\nfirst\n")
	save("drafts/a.md", "# A\n\nsecond\n")
	save("drafts/b.md", "# B\n")
	save("other.md", "# Other\n\nThe original text of the other note.\n")
	save("private/p.md", "# Private\n")
	save("assets/data.txt", "plain text\n")
	move("drafts/a.md", "final/a.md")
	save("final/a.md", "# A\n\nthird\n")
	move("drafts", "archive/drafts")
	save("archive/drafts/b.md", "# B\n\nedited\n")
	git.commit(`Move "other.md" to "elsewhere.md"`, nil, map[string][]byte{"other.md": nil, "elsewhere.md": []byte("# Elsewhere\n\nRewritten from scratch; nothing like it was.\n")})
	git.commit("Rename by hand", map[string]string{"final/renamed.md": "final/a.md"}, nil)
	git.commit("Update archive/drafts/b.md", nil, map[string][]byte{"archive/drafts/b.md": nil})
	save("archive/drafts/b.md", "# B, a new note at an old path\n")
	save("final/renamed.md", "---\nfmn-container: archive\n---\n# A\n\nfourth\n")
	save("final/renamed.md", "# A\n\nfifth\n")
	many := map[string][]byte{}
	for i := range 40 {
		many[fmt.Sprintf("many/n-%02d.md", i)] = []byte(fmt.Sprintf("# Note %d\n", i))
	}
	many["many/same.md"] = []byte("# Note 7\n") // a blob two paths share
	git.commit("Add many notes", nil, many)
	large := map[string][]byte{}
	for i := range 5 {
		large[fmt.Sprintf("large/l-%d.md", i)] = []byte(strings.Repeat(fmt.Sprintf("Large note %d. ", i), 1<<20/16))
	}
	git.commit("Add large notes", nil, large)
	// A side branch with an executable note and a symbolic link, merged
	// back after another commit on main.
	git.git("checkout", "-q", "-b", "side")
	if err := os.MkdirAll(filepath.Join(git.root, "side"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(git.root, "side", "x.md"), []byte("# Executable\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("s.md", filepath.Join(git.root, "side", "link.md")); err != nil {
		t.Fatal(err)
	}
	git.git("add", "-A")
	git.commit("Add a side branch", nil, map[string][]byte{"side/s.md": []byte("# Side\n")})
	git.git("checkout", "-q", "main")
	save("main.md", "# Main, meanwhile\n")
	git.git("merge", "-q", "--no-ff", "-m", "Merge the side branch", "side")
	save("final/renamed.md", "# A\n\nsixth, after the merge\n")
	git.commit("Commit a note that is not UTF-8", nil, map[string][]byte{"bad.md": []byte("not UTF-8: \xff\xfe\n")})

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.OpenDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	notes := openTestNotes(t, &testNotebook{db: pgtest.Org(t), blobs: blobs})
	if _, err := gitexport.ImportGitDir(context.Background(), notes.Notebook("notes"), filepath.Join(git.root, ".git")); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(context.Background(), notes, "notes", filepath.Join(dir, "state"), excluded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if h := mustHistory(t, db, ctx, "final/renamed.md"); len(h) != 8 || h[len(h)-1].Path != "drafts/a.md" {
		t.Fatalf("the imported note's history does not reach its first path: %+v", h)
	}
	if h := mustHistory(t, db, ctx, "elsewhere.md"); len(h) != 2 || h[1].Path != "other.md" {
		t.Fatalf("a move only its subject records was not followed: %+v", h)
	}
	if h := mustHistory(t, db, ctx, "side"); len(h) != 1 || h[0].Message != "Add a side branch" {
		t.Fatalf("the merge is listed in a folder's history: %+v", h)
	}
	for _, c := range mustHistory(t, db, ctx, "final/renamed.md") {
		if c.AuthorName != "Grace Hopper" || !historyTimeFormat.MatchString(c.AuthoredAt) {
			t.Fatalf("an imported history entry: %+v", c)
		}
		if _, err := db.Read(ctx, c.Path, c.Revision); err != nil {
			t.Fatalf("history entry %s does not open at %s: %v", c.Path, c.Revision, err)
		}
	}
	status, err := db.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := db.HistoryRevisions(ctx, status.Revision, "")
	if err != nil || len(revisions) == 0 || revisions[0].Revision != status.Revision {
		t.Fatalf("revisions: %+v %v", revisions, err)
	}
	// The head committed the note that is not UTF-8, which history notes
	// refuse, as the Git backend refused it.
	known := map[string]SnapshotFile{}
	for i, r := range revisions {
		_, err := readHistoryNotes(t, db, r.Revision, known)
		if i == 0 && errorText(err) != "historical note is not UTF-8" || i > 0 && err != nil {
			t.Fatalf("history notes of revision %d: %v", i, err)
		}
	}
	// Every Markdown postimage was cited: none is lost to a batch boundary.
	if len(known) < 40 {
		t.Fatalf("history notes cited %d blobs", len(known))
	}
}

// historyHeldBlobs holds reads of chosen keys at the blob store, an external IO
// boundary, as a stalled disk or object store would, until released.
type historyHeldBlobs struct {
	blob.Store
	mu        sync.Mutex
	recording map[string]bool // keys written while recording, or nil
	held      map[string]bool
	entered   chan struct{}
	closed    bool
	released  chan struct{}
}

func (b *historyHeldBlobs) Put(ctx context.Context, key string, body io.ReadSeeker) error {
	b.mu.Lock()
	if b.recording != nil {
		b.recording[key] = true
	}
	b.mu.Unlock()
	return b.Store.Put(ctx, key, body)
}

func (b *historyHeldBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := b.wait(ctx, key); err != nil {
		return nil, err
	}
	return b.Store.Get(ctx, key)
}

func (b *historyHeldBlobs) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if err := b.wait(ctx, key); err != nil {
		return nil, err
	}
	return b.Store.GetRange(ctx, key, offset, length)
}

func (b *historyHeldBlobs) wait(ctx context.Context, key string) error {
	b.mu.Lock()
	if !b.held[key] {
		b.mu.Unlock()
		return nil
	}
	if !b.closed {
		close(b.entered)
		b.closed = true
	}
	released := b.released
	b.mu.Unlock()
	select {
	case <-released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// record returns the keys write stores.
func (b *historyHeldBlobs) record(write func()) map[string]bool {
	b.mu.Lock()
	b.recording = map[string]bool{}
	b.mu.Unlock()
	write()
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := b.recording
	b.recording = nil
	return keys
}

// hold makes reads of keys wait until release runs or the test ends;
// entered closes at the first.
func (b *historyHeldBlobs) hold(t testing.TB, keys map[string]bool) (entered <-chan struct{}, release func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held, b.entered, b.closed, b.released = keys, make(chan struct{}), false, make(chan struct{})
	var once sync.Once
	released := b.released
	release = func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	return b.entered, release
}

// newHistoryHeldStore returns a new notebook whose blob store can hold reads,
// and reopen, which opens it again through a notes store with cold caches,
// so that its reads of content reach the blob store.
func newHistoryHeldStore(t *testing.T) (*Store, *historyHeldBlobs, func(*Store) *Store) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	d, err := blob.OpenDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	blobs := &historyHeldBlobs{Store: d}
	db := pgtest.Org(t)
	open := func() *notestore.Store {
		notes, err := notestore.Open(context.Background(), db, blobs, notestore.Options{ImportEntryID: NoteEntryID})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(notes.Close)
		return notes
	}
	state := filepath.Join(dir, "projects", "notes")
	s, err := CreateDB(context.Background(), open(), "notes", state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	reopen := func(old *Store) *Store {
		old.Close()
		again, err := OpenDB(context.Background(), open(), "notes", state, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { again.Close() })
		return again
	}
	return s, blobs, reopen
}

// History reads never wait for a commit: while another gateway's commit
// waits inside its transaction, holding the notebook's head, every history
// method answers from the revisions committed before it.
func TestHistoryAnswersWhileACommitIsHeld(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	doc, err := s.Save(ctx, "read.md", []byte("committed\n"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := OpenDB(ctx, openTestNotes(t, testEnv(t, s)), "notes", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	waiting, release := pgtest.HoldWrites(t, testEnv(t, s).db, "revisions")
	defer release() // before closing the other store, whose held save holds its lock
	saved := make(chan error, 1)
	go func() {
		_, err := other.Save(ctx, "read.md", []byte("held\n"), doc.Revision, "test")
		saved <- err
	}()
	waiting(1)
	if history, err := s.History(ctx, "read.md"); err != nil || len(history) != 1 || history[0].Revision != doc.Revision {
		t.Fatal("note history:", history, err)
	}
	if history, err := s.HistoryScoped(ctx, "read.md", historyScope); err != nil || len(history) != 1 {
		t.Fatal("scoped history:", history, err)
	}
	if history, err := s.History(ctx, ""); err != nil || len(history) != 2 {
		t.Fatal("notebook history:", history, err)
	}
	if revisions, err := s.HistoryRevisions(ctx, doc.Revision, ""); err != nil || len(revisions) != 2 {
		t.Fatal("history revisions:", revisions, err)
	}
	if batch, err := s.HistoryNotes(ctx, doc.Revision, 0, nil); err != nil || len(batch.Files) != 1 || batch.Files[0].Content != "committed\n" {
		t.Fatal("history notes:", batch, err)
	}
	if history, err := s.TaskHistory(ctx, "t-aaaa1111"); err != nil || len(history) != 0 {
		t.Fatal("task history:", history, err)
	}
	release()
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
}

// Every history answers up to the head the store saw when it was asked, and
// an unknown or unrelated revision is refused as the Git backend refuses it.
func TestHistoryRevisionsChecksAncestry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first := historySave(t, ctx, s, "a.md", "one\n")
	second := historySave(t, ctx, s, "a.md", "two\n")
	third := historySave(t, ctx, s, "b.md", "three\n")
	all, err := s.HistoryRevisions(ctx, third, "")
	if err != nil || len(all) != 4 || all[0].Revision != third || all[1].Revision != second || all[2].Revision != first {
		t.Fatal("every revision:", all, err)
	}
	if after, err := s.HistoryRevisions(ctx, third, first); err != nil || len(after) != 2 || after[1].Revision != second {
		t.Fatal("revisions after a checkpoint:", after, err)
	}
	if none, err := s.HistoryRevisions(ctx, second, second); err != nil || none == nil || len(none) != 0 {
		t.Fatal("no revisions after the target itself:", none, err)
	}
	for _, c := range [][2]string{{first, third}, {strings.Repeat("1", 40), ""}, {third, strings.Repeat("1", 40)}} {
		if _, err := s.HistoryRevisions(ctx, c[0], c[1]); !errors.Is(err, ErrConflict) {
			t.Fatalf("revisions %v: %v", c, err)
		}
	}
	for _, c := range [][2]string{{"HEAD", ""}, {third, "main"}, {strings.ToUpper(third), ""}} {
		if _, err := s.HistoryRevisions(ctx, c[0], c[1]); !errors.Is(err, ErrForbidden) {
			t.Fatalf("revisions %v: %v", c, err)
		}
	}
	if _, err := s.HistoryNotes(ctx, strings.Repeat("1", 40), 0, nil); err == nil {
		t.Fatal("history notes of an unknown revision")
	}
	if _, err := s.HistoryNotes(ctx, "HEAD", 0, nil); !errors.Is(err, ErrForbidden) {
		t.Fatal("history notes of a ref:", err)
	}
}

// Budget: per-note history over 5,000 revisions, p95 < 50 ms. The note
// is renamed 20 revisions before the head, so its history follows the
// rename; all but one in a hundred of its versions are archived, so a
// scoped history reads all 5,000 to find 50 eligible ones; another note
// changes in one revision in 150.
func BenchmarkHistoryOfANoteWith5000Revisions(b *testing.B) {
	s := newTestStore(b)
	ctx := context.Background()
	author := gitobj.Signature{Name: "Ada Lovelace", Email: "ada@example.com"}
	path := "journal/daily.md"
	for i := range 5000 {
		subject := "Update " + path
		var files []notestore.FileChange
		if i == 4980 {
			subject = fmt.Sprintf("Move %q to %q", path, "journal/renamed.md")
			files = []notestore.FileChange{{Path: "journal/renamed.md", From: path}}
			path = "journal/renamed.md"
		} else {
			content := fmt.Sprintf("# Daily\n\nEntry %d\n", i)
			if i%100 != 0 {
				content = "---\nfmn-container: archive\n---\n" + content
			}
			files = []notestore.FileChange{{Path: path, Content: []byte(content)}}
		}
		if i%3 == 0 {
			files = append(files, notestore.FileChange{Path: fmt.Sprintf("other/%d.md", i%50), Content: []byte(fmt.Sprint(i))})
		}
		if i%100 == 7 {
			subject = fmt.Sprintf("Task t-bench%04d added by p-ada", i)
		}
		if _, err := s.notes.Apply(ctx, notestore.ChangeSet{Subject: subject, Body: "Gitnotes-Source: browser\nGitnotes-User: u-1", Author: author, Files: files}); err != nil {
			b.Fatal(err)
		}
	}
	status, err := s.Status(ctx)
	if err != nil {
		b.Fatal(err)
	}
	// Until Postgres has statistics on the new rows, as just after an
	// import, the notes store plans its path query as a scan of the
	// notebook's changes per revision.
	start := time.Now()
	if _, err := s.History(ctx, "other/7.md"); err != nil {
		b.Fatal(err)
	}
	b.Logf("history of a rarely changed note before ANALYZE: %v", time.Since(start))
	if _, err := testEnv(b, s).db.Pool.Exec(ctx, "ANALYZE revisions, revision_changes"); err != nil {
		b.Fatal(err)
	}
	cases := []struct {
		name string
		run  func() (int, error)
		want int
	}{
		{"note", func() (int, error) { h, err := s.History(ctx, "journal/renamed.md"); return len(h), err }, 50},
		{"scoped note", func() (int, error) {
			h, err := s.HistoryScoped(ctx, "journal/renamed.md", historyScope)
			return len(h), err
		}, 50},
		{"rarely changed note", func() (int, error) { h, err := s.History(ctx, "other/7.md"); return len(h), err }, 33},
		{"folder", func() (int, error) { h, err := s.History(ctx, "journal"); return len(h), err }, 50},
		{"notebook", func() (int, error) { h, err := s.History(ctx, ""); return len(h), err }, 50},
		{"task", func() (int, error) { h, err := s.TaskHistory(ctx, "t-bench2507"); return len(h), err }, 1},
		{"revisions", func() (int, error) { r, err := s.HistoryRevisions(ctx, status.Revision, ""); return len(r), err }, 5001},
		{"notes of a revision", func() (int, error) { n, err := s.HistoryNotes(ctx, status.Revision, 0, nil); return len(n.Files), err }, 1},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			var latency []time.Duration
			for b.Loop() {
				start := time.Now()
				n, err := c.run()
				latency = append(latency, time.Since(start))
				if err != nil || n != c.want {
					b.Fatal(n, err)
				}
			}
			slices.Sort(latency)
			at := func(p float64) float64 {
				return float64(latency[min(len(latency)-1, int(p*float64(len(latency))))]) / float64(time.Millisecond)
			}
			b.ReportMetric(at(0.50), "p50-ms")
			b.ReportMetric(at(0.95), "p95-ms")
		})
	}
}
