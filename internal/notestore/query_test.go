package notestore_test

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// PathVersion names the commit "git log -1 rev -- path" names along the
// first-parent line, deletions and renames away included, for every path the
// history ever held, at every revision.
func TestPathVersionIsGitLogOfEachPath(t *testing.T) {
	e := newEnv(t, notestore.Options{ImportEntryID: declaredID})
	g := buildNotebookHistory(t)
	n := e.store.Notebook("imported")
	importAll(t, g, n)
	paths := map[string]bool{"never.md": true}
	for _, p := range strings.Fields(g.run(nil, "log", "--all", "--format=", "--name-only", "--no-renames")) {
		paths[p] = true
	}
	for _, rev := range strings.Fields(g.run(nil, "rev-list", "HEAD")) {
		for p := range paths {
			want := strings.TrimSpace(g.run(nil, "log", "-1", "--first-parent", "--format=%H", rev, "--", p))
			got, err := n.PathVersion(e.ctx, g.id(rev), p)
			if err != nil {
				t.Fatal(rev, p, err)
			}
			if (got == gitobj.ID{}) != (want == "") || want != "" && got.String() != want {
				t.Fatalf("%s at %s: %s, git says %q", p, rev, got, want)
			}
		}
	}
	if _, err := n.PathVersion(e.ctx, g.id(strings.Repeat("1", 40)), "book.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("an unknown revision:", err)
	}
}

func TestPathVersionCountsWritesRenamesAndDeletes(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	added := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "a")}})
	moved := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{{Path: "b.md", From: "a.md"}}})
	gone := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{{Path: "b.md"}}})
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("other.md", "o")}})
	for _, c := range []struct {
		rev  gitobj.ID
		path string
		want gitobj.ID
	}{
		{zero, "a.md", moved.Head.Commit},
		{zero, "b.md", gone.Head.Commit},
		{added.Head.Commit, "a.md", added.Head.Commit},
		{added.Head.Commit, "b.md", zero},
		{moved.Head.Commit, "b.md", moved.Head.Commit},
		{zero, "other.md", mustStat(t, n, "other.md").Version},
	} {
		if got, err := n.PathVersion(e.ctx, c.rev, c.path); err != nil || got != c.want {
			t.Errorf("%s at %s: %s, want %s (%v)", c.path, c.rev, got, c.want, err)
		}
	}
}

func TestCommitByOperationFindsWrittenAndImportedRevisions(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	r := apply(t, n, notestore.ChangeSet{OperationID: "op-1", RequestID: "req-1", Files: []notestore.FileChange{write("a.md", "a")}})
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "b")}})
	info, ok, err := n.CommitByOperation(e.ctx, "op-1")
	if err != nil || !ok || info.ID != r.Head.Commit || len(info.Changes) != 1 {
		t.Fatal("the operation's revision:", info.ID, ok, err)
	}
	if _, ok, err := n.CommitByOperation(e.ctx, "op-2"); ok || err != nil {
		t.Fatal("an unknown operation:", ok, err)
	}

	g := newGitRepo(t, false)
	g.commit(ada, "Initialize notes notebook", nil)
	g.write("a.md", "imported\n", 0o644)
	want := g.commit(ada, "Imported change", []gitobj.Trailer{{Key: "Gitnotes-Source", Value: "mcp"}, {Key: "Gitnotes-Operation", Value: "op-9"}, {Key: "Gitnotes-Request", Value: "req-9"}})
	imported := e.store.Notebook("imported")
	importAll(t, g, imported)
	if info, ok, err := imported.CommitByOperation(e.ctx, "op-9"); err != nil || !ok || info.ID != want {
		t.Fatal("an imported operation:", info.ID, ok, err)
	}
}

