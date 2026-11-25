package notestore_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type env struct {
	t     *testing.T
	ctx   context.Context
	db    *pg.DB
	blobs *blob.Dir
	store *notestore.Store
}

func newEnv(t *testing.T, o notestore.Options) *env {
	t.Helper()
	db := pgtest.Org(t)
	dir, err := blob.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dir.Close() })
	e := &env{t: t, ctx: context.Background(), db: db, blobs: dir}
	e.store = e.open(db, o)
	return e
}

func (e *env) open(db *pg.DB, o notestore.Options) *notestore.Store {
	e.t.Helper()
	s, err := notestore.Open(e.ctx, db, e.blobs, o)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(s.Close)
	return s
}

var (
	ada  = gitobj.Signature{Name: "Ada Author", Email: "ada@example.com", When: 1700000000, TZ: "+0100"}
	bob  = gitobj.Signature{Name: "Bob", Email: "bob@example.com", When: 1700000100, TZ: "-0500"}
	zero gitobj.ID
)

func (e *env) create(id string) *notestore.Notebook {
	e.t.Helper()
	if _, err := e.store.Create(e.ctx, id, ada); err != nil {
		e.t.Fatal(err)
	}
	return e.store.Notebook(id)
}

func apply(t *testing.T, n *notestore.Notebook, cs notestore.ChangeSet) notestore.CommitResult {
	t.Helper()
	if cs.Subject == "" {
		cs.Subject = "Change notes"
	}
	if cs.Author.Name == "" {
		cs.Author = ada
	}
	r, err := n.Apply(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func write(path, content string) notestore.FileChange {
	return notestore.FileChange{Path: path, Content: []byte(content)}
}

func read(t *testing.T, n *notestore.Notebook, rev gitobj.ID, path string) string {
	t.Helper()
	content, _, err := n.Read(context.Background(), rev, path)
	if err != nil {
		t.Fatal(path, err)
	}
	return string(content)
}

func paths(files []notestore.File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// gitRepo is a working tree Git commits the same changes into, so a test can
// compare the commit IDs the store computes with Git's own.
type gitRepo struct {
	t   *testing.T
	dir string
	env []string
}

func newGitRepo(t *testing.T, bare bool) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("the git executable is required:", err)
	}
	g := &gitRepo{t: t, dir: t.TempDir(), env: append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C")}
	args := []string{"init", "-q", "--initial-branch=main"}
	if bare {
		args = append(args, "--bare")
	}
	g.run(nil, args...)
	return g
}

func (g *gitRepo) run(stdin []byte, args ...string) string {
	g.t.Helper()
	return g.runEnv(nil, stdin, args...)
}

func (g *gitRepo) runEnv(env []string, stdin []byte, args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = g.dir, append(slices.Clone(g.env), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

func (g *gitRepo) id(s string) gitobj.ID {
	g.t.Helper()
	id, err := gitobj.ParseID(strings.TrimSpace(s))
	if err != nil {
		g.t.Fatal(err)
	}
	return id
}

// commit commits the working tree as author with Git's own message handling.
func (g *gitRepo) commit(author gitobj.Signature, subject string, trailers []gitobj.Trailer) gitobj.ID {
	g.t.Helper()
	date := func(s gitobj.Signature) string { return strconv.FormatInt(s.When, 10) + " " + s.TZ }
	env := []string{"GIT_AUTHOR_NAME=" + author.Name, "GIT_AUTHOR_EMAIL=" + author.Email, "GIT_AUTHOR_DATE=" + date(author),
		"GIT_COMMITTER_NAME=ForgetMeNote", "GIT_COMMITTER_EMAIL=forgetmenote@localhost", "GIT_COMMITTER_DATE=" + date(author)}
	g.runEnv(env, nil, "add", "-A")
	args := []string{"commit", "-q", "--allow-empty", "-m", subject}
	for _, tr := range trailers {
		args = append(args, "--trailer", tr.Key+": "+tr.Value)
	}
	g.runEnv(env, nil, args...)
	return g.id(g.run(nil, "rev-parse", "HEAD"))
}

func (g *gitRepo) write(path, content string, mode os.FileMode) {
	g.t.Helper()
	full := filepath.Join(g.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		g.t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		g.t.Fatal(err)
	}
}

func TestCreateWritesTheCommitGitWould(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	head, err := e.store.Create(e.ctx, "notes", ada)
	if err != nil {
		t.Fatal(err)
	}
	g := newGitRepo(t, false)
	want := g.commit(ada, "Initialize notes notebook", nil)
	if head.Seq != 1 || head.Commit != want {
		t.Fatalf("head %+v, git commit %s", head, want)
	}
	n := e.store.Notebook("notes")
	info, err := n.Commit(e.ctx, head.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if info.Seq != 1 || info.Commit.Tree != gitobj.EmptyTree || info.Commit.Committer.Name != "ForgetMeNote" || len(info.Changes) != 0 {
		t.Fatalf("initial commit: %+v", info)
	}
	if files, err := n.Files(e.ctx, zero); err != nil || len(files) != 0 {
		t.Fatal("files of a new notebook:", files, err)
	}
	if _, err := e.store.Create(e.ctx, "notes", ada); !errors.Is(err, fs.ErrExist) {
		t.Fatal("creating an existing notebook:", err)
	}
	if _, err := e.store.Create(e.ctx, "Not/Valid", ada); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("invalid notebook ID:", err)
	}
	if _, err := e.store.Notebook("missing").Head(e.ctx); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("head of a missing notebook:", err)
	}
	if _, err := e.store.Notebook("missing").Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, Files: []notestore.FileChange{write("a.md", "a")}}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("writing a missing notebook:", err)
	}
}

func TestApplyCommitsWhatGitCommits(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	g := newGitRepo(t, false)
	g.commit(ada, "Initialize notes notebook", nil)
	big := strings.Repeat("A paragraph of a long note, with some words in it.\n\n", 400)
	binary := bytes.Repeat([]byte{0, 1, 2, 3, 0xff}, 3000)
	trailers := []gitobj.Trailer{{Key: "Gitnotes-Source", Value: "browser"}, {Key: "Gitnotes-User", Value: "u1"}, {Key: "Gitnotes-Client", Value: "web"}}
	steps := []struct {
		subject string
		author  gitobj.Signature
		changes []notestore.FileChange
		git     func()
	}{
		{"Add notes", ada, []notestore.FileChange{
			write("Inbox/first.md", "# First\n\nhello\n"),
			write("AGENTS.md", "Be kind.\n"),
			write("notes/long/manuscript.md", big),
			{Path: "assets/data.bin", Content: binary},
			{Path: "tools/run.sh", Content: []byte("#!/bin/sh\necho hi\n"), Mode: gitobj.ModeExecutable},
			{Path: "link", Content: []byte("Inbox/first.md"), Mode: gitobj.ModeSymlink},
		}, func() {
			g.write("Inbox/first.md", "# First\n\nhello\n", 0o644)
			g.write("AGENTS.md", "Be kind.\n", 0o644)
			g.write("notes/long/manuscript.md", big, 0o644)
			g.write("assets/data.bin", string(binary), 0o644)
			g.write("tools/run.sh", "#!/bin/sh\necho hi\n", 0o755)
			os.Symlink("Inbox/first.md", filepath.Join(g.dir, "link"))
		}},
		{"Edit and move", bob, []notestore.FileChange{
			write("Inbox/first.md", "# First\n\nhello again\n"),
			{Path: "notes/first.md", From: "Inbox/first.md"},
			write("notes/long/manuscript.md", strings.Replace(big, "some words", "other words", 1)),
		}, func() {
			os.Remove(filepath.Join(g.dir, "Inbox/first.md"))
			g.write("notes/first.md", "# First\n\nhello again\n", 0o644)
			g.write("notes/long/manuscript.md", strings.Replace(big, "some words", "other words", 1), 0o644)
		}},
		{"Delete the folder's last file", ada, []notestore.FileChange{
			{Path: "tools/run.sh"},
			{Path: "link"},
			write("tools", "now a file\n"),
		}, func() {
			os.RemoveAll(filepath.Join(g.dir, "tools"))
			os.Remove(filepath.Join(g.dir, "link"))
			g.write("tools", "now a file\n", 0o644)
		}},
	}
	for i, step := range steps {
		r := apply(t, n, notestore.ChangeSet{Subject: step.subject, Author: step.author, Trailers: trailers, Files: step.changes})
		step.git()
		want := g.commit(step.author, step.subject, trailers)
		if r.Head.Commit != want || r.Head.Seq != int64(i+2) {
			t.Fatalf("step %d: head %+v, git %s\n%s", i, r.Head, want, g.run(nil, "cat-file", "-p", "HEAD^{tree}"))
		}
	}
	files, err := n.Files(e.ctx, zero)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(files), " "); got != "AGENTS.md assets/data.bin notes/first.md notes/long/manuscript.md tools" {
		t.Fatalf("files: %s", got)
	}
	if got := read(t, n, zero, "notes/first.md"); got != "# First\n\nhello again\n" {
		t.Fatalf("moved note: %q", got)
	}
	if got := read(t, n, zero, "notes/long/manuscript.md"); got != strings.Replace(big, "some words", "other words", 1) {
		t.Fatal("manuscript content differs")
	}
	content, f, err := n.Read(e.ctx, zero, "assets/data.bin")
	if err != nil || !bytes.Equal(content, binary) || f.Size != int64(len(binary)) || f.Blob != gitobj.BlobID(binary) {
		t.Fatal("binary:", f, err)
	}
	head, _ := n.Head(e.ctx)
	info, err := n.Commit(e.ctx, head.Commit)
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, c := range info.Changes {
		ops = append(ops, c.Op+":"+c.Path)
	}
	if got := strings.Join(ops, " "); got != "delete:link add:tools delete:tools/run.sh" {
		t.Fatalf("changes: %s", got)
	}
	if len(info.Trailers) != 3 || info.Trailers[0] != trailers[0] {
		t.Fatalf("trailers: %v", info.Trailers)
	}
	// The objects export as a repository Git accepts with the same history.
	bare := newGitRepo(t, true)
	if err := n.Objects(e.ctx, 0, func(kind string, id gitobj.ID, body []byte) error {
		got, err := gitobj.WriteLoose(bare.dir, kind, body)
		if err == nil && got != id {
			t.Fatalf("%s %s hashed to %s", kind, id, got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	bare.run(nil, "update-ref", "refs/heads/main", head.Commit.String())
	bare.run(nil, "fsck", "--strict", "--no-dangling")
	if got, want := bare.run(nil, "log", "--format=%H %s"), g.run(nil, "log", "--format=%H %s"); got != want {
		t.Fatalf("exported log:\n%s\ngit log:\n%s", got, want)
	}
}

func TestExpectationsGuardEveryWrite(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	first := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "one")}})
	v := first.Versions["a.md"]
	if v != first.Head.Commit {
		t.Fatalf("version %s, head %s", v, first.Head.Commit)
	}
	stale := gitobj.BlobID([]byte("stale"))
	for name, cs := range map[string]notestore.ChangeSet{
		"head":    {ExpectHead: &stale, Files: []notestore.FileChange{write("a.md", "two")}},
		"version": {ExpectVersion: map[string]gitobj.ID{"a.md": stale}, Files: []notestore.FileChange{write("a.md", "two")}},
		"absent":  {ExpectVersion: map[string]gitobj.ID{"a.md": {}}, Files: []notestore.FileChange{write("a.md", "two")}},
		"exists":  {Files: []notestore.FileChange{write("b.md", "b"), {Path: "b.md", From: "a.md"}}},
	} {
		cs.Subject, cs.Author = "x", ada
		_, err := n.Apply(e.ctx, cs)
		var conflict *notestore.ConflictError
		if !errors.Is(err, notestore.ErrConflict) || !errors.As(err, &conflict) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if h, _ := n.Head(e.ctx); h != first.Head {
		t.Fatal("a refused change moved the head")
	}
	second := apply(t, n, notestore.ChangeSet{ExpectHead: &first.Head.Commit, ExpectVersion: map[string]gitobj.ID{"a.md": v, "new.md": {}},
		Files: []notestore.FileChange{write("a.md", "two"), write("new.md", "new")}})
	if second.Versions["a.md"] != second.Head.Commit || second.Versions["new.md"] != second.Head.Commit {
		t.Fatal("versions:", second.Versions)
	}
	for name, cs := range map[string]notestore.ChangeSet{
		"dotgit":     {Files: []notestore.FileChange{write(".git/config", "x")}},
		"dotdot":     {Files: []notestore.FileChange{write("a/../b.md", "x")}},
		"slash":      {Files: []notestore.FileChange{write("/abs.md", "x")}},
		"empty":      {Files: []notestore.FileChange{write("a//b.md", "x")}},
		"mode":       {Files: []notestore.FileChange{{Path: "m", Content: []byte("x"), Mode: gitobj.ModeGitlink}}},
		"entry":      {Files: []notestore.FileChange{{Path: "e.md", Content: []byte("x"), EntryID: "bad id"}}},
		"no subject": {Subject: " ", Files: []notestore.FileChange{write("c.md", "x")}},
	} {
		if cs.Subject == "" {
			cs.Subject = "x"
		}
		cs.Author = ada
		if _, err := n.Apply(e.ctx, cs); !errors.Is(err, notestore.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, Files: []notestore.FileChange{{Path: "missing.md"}}}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("deleting a missing file:", err)
	}
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, Files: []notestore.FileChange{write("a.md/inner.md", "x")}}); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a file under a file:", err)
	}
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("dir/x.md", "x")}})
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, Files: []notestore.FileChange{write("dir", "x")}}); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a file over a folder:", err)
	}
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, Files: []notestore.FileChange{{Path: "taken.md", Content: []byte("x"), EntryID: mustEntry(t, n, "a.md")}}}); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("reusing a live entry ID:", err)
	}
}

