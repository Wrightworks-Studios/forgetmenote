package notestore_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
)

const leakID = "1eaf1eaf1eaf1eaf1eaf1eaf1eaf1eaf"

func newSecret(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 12)
	rand.Read(raw)
	return "sk-live-" + hex.EncodeToString(raw)
}

func leakNote(body string) string {
	return "---\nfmn-id: " + leakID + "\ntitle: Keys\n---\n\n" + body
}

// leakyHistory writes a history with secret in a note that is edited,
// renamed and kept, and in a large manuscript that later drops it again.
// It returns every revision's commit, oldest first.
func leakyHistory(t *testing.T, n *notestore.Notebook, secret string) []gitobj.ID {
	t.Helper()
	book := manuscript(300)
	leaked := strings.Replace(book, "Paragraph 150.", "Paragraph 150. The vault code is "+secret+".", 1)
	head, err := n.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	commits := []gitobj.ID{head.Commit}
	sig := ada
	step := func(cs notestore.ChangeSet) {
		t.Helper()
		sig.When += 60
		cs.Author = sig
		cs.Trailers = []gitobj.Trailer{{Key: "Gitnotes-Source", Value: "mcp"}, {Key: "Gitnotes-User", Value: "ada"}}
		commits = append(commits, apply(t, n, cs).Head.Commit)
	}
	step(notestore.ChangeSet{Subject: "Start", Files: []notestore.FileChange{write("keep.md", "Unrelated.\n"), write("big.md", book)}})
	step(notestore.ChangeSet{Subject: "Leak", Files: []notestore.FileChange{{Path: "Inbox/leak.md", Content: []byte(leakNote("The key is " + secret + ".\n")), EntryID: leakID}, write("big.md", leaked)}})
	step(notestore.ChangeSet{Subject: "Edit the leak", Files: []notestore.FileChange{write("Inbox/leak.md", leakNote("The key is "+secret+", still.\n"))}})
	step(notestore.ChangeSet{Subject: `Move "Inbox/leak.md" to "Projects/leak.md"`, Files: []notestore.FileChange{{Path: "Projects/leak.md", From: "Inbox/leak.md",
		Content: []byte(leakNote("The key is " + secret + ", moved.\n"))}}})
	step(notestore.ChangeSet{Subject: "Unrelated edit", Files: []notestore.FileChange{write("keep.md", "Unrelated, edited.\n")}})
	step(notestore.ChangeSet{Subject: "Drop the paragraph", Files: []notestore.FileChange{write("big.md", strings.Replace(book, "Paragraph 150.", "Paragraph 150, rewritten.", 1))}})
	step(notestore.ChangeSet{Subject: "Nothing", AllowEmpty: true})
	return commits
}

// contents returns every file of every revision by commit and path.
func contents(t *testing.T, n *notestore.Notebook, commits []gitobj.ID) map[gitobj.ID]map[string]string {
	t.Helper()
	out := map[gitobj.ID]map[string]string{}
	for _, c := range commits {
		files, err := n.Files(t.Context(), c)
		if err != nil {
			t.Fatal(err)
		}
		out[c] = map[string]string{}
		for _, f := range files {
			out[c][f.Path] = read(t, n, c, f.Path)
		}
	}
	return out
}

func redact(secret string) notestore.RewriteFunc {
	return func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
		data, err := content()
		if err != nil || !bytes.Contains(data, []byte(secret)) {
			return nil, true, err
		}
		return bytes.ReplaceAll(data, []byte(secret), []byte("[purged]")), true, nil
	}
}

func purgeOptions() notestore.RewriteOptions {
	return notestore.RewriteOptions{Actor: "owner", Reason: "a leaked key", Scope: map[string]any{"kind": "text", "notes": 2}}
}