func TestRequestIDIsTheTrailerAndTheReplayKey(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	cs := notestore.ChangeSet{Subject: "Apply group", Author: ada, OperationID: "op-1", RequestID: "req-1", Trailers: []gitobj.Trailer{{Key: "Gitnotes-Source", Value: "mcp"}}, Files: []notestore.FileChange{write("a.md", "a")}}
	first := apply(t, n, cs)
	info, _ := n.Commit(e.ctx, first.Head.Commit)
	if want := "Apply group\n\nGitnotes-Source: mcp\nGitnotes-Operation: op-1\nGitnotes-Request: req-1\n"; info.Commit.Message != want {
		t.Fatalf("message %q, want %q", info.Commit.Message, want)
	}
	// A retry after another write is the same request, whatever its expectations.
	other := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("b.md", "b")}})
	cs.ExpectHead = &other.Head.Commit
	again := apply(t, n, cs)
	if !again.Replayed || again.Head != first.Head {
		t.Fatalf("a retry: %+v, first %+v", again, first)
	}
	cs.RequestID = "req-2"
	if _, err := n.Apply(e.ctx, cs); !errors.Is(err, notestore.ErrOperationMismatch) {
		t.Fatal("another request under the operation:", err)
	}

	// Trailers the caller placed stay where it put them.
	body := "Gitnotes-Source: collaboration\nGitnotes-Operation: op-3\nGitnotes-Request: req-3\nCollaboration-Receipt: {\"operation\":\"x\"}\n"
	placed := apply(t, n, notestore.ChangeSet{Subject: "Checkpoint live edits to a.md", Body: body, OperationID: "op-3", RequestID: "req-3", AllowEmpty: true})
	info, _ = n.Commit(e.ctx, placed.Head.Commit)
	if want := "Checkpoint live edits to a.md\n\n" + body; info.Commit.Message != want {
		t.Fatalf("message %q, want %q", info.Commit.Message, want)
	}
	for _, bad := range []notestore.ChangeSet{
		{Subject: "x", Body: body, OperationID: "op-3", RequestID: "other", AllowEmpty: true},
		{Subject: "x", RequestID: "req", AllowEmpty: true},
		{Subject: "x", OperationID: "op", RequestID: "req\nGitnotes-User: forged", AllowEmpty: true},
	} {
		bad.Author = ada
		if _, err := n.Apply(e.ctx, bad); !errors.Is(err, notestore.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestReceiptsListByKind(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	for _, r := range []notestore.Receipt{
		{Kind: "capture", Operation: "c1", RequestHash: "h1"},
		{Kind: "group", Operation: "g1", RequestHash: "h2"},
		{Kind: "capture", Operation: "c2", RequestHash: "h3", State: notestore.ReceiptDone, Result: []byte("{}")},
	} {
		if err := n.PutReceipt(e.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	e.store.Notebook("other").PutReceipt(e.ctx, notestore.Receipt{Kind: "capture", Operation: "c3", RequestHash: "h"})
	got, err := n.Receipts(e.ctx, "capture")
	if err != nil || len(got) != 2 || got[0].Operation != "c1" || got[1].Operation != "c2" || got[1].State != notestore.ReceiptDone || string(got[1].Result) != "{}" || got[1].Done.IsZero() || !got[0].Done.IsZero() {
		t.Fatalf("capture receipts: %+v %v", got, err)
	}
	if got, err := n.Receipts(e.ctx, "live"); err != nil || len(got) != 0 {
		t.Fatal("no receipts of a kind:", got, err)
	}
}

func TestHistoryOfAFolderTheNotebookAndAMessage(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	subjects := func(infos []notestore.CommitInfo, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, info := range infos {
			out = append(out, strings.SplitN(info.Commit.Message, "\n", 2)[0])
		}
		return strings.Join(out, ", ")
	}
	step := func(subject string, files ...notestore.FileChange) {
		apply(t, n, notestore.ChangeSet{Subject: subject, Files: files})
	}
	step("Task t-1: one a", write("one/a.md", "a"))
	step("Add one b", write("one/b.md", "b"))
	step("Task t-2: two", write("two/a.md", "a"), write("one.md", "file beside the folder"))
	step("Move out", notestore.FileChange{Path: "two/b.md", From: "one/b.md"})
	step("Edit online", write("online/a.md", "o"))
	step("Task t-1 again", write("two/a.md", "again"))
	for _, c := range []struct {
		q    notestore.HistoryQuery
		want string
	}{
		{notestore.HistoryQuery{Prefix: "one"}, "Move out, Add one b, Task t-1: one a"},
		{notestore.HistoryQuery{Prefix: "one/"}, "Move out, Add one b, Task t-1: one a"},
		{notestore.HistoryQuery{Prefix: "one/a.md"}, "Task t-1: one a"},
		{notestore.HistoryQuery{Prefix: "two", Limit: 2}, "Task t-1 again, Move out"},
		{notestore.HistoryQuery{}, "Task t-1 again, Edit online, Move out, Task t-2: two, Add one b, Task t-1: one a, Initialize notes notebook"},
		{notestore.HistoryQuery{Limit: 2, Before: 6}, "Move out, Task t-2: two"},
		{notestore.HistoryQuery{MessageContains: "Task t-1"}, "Task t-1 again, Task t-1: one a"},
		{notestore.HistoryQuery{MessageContains: "Task t-1", Prefix: "two"}, "Task t-1 again"},
		{notestore.HistoryQuery{MessageContains: "forgetmenote@localhost"}, ""},
	} {
		if got := subjects(n.History(e.ctx, c.q)); got != c.want {
			t.Errorf("%+v: %q, want %q", c.q, got, c.want)
		}
	}
	for _, bad := range []notestore.HistoryQuery{{Path: "one/a.md", Prefix: "one"}, {EntryID: "x", MessageContains: "Task"}, {MessageContains: "two\nlines"}} {
		if _, err := n.History(e.ctx, bad); !errors.Is(err, notestore.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestReadBlobsReadsOnlyTheNotebooksContent(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	large := strings.Repeat("A paragraph of a long note.\n\n", 2000)
	first := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "old"), write("big.md", large)}})
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "new")}})
	old, _ := n.Stat(e.ctx, first.Head.Commit, "a.md")
	blobs := []gitobj.ID{mustStat(t, n, "big.md").Blob, old.Blob, mustStat(t, n, "a.md").Blob}
	var got []string
	if err := n.ReadBlobs(e.ctx, blobs, func(i int, content []byte) error {
		got = append(got, string(content))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{large, "old", "new"}) {
		t.Fatal("contents:", len(got))
	}
	other := e.create("other")
	apply(t, other, notestore.ChangeSet{Files: []notestore.FileChange{write("secret.md", "secret")}})
	secret := gitobj.BlobID([]byte("secret"))
	called := false
	if err := n.ReadBlobs(e.ctx, []gitobj.ID{old.Blob, secret}, func(int, []byte) error { called = true; return nil }); !errors.Is(err, fs.ErrNotExist) || called {
		t.Fatal("another notebook's content:", err, called)
	}
	stop := errors.New("stop")
	if err := n.ReadBlobs(e.ctx, blobs, func(int, []byte) error { return stop }); !errors.Is(err, stop) {
		t.Fatal("a consumer's error:", err)
	}
}
