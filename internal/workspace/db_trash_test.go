package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
)

// importedTestStore opens, as a test store of its own, a notebook
// imported from a Git export of s, s's backup bundle, as the cutover
// imports a notebook from its Git repository: entries follow only the
// renames Git infers. s needs an exporter (backupTestStore).
func importedTestStore(t *testing.T, s *Store, excluded []string) *Store {
	t.Helper()
	return openImportedTestStore(t, excluded, func(notes *notestore.Store) error {
		return importFile(t, notes, "notes", backupFile(t, s))
	})
}

// openImportedTestStore opens notebook "notes" of a new org and blob
// directory once into has imported it, as a test store of its own.
func openImportedTestStore(t *testing.T, excluded []string, into func(*notestore.Store) error) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.OpenDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	env := &testNotebook{db: pgtest.Org(t), blobs: blobs}
	notes := openTestNotes(t, env)
	if err := into(notes); err != nil {
		t.Fatal(err)
	}
	imported, err := OpenDB(context.Background(), notes, "notes", filepath.Join(dir, "projects", "notes"), excluded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { imported.Close() })
	testNotebooks.Store(imported, env)
	return imported
}

// saveNote saves content at path on top of the current head.
func saveNote(t *testing.T, s *Store, path, content string) Document {
	t.Helper()
	ctx := context.Background()
	expected := ""
	if _, err := s.Read(ctx, path, ""); err == nil {
		status, err := s.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		expected = status.Revision
	}
	doc, err := s.Save(ctx, path, []byte(content), expected, "writer")
	if err != nil {
		t.Fatal(path, err)
	}
	return doc
}

func headOf(t *testing.T, s *Store) string {
	t.Helper()
	status, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return status.Revision
}