// orgBytes is every byte of every row db's org has in any table, as a
// binary COPY gives them: text and bytea columns exactly as stored.
func orgBytes(t *testing.T, db *pg.DB) []byte {
	t.Helper()
	conn, err := db.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	rows, err := conn.Query(t.Context(), `SELECT table_name FROM information_schema.columns WHERE table_schema = 'public' AND column_name = 'org' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		tables = append(tables, name)
	}
	rows.Close()
	if len(tables) < 10 {
		t.Fatal("tables:", tables)
	}
	var out bytes.Buffer
	for _, table := range tables {
		if _, err := conn.Conn().PgConn().CopyTo(t.Context(), &out, `COPY (SELECT * FROM "`+table+`" WHERE org = '`+db.Org+`') TO STDOUT WITH (FORMAT binary)`); err != nil {
			t.Fatal(table, err)
		}
	}
	return out.Bytes()
}

// fileBytes is every byte of every file under dir. A block sweep may run
// meanwhile: a file or folder it removes after the walk listed it holds
// nothing any more, which is no error.
func fileBytes(t *testing.T, dir string) []byte {
	t.Helper()
	var out bytes.Buffer
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		out.Write(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// export writes every object of n into a new bare repository and points main
// at the head.
func export(t *testing.T, n *notestore.Notebook) *gitRepo {
	t.Helper()
	g := newGitRepo(t, true)
	if err := n.Objects(t.Context(), 0, func(kind string, id gitobj.ID, body []byte) error {
		got, err := gitobj.WriteLoose(g.dir, kind, body)
		if err == nil && got != id {
			t.Fatalf("%s %s hashes to %s", kind, id, got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	head, err := n.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	g.run(nil, "update-ref", "refs/heads/main", head.Commit.String())
	g.run(nil, "fsck", "--strict", "--no-dangling")
	return g
}

// A purge's rewrite replaces content in every revision from the first it
// names: each rewritten commit keeps its author, committer, times and
// message, gains the purge trailer and a new ID, and every old ID maps to its
// replacement. Earlier revisions keep their IDs; nothing stored keeps the
// content, while another file's content shared with the old versions stays.
func TestRewriteReplacesContentInEveryLaterRevision(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	secret := newSecret(t)
	commits := leakyHistory(t, n, secret)
	before := contents(t, n, commits)
	oldInfos := map[gitobj.ID]notestore.CommitInfo{}
	for _, c := range commits {
		info, err := n.Commit(e.ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		oldInfos[c] = info
	}
	head := commits[len(commits)-1]
	if !bytes.Contains(orgBytes(t, e.db), []byte(secret)) || !bytes.Contains(fileBytes(t, e.blobs.Path()), []byte(secret)) {
		t.Fatal("the fixture does not store the secret in rows and blocks")
	}

	result, err := n.Rewrite(e.ctx, 3, "purge-1", redact(secret), notestore.RewriteOptions{ExpectHead: &head, Actor: "owner", Reason: "a leaked key", Scope: map[string]any{"kind": "text"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Head.Seq != int64(len(commits)) || len(result.Rewritten) != len(commits)-2 || result.FromSeq != 3 {
		t.Fatalf("result %+v", result)
	}
	if got, _ := n.Head(e.ctx); got != result.Head || result.Head.Commit != result.Rewritten[head] {
		t.Fatal("head:", got, result.Head)
	}
	var after []gitobj.ID
	for i, c := range commits {
		if i < 2 {
			if _, ok := result.Rewritten[c]; ok {
				t.Fatal("a revision before the purge was rewritten")
			}
			after = append(after, c)
			continue
		}
		next := result.Rewritten[c]
		after = append(after, next)
		if _, err := n.Commit(e.ctx, c); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("an old commit is still a revision:", err)
		}
		if rec, ok, err := n.Rewritten(e.ctx, c); err != nil || !ok || rec.Commit != next || !slices.Equal(rec.Purges, []string{"purge-1"}) {
			t.Fatal("mapping:", rec, ok, err)
		}
		if old, ok, err := n.Original(e.ctx, next, "purge-1"); err != nil || !ok || old != c {
			t.Fatal("original:", old, ok, err)
		}
		info, err := n.Commit(e.ctx, next)
		if err != nil {
			t.Fatal(err)
		}
		was := oldInfos[c]
		if info.Seq != was.Seq || info.Commit.Author != was.Commit.Author || info.Commit.Committer != was.Commit.Committer {
			t.Fatalf("revision %d: %+v, was %+v", was.Seq, info.Commit, was.Commit)
		}
		if want := gitobj.CleanMessage(was.Commit.Message, "", []gitobj.Trailer{{Key: notestore.PurgeTrailer, Value: "purge-1"}}); info.Commit.Message != want {
			t.Fatalf("message %q, want %q", info.Commit.Message, want)
		}
		if len(info.Commit.Parents) != 1 || info.Commit.Parents[0] != after[i-1] {
			t.Fatal("parents:", info.Commit.Parents)
		}
	}
	if _, ok, _ := n.Rewritten(e.ctx, commits[1]); ok {
		t.Fatal("an unrewritten commit maps")
	}
	cold := e.open(e.db, notestore.Options{}).Notebook("notes")
	for i, c := range after {
		got := contents(t, cold, []gitobj.ID{c})[c]
		want := map[string]string{}
		for p, content := range before[commits[i]] {
			want[p] = strings.ReplaceAll(content, secret, "[purged]")
		}
		if !maps.Equal(got, want) {
			t.Fatalf("revision %d: %v\nwant %v", i+1, slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
		}
	}
	// History follows the note through its rename; Versions name the new
	// commits, and a change the purge made a no-op is no change.
	history, err := cold.History(e.ctx, notestore.HistoryQuery{EntryID: leakID})
	if err != nil || len(history) != 3 || history[0].ID != after[4] || history[2].ID != after[2] {
		t.Fatal("history:", len(history), err)
	}
	if f, err := cold.Stat(e.ctx, gitobj.ID{}, "keep.md"); err != nil || f.Version != after[5] {
		t.Fatal("keep.md:", f, err)
	}
	if f, err := cold.Stat(e.ctx, after[7], "big.md"); err != nil || f.Version != after[6] {
		t.Fatal("big.md:", f, err)
	}
	if v, err := cold.PathVersion(e.ctx, after[5], "Inbox/leak.md"); err != nil || v != after[4] {
		t.Fatal("the rename's version:", v, err)
	}
	record, err := cold.Purge(e.ctx, "purge-1")
	if err != nil || record.Actor != "owner" || record.Reason != "a leaked key" || record.FromSeq != 3 || string(record.Scope) != `{"kind": "text"}` {
		t.Fatalf("purge record %+v %v", record, err)
	}
	if result.Removed.Versions == 0 || result.Removed.Trees == 0 || result.Removed.Blocks == 0 || result.Removed.Segments == 0 {
		t.Fatalf("removed %+v", result.Removed)
	}
	if bytes.Contains(orgBytes(t, e.db), []byte(secret)) {
		t.Fatal("a stored row still holds the secret")
	}
	if bytes.Contains(fileBytes(t, e.blobs.Path()), []byte(secret)) {
		t.Fatal("a stored block still holds the secret")
	}
	g := export(t, cold)
	if log := strings.Fields(g.run(nil, "rev-list", "--reverse", "main")); len(log) != len(after) || log[2] != after[2].String() {
		t.Fatal("exported history:", log)
	}
	if strings.Contains(g.run(nil, "cat-file", "--batch-all-objects", "--batch"), secret) {
		t.Fatal("the export holds the secret")
	}
	// Writes carry on from the rewritten head.
	next := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("later.md", "Later.\n")}}).Head
	if next.Seq != result.Head.Seq+1 {
		t.Fatal(next)
	}
	if _, err := n.Rewrite(e.ctx, 3, "purge-1", redact(secret), purgeOptions()); err == nil {
		t.Fatal("a purge ID used twice")
	}
}

// Dropping a file removes it from every revision: its history is gone, and
// a later purge of the rewritten history maps the first commits on.
func TestRewriteDropsAFileFromEveryVersion(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	secret := newSecret(t)
	commits := leakyHistory(t, n, secret)
	drop := func(f notestore.RewriteFile, _ func() ([]byte, error)) ([]byte, bool, error) {
		return nil, f.EntryID != leakID, nil
	}
	result, err := n.Rewrite(e.ctx, 3, "drop-note", drop, purgeOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Entries, []string{leakID}) {
		t.Fatal("entries:", result.Entries)
	}
	for _, c := range commits[2:] {
		files, err := n.Files(e.ctx, result.Rewritten[c])
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if f.EntryID == leakID || strings.Contains(f.Path, "leak") {
				t.Fatal("the dropped note remains:", f.Path)
			}
		}
	}
	if history, err := n.History(e.ctx, notestore.HistoryQuery{EntryID: leakID}); err != nil || len(history) != 0 {
		t.Fatal("history of the dropped note:", len(history), err)
	}
	// The revision that only moved the note keeps its place, empty.
	if info, err := n.Commit(e.ctx, result.Rewritten[commits[4]]); err != nil || len(info.Changes) != 0 {
		t.Fatal("the move:", info.Changes, err)
	}
	export(t, n)

	// The big manuscript still held the secret; a second purge rewrites the
	// first one's commits, and references follow both.
	second, err := n.Rewrite(e.ctx, 3, "redact-2", redact(secret), purgeOptions())
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, err := n.Rewritten(e.ctx, commits[3])
	if err != nil || !ok || rec.Commit != second.Rewritten[result.Rewritten[commits[3]]] || !slices.Equal(rec.Purges, []string{"drop-note", "redact-2"}) {
		t.Fatal("chained mapping:", rec, ok, err)
	}
	if bytes.Contains(orgBytes(t, e.db), []byte(secret)) || bytes.Contains(fileBytes(t, e.blobs.Path()), []byte(secret)) {
		t.Fatal("the secret is still stored")
	}
	export(t, n)
}

// A rewrite that would change a file of the revision before it starts is
// refused, as is one whose head moved since its plan; neither changes
// anything.
func TestRewriteRefusesToKeepContentItWouldChange(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	secret := newSecret(t)
	commits := leakyHistory(t, n, secret)
	head := commits[len(commits)-1]
	if _, err := n.Rewrite(e.ctx, 4, "late", redact(secret), purgeOptions()); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("a rewrite after the content's first revision:", err)
	}
	stale := commits[len(commits)-2]
	o := purgeOptions()
	o.ExpectHead = &stale
	if _, err := n.Rewrite(e.ctx, 3, "stale", redact(secret), o); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a rewrite planned at another head:", err)
	}
	if _, err := n.Rewrite(e.ctx, 3, "no reason", redact(secret), notestore.RewriteOptions{Actor: "owner"}); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("a purge without a reason:", err)
	}
	if got, _ := n.Head(e.ctx); got.Commit != head {
		t.Fatal("a refused rewrite moved the head")
	}
	if _, err := n.Purge(e.ctx, "late"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a refused rewrite recorded its purge:", err)
	}
	if !bytes.Contains(orgBytes(t, e.db), []byte(secret)) {
		t.Fatal("a refused rewrite removed content")
	}
	contents(t, e.open(e.db, notestore.Options{}).Notebook("notes"), commits)
}

// Live documents of every changed entry, and of entries the caller names,
// start a new epoch without their updates; the others keep theirs, based on
// the rewritten commit.
func TestRewriteResetsLiveDocumentsAndRemapsTheirBase(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	secret := newSecret(t)
	commits := leakyHistory(t, n, secret)
	keep, err := n.Stat(e.ctx, gitobj.ID{}, "keep.md")
	if err != nil {
		t.Fatal(err)
	}
	const other = "0123456789abcdef0123456789abcdef"
	for _, entry := range []string{leakID, keep.EntryID, other} {
		if _, err := n.LiveAppend(e.ctx, entry, 0, []byte("typed "+secret), "ada"); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.LiveCompact(e.ctx, keep.EntryID, 0, []byte("snapshot"), 1, commits[5]); err != nil {
		t.Fatal(err)
	}
	if _, err := n.LiveAppend(e.ctx, keep.EntryID, 0, []byte("after the snapshot"), "ada"); err != nil {
		t.Fatal(err)
	}
	o := purgeOptions()
	o.ResetLive = []string{other}
	result, err := n.Rewrite(e.ctx, 3, "live", redact(secret), o)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.LiveReset, []string{other, leakID}) {
		t.Fatal("reset:", result.LiveReset)
	}
	for _, entry := range []string{leakID, other} {
		if st, err := n.LiveState(e.ctx, entry); err != nil || st.Epoch != 1 || st.Snapshot != nil || len(st.Updates) != 0 || !st.Base.IsZero() {
			t.Fatalf("%s: %+v %v", entry, st, err)
		}
	}
	st, err := n.LiveState(e.ctx, keep.EntryID)
	if err != nil || st.Epoch != 0 || st.Base != result.Rewritten[commits[5]] || len(st.Updates) != 1 {
		t.Fatalf("unchanged entry: %+v %v", st, err)
	}
	live, err := n.LiveEntries(e.ctx)
	if err != nil || len(live) != 3 {
		t.Fatal(live, err)
	}
}

// A retried change set answers after a purge with the commits that replaced
// its own, and without the files the purge dropped, which its receipt no
// longer names.
func TestRewriteRemapsCommitReceipts(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	secret := newSecret(t)
	leakyHistory(t, n, secret)
	cs := notestore.ChangeSet{Subject: "Operation", OperationID: "op-1", Files: []notestore.FileChange{
		write("Projects/leak.md", leakNote("The key is "+secret+", again.\n")), write("keep.md", "Kept again.\n")}}
	first := apply(t, n, cs)
	before := apply(t, n, notestore.ChangeSet{Subject: "Before", OperationID: "op-0", AllowEmpty: true})
	drop := func(f notestore.RewriteFile, _ func() ([]byte, error)) ([]byte, bool, error) {
		return nil, f.EntryID != leakID, nil
	}
	result, err := n.Rewrite(e.ctx, 3, "drop-note", drop, purgeOptions())
	if err != nil {
		t.Fatal(err)
	}
	again := apply(t, n, cs)
	if !again.Replayed || again.Head != (notestore.Head{Seq: first.Head.Seq, Commit: result.Rewritten[first.Head.Commit]}) {
		t.Fatalf("replayed %+v", again)
	}
	if _, ok := again.Versions["Projects/leak.md"]; ok || again.Versions["keep.md"] != result.Rewritten[first.Versions["keep.md"]] {
		t.Fatal("versions:", again.Versions)
	}
	if _, err := n.Commit(e.ctx, again.Head.Commit); err != nil {
		t.Fatal(err)
	}
	if replay := apply(t, n, notestore.ChangeSet{Subject: "Before", OperationID: "op-0", AllowEmpty: true}); replay.Head.Commit != result.Rewritten[before.Head.Commit] {
		t.Fatal("the other receipt:", replay.Head)
	}
}

// A purge of what only live documents hold records the purge and resets
// them in one transaction at the head it expects, and changes no revision.
func TestPurgeLiveRecordsThePurgeAndResetsTheDocuments(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	secret := newSecret(t)
	commits := leakyHistory(t, n, secret)
	head := commits[len(commits)-1]
	if _, err := n.LiveAppend(e.ctx, leakID, 0, []byte("typed "+secret), "ada"); err != nil {
		t.Fatal(err)
	}
	o := purgeOptions()
	o.ResetLive = []string{leakID}
	stale := commits[len(commits)-2]
	o.ExpectHead = &stale
	if _, err := n.PurgeLive(e.ctx, "live-stale", o); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a purge planned at another head:", err)
	}
	if _, err := n.PurgeLive(e.ctx, "live-none", purgeOptions()); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("a purge of no live document:", err)
	}
	o.ExpectHead = &head
	result, err := n.PurgeLive(e.ctx, "live-1", o)
	if err != nil || result.Head.Commit != head || !slices.Equal(result.LiveReset, []string{leakID}) {
		t.Fatal(result, err)
	}
	if _, err := n.PurgeLive(e.ctx, "live-1", o); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("a purge that already ran:", err)
	}
	if st, err := n.LiveState(e.ctx, leakID); err != nil || st.Epoch != 1 || len(st.Updates) != 0 {
		t.Fatalf("live %+v %v", st, err)
	}
	record, err := n.Purge(e.ctx, "live-1")
	if err != nil || record.Actor != "owner" || record.FromSeq != 0 {
		t.Fatalf("record %+v %v", record, err)
	}
	for _, id := range []string{"live-stale", "live-none"} {
		if _, err := n.Purge(e.ctx, id); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("a refused purge was recorded:", id, err)
		}
	}
	if got, _ := n.Head(e.ctx); got.Commit != head {
		t.Fatal("the head moved")
	}
}

// Content another notebook holds stays readable there, though the purged
// notebook's copy goes.
func TestRewriteKeepsContentAnotherNotebookShares(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	a, b := e.create("alpha"), e.create("beta")
	secret := newSecret(t)
	leakyHistory(t, a, secret)
	leaked, _, err := a.Read(e.ctx, gitobj.ID{}, "Projects/leak.md")
	if err != nil {
		t.Fatal(err)
	}
	shared := strings.Replace(manuscript(300), "Paragraph 150.", "Paragraph 150. The vault code is "+secret+".", 1)
	apply(t, b, notestore.ChangeSet{Files: []notestore.FileChange{write("copy.md", shared), write("leak-copy.md", string(leaked))}})
	if _, err := a.Rewrite(e.ctx, 3, "alpha-only", redact(secret), purgeOptions()); err != nil {
		t.Fatal(err)
	}
	cold := e.open(e.db, notestore.Options{}).Notebook("beta")
	if got := read(t, cold, gitobj.ID{}, "copy.md"); got != shared {
		t.Fatal("the other notebook's manuscript was damaged")
	}
	if got := read(t, cold, gitobj.ID{}, "leak-copy.md"); got != string(leaked) {
		t.Fatal("the other notebook's note was damaged")
	}
	alpha := e.open(e.db, notestore.Options{}).Notebook("alpha")
	files, err := alpha.Files(e.ctx, gitobj.ID{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.Contains(read(t, alpha, gitobj.ID{}, f.Path), secret) {
			t.Fatal(f.Path, "still holds the secret")
		}
	}
}

// Imported history keeps its shape through a rewrite: merges keep both
// parents, rewritten, submodules, symbolic links and executables keep their
// modes, and every rewritten tree is the old one with only the rewritten
// files changed, as Git sees the export.
func TestRewriteOfImportedHistoryWithBranches(t *testing.T) {
	e := newEnv(t, notestore.Options{ImportEntryID: declaredID})
	g := buildNotebookHistory(t)
	n := e.store.Notebook("imported")
	head := importAll(t, g, n)
	edit := func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
		if f.Path != "book.md" {
			return nil, true, nil
		}
		data, err := content()
		return bytes.ReplaceAll(data, []byte("Call me Ishmael"), []byte("[purged]")), true, err
	}
	result, err := n.Rewrite(e.ctx, 1, "imported-purge", edit, purgeOptions())
	if err != nil {
		t.Fatal(err)
	}
	if result.Head.Seq != head.Seq || len(result.Rewritten) != int(head.Seq) {
		t.Fatal(result.Head, len(result.Rewritten))
	}
	out := export(t, n)
	for old, next := range result.Rewritten {
		oldParents := strings.Fields(g.run(nil, "rev-list", "--parents", "-n", "1", old.String()))[1:]
		newParents := strings.Fields(out.run(nil, "rev-list", "--parents", "-n", "1", next.String()))[1:]
		if len(oldParents) != len(newParents) {
			t.Fatal("parents of", old, oldParents, newParents)
		}
		for i, p := range oldParents {
			if newParents[i] != result.Rewritten[g.id(p)].String() {
				t.Fatal("parent", i, "of", old)
			}
		}
		was, is := strings.Split(g.run(nil, "ls-tree", "-r", old.String()), "\n"), strings.Split(out.run(nil, "ls-tree", "-r", next.String()), "\n")
		if len(was) != len(is) {
			t.Fatal("tree of", old, was, is)
		}
		for i := range was {
			wf, nf := strings.Fields(was[i]), strings.Fields(is[i])
			if len(wf) == 0 {
				continue
			}
			if wf[0] != nf[0] || wf[1] != nf[1] || wf[3] != nf[3] {
				t.Fatal("entry", was[i], is[i])
			}
			if wf[3] == "book.md" {
				want := strings.ReplaceAll(g.run(nil, "cat-file", "blob", wf[2]), "Call me Ishmael", "[purged]")
				if out.run(nil, "cat-file", "blob", nf[2]) != want {
					t.Fatal("book.md at", old)
				}
			} else if wf[2] != nf[2] {
				t.Fatal("an untouched file changed:", wf[3])
			}
		}
	}
}

// A rewritten commit is no longer what its signature signed, so the
// signature goes; other headers stay.
func TestRewriteDropsSignatures(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.store.Notebook("signed")
	commit := gitobj.Commit{Tree: gitobj.EmptyTree, Author: ada, Committer: ada, Message: "Signed\n",
		Extra: []byte("encoding ISO-8859-1\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n iQEz\n -----END PGP SIGNATURE-----\n")}
	if _, err := n.ImportCommit(e.ctx, gitobj.EncodeCommit(commit), map[gitobj.ID][]byte{gitobj.EmptyTree: {}}, nil); err != nil {
		t.Fatal(err)
	}
	result, err := n.Rewrite(e.ctx, 1, "unsign", func(notestore.RewriteFile, func() ([]byte, error)) ([]byte, bool, error) { return nil, true, nil }, purgeOptions())
	if err != nil {
		t.Fatal(err)
	}
	info, err := n.Commit(e.ctx, result.Head.Commit)
	if err != nil || string(info.Commit.Extra) != "encoding ISO-8859-1\n" {
		t.Fatalf("extra %q %v", info.Commit.Extra, err)
	}
	raw, _ := json.Marshal(info.Trailers)
	if !strings.Contains(string(raw), `"Value":"unsign"`) {
		t.Fatal(string(raw))
	}
	export(t, n)
}