func mustEntry(t *testing.T, n *notestore.Notebook, path string) string {
	t.Helper()
	f, err := n.Stat(context.Background(), zero, path)
	if err != nil {
		t.Fatal(err)
	}
	return f.EntryID
}

func TestEmptyChangeSetsCommitOnlyWhenAllowed(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	first := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "same")}})
	again := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "same")}})
	if again.Head != first.Head || again.Versions["a.md"] != first.Head.Commit {
		t.Fatalf("an unchanged write committed: %+v", again)
	}
	empty := apply(t, n, notestore.ChangeSet{AllowEmpty: true, Subject: "Checkpoint"})
	if empty.Head.Seq != first.Head.Seq+1 {
		t.Fatal("an allowed empty commit was not made:", empty.Head)
	}
	info, _ := n.Commit(e.ctx, empty.Head.Commit)
	parent, _ := n.Commit(e.ctx, first.Head.Commit)
	if info.Commit.Tree != parent.Commit.Tree || len(info.Changes) != 0 || info.Commit.Parents[0] != first.Head.Commit {
		t.Fatalf("empty commit: %+v", info)
	}
}

func TestOperationIDReturnsTheFirstResult(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	cs := notestore.ChangeSet{Subject: "Capture", Author: ada, OperationID: "op-1", Files: []notestore.FileChange{write("a.md", "a")}}
	first := apply(t, n, cs)
	cs.Author.When++ // a retry is signed later; it is still the same request
	again := apply(t, n, cs)
	if !again.Replayed || again.Head != first.Head || again.Versions["a.md"] != first.Versions["a.md"] {
		t.Fatalf("replay: %+v, first %+v", again, first)
	}
	if h, _ := n.Head(e.ctx); h != first.Head {
		t.Fatal("a replay committed")
	}
	cs.Files = []notestore.FileChange{write("a.md", "different")}
	if _, err := n.Apply(e.ctx, cs); !errors.Is(err, notestore.ErrOperationMismatch) {
		t.Fatal("a reused operation ID:", err)
	}
	r, ok, err := n.Receipt(e.ctx, "commit", "op-1")
	if err != nil || !ok || r.State != notestore.ReceiptDone || r.Done.IsZero() {
		t.Fatal("commit receipt:", r, ok, err)
	}
}

