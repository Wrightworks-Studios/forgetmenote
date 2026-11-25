package workspace

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// A bundle restore keeps the Trash and its notes restorable.
func TestTrashSurvivesABundleRestore(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "discard.md", []byte("original"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	moved, err := s.TrashNotes(ctx, []string{doc.Path}, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	restored := restore(t, s)
	listing, err := restored.ListTrash(ctx)
	if err != nil || len(listing.Entries) != 1 {
		t.Fatal(listing, err)
	}
	if _, err = restored.RestoreTrash(ctx, moved.Entries[0].ID, "", listing.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
}

// The Trash's archives travel with a bundle, and a handoff snapshot that
// holds them validates: the restored notebook has every record, and moves
// to its Trash keep working.
func TestTrashArchivesSurviveABundleRestoreAndAHandoff(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	lowerTrashLimits(t, 2, 8<<20, 100000, 1000)
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		saveNote(t, s, p, "# "+p+"\n")
		if _, err := s.TrashNotes(ctx, []string{p}, headOf(t, s), "owner"); err != nil {
			t.Fatal(err)
		}
		emptyTrash(t, s)
	}
	if archives := trashArchivesAt(t, s); len(archives) == 0 {
		t.Fatal("the fixture archived nothing")
	}
	want, err := s.TrashPolicy(ctx)
	if err != nil || len(want.Records) != 3 {
		t.Fatal(want, err)
	}
	snapshot, err := s.NotebookSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateNotebookSnapshot(ctx, snapshot); err != nil {
		t.Error("a handoff of a notebook with Trash archives:", err)
	}
	archive := snapshot.Files[trashArchivePath(1)]
	delete(snapshot.Files, trashArchivePath(1))
	snapshot.Files[trashArchivePath(2)] = archive
	if err := ValidateNotebookSnapshot(ctx, snapshot); !errors.Is(err, ErrInvalid) {
		t.Error("a handoff with a gap in the Trash archives:", err)
	}

	restored := restore(t, s)
	got, err := restored.TrashPolicy(ctx)
	if err != nil || !reflect.DeepEqual(got.Records, want.Records) {
		t.Errorf("the restored Trash: %+v %v, want %+v", got.Records, err, want.Records)
	}
	saveNote(t, restored, "d.md", "# d\n")
	if _, err := restored.TrashNotes(ctx, []string{"d.md"}, headOf(t, restored), "owner"); err != nil {
		t.Error("moving a note to the restored Trash:", err)
	}
}

// A bundle import refuses Trash archives that readers would misread or
// skip, as a handoff does, and keeps no notebook: an archive that is not
// JSON, one holding a note still in the Trash, one past a gap in their
// numbering, archives without an index, and a record in two files.
func TestImportRefusesInvalidTrashArchives(t *testing.T) {
	ctx := context.Background()
	notes := newTestNotes(t)
	const id = "0123456789abcdef0123456789abcdef"
	record := func(state string) string {
		return `{"id":"` + id + `","path":"Trash/` + id + `.md","originalPath":"notes/a.md","title":"A","deletedAt":"2026-11-01T00:00:00Z","deletedBy":"owner","history":{"notes/a.md":[]}` + state + `}`
	}
	file := func(records ...string) string {
		return `{"version":1,"records":[` + strings.Join(records, ",") + `]}` + "\n"
	}
	empty, tombstone := file(), file(record(`,"emptied":true`))
	for i, c := range []struct {
		name  string
		files map[string]string
	}{
		{"not JSON", map[string]string{trashIndexPath: empty, trashArchivePath(1): "not JSON"}},
		{"a note still in the Trash", map[string]string{trashIndexPath: empty, trashArchivePath(1): file(record(""))}},
		{"a gap", map[string]string{trashIndexPath: empty, trashArchivePath(2): tombstone}},
		{"a gap after an archive", map[string]string{trashIndexPath: empty, trashArchivePath(1): file(), trashArchivePath(3): tombstone}},
		{"no index", map[string]string{trashArchivePath(1): tombstone}},
		{"a record in two files", map[string]string{trashIndexPath: tombstone, trashArchivePath(1): tombstone}},
	} {
		t.Run(c.name, func(t *testing.T) {
			snapshot := NotebookSnapshot{Revision: strings.Repeat("a", 40), Files: map[string][]byte{}}
			for p, content := range c.files {
				snapshot.Files[p] = []byte(content)
			}
			if err := ValidateNotebookSnapshot(ctx, snapshot); !errors.Is(err, ErrInvalid) {
				t.Error("a handoff:", err)
			}
			notebook := fmt.Sprintf("refused-%d", i)
			if err := importFile(t, notes, notebook, gitBundle(t, c.files)); !errors.Is(err, ErrInvalid) {
				t.Error("an import:", err)
			}
			absent(t, notes, notebook)
		})
	}
	// A notebook with a valid archive imports and reads its records.
	if err := importFile(t, notes, "restored", gitBundle(t, map[string]string{trashIndexPath: empty, trashArchivePath(1): tombstone})); err != nil {
		t.Fatal(err)
	}
	if policy, err := openImported(t, notes, "restored").TrashPolicy(ctx); err != nil || len(policy.Records) != 1 || policy.Records[0].ID != id {
		t.Errorf("the imported Trash: %+v %v", policy, err)
	}
}