// trashHistoryFixture builds the histories the Trash walk distinguishes:
// moves with and without rewritten content, a path reused by a new note, a
// note kept by its fmn-id, a deleted note's fmn-id given to a new one, and a
// note trashed, restored and edited. Its store has an exporter.
func trashHistoryFixture(t *testing.T) *Store {
	t.Helper()
	g := backupTestStore(t)
	ctx := context.Background()
	saveNote(t, g, "notes/a.md", "# A\n\nFirst draft, see [other](../other.md).\n")
	saveNote(t, g, "other.md", "# Other\n")
	saveNote(t, g, "notes/a.md", "# A\n\nSecond draft, see [other](../other.md).\n")
	discussion, err := g.Comments(ctx, "notes/a.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.AddComment(ctx, "notes/a.md", "Keep this discussion", discussion.Revision, "reviewer"); err != nil {
		t.Fatal(err)
	}
	// A deeper folder rewrites the note's links, so its content changes.
	if _, err = g.Move(ctx, "notes", "archive/2026", headOf(t, g), "owner"); err != nil {
		t.Fatal(err)
	}
	saveNote(t, g, "archive/2026/a.md", "# A\n\nThird draft, see [other](../../other.md).\n")
	saveNote(t, g, "m.md", "# M\n\nSee [other](other.md).\n")
	if _, err = g.Move(ctx, "m.md", "deep/er/m.md", headOf(t, g), "owner"); err != nil {
		t.Fatal(err)
	}

	saveNote(t, g, "fresh.md", "# Fresh\n\nThe first note here.\n")
	commitFixture(t, g, notestore.FileChange{Path: "fresh.md"})
	saveNote(t, g, "fresh.md", "# Fresh\n\nAnother note at the same path.\n")

	kept, _ := NewNoteID()
	content, _ := WithNoteID([]byte("# Kept\n\nAn identity.\n"), kept)
	saveNote(t, g, "kept.md", string(content))
	if _, err = g.Move(ctx, "kept.md", "shelf/kept.md", headOf(t, g), "owner"); err != nil {
		t.Fatal(err)
	}
	content, _ = WithNoteID([]byte("# Kept\n\nAn identity, edited.\n"), kept)
	saveNote(t, g, "shelf/kept.md", string(content))

	reused, _ := NewNoteID()
	content, _ = WithNoteID([]byte("# Gone\n"), reused)
	saveNote(t, g, "gone.md", string(content))
	commitFixture(t, g, notestore.FileChange{Path: "gone.md"})
	content, _ = WithNoteID([]byte("# Reborn\n\nOther text under the old identity.\n"), reused)
	saveNote(t, g, "reborn.md", string(content))

	trashed, err := g.TrashNotes(ctx, []string{"archive/2026/a.md"}, headOf(t, g), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.RestoreTrash(ctx, trashed.Entries[0].ID, "restored.md", trashed.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	saveNote(t, g, "restored.md", "# A\n\nFourth draft, see [other](other.md).\n")

	saveNote(t, g, "private/secret.md", "# Secret\n")
	saveNote(t, g, "later.md", "# Later\n\nStill in the Trash.\n")
	if _, err = g.TrashNotes(ctx, []string{"private/secret.md", "later.md"}, headOf(t, g), "owner"); err != nil {
		t.Fatal(err)
	}
	saveNote(t, g, "broken.md", "# Broken\n")
	commitFixture(t, g, fixture(commentPath("broken.md"), "not a discussion"))
	return g
}

// backendOf returns s's backend.
func backendOf(s *Store) *dbNotebook { return s.backend.(*dbNotebook) }

func trashHistoryOf(s *Store, path string) (map[string][]string, error) {
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	return backendOf(s).trashHistory(ctx, path)
}

func prepareTrashOf(s *Store, paths []string) (trashPreparation, error) {
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := backendOf(s)
	index, err := n.loadTrash(ctx)
	if err != nil {
		return trashPreparation{}, err
	}
	return n.prepareTrash(ctx, paths, index, n.trashRevision)
}

// stagedTrash is what stageTrash staged, with the random Trash ID and the
// deletion time replaced.
type stagedTrash struct {
	Entry   TrashEntry
	Changes map[string]string
	Records []TrashRecord
}

func stageTrashOf(s *Store, path string, prepare bool) (stagedTrash, error) {
	ctx := context.Background()
	var prepared *trashPreparation
	if prepare {
		p, err := prepareTrashOf(s, []string{path})
		if err != nil {
			return stagedTrash{}, err
		}
		prepared = &p
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changes := map[string][]byte{}
	var index TrashIndex
	var entry TrashEntry
	var err error
	n := backendOf(s)
	if index, err = n.loadTrash(ctx); err == nil {
		entry, err = n.stageTrash(ctx, &index, path, "owner", changes, prepared)
	}
	if err != nil {
		return stagedTrash{}, err
	}
	if err = stageTrashIndex(index, 1, changes); err != nil {
		return stagedTrash{}, err
	}
	// Only the staged record's deletion time is normalized; older records
	// keep theirs.
	replacer := strings.NewReplacer(entry.ID, "<id>", commentPath(entry.Path), "<discussion>")
	normal := func(s string) string {
		return stagedDeletedAt.ReplaceAllString(replacer.Replace(s), `${1}"<deleted>"`)
	}
	out := stagedTrash{Changes: map[string]string{}}
	for p, content := range changes {
		value := "<deleted>"
		if content != nil {
			value = normal(string(content))
		}
		out.Changes[normal(p)] = value
	}
	entry.DeletedAt = "<deleted>"
	raw, _ := json.Marshal(entry)
	json.Unmarshal([]byte(normal(string(raw))), &out.Entry)
	raw, _ = json.Marshal(index.Records)
	json.Unmarshal([]byte(normal(string(raw))), &out.Records)
	return out, nil
}

var stagedDeletedAt = regexp.MustCompile(`("id"\s*:\s*"<id>"[^{}]*?"deletedAt"\s*:\s*)"[^"]*"`)

func errorText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// A notebook imported from Git reads its Trash index, the histories the
// Trash records and the discussions its moves carry as the Git backend
// read them: the cases the fixture is for, the staged Trash moves and
// their refusals, the listing and confirmation, and the same with an
// exclusion after a restart with cold caches.
func TestDatabaseTrashReadsAnImportedHistory(t *testing.T) {
	g := trashHistoryFixture(t)
	s := importedTestStore(t, g, nil)
	ctx := context.Background()
	if headOf(t, s) != headOf(t, g) {
		t.Fatal("the import is at another head")
	}
	for _, path := range []string{"restored.md", "deep/er/m.md", "fresh.md", "shelf/kept.md", "reborn.md", "other.md", "broken.md"} {
		if _, err := trashHistoryOf(s, path); err != nil {
			t.Fatal(path, err)
		}
	}
	// The cases the fixture is for, spelled out.
	restored, _ := trashHistoryOf(s, "restored.md")
	if len(restored["restored.md"]) != 2 || len(restored["archive/2026/a.md"]) != 2 || len(restored["notes/a.md"]) != 2 || len(restored) != 3 {
		t.Error("the restored note's generations:", restored)
	}
	if fresh, _ := trashHistoryOf(s, "fresh.md"); len(fresh["fresh.md"]) != 1 || len(fresh) != 1 {
		t.Error("a reused path inherited the old note's history:", fresh)
	}
	if reborn, _ := trashHistoryOf(s, "reborn.md"); len(reborn) != 1 || len(reborn["reborn.md"]) != 1 {
		t.Error("a reused fmn-id inherited the deleted note's history:", reborn)
	}
	if kept, _ := trashHistoryOf(s, "shelf/kept.md"); len(kept["shelf/kept.md"]) != 2 || len(kept["kept.md"]) != 1 {
		t.Error("the moved note's history:", kept)
	}
	if moved, _ := trashHistoryOf(s, "deep/er/m.md"); len(moved["deep/er/m.md"]) != 1 || len(moved["m.md"]) != 1 {
		t.Error("the note moved with rewritten links:", moved)
	}
	// The import gave the moved note a new entry, so only the Move subject
	// leads the walk to its source.
	if f, err := s.notes.Stat(ctx, gitobj.ID{}, "deep/er/m.md"); err != nil {
		t.Error(err)
	} else if h, err := s.notes.History(ctx, notestore.HistoryQuery{EntryID: f.EntryID}); err != nil || len(h) != 1 {
		t.Error("the moved note kept its entry:", len(h), err)
	}

	paths := []string{"restored.md", "fresh.md", "shelf/kept.md", "reborn.md", "other.md"}
	prepared, err := prepareTrashOf(s, paths)
	if err != nil || len(prepared.contents) == 0 {
		t.Fatalf("prepared Trash: %+v %v", prepared, err)
	}
	for _, path := range []string{"restored.md", "shelf/kept.md"} {
		for _, prepare := range []bool{true, false} {
			staged, err := stageTrashOf(s, path, prepare)
			if err != nil || staged.Entry.OriginalPath != path || staged.Changes[path] != "<deleted>" {
				t.Errorf("staged Trash of %s (prepared %v): %+v %v", path, prepare, staged, err)
			}
		}
	}
	if staged, _ := stageTrashOf(s, "restored.md", true); !strings.Contains(staged.Changes["<discussion>"], "Keep this discussion") || staged.Changes[commentPath("restored.md")] != "<deleted>" {
		t.Error("the discussion did not move with its note:", staged.Changes)
	}
	for _, c := range []struct {
		path    string
		prepare bool
	}{{"Trash/x.md", false}, {"AGENTS.md", false}, {"missing.md", true}, {"missing.md", false}, {"broken.md", true}, {"broken.md", false}, {"assets/x.png", false}} {
		if _, err := stageTrashOf(s, c.path, c.prepare); err == nil {
			t.Errorf("staged %s (prepared %v)", c.path, c.prepare)
		}
	}

	listing, err := s.ListTrash(ctx)
	if err != nil || len(listing.Entries) != 2 || listing.Revision != headOf(t, s) || listing.Confirmation == "" {
		t.Errorf("listing: %+v %v", listing, err)
	}
	policy, err := s.TrashPolicy(ctx)
	if err != nil || len(policy.Records) != 3 {
		t.Errorf("policy: %+v %v", policy, err)
	}

	// Excluded notes are hidden, after a restart with cold caches.
	s = reopenTestStoreExcluding(t, s, []string{"private"})
	listing, err = s.ListTrash(ctx)
	if err != nil || len(listing.Entries) != 1 || listing.Entries[0].OriginalPath != "later.md" {
		t.Errorf("listing with exclusions: %+v %v", listing, err)
	}
	policy, err = s.TrashPolicy(ctx)
	hidden := 0
	for _, r := range policy.Records {
		if r.Hidden {
			hidden++
		}
	}
	if err != nil || len(policy.Records) != 3 || hidden != 1 {
		t.Errorf("policy with exclusions: %+v %v", policy, err)
	}
	if _, err := stageTrashOf(s, "private/other.md", false); !errors.Is(err, ErrForbidden) {
		t.Error("staged an excluded note:", err)
	}
}

// Notebooks FMN writes are linear. A merge in a note's history, which only
// an imported repository can have, is refused rather than recorded in part;
// a note the merge left alone keeps its history.
func TestDatabaseTrashHistoryRefusesMergesItCannotFollow(t *testing.T) {
	git := newHistoryRepository(t)
	git.commit("Update a.md", nil, map[string][]byte{"a.md": []byte("# A\n")})
	git.commit("Update b.md", nil, map[string][]byte{"b.md": []byte("# B\n")})
	git.git("checkout", "-q", "-b", "side")
	git.commit("Update a.md", nil, map[string][]byte{"a.md": []byte("# A\n\nOn the side.\n")})
	git.git("checkout", "-q", "main")
	git.commit("Update b.md", nil, map[string][]byte{"b.md": []byte("# B\n\nOn main.\n")})
	git.git("merge", "-q", "--no-ff", "-m", "Merge side", "side")
	s := openImportedTestStore(t, nil, func(notes *notestore.Store) error {
		_, err := gitexport.ImportGitDir(context.Background(), notes.Notebook("notes"), filepath.Join(git.root, ".git"))
		return err
	})
	if got, err := trashHistoryOf(s, "b.md"); err != nil || len(got) != 1 || len(got["b.md"]) != 2 {
		t.Errorf("history of b.md: %v %v", got, err)
	}
	if got, err := trashHistoryOf(s, "a.md"); errorText(err) != "cannot establish this note's complete history" {
		t.Errorf("history across a merge: %v %v", got, err)
	}
}

// commitTrash moves paths into the Trash as one revision, the way a mutation
// built on the staging helpers does, keeping each note's identity.
func commitTrash(t *testing.T, s *Store, paths ...string) ([]TrashEntry, string) {
	t.Helper()
	n, ctx := backendOf(s), context.Background()
	n.mu.Lock()
	defer n.mu.Unlock()
	index, err := n.loadTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head := n.trashRevision
	prepared, err := n.prepareTrash(ctx, paths, index, head)
	if err != nil {
		t.Fatal(err)
	}
	changes, renames := map[string][]byte{}, map[string]string{}
	var entries []TrashEntry
	for _, path := range paths {
		entry, err := n.stageTrash(ctx, &index, path, "owner", changes, &prepared)
		if err != nil {
			t.Fatal(err)
		}
		addTrashMove(renames, changes, entry.OriginalPath, entry.Path)
		entries = append(entries, entry)
	}
	if err = stageTrashIndex(index, len(paths), changes); err != nil {
		t.Fatal(err)
	}
	result, err := n.applyChanges(ctx, head, "Move notes to Trash", orderedChanges(changes, renames))
	if err != nil {
		t.Fatal(err)
	}
	return entries, result.Head.Commit.String()
}

// commitRestore restores a Trash entry to to as one revision, keeping its
// identity when keep is set and writing it as a new file otherwise.
func commitRestore(t *testing.T, s *Store, entry TrashEntry, to string, keep bool) string {
	t.Helper()
	n, ctx := backendOf(s), context.Background()
	n.mu.Lock()
	defer n.mu.Unlock()
	index, err := n.loadTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := n.read(ctx, entry.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string][]byte{entry.Path: nil, to: rewriteLinks(doc.Content, entry.Path, to, entry.Path, to)}
	if err = n.stageTrashDiscussion(ctx, entry.Path, to, changes); err != nil {
		t.Fatal(err)
	}
	for i := range index.Records {
		if index.Records[i].ID == entry.ID {
			index.Records[i].Restored = true
		}
	}
	if err = stageTrashIndex(index, 0, changes); err != nil {
		t.Fatal(err)
	}
	renames := map[string]string{}
	if keep {
		addTrashMove(renames, changes, entry.Path, to)
	}
	result, err := n.applyChanges(ctx, n.trashRevision, "Restore note from Trash", orderedChanges(changes, renames))
	if err != nil {
		t.Fatal(err)
	}
	return result.Head.Commit.String()
}

// A Trash move staged by the helpers commits as one revision that keeps
// the note's and its discussion's identities; the record keeps the note's
// history, which a restore, with or without the identity, continues.
func TestDatabaseTrashStagingCommitsAsOneRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first := saveNote(t, s, "notes/a.md", "# A\n\nSee [other](../other.md).\n")
	saveNote(t, s, "other.md", "# Other\n")
	discussion, _ := json.MarshalIndent([]Comment{{ID: "c1", Author: "agent", Text: "Check this", Created: "2026-11-22T00:00:00Z", Finding: &Finding{Key: "k", Path: "notes/a.md", Title: "Check"}}}, "", "  ")
	commitFixture(t, s, fixture(commentPath("notes/a.md"), string(discussion)))
	before, err := s.notes.Stat(ctx, gitobj.ID{}, "notes/a.md")
	if err != nil {
		t.Fatal(err)
	}
	beforeDiscussion, _ := s.notes.Stat(ctx, gitobj.ID{}, commentPath("notes/a.md"))

	entries, trashed := commitTrash(t, s, "notes/a.md")
	entry := entries[0]
	if _, err := s.Read(ctx, "notes/a.md", ""); !errors.Is(err, os.ErrNotExist) {
		t.Error("the note stayed at its path:", err)
	}
	if doc, err := s.Read(ctx, entry.Path, ""); err != nil || string(doc.Content) != "# A\n\nSee [other](../other.md).\n" {
		t.Errorf("the note in the Trash: %q %v", doc.Content, err)
	}
	if f, err := s.notes.Stat(ctx, gitobj.ID{}, entry.Path); err != nil || f.EntryID != before.EntryID {
		t.Error("the trashed note lost its identity:", f, err)
	}
	if f, err := s.notes.Stat(ctx, gitobj.ID{}, commentPath(entry.Path)); err != nil || f.EntryID != beforeDiscussion.EntryID {
		t.Error("the discussion lost its identity:", f, err)
	}
	d, err := s.Comments(ctx, entry.Path)
	if err != nil || len(d.Comments) != 1 || d.Comments[0].Finding.Path != entry.Path || d.Revision != trashed {
		t.Errorf("the discussion in the Trash: %+v %v", d, err)
	}
	info, err := s.notes.Commit(ctx, mustParse(t, trashed))
	if err != nil || commitSubject(info.Commit.Message) != "Move notes to Trash" || len(info.Changes) != 3 {
		t.Errorf("the Trash revision: %+v %v", info, err)
	}
	listing, err := s.ListTrash(ctx)
	if err != nil || listing.Revision != trashed || len(listing.Entries) != 1 || listing.Entries[0] != entry || entry.OriginalPath != "notes/a.md" || entry.Title != "A" || entry.DeletedBy != "owner" {
		t.Errorf("the listing: %+v %v", listing, err)
	}
	policy, err := s.TrashPolicy(ctx)
	if err != nil || len(policy.Records) != 1 || !reflect.DeepEqual(policy.Records[0].History, map[string][]string{"notes/a.md": {first.Revision}}) {
		t.Errorf("the record: %+v %v", policy, err)
	}
	if again, err := reopenTestStore(t, s).ListTrash(ctx); err != nil || !reflect.DeepEqual(again, listing) {
		t.Errorf("the listing after a restart: %+v %v", again, err)
	}

	restored := commitRestore(t, s, entry, "restored.md", true)
	edited := saveNote(t, s, "restored.md", "# A\n\nSee [other](other.md), again.\n")
	history, err := trashHistoryOf(s, "restored.md")
	if want := map[string][]string{"restored.md": {edited.Revision, restored}, "notes/a.md": {first.Revision}}; err != nil || !reflect.DeepEqual(history, want) {
		t.Errorf("the restored note's history: %v %v, want %v", history, err, want)
	}
	if d, err := s.Comments(ctx, "restored.md"); err != nil || len(d.Comments) != 1 || d.Comments[0].Finding.Path != "restored.md" {
		t.Errorf("the restored discussion: %+v %v", d, err)
	}

	// A restore written as a new file reconnects to its record by its subject.
	plain := saveNote(t, s, "plain.md", "# Plain\n")
	plainFile, _ := s.notes.Stat(ctx, gitobj.ID{}, "plain.md")
	entries, _ = commitTrash(t, s, "plain.md")
	restored = commitRestore(t, s, entries[0], "deeper/plain.md", false)
	if f, _ := s.notes.Stat(ctx, gitobj.ID{}, "deeper/plain.md"); f.EntryID == plainFile.EntryID {
		t.Fatal("the fixture kept the identity")
	}
	history, err = trashHistoryOf(s, "deeper/plain.md")
	if want := map[string][]string{"deeper/plain.md": {restored}, "plain.md": {plain.Revision}}; err != nil || !reflect.DeepEqual(history, want) {
		t.Errorf("the reconnected history: %v %v, want %v", history, err, want)
	}

	// Two notes in one revision each keep their own history.
	one, two := saveNote(t, s, "one.md", "# One\n"), saveNote(t, s, "two.md", "# Two\n")
	entries, _ = commitTrash(t, s, "one.md", "two.md")
	policy, _ = s.TrashPolicy(ctx)
	for _, r := range policy.Records {
		switch r.ID {
		case entries[0].ID:
			if !reflect.DeepEqual(r.History, map[string][]string{"one.md": {one.Revision}}) {
				t.Error("one.md's record:", r.History)
			}
		case entries[1].ID:
			if !reflect.DeepEqual(r.History, map[string][]string{"two.md": {two.Revision}}) {
				t.Error("two.md's record:", r.History)
			}
		}
	}
}

// The DB backend's Trash walk keeps the Git walk's boundaries for revisions
// it writes itself: a move without the note's identity is followed by its
// Move subject, a new note at a path or under an old fmn-id starts over.
func TestDatabaseTrashHistoryKeepsGenerationBoundaries(t *testing.T) {
	s := newTestStore(t)
	n, ctx := backendOf(s), context.Background()
	created := saveNote(t, s, "a.md", "# A\n")
	s.mu.Lock()
	moved, err := n.applyChanges(ctx, created.Revision, `Move "a.md" to "b.md"`, orderedChanges(map[string][]byte{"a.md": nil, "b.md": []byte("# B\n\nRewritten.\n")}, nil))
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	history, err := trashHistoryOf(s, "b.md")
	if want := map[string][]string{"b.md": {moved.Head.Commit.String()}, "a.md": {created.Revision}}; err != nil || !reflect.DeepEqual(history, want) {
		t.Errorf("a move without identity: %v %v, want %v", history, err, want)
	}

	id, _ := NewNoteID()
	content, _ := WithNoteID([]byte("# Gone\n"), id)
	gone := saveNote(t, s, "gone.md", string(content))
	s.mu.Lock()
	removed, err := n.applyChanges(ctx, gone.Revision, "Remove gone.md", orderedChanges(map[string][]byte{"gone.md": nil}, nil))
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	content, _ = WithNoteID([]byte("# Reborn\n"), id)
	reborn := saveNote(t, s, "reborn.md", string(content))
	if f, _ := s.notes.Stat(ctx, gitobj.ID{}, "reborn.md"); f.EntryID != id {
		t.Fatal("the fixture gave the new note another identity")
	}
	history, err = trashHistoryOf(s, "reborn.md")
	if want := map[string][]string{"reborn.md": {reborn.Revision}}; err != nil || !reflect.DeepEqual(history, want) {
		t.Errorf("a reused fmn-id: %v %v, want %v (removed at %s)", history, err, want, removed.Head.Commit)
	}

	edited := saveNote(t, s, "b.md", "# B\n\nEdited.\n")
	s.mu.Lock()
	_, err = n.applyChanges(ctx, edited.Revision, "Replace b.md", orderedChanges(map[string][]byte{"b.md": nil}, nil))
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	again := saveNote(t, s, "b.md", "# B\n\nA new note.\n")
	history, err = trashHistoryOf(s, "b.md")
	if want := map[string][]string{"b.md": {again.Revision}}; err != nil || !reflect.DeepEqual(history, want) {
		t.Errorf("a reused path: %v %v, want %v", history, err, want)
	}
}

// The staging helpers refuse what their originals refuse: notes that are not
// eligible, locked notes, an occupied discussion and an oversized batch.
func TestDatabaseTrashStagingRefusals(t *testing.T) {
	s := newTestStore(t)
	n, ctx := backendOf(s), context.Background()
	saveNote(t, s, "a.md", "# A\n")
	saveNote(t, s, "locked.md", "# Locked\n")
	commitFixture(t, s, fixture(locksPath, `{"version":1,"paths":["locked.md"]}`), fixture(commentPath("a.md"), "[]"), fixture(commentPath("b.md"), "[]"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := n.loadLocks(ctx); err != nil {
		t.Fatal(err)
	}
	index, err := n.loadTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.stageTrash(ctx, &index, "locked.md", "owner", map[string][]byte{}, nil); !errors.Is(err, ErrForbidden) {
		t.Error("staged a locked note:", err)
	}
	if _, err := n.prepareTrash(ctx, []string{"a.md", "missing.md"}, index, n.trashRevision); !errors.Is(err, os.ErrNotExist) {
		t.Error("prepared a missing note:", err)
	}
	if err := n.stageTrashDiscussion(ctx, "a.md", "b.md", map[string][]byte{}); errorText(err) != "destination already has a discussion" {
		t.Error("moved a discussion onto another:", err)
	}
	changes := map[string][]byte{}
	if err := n.stageTrashDiscussion(ctx, "none.md", "b.md", changes); err != nil || len(changes) != 0 {
		t.Error("staged a discussion that does not exist:", changes, err)
	}
	if len(index.Records) != 0 {
		t.Error("a refused staging recorded a note:", index.Records)
	}
	t.Setenv(capacity.IndexBytes.Env(), "4")
	if _, err := n.prepareTrash(ctx, []string{"a.md"}, index, n.trashRevision); errorText(err) != "Trash transaction exceeds FMN_LIMIT_INDEX_BYTES; move fewer notes at a time" {
		t.Error("prepared beyond the budget:", err)
	}
}

// The listing and policy read the committed index of the head: an empty
// Trash, an index another gateway committed, and an invalid one.
func TestDatabaseTrashListingReadsTheCommittedIndex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	empty, err := s.ListTrash(ctx)
	head := headOf(t, s)
	want := TrashListing{Revision: head, Entries: []TrashEntry{}, Confirmation: ContentHash([]byte("empty-trash\x00" + head + "\x00" + `{"version":1,"records":[]}`))}
	if err != nil || !reflect.DeepEqual(empty, want) {
		t.Errorf("an empty Trash: %+v %v", empty, err)
	}
	if policy, err := s.TrashPolicy(ctx); err != nil || policy.Version != 1 || policy.Records == nil || len(policy.Records) != 0 {
		t.Errorf("an empty policy: %+v %v", policy, err)
	}

	entry := TrashEntry{ID: strings.Repeat("a", 32), Path: "Trash/" + strings.Repeat("a", 32) + ".md", OriginalPath: "a.md", Title: "A", DeletedAt: "2026-11-22T00:00:00Z", DeletedBy: "owner"}
	emptied := TrashEntry{ID: strings.Repeat("b", 32), Path: "Trash/" + strings.Repeat("b", 32) + ".md", OriginalPath: "b.md", Title: "B"}
	index := TrashIndex{Version: 1, Records: []TrashRecord{{TrashEntry: entry, History: map[string][]string{"a.md": {head}}}, {TrashEntry: emptied, History: map[string][]string{}, Emptied: true}}}
	changes := map[string][]byte{}
	if err := stageTrashIndex(index, 0, changes); err != nil {
		t.Fatal(err)
	}
	committed := commitFixture(t, s, fixture(trashIndexPath, string(changes[trashIndexPath])), fixture(entry.Path, "# A\n"))
	listing, err := s.ListTrash(ctx)
	raw, _ := json.Marshal(index)
	if err != nil || listing.Revision != committed || !reflect.DeepEqual(listing.Entries, []TrashEntry{entry}) || listing.Confirmation != ContentHash(append([]byte("empty-trash\x00"+committed+"\x00"), raw...)) {
		t.Errorf("a committed index: %+v %v", listing, err)
	}
	policy, err := s.TrashPolicy(ctx)
	if err != nil || !reflect.DeepEqual(policy, index) {
		t.Errorf("a committed policy: %+v %v", policy, err)
	}
	unrelated := commitFixture(t, s, fixture("c.md", "# C\n"))
	if again, err := s.ListTrash(ctx); err != nil || again.Revision != unrelated || again.Confirmation == listing.Confirmation {
		t.Error("the confirmation outlived its revision:", again, err)
	}

	commitFixture(t, s, fixture(trashIndexPath, `{"version":2,"records":[]}`))
	const damaged = "the notebook's Trash is damaged (.gitnotes-trash.json: invalid Trash index); restore the notebook from a backup"
	if _, err := s.ListTrash(ctx); errorText(err) != damaged {
		t.Error("listed an invalid index:", err)
	}
	if policy, err := s.TrashPolicy(ctx); errorText(err) != damaged || policy.Version != 1 || len(policy.Records) != 0 {
		t.Error("an invalid policy:", policy, err)
	}
}

// findTrashOf is what a search that includes the Trash finds in it, as
// FindScoped answers it.
func findTrashOf(s *Store, query, tag, status, path string) ([]Entry, error) {
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "include", Drafts: "include"}, Collection: "all"}
	found, err := s.FindScoped(context.Background(), query, tag, status, path, true, 0, scope)
	var trash []Entry
	for _, e := range found.Entries {
		if e.TrashID != "" {
			trash = append(trash, e)
		}
	}
	return trash, err
}

// A Trash move and a restore keep the note's and its discussion's
// identities in the notes store, so their histories follow them; the
// Trash, its index and the restored notes survive a restart.
func TestDatabaseTrashKeepsIdentitiesAndSurvivesRestart(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first := saveNote(t, s, "notes/a.md", "# A\n\nSee [other](../other.md).\n")
	d, err := s.AddComment(ctx, "notes/a.md", "Keep this discussion", first.Revision, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	note, _ := s.notes.Stat(ctx, gitobj.ID{}, "notes/a.md")
	discussion, _ := s.notes.Stat(ctx, gitobj.ID{}, commentPath("notes/a.md"))
	trashed, err := s.TrashNotes(ctx, []string{"notes/a.md"}, d.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	entry := trashed.Entries[0]
	if trashed.Revision != headOf(t, s) || entry.OriginalPath != "notes/a.md" || entry.Title != "A" || entry.DeletedBy != "owner" || entry.Path != "Trash/"+entry.ID+".md" {
		t.Fatalf("the move: %+v", trashed)
	}
	if f, err := s.notes.Stat(ctx, gitobj.ID{}, entry.Path); err != nil || f.EntryID != note.EntryID {
		t.Error("the trashed note lost its identity:", f, err)
	}
	if f, err := s.notes.Stat(ctx, gitobj.ID{}, commentPath(entry.Path)); err != nil || f.EntryID != discussion.EntryID {
		t.Error("the trashed discussion lost its identity:", f, err)
	}
	info, err := s.notes.Commit(ctx, mustParse(t, trashed.Revision))
	if err != nil || commitSubject(info.Commit.Message) != "Move notes to Trash" || len(info.Changes) != 3 {
		t.Errorf("the Trash revision: %+v %v", info, err)
	}

	s = reopenTestStore(t, s)
	listing, err := s.ListTrash(ctx)
	if err != nil || len(listing.Entries) != 1 || listing.Entries[0] != entry || listing.Revision != trashed.Revision {
		t.Fatalf("the Trash after a restart: %+v %v", listing, err)
	}
	restored, err := s.RestoreTrash(ctx, entry.ID, "", listing.Revision, "owner")
	if err != nil || len(restored.Entries) != 1 || restored.Entries[0].Path != "notes/a.md" || restored.Revision != headOf(t, s) {
		t.Fatalf("the restore: %+v %v", restored, err)
	}
	if f, err := s.notes.Stat(ctx, gitobj.ID{}, "notes/a.md"); err != nil || f.EntryID != note.EntryID {
		t.Error("the restored note lost its identity:", f, err)
	}
	if f, err := s.notes.Stat(ctx, gitobj.ID{}, commentPath("notes/a.md")); err != nil || f.EntryID != discussion.EntryID {
		t.Error("the restored discussion lost its identity:", f, err)
	}
	if doc, err := s.Read(ctx, "notes/a.md", ""); err != nil || string(doc.Content) != "# A\n\nSee [other](../other.md).\n" {
		t.Errorf("the restored note: %q %v", doc.Content, err)
	}
	if d, err := s.Comments(ctx, "notes/a.md"); err != nil || len(d.Comments) != 1 {
		t.Errorf("the restored discussion: %+v %v", d, err)
	}
	history, err := s.notes.History(ctx, notestore.HistoryQuery{EntryID: note.EntryID})
	if err != nil || len(history) != 3 {
		t.Errorf("the note's history: %d revisions, %v", len(history), err)
	}
	if again, err := reopenTestStore(t, s).TrashPolicy(ctx); err != nil || len(again.Records) != 1 || !again.Records[0].Restored {
		t.Errorf("the policy after another restart: %+v %v", again, err)
	}
}

// Moves into and out of the Trash checkpoint pending live edits first, and
// commit nothing when they cannot; emptying it never checkpoints unrelated
// live text.
func TestDatabaseTrashChecksLiveEditsFirstAndSurvivesRestart(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveNote(t, s, "live.md", "# Live\n")
	doc := saveNote(t, s, "a.md", "# A\n")
	trashed, err := s.TrashNotes(ctx, []string{"a.md"}, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	saveNote(t, s, "b.md", "# B\n")
	head := headOf(t, s)
	s.mu.Lock()
	s.live["live.md"] = &liveRecord{Path: "live.md", Uncommitted: []string{"op"}}
	s.mu.Unlock()
	if _, err := s.TrashNotes(ctx, []string{"b.md"}, head, "owner"); !errors.Is(err, ErrLivePending) {
		t.Error("a Trash move past pending live edits:", err)
	}
	if _, err := s.RestoreTrash(ctx, trashed.Entries[0].ID, "", head, "owner"); !errors.Is(err, ErrLivePending) {
		t.Error("a restore past pending live edits:", err)
	}
	if headOf(t, s) != head {
		t.Fatal("a refused move committed")
	}
	listing, err := s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	emptied, err := s.EmptyTrash(ctx, listing.Revision, listing.Confirmation)
	if err != nil || len(emptied.Entries) != 1 || emptied.Revision == head {
		t.Fatalf("emptying beside pending live edits: %+v %v", emptied, err)
	}
	s.mu.Lock()
	pending := len(s.live["live.md"].Uncommitted)
	s.live["live.md"].Uncommitted = nil
	s.mu.Unlock()
	if pending != 1 {
		t.Error("emptying the Trash touched unrelated live edits")
	}
	s = reopenTestStore(t, s)
	if listing, err := s.ListTrash(ctx); err != nil || len(listing.Entries) != 0 || listing.Revision != emptied.Revision {
		t.Errorf("the emptied Trash after a restart: %+v %v", listing, err)
	}
	if policy, err := s.TrashPolicy(ctx); err != nil || len(policy.Records) != 1 || !policy.Records[0].Emptied {
		t.Errorf("the tombstone after a restart: %+v %v", policy, err)
	}
	if old, err := s.Read(ctx, "a.md", doc.Revision); err != nil || string(old.Content) != "# A\n" {
		t.Errorf("emptying erased history: %q %v", old.Content, err)
	}
}

// Notes trashed from a folder the notebook later excludes stay out of the
// Trash's listing and search after a restart, and the Trash is not emptied
// past them.
func TestDatabaseTrashKeepsBrowseExclusionsAfterRestart(t *testing.T) {
	for _, excluded := range []string{"private", "Trash"} {
		t.Run(excluded, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			doc, err := s.Save(ctx, "private/excluded.md", []byte("excluded content"), "", "writer")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.TrashNotes(ctx, []string{doc.Path}, doc.Revision, "owner"); err != nil {
				t.Fatal(err)
			}
			env := testEnv(t, s)
			s.Close()
			reopened, err := OpenDB(ctx, openTestNotes(t, env), "notes", filepath.Dir(s.Root), []string{excluded})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			listing, err := reopened.ListTrash(ctx)
			if err != nil || len(listing.Entries) != 0 {
				t.Fatal("excluded note listed", err)
			}
			snapshot, err := reopened.Snapshot(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range snapshot.Files {
				if IsTrash(f.Path) {
					t.Fatal("excluded note reentered search snapshot")
				}
			}
			entries, err := findTrashOf(reopened, "excluded content", "", "", "")
			if err != nil || len(entries) != 0 {
				t.Fatalf("excluded note included in search: %d entries, %v", len(entries), err)
			}
			if _, err = reopened.EmptyTrash(ctx, listing.Revision, listing.Confirmation); errorText(err) != "Trash contains excluded notes; review notebook exclusions before emptying" {
				t.Fatal("emptied past excluded notes:", err)
			}
		})
	}
}

// Trash mutations on two gateways from the same revision have one winner,
// even before the loser hears of the winner's commit.
func TestDatabaseTrashOnTwoGatewaysHasOneWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	other, err := OpenDB(ctx, openTestNotes(t, testEnv(t, s)), "notes", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	saveNote(t, s, "a.md", "# A\n")
	doc := saveNote(t, s, "b.md", "# B\n")
	caughtUp := func(revision string) {
		for {
			if listing, err := other.ListTrash(ctx); err == nil && listing.Revision == revision {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	caughtUp(doc.Revision)
	first, err := s.TrashNotes(ctx, []string{"a.md"}, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.TrashNotes(ctx, []string{"b.md"}, doc.Revision, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatal("a second Trash move from the same revision:", err)
	}
	caughtUp(first.Revision)
	listing, err := other.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := s.RestoreTrash(ctx, first.Entries[0].ID, "", first.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.EmptyTrash(ctx, listing.Revision, listing.Confirmation); !errors.Is(err, ErrConflict) {
		t.Fatal("emptied a Trash restored elsewhere:", err)
	}
	if _, err := other.RestoreTrash(ctx, first.Entries[0].ID, "elsewhere.md", first.Revision, "owner"); !errors.Is(err, ErrConflict) && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restored a note twice:", err)
	}
	if doc, err := s.Read(ctx, "a.md", ""); err != nil || doc.Revision != restored.Revision {
		t.Fatal("the restored note:", doc, err)
	}
	if _, err := s.Read(ctx, "elsewhere.md", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the note was restored twice:", err)
	}
}

// A restore never overwrites what the head has at its destination, not even
// a note where one of its folders would be.
func TestDatabaseRestoreRefusesOccupiedDestinations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveNote(t, s, "a.md", "# A\n")
	doc := saveNote(t, s, "taken.md", "# Taken\n")
	trashed, err := s.TrashNotes(ctx, []string{"a.md"}, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"taken.md", "taken.md/a.md", "taken.md/deeper/a.md"} {
		if _, err := s.RestoreTrash(ctx, trashed.Entries[0].ID, to, trashed.Revision, "owner"); errorText(err) != "restore destination is occupied: "+ErrConflict.Error() || !errors.Is(err, ErrConflict) {
			t.Errorf("a restore to %s: %v", to, err)
		}
	}
	if restored, err := s.RestoreTrash(ctx, trashed.Entries[0].ID, "free/a.md", trashed.Revision, "owner"); err != nil || restored.Entries[0].Path != "free/a.md" {
		t.Fatal("a restore to a free path:", restored, err)
	}
}

// Emptying the Trash refuses, and commits nothing, when a folder lies where
// a Trash note's discussion belongs, which Git could not remove either.
func TestDatabaseEmptyTrashRefusesAFolderForADiscussion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc := saveNote(t, s, "a.md", "# A\n")
	trashed, err := s.TrashNotes(ctx, []string{"a.md"}, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	commitFixture(t, s, fixture(commentPath(trashed.Entries[0].Path)+"/x", "not a discussion"))
	listing, err := s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EmptyTrash(ctx, listing.Revision, listing.Confirmation); !errors.Is(err, ErrForbidden) {
		t.Fatal("emptied past a folder at a discussion path:", err)
	}
	if again, err := s.ListTrash(ctx); err != nil || again.Revision != listing.Revision || len(again.Entries) != 1 {
		t.Fatal("a refused empty changed the Trash:", again, err)
	}
}

// earlierBuildCommit commits files on top of s's head as an earlier storage
// v2 build could, which let a revision write a name Git refuses, such as
// git~1. Every writer refuses such names now, so the commit's new trees go
// straight into the database, where ImportCommit finds them stored.
func earlierBuildCommit(t *testing.T, s *Store, files map[string]string) string {
	t.Helper()
	ctx := context.Background()
	head, err := s.notes.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	trees := map[gitobj.ID][]byte{}
	var root gitobj.ID
	if err := s.notes.Objects(ctx, 0, func(kind string, id gitobj.ID, body []byte) error {
		if kind == "tree" {
			trees[id] = body
		}
		if kind == "commit" && id == head.Commit {
			c, err := gitobj.ParseCommit(body)
			root = c.Tree
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	blobs, added := map[gitobj.ID][]byte{}, map[gitobj.ID][]byte{}
	// put returns tree with blob written at path, adding the trees it makes.
	var put func(tree gitobj.ID, path []string, blob gitobj.ID) gitobj.ID
	put = func(tree gitobj.ID, path []string, blob gitobj.ID) gitobj.ID {
		var entries []gitobj.TreeEntry
		if body, ok := trees[tree]; ok {
			var err error
			if entries, err = gitobj.ParseTree(body); err != nil {
				t.Fatal(err)
			}
		}
		entry := gitobj.TreeEntry{Name: path[0], Mode: gitobj.ModeFile, ID: blob}
		i := slices.IndexFunc(entries, func(e gitobj.TreeEntry) bool { return e.Name == path[0] })
		if len(path) > 1 {
			var sub gitobj.ID
			if i >= 0 {
				sub = entries[i].ID
			}
			entry = gitobj.TreeEntry{Name: path[0], Mode: gitobj.ModeTree, ID: put(sub, path[1:], blob)}
		}
		if i >= 0 {
			entries[i] = entry
		} else {
			entries = append(entries, entry)
		}
		body := gitobj.EncodeTree(entries)
		id := gitobj.HashObject("tree", body)
		trees[id], added[id] = body, body
		return id
	}
	for path, content := range files {
		blob := gitobj.BlobID([]byte(content))
		blobs[blob] = []byte(content)
		root = put(root, strings.Split(path, "/"), blob)
	}
	db := testEnv(t, s).db
	for id, body := range added {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO git_trees (org, notebook, tree_sha, body) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, db.Org, s.notebook, id[:], body); err != nil {
			t.Fatal(err)
		}
	}
	sig := gitobj.Signature{Name: "Ada", Email: "ada@example.com", When: time.Now().Unix(), TZ: "+0000"}
	raw := gitobj.EncodeCommit(gitobj.Commit{Tree: root, Parents: []gitobj.ID{head.Commit}, Author: sig, Committer: sig, Message: "Update notes from an earlier build\n"})
	next, err := s.notes.ImportCommit(ctx, raw, nil, func(id gitobj.ID) ([]byte, error) {
		if content, ok := blobs[id]; ok {
			return content, nil
		}
		return nil, fs.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	return next.Commit.String()
}

// A note an earlier storage v2 build stored under a name Git refuses can be
// moved into the Trash, alone or by an edit group's delete, as Move can move
// it elsewhere: the path it leaves is read, never written. Writing it there
// again, or restoring it there, stays refused; it can be restored elsewhere.
func TestDatabaseTrashTakesNotesStoredUnderNamesGitRefuses(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveNote(t, s, "keep.md", "# Keep\n")
	commitFixture(t, s, fixture(locksPath, `{"version":1,"paths":["GIT~1 /locked.md"]}`))
	head := earlierBuildCommit(t, s, map[string]string{"git~1/b.md": "# B\n", "git~1/c.md": "# C\n", "GIT~1 /locked.md": "# Locked\n"})
	if doc, err := s.Read(ctx, "git~1/b.md", ""); err != nil || string(doc.Content) != "# B\n" {
		t.Fatalf("reading the note: %q %v", doc.Content, err)
	}
	if _, err := s.Save(ctx, "git~1/b.md", []byte("# Changed\n"), head, "writer"); !errors.Is(err, ErrForbidden) {
		t.Fatal("wrote the note where it is:", err)
	}
	if _, err := s.TrashNotes(ctx, []string{"GIT~1 /locked.md"}, head, "writer"); !errors.Is(err, ErrForbidden) {
		t.Fatal("trashed a locked note:", err)
	}
	trashed, err := s.TrashNotes(ctx, []string{"git~1/b.md"}, head, "writer")
	if err != nil || len(trashed.Entries) != 1 || trashed.Entries[0].OriginalPath != "git~1/b.md" {
		t.Fatalf("moving the note into the Trash: %+v %v", trashed, err)
	}
	c, err := s.Read(ctx, "git~1/c.md", "")
	if err != nil {
		t.Fatal(err)
	}
	group := EditGroup{ID: "delete c", Summary: "Discard c", Edits: []NoteEdit{{Path: "git~1/c.md", Revision: c.Revision, Hash: ContentHash(c.Content), Delete: true}}}
	if _, err := s.ApplyGroup(ctx, group); err != nil {
		t.Fatal("an edit group deleting the note:", err)
	}
	files, err := s.notes.Files(ctx, gitobj.ID{})
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, f := range files {
		if strings.HasPrefix(f.Path, "git~1/") {
			left = append(left, f.Path)
		}
	}
	if len(left) != 0 {
		t.Fatal("still in the folder Git refuses:", left)
	}
	listing, err := s.ListTrash(ctx)
	if err != nil || len(listing.Entries) != 2 {
		t.Fatalf("the Trash: %+v %v", listing, err)
	}
	if _, err := s.RestoreTrash(ctx, trashed.Entries[0].ID, "", headOf(t, s), "writer"); !errors.Is(err, ErrForbidden) {
		t.Fatal("restored the note under the name Git refuses:", err)
	}
	restored, err := s.RestoreTrash(ctx, trashed.Entries[0].ID, "b.md", headOf(t, s), "writer")
	if err != nil {
		t.Fatal("restoring the note elsewhere:", err)
	}
	if doc, err := s.Read(ctx, "b.md", restored.Revision); err != nil || string(doc.Content) != "# B\n" {
		t.Fatalf("the restored note: %q %v", doc.Content, err)
	}
}