// An operation that changed nothing is answered the same way when it is
// retried, even after another write changed what it wrote.
func TestOperationIDReturnsTheFirstResultOfANoOp(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	before := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "one")}})
	cs := notestore.ChangeSet{Subject: "Capture", Author: ada, OperationID: "op-1", Files: []notestore.FileChange{write("a.md", "one")}}
	first := apply(t, n, cs)
	if first.Replayed || first.Head != before.Head {
		t.Fatal("a change set that changes nothing:", first)
	}
	later := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "two")}})
	again := apply(t, n, cs)
	if !again.Replayed || again.Head != first.Head || again.Versions["a.md"] != before.Versions["a.md"] {
		t.Fatalf("replay: %+v, first %+v", again, first)
	}
	if h, _ := n.Head(e.ctx); h != later.Head || read(t, n, zero, "a.md") != "two" {
		t.Fatal("a replay of a no-op committed")
	}
}

func TestRenamesKeepTheEntryAndHistoryFollowsIt(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	const id = "6d6f62792d6469636b2d6e6f76656c31"
	r1 := apply(t, n, notestore.ChangeSet{Subject: "Add", Files: []notestore.FileChange{{Path: "a.md", Content: []byte("v1"), EntryID: id}, write("other.md", "o")}})
	r2 := apply(t, n, notestore.ChangeSet{Subject: "Edit", Files: []notestore.FileChange{write("a.md", "v2")}})
	apply(t, n, notestore.ChangeSet{Subject: "Unrelated", Files: []notestore.FileChange{write("other.md", "o2")}})
	r4 := apply(t, n, notestore.ChangeSet{Subject: "Move", Files: []notestore.FileChange{{Path: "dir/b.md", From: "a.md"}}})
	r5 := apply(t, n, notestore.ChangeSet{Subject: "Edit moved", Files: []notestore.FileChange{write("dir/b.md", "v3")}})
	r6 := apply(t, n, notestore.ChangeSet{Subject: "New note where the old one was", Files: []notestore.FileChange{write("a.md", "unrelated")}})
	f, err := n.Stat(e.ctx, zero, "dir/b.md")
	if err != nil || f.EntryID != id || f.Version != r5.Head.Commit {
		t.Fatal("moved file:", f, err)
	}
	want := []gitobj.ID{r5.Head.Commit, r4.Head.Commit, r2.Head.Commit, r1.Head.Commit}
	for name, q := range map[string]notestore.HistoryQuery{"entry": {EntryID: id}, "path": {Path: "dir/b.md"}} {
		history, err := n.History(e.ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var got []gitobj.ID
		for _, c := range history {
			got = append(got, c.ID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s history %v, want %v", name, got, want)
		}
	}
	if h, _ := n.History(e.ctx, notestore.HistoryQuery{EntryID: id, Limit: 2, Before: r5.Head.Seq}); len(h) != 2 || h[0].ID != r4.Head.Commit || h[1].ID != r2.Head.Commit {
		t.Fatal("history page:", h)
	}
	if h, _ := n.History(e.ctx, notestore.HistoryQuery{Path: "a.md"}); len(h) != 1 || h[0].ID != r6.Head.Commit {
		t.Fatal("the new note at the old path has its own history:", h)
	}
	if h, _ := n.History(e.ctx, notestore.HistoryQuery{Path: "a.md", Before: r6.Head.Seq}); len(h) != 4 || h[0].ID != r5.Head.Commit {
		t.Fatal("before it, the path's last entry is the moved note:", len(h))
	}
	move, _ := n.Commit(e.ctx, r4.Head.Commit)
	if len(move.Changes) != 1 || move.Changes[0] != (notestore.Change{Op: "rename", Path: "dir/b.md", OldPath: "a.md", EntryID: id, Blob: gitobj.BlobID([]byte("v2")), Mode: gitobj.ModeFile, Size: 2}) {
		t.Fatalf("rename change: %+v", move.Changes)
	}
	// A swap of two paths keeps both entries.
	other := mustEntry(t, n, "other.md")
	apply(t, n, notestore.ChangeSet{Subject: "Swap", Files: []notestore.FileChange{{Path: "tmp.md", From: "other.md"}, {Path: "other.md", From: "dir/b.md"}, {Path: "dir/b.md", From: "tmp.md"}}})
	if mustEntry(t, n, "other.md") != id || mustEntry(t, n, "dir/b.md") != other {
		t.Fatal("swap lost an entry")
	}
}

func TestHistoricalReadsSeeEachRevision(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	r1 := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "a1"), write("b/c.md", "c1"), write("gone.md", "g")}})
	r2 := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "a2"), {Path: "gone.md"}, {Path: "b/d.md", From: "b/c.md"}, write("new.md", "n")}})
	files, err := n.Files(e.ctx, r1.Head.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(files), " "); got != "a.md b/c.md gone.md" {
		t.Fatal("files at r1:", got)
	}
	for _, f := range files {
		if f.Version != r1.Head.Commit || f.EntryID == "" || f.Size != int64(len(read(t, n, r1.Head.Commit, f.Path))) {
			t.Fatalf("file at r1: %+v", f)
		}
	}
	if got := read(t, n, r1.Head.Commit, "a.md"); got != "a1" {
		t.Fatal("a.md at r1:", got)
	}
	if _, err := n.Stat(e.ctx, r1.Head.Commit, "new.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a later file at r1:", err)
	}
	if _, err := n.Stat(e.ctx, r1.Head.Commit, "b"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a folder is not a file:", err)
	}
	if _, err := n.Files(e.ctx, gitobj.BlobID([]byte("nope"))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("an unknown revision:", err)
	}
	moved := mustEntry(t, n, "b/d.md")
	changes, err := n.Changes(e.ctx, r1.Head.Commit, zero)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range changes {
		got = append(got, c.Op+":"+c.OldPath+">"+c.Path)
		if c.Path == "b/d.md" && c.EntryID != moved {
			t.Fatal("rename lost its entry")
		}
	}
	if strings.Join(got, " ") != "modify:>a.md rename:b/c.md>b/d.md delete:>gone.md add:>new.md" {
		t.Fatalf("changes: %v", got)
	}
	all, _ := n.Changes(e.ctx, zero, r2.Head.Commit)
	if len(all) != 3 || all[0].Op != "add" || all[1].Path != "b/d.md" {
		t.Fatalf("changes since the start: %+v", all)
	}
	revs, err := n.Revisions(e.ctx, 1, 10)
	if err != nil || len(revs) != 2 || revs[0].ID != r1.Head.Commit || revs[1].ID != r2.Head.Commit || len(revs[1].Changes) != 4 {
		t.Fatal("revisions:", revs, err)
	}
	if blob, err := n.ReadBlob(e.ctx, gitobj.BlobID([]byte("g"))); err != nil || string(blob) != "g" {
		t.Fatal("a deleted file's blob:", blob, err)
	}
	other := e.create("other")
	if _, err := other.ReadBlob(e.ctx, gitobj.BlobID([]byte("g"))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("another notebook's blob was readable:", err)
	}
}

