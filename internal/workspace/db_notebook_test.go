package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// A notebook answers without a Git process: with no git executable to
// find, it saves, reads history, lists, searches and lists changes, and
// it never makes a working tree under Root.
func TestDatabaseNotebooksNeverRunGit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t.Setenv("PATH", t.TempDir())
	first, err := s.Save(ctx, "notes/a.md", []byte("# One\n"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Save(ctx, first.Path, []byte("# Two\n"), first.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	if doc, err := s.Read(ctx, first.Path, first.Revision); err != nil || string(doc.Content) != "# One\n" {
		t.Fatal("historical read:", doc, err)
	}
	if status, err := s.Status(ctx); err != nil || status.Revision != second.Revision || status.Dirty {
		t.Fatal("status:", status, err)
	}
	if _, err := os.Stat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a notebook kept in the database made a working tree:", err)
	}
	if entries, err := s.List(ctx); err != nil || len(entries) != 1 {
		t.Fatal("list:", entries, err)
	}
	if entries, err := s.Find(ctx, "two", "", "", ""); err != nil || len(entries) != 1 {
		t.Fatal("search:", entries, err)
	}
	if changes, err := s.ChangesSince(ctx, first.Revision, ""); err != nil || len(changes.Files) != 1 || len(changes.Commits) != 1 {
		t.Fatal("changes:", changes, err)
	}
	if history, err := s.History(ctx, first.Path); err != nil || len(history) != 2 {
		t.Fatal("history:", history, err)
	}
}

// The workspace starts no process directly: none of its files imports
// os/exec or procgroup, and a notebook is read and written through the notes
// store. The git executable runs only in the packages that read or write Git
// repositories: internal/gitexport (exports, the bundle checks of an import
// and the export a purge rebuilds, which the workspace calls on, and the
// cutover import) and internal/reposource (repository mirrors).
func TestWorkspaceStartsNoProcessDirectly(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil || len(names) == 0 {
		t.Fatal(names, err)
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range f.Imports {
			if path, _ := strconv.Unquote(spec.Path.Value); path == "os/exec" || path == "forgetmenote/internal/procgroup" {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
}

func TestCreateOpenAndDeleteDatabaseNotebooks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	status, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.notes.Commit(ctx, mustParse(t, status.Revision))
	if err != nil || info.Seq != 1 || info.Commit.Message != "Initialize notes notebook\n" || info.Commit.Author.Name != "ForgetMeNote" || info.Commit.Author.Email != "forgetmenote@localhost" {
		t.Fatal("initial commit:", info.Commit, err)
	}
	if _, err := CreateDB(ctx, s.noteStore, "notes", t.TempDir(), nil); !errors.Is(err, fs.ErrExist) {
		t.Fatal("creating an existing notebook:", err)
	}
	if _, err := OpenDB(ctx, s.noteStore, "missing", t.TempDir(), nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("opening a missing notebook:", err)
	}
	if _, err := OpenDB(ctx, s.noteStore, "Not/Valid", t.TempDir(), nil); err == nil {
		t.Fatal("opened an invalid notebook ID")
	}
	saved, err := s.Save(ctx, "kept.md", []byte("kept"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	again := reopenTestStore(t, s)
	if doc, err := again.Read(ctx, "kept.md", ""); err != nil || string(doc.Content) != "kept" || doc.Revision != saved.Revision || doc.Version != saved.Version {
		t.Fatal("after reopening:", doc, err)
	}
	if err := DeleteDB(ctx, again.noteStore, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := again.Status(ctx); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("status of a deleted notebook:", err)
	}
	if _, err := OpenDB(ctx, again.noteStore, "notes", t.TempDir(), nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("opening a deleted notebook:", err)
	}
}

func mustParse(t testing.TB, revision string) gitobj.ID {
	t.Helper()
	id, err := gitobj.ParseID(revision)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// commitShapes are the contexts and summaries the workspace commits with:
// each attribution, a group's operation, and a live checkpoint's audit.
func commitShapes() map[string]struct {
	ctx     context.Context
	summary string
} {
	user := attribution.Identity{ID: "u-1", Name: "Ada Lovelace", Email: "ada@example.com", Source: "browser"}
	mcp := attribution.Identity{ID: "u-2", Name: "Agent", Email: "agent@example.com", Source: "mcp", Client: "Claude Desktop"}
	op := func(c string) operationIdentity {
		return operationIdentity{ID: strings.Repeat(c, 64), Request: ContentHash([]byte(c))}
	}
	audit := `Collaboration-Receipt: {"operation":"o1","receipt":{"request":"r","revision":"g:1","actor":{"id":"u-1","name":"Ada"},"time":"2026-11-22T10:00:00Z"}}` + "\n" +
		`Collaboration-Receipt: {"operation":"o2","receipt":{"request":"s","revision":"g:2","actor":{"id":"u-2","name":"Agent"},"time":"2026-11-22T10:00:01Z"}}` + "\n"
	collaboration := attribution.Identity{Name: "ForgetMeNote collaboration", Email: "forgetmenote@localhost", Source: "collaboration"}
	background := context.Background()
	with := func(identity attribution.Identity) context.Context { return attribution.With(background, identity) }
	return map[string]struct {
		ctx     context.Context
		summary string
	}{
		"system":            {background, "Update notes/a.md"},
		"browser":           {with(user), "Update notes/a.md"},
		"client":            {with(mcp), `Move "notes/a b.md" to "archive/a b.md"`},
		"empty source":      {with(attribution.Identity{Name: "Nobody", Email: "nobody@example.com"}), "Empty Trash"},
		"spaced client":     {with(attribution.Identity{Name: "Agent", Email: "a@example.com", Source: "api", Client: "  padded client  "}), "Organize note: tags"},
		"group":             {context.WithValue(with(mcp), operationKey{}, op("a")), "Tidy the chapter notes"},
		"multi-line":        {with(user), "First line\r\nsecond line  \n"},
		"comment-like":      {with(user), "# not a comment"},
		"trailer-like":      {with(user), "Fixes: a typo"},
		"crud in name":      {with(attribution.Identity{Name: " <Ada>, ", Email: "  ada@example.com ", Source: "browser"}), "Update a.md"},
		"live checkpoint":   {attribution.With(context.WithValue(context.WithValue(background, operationKey{}, op("b")), collaborationAuditKey{}, audit), collaboration), "Checkpoint live edits to notes/a.md"},
		"checkpoint, empty": {attribution.With(context.WithValue(context.WithValue(background, operationKey{}, op("c")), collaborationAuditKey{}, ""), collaboration), "Checkpoint live edits to b.md"},
	}
}

// Each shape commits, through the notes store, the message, author and
// committer the Git backend committed, as recorded from it, and the message
// is what "git commit -F -" stores for the same summary and trailers, byte
// for byte.
func TestDatabaseCommitsMatchGitCommits(t *testing.T) {
	db := newTestStore(t)
	backend := db.backend.(*dbNotebook)
	repository := t.TempDir()
	git := func(identity attribution.Identity, input string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repository, "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Stdin = strings.NewReader(input)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME="+identity.Name, "GIT_AUTHOR_EMAIL="+identity.Email, "GIT_COMMITTER_NAME=ForgetMeNote", "GIT_COMMITTER_EMAIL=forgetmenote@localhost")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(args, err)
		}
		return out
	}
	git(attribution.Identity{}, "", "init", "--quiet", "--initial-branch=main")
	// What the Git backend committed for each shape at storage-v2 14efbd3:
	// its message, author and committer.
	recorded := map[string][3]string{
		"browser":           {"Update notes/a.md\n\nGitnotes-Source: browser\nGitnotes-User: u-1\n", "Ada Lovelace <ada@example.com>", "ForgetMeNote <forgetmenote@localhost>"},
		"checkpoint, empty": {"Checkpoint live edits to b.md\n\nGitnotes-Source: collaboration\nGitnotes-Operation: cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\nGitnotes-Request: 2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6\n", "ForgetMeNote collaboration <forgetmenote@localhost>", "ForgetMeNote <forgetmenote@localhost>"},
		"client":            {"Move \"notes/a b.md\" to \"archive/a b.md\"\n\nGitnotes-Source: mcp\nGitnotes-User: u-2\nGitnotes-Client: Claude Desktop\n", "Agent <agent@example.com>", "ForgetMeNote <forgetmenote@localhost>"},
		"comment-like":      {"# not a comment\n\nGitnotes-Source: browser\nGitnotes-User: u-1\n", "Ada Lovelace <ada@example.com>", "ForgetMeNote <forgetmenote@localhost>"},
		"crud in name":      {"Update a.md\n\nGitnotes-Source: browser\n", "Ada <ada@example.com>", "ForgetMeNote <forgetmenote@localhost>"},
		"empty source":      {"Empty Trash\n\nGitnotes-Source:\n", "Nobody <nobody@example.com>", "ForgetMeNote <forgetmenote@localhost>"},
		"group":             {"Tidy the chapter notes\n\nGitnotes-Source: mcp\nGitnotes-User: u-2\nGitnotes-Client: Claude Desktop\nGitnotes-Operation: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nGitnotes-Request: ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb\n", "Agent <agent@example.com>", "ForgetMeNote <forgetmenote@localhost>"},
		"live checkpoint":   {"Checkpoint live edits to notes/a.md\n\nGitnotes-Source: collaboration\nGitnotes-Operation: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\nGitnotes-Request: 3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d\nCollaboration-Receipt: {\"operation\":\"o1\",\"receipt\":{\"request\":\"r\",\"revision\":\"g:1\",\"actor\":{\"id\":\"u-1\",\"name\":\"Ada\"},\"time\":\"2026-11-22T10:00:00Z\"}}\nCollaboration-Receipt: {\"operation\":\"o2\",\"receipt\":{\"request\":\"s\",\"revision\":\"g:2\",\"actor\":{\"id\":\"u-2\",\"name\":\"Agent\"},\"time\":\"2026-11-22T10:00:01Z\"}}\n", "ForgetMeNote collaboration <forgetmenote@localhost>", "ForgetMeNote <forgetmenote@localhost>"},
		"multi-line":        {"First line  second line\n\nGitnotes-Source: browser\nGitnotes-User: u-1\n", "Ada Lovelace <ada@example.com>", "ForgetMeNote <forgetmenote@localhost>"},
		"spaced client":     {"Organize note: tags\n\nGitnotes-Source: api\nGitnotes-Client:   padded client\n", "Agent <a@example.com>", "ForgetMeNote <forgetmenote@localhost>"},
		"system":            {"Update notes/a.md\n\nGitnotes-Source: system\n", "ForgetMeNote <forgetmenote@localhost>", "ForgetMeNote <forgetmenote@localhost>"},
		"trailer-like":      {"Fixes: a typo\n\nGitnotes-Source: browser\nGitnotes-User: u-1\n", "Ada Lovelace <ada@example.com>", "ForgetMeNote <forgetmenote@localhost>"},
	}
	for name, shape := range commitShapes() {
		t.Run(name, func(t *testing.T) {
			subject, body, err := commitMessage(shape.ctx, shape.summary)
			if err != nil {
				t.Fatal(err)
			}
			identity := attribution.From(shape.ctx)
			git(identity, subject+"\n\n"+body, "commit", "--quiet", "--allow-empty", "-F", "-")
			want, err := gitobj.ParseCommit(git(identity, "", "cat-file", "commit", "HEAD"))
			if err != nil {
				t.Fatal(err)
			}
			if got := gitobj.CleanMessage(subject, body, nil); got != want.Message {
				t.Fatalf("CleanMessage %q\n git stores %q", got, want.Message)
			}
			db.mu.Lock()
			head, err := backend.head(shape.ctx)
			var result notestore.CommitResult
			if err == nil {
				result, err = backend.applyChanges(shape.ctx, head, shape.summary, nil)
			}
			db.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			info, err := db.notes.Commit(context.Background(), result.Head.Commit)
			if err != nil {
				t.Fatal(err)
			}
			got := info.Commit
			if got.Message != want.Message {
				t.Fatalf("stored %q\n git stores %q", got.Message, want.Message)
			}
			literal, ok := recorded[name]
			if !ok {
				t.Fatal("no recorded commit for", name)
			}
			if got.Message != literal[0] || got.Author.Name+" <"+got.Author.Email+">" != literal[1] || got.Committer.Name+" <"+got.Committer.Email+">" != literal[2] {
				t.Fatalf("stored %q by %s <%s>, committed by %s <%s>\nwant %q", got.Message, got.Author.Name, got.Author.Email, got.Committer.Name, got.Committer.Email, literal)
			}
			if got.Author.Name != want.Author.Name || got.Author.Email != want.Author.Email || got.Author.TZ != want.Author.TZ || got.Committer.Name != want.Committer.Name || got.Committer.Email != want.Committer.Email || got.Committer.When != got.Author.When {
				t.Fatalf("signatures %+v %+v, git %+v %+v", got.Author, got.Committer, want.Author, want.Committer)
			}
		})
	}
}

// commitRecord reads a revision as the Git backend's log format does, with
// times in the numeric offset production's Git prints.
func TestCommitRecordsReadAsGitLogDoes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	bare := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	for _, tz := range []string{"+0000", "-0700", "+0530"} {
		author := gitobj.Signature{Name: "Ada", Email: "ada@example.com", When: 1764000000, TZ: tz}
		r, err := s.notes.Apply(ctx, notestore.ChangeSet{Author: author, Subject: "A subject\nwrapped onto a second line", Body: "Body text.\n\nGitnotes-Source: mcp\nGitnotes-User: u-1\nGitnotes-Client: Claude", Files: []notestore.FileChange{fixture("a.md", tz)}})
		if err != nil {
			t.Fatal(err)
		}
		info, err := s.notes.Commit(ctx, r.Head.Commit)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.notes.Objects(ctx, 0, func(kind string, _ gitobj.ID, body []byte) error {
			_, err := gitobj.WriteLoose(bare, kind, body)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("git", "--git-dir", bare, "log", "-1", "--format=%H%x00%s%x00%an%x00%ae%x00%cn%x00%ce%x00%aI%x00%(trailers:key=Gitnotes-Source,valueonly)%x00%(trailers:key=Gitnotes-User,valueonly)%x00%(trailers:key=Gitnotes-Client,valueonly)", r.Head.Commit.String()).Output()
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Split(strings.TrimSuffix(string(out), "\n"), "\x00")
		want := Commit{Revision: f[0], Message: f[1], AuthorName: f[2], AuthorEmail: f[3], CommitterName: f[4], CommitterEmail: f[5], AuthoredAt: f[6], Source: strings.TrimSpace(f[7]), UserID: strings.TrimSpace(f[8]), Client: strings.TrimSpace(f[9])}
		got := commitRecord(info)
		// Git 2.54 spells UTC "Z"; production's Git and this backend print +00:00.
		want.AuthoredAt = strings.TrimSuffix(want.AuthoredAt, "Z")
		if tz == "+0000" {
			if !strings.HasSuffix(got.AuthoredAt, "+00:00") {
				t.Fatal("UTC is not a numeric offset:", got.AuthoredAt)
			}
			got.AuthoredAt = strings.TrimSuffix(got.AuthoredAt, "+00:00")
			want.AuthoredAt = strings.TrimSuffix(want.AuthoredAt, "+00:00")
		}
		if got != want {
			t.Fatalf("%s: record %+v\n git log %+v", tz, got, want)
		}
	}
}

func TestSaveFencesOnTheInstalledAuthorityGeneration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "a.md", []byte("one"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	s.SetAuthorityGeneration(5)
	if _, err := s.Save(WithAuthority(ctx, 4), doc.Path, []byte("stale owner"), doc.Revision, "test"); !errors.Is(err, ErrConflict) {
		t.Fatal("a write under an older generation:", err)
	}
	if doc, err = s.Save(WithAuthority(ctx, 5), doc.Path, []byte("current owner"), doc.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, doc.Path, []byte("unfenced request"), doc.Revision, "test"); err != nil {
		t.Fatal("a request without a generation takes the installed one:", err)
	}
	s.SetReadOnly(true)
	if _, err := s.Save(ctx, "b.md", []byte("x"), "", "test"); err == nil {
		t.Fatal("a read-only notebook accepted a save")
	}
}

// Without a working tree, what Git refused through the file system is
// refused by the tree: links, submodules, folders and unknown revisions.
func TestReadsAndSavesRespectTreeEntries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	head := commitFixture(t, s,
		fixture("notes/a.md", "a"),
		notestore.FileChange{Path: "linked", Content: []byte("notes"), Mode: gitobj.ModeSymlink},
		notestore.FileChange{Path: "run.md", Content: []byte("#!/bin/sh\n"), Mode: gitobj.ModeExecutable},
	)
	for _, path := range []string{"linked", "run.md", "notes"} {
		if _, err := s.Read(ctx, path, ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("read %s: %v", path, err)
		}
	}
	if _, err := s.Read(ctx, "linked/a.md", ""); !errors.Is(err, os.ErrNotExist) {
		t.Error("read through a link:", err)
	}
	if _, err := s.Save(ctx, "linked", []byte("over a link"), head, "test"); !errors.Is(err, ErrForbidden) {
		t.Error("save over a link:", err)
	}
	if _, err := s.Save(ctx, "linked/new.md", []byte("x"), "", "test"); err == nil {
		t.Error("saved under a link")
	}
	unknown := strings.Repeat("0", 40)
	if _, err := s.Read(ctx, "notes/a.md", unknown); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Error("read at an unknown revision:", err)
	}
	initial, err := s.notes.Revisions(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx, "notes/a.md", initial[0].ID.String()); !errors.Is(err, os.ErrNotExist) {
		t.Error("a note absent at the initial revision:", err)
	}
	if after, _ := s.Status(ctx); after.Revision != head {
		t.Error("a refused save committed")
	}
}

// A note keeps its store identity, its fmn-id, from creation through
// renames, so its history is one entry.
func TestNewNotesAreKeptUnderTheirIdentity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := NewNoteID()
	content, _ := WithNoteID([]byte("# One\n"), id)
	doc, err := s.Save(ctx, "one.md", content, "", "author")
	if err != nil {
		t.Fatal(err)
	}
	if f, err := s.notes.Stat(ctx, gitobj.ID{}, "one.md"); err != nil || f.EntryID != id {
		t.Fatal("the note's entry:", f, err)
	}
	plain, err := s.Save(ctx, "plain.md", []byte("# Plain\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	backend := s.backend.(*dbNotebook)
	s.mu.Lock()
	result, err := backend.applyChanges(ctx, plain.Revision, `Move "one.md" to "two.md"`, orderedChanges(map[string][]byte{"one.md": nil, "two.md": content}, map[string]string{"two.md": "one.md"}))
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	moved, err := s.notes.Stat(ctx, gitobj.ID{}, "two.md")
	if err != nil || moved.EntryID != id {
		t.Fatal("a renamed note's entry:", moved, err)
	}
	history, err := s.notes.History(ctx, notestore.HistoryQuery{Path: "two.md"})
	if err != nil || len(history) != 2 || history[0].ID != result.Head.Commit || history[1].ID.String() != doc.Revision {
		t.Fatal("the renamed note's history:", len(history), err)
	}
	if _, err := s.Save(ctx, "two.md", bytes.Replace(content, []byte(id), []byte(strings.Repeat("c", 32)), 1), result.Head.Commit.String(), "author"); err == nil {
		t.Fatal("changed a note's fmn-id")
	}
}

func TestOrderedChangesRenameFirstThenDeleteThenWrite(t *testing.T) {
	got := orderedChanges(map[string][]byte{
		"b.md": []byte("b"), "a.md": nil, "moved.md": []byte("m"), "old.md": nil, "keep.md": nil, "source.md": []byte("new at the source"),
	}, map[string]string{"moved.md": "old.md", "copy.md": "source.md"})
	var order []string
	for _, c := range got {
		order = append(order, c.From+">"+c.Path)
	}
	if want := "source.md>copy.md old.md>moved.md >a.md >keep.md >b.md >source.md"; strings.Join(order, " ") != want {
		t.Fatalf("order %q, want %q", strings.Join(order, " "), want)
	}
	if got[0].Content != nil || string(got[1].Content) != "m" {
		t.Fatal("rename contents:", got[0], got[1])
	}
}

// An operation's revision is found again by its trailers: a retry of the
// same request replays it, another request under its ID conflicts.
func TestOperationRevisionsReplayTheirRequest(t *testing.T) {
	s := newTestStore(t)
	backend := s.backend.(*dbNotebook)
	group := EditGroup{ID: "g-1", Summary: "Group", Edits: []NoteEdit{{Path: "a.md", Content: "a"}}}
	identity := attribution.Identity{ID: "u-1", Name: "Ada", Email: "ada@example.com", Source: "mcp"}
	ctx := attribution.With(context.Background(), identity)
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, err := backend.groupReceipt(ctx, group, identity.ID); err != nil || r.Replayed {
		t.Fatal("before the group:", r, err)
	}
	raw, _ := json.Marshal(group)
	op := operationIdentity{ID: ContentHash([]byte(identity.ID + "\x00" + group.ID)), Request: ContentHash(raw)}
	head, _ := backend.head(ctx)
	first, err := backend.applyChanges(context.WithValue(ctx, operationKey{}, op), head, group.Summary, []pathChange{{Path: "a.md", Content: []byte("a")}})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := backend.groupReceipt(ctx, group, identity.ID); err != nil || !r.Replayed || r.Revision != first.Head.Commit.String() {
		t.Fatal("the group's receipt:", r, err)
	}
	group.Summary = "Another request"
	if _, err := backend.groupReceipt(ctx, group, identity.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("another request under the operation:", err)
	}
	// Committing the operation again replays it in the store too.
	again, err := backend.applyChanges(context.WithValue(ctx, operationKey{}, op), first.Head.Commit.String(), group.Summary, []pathChange{{Path: "a.md", Content: []byte("a")}})
	if err != nil || !again.Replayed || again.Head != first.Head {
		t.Fatal("a repeated operation:", again, err)
	}
	op.Request = strings.Repeat("e", 64)
	if _, err := backend.applyChanges(context.WithValue(ctx, operationKey{}, op), first.Head.Commit.String(), group.Summary, nil); !errors.Is(err, ErrConflict) {
		t.Fatal("a mismatched request:", err)
	}
}

// clean checkpoints pending live edits before a write; until live editing
// is ported, a write with pending edits fails and one without succeeds.
func TestWritesCheckpointPendingLiveEditsFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "a.md", []byte("a"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.live["a.md"] = &liveRecord{Path: "a.md", Uncommitted: []string{"op"}}
	s.mu.Unlock()
	if _, err := s.Save(ctx, "b.md", []byte("b"), "", "test"); !errors.Is(err, ErrLivePending) {
		t.Fatal("a write past pending live edits:", err)
	}
	if _, err := s.Save(WithExplicitCheckpoint(ctx), "b.md", []byte("b"), "", "test"); !errors.Is(err, ErrLivePending) {
		t.Fatal("an explicit-checkpoint client past pending live edits:", err)
	}
	if status, err := s.Status(ctx); err != nil || status.Dirty || status.Revision != doc.Revision {
		t.Fatal("status with pending live edits:", status, err)
	}
	s.mu.Lock()
	s.live["a.md"].Uncommitted = nil
	s.mu.Unlock()
	if _, err := s.Save(ctx, "b.md", []byte("b"), "", "test"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.liveStorageFailed = true
	s.mu.Unlock()
	if _, err := s.Status(ctx); err == nil {
		t.Fatal("status hid failed live storage")
	}
}

// Another gateway's commits reach this store's watchers through the notes
// store, as do this store's own.
func TestWatchersHearOtherGatewaysCommits(t *testing.T) {
	s := newTestStore(t)
	hints, cancel, err := s.Watch()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	other, err := OpenDB(context.Background(), openTestNotes(t, testEnv(t, s)), "notes", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	saved, err := other.Save(context.Background(), "a.md", []byte("from the other gateway"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-hints:
	case <-time.After(10 * time.Second):
		t.Fatal("no hint of another gateway's commit")
	}
	if doc, err := s.Read(context.Background(), "a.md", ""); err != nil || doc.Revision != saved.Revision {
		t.Fatal("read after the hint:", doc, err)
	}
	s.Close()
	if _, err := other.Save(context.Background(), "b.md", []byte("after close"), "", "test"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hints:
		t.Fatal("a closed store was hinted")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestStateDirectoryHoldsNoRepository(t *testing.T) {
	s := newTestStore(t)
	if filepath.Base(s.Root) != "workspace" {
		t.Fatal("root:", s.Root)
	}
	if info, err := os.Stat(filepath.Dir(s.Root)); err != nil || !info.IsDir() {
		t.Fatal("the state directory:", err)
	}
}

// Two gateways saving one note from the same revision have one winner, even
// when the loser has not heard of the winner's commit yet: its write is
// checked against the head it validated.
func TestTwoGatewaysSavingOneNoteHaveOneWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	other, err := OpenDB(ctx, openTestNotes(t, testEnv(t, s)), "notes", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for i := range 20 {
		path := "note" + strings.Repeat("x", i) + ".md"
		base, err := s.Save(ctx, path, []byte("base"), "", "test")
		if err != nil {
			t.Fatal(err)
		}
		// The other gateway reads the note, so its read model is loaded.
		for {
			if doc, err := other.Read(ctx, path, ""); err == nil && doc.Revision == base.Revision {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if _, err := s.Save(ctx, path, []byte("first"), base.Revision, "test"); err != nil {
			t.Fatal(err)
		}
		if _, err := other.Save(ctx, path, []byte("second"), base.Revision, "test"); !errors.Is(err, ErrConflict) {
			t.Fatalf("round %d: a lost update: %v", i, err)
		}
		if doc, err := s.Read(ctx, path, ""); err != nil || string(doc.Content) != "first" {
			t.Fatalf("round %d: %q %v", i, doc.Content, err)
		}
	}
}