func TestReceiptsAreBoundToTheirRequestAndSwept(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	if _, ok, err := n.Receipt(e.ctx, "capture", "c1"); ok || err != nil {
		t.Fatal("missing receipt:", ok, err)
	}
	if err := n.PutReceipt(e.ctx, notestore.Receipt{Kind: "capture", Operation: "c1", RequestHash: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := n.PutReceipt(e.ctx, notestore.Receipt{Kind: "capture", Operation: "c1", RequestHash: "h1", State: notestore.ReceiptDone, Result: []byte(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	r, ok, err := n.Receipt(e.ctx, "capture", "c1")
	if err != nil || !ok || r.State != "done" || string(r.Result) != `{"ok":true}` || r.Done.IsZero() {
		t.Fatal("receipt:", r, ok, err)
	}
	if err := n.PutReceipt(e.ctx, notestore.Receipt{Kind: "capture", Operation: "c1", RequestHash: "h2"}); !errors.Is(err, notestore.ErrOperationMismatch) {
		t.Fatal("another request:", err)
	}
	if err := n.PutReceipt(e.ctx, notestore.Receipt{Kind: "capture", Operation: "c2", RequestHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if swept, err := e.store.SweepReceipts(e.ctx, time.Now()); err != nil || swept != 0 {
		t.Fatal("fresh receipts swept:", swept, err)
	}
	if swept, err := e.store.SweepReceipts(e.ctx, time.Now().Add(notestore.ReceiptTTL+time.Hour)); err != nil || swept != 1 {
		t.Fatal("expired receipts:", swept, err)
	}
	if _, ok, _ := n.Receipt(e.ctx, "capture", "c2"); !ok {
		t.Fatal("a pending receipt was swept")
	}
}

func TestLiveDocumentsKeepUpdatesPerEpoch(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	const entry = "0123456789abcdef0123456789abcdef"
	if st, err := n.LiveState(e.ctx, entry); err != nil || st.Epoch != 0 || st.Updates != nil {
		t.Fatal("new live document:", st, err)
	}
	for i, u := range []string{"u1", "u2", "u3"} {
		seq, err := n.LiveAppend(e.ctx, entry, 0, []byte(u), "alice")
		if err != nil || seq != int64(i+1) {
			t.Fatal("append:", seq, err)
		}
	}
	base := gitobj.BlobID([]byte("base"))
	if err := n.LiveCompact(e.ctx, entry, 0, []byte("snap12"), 2, base); err != nil {
		t.Fatal(err)
	}
	st, err := n.LiveState(e.ctx, entry)
	if err != nil || string(st.Snapshot) != "snap12" || st.SnapshotSeq != 2 || st.Base != base || len(st.Updates) != 1 || string(st.Updates[0].Update) != "u3" || st.Updates[0].Actor != "alice" {
		t.Fatalf("compacted state: %+v %v", st, err)
	}
	if err := n.LiveCompact(e.ctx, entry, 0, nil, 9, base); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("compacting past the log:", err)
	}
	epoch, err := n.LiveReset(e.ctx, entry)
	if err != nil || epoch != 1 {
		t.Fatal("reset:", epoch, err)
	}
	if _, err := n.LiveAppend(e.ctx, entry, 0, []byte("late"), "bob"); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("an update for the old epoch:", err)
	}
	if err := n.LiveCompact(e.ctx, entry, 0, nil, 0, zero); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("compacting the old epoch:", err)
	}
	seq, err := n.LiveAppend(e.ctx, entry, 1, []byte("fresh"), "bob")
	if err != nil {
		t.Fatal(err)
	}
	st, _ = n.LiveState(e.ctx, entry)
	if st.Epoch != 1 || st.Snapshot != nil || !st.Base.IsZero() || len(st.Updates) != 1 || st.Updates[0].Seq != seq || string(st.Updates[0].Update) != "fresh" {
		t.Fatalf("after reset: %+v", st)
	}
	if _, err := n.LiveAppend(e.ctx, "other0123456789abcdef0123456789a", 3, []byte("x"), "a"); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("an update for a document with no such epoch:", err)
	}
}
