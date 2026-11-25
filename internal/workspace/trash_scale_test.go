package workspace

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"forgetmenote/internal/notestore"
)

// Emptying a Trash of thousands of notes, which holds the notebook's lock,
// checks each note's discussion path without copying the notebook's file
// list for each: its allocations grow with the notes, not with the notes
// times the notebook's files.
func TestEmptyTrashOfThousandsOfNotesAllocatesLinearly(t *testing.T) {
	if testing.Short() {
		t.Skip("commits thousands of notes")
	}
	ctx := context.Background()
	s := newTestStore(t)
	const n = 2000
	index := TrashIndex{Version: 1, Records: []TrashRecord{}}
	var changes []notestore.FileChange
	for i := range n {
		id := fmt.Sprintf("%032x", i+1)
		record := TrashRecord{TrashEntry: TrashEntry{ID: id, Path: "Trash/" + id + ".md", OriginalPath: fmt.Sprintf("notes/n%d.md", i), Title: "N", DeletedAt: "2026-11-23T00:00:00Z", DeletedBy: "owner"}, History: map[string][]string{}}
		index.Records = append(index.Records, record)
		changes = append(changes, fixture(record.Path, "# N\n"), fixture(fmt.Sprintf("live/l%d.md", i), "# L\n"))
	}
	raw, err := encodeTrashIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	commitFixture(t, s, append(changes, fixture(trashIndexPath, string(raw)))...)
	listing, err := s.ListTrash(ctx)
	if err != nil || len(listing.Entries) != n {
		t.Fatal(len(listing.Entries), err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	result, err := s.EmptyTrash(ctx, listing.Revision, listing.Confirmation)
	runtime.ReadMemStats(&after)
	if err != nil || len(result.Entries) != n {
		t.Fatal(len(result.Entries), err)
	}
	// Copying 4,001 files' listing for each of 2,000 notes allocates about
	// 700 MB; the work that grows with the notes about 25 MB.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 150<<20 {
		t.Fatalf("emptying a Trash of %d notes allocated %d MB", n, allocated>>20)
	}
}

// Damage to the notebook's own stored Trash files is the server's, never
// the request's: a damaged index and a damaged archive are refused alike,
// as no ErrInvalid, naming the file and how to recover it, by the readers
// and when the notebook opens. A bundle import, whose bundle brought the
// damage, still refuses it as its input (TestImportRefusesInvalidTrashArchives).
func TestDamagedTrashFilesAreServerErrors(t *testing.T) {
	ctx := context.Background()
	const id = "0123456789abcdef0123456789abcdef"
	record := func(state string) string {
		return `{"id":"` + id + `","path":"Trash/` + id + `.md","originalPath":"notes/a.md","title":"A","deletedAt":"","deletedBy":"","history":{"notes/a.md":[]}` + state + `}`
	}
	empty := `{"version":1,"records":[]}`
	for _, c := range []struct {
		name, file string
		files      map[string]string
	}{
		{"an index of another version", trashIndexPath, map[string]string{trashIndexPath: `{"version":2,"records":[]}`}},
		{"an invalid index entry", trashIndexPath, map[string]string{trashIndexPath: `{"version":1,"records":[{"id":"bad"}]}`}},
		{"an archive that is not JSON", trashArchivePath(1), map[string]string{trashIndexPath: empty, trashArchivePath(1): "not JSON"}},
		{"an archive holding a note in the Trash", trashArchivePath(1), map[string]string{trashIndexPath: empty, trashArchivePath(1): `{"version":1,"records":[` + record("") + `]}`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			commitFiles(t, s, c.files)
			check := func(what string, err error) {
				t.Helper()
				if err == nil || errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.file) || !strings.Contains(err.Error(), "restore the notebook from a backup") {
					t.Errorf("%s: %v (invalid %t)", what, err, errors.Is(err, ErrInvalid))
				}
			}
			_, err := s.TrashPolicy(ctx)
			check("the Trash policy", err)
			if c.file == trashIndexPath {
				_, err = s.ListTrash(ctx)
				check("the Trash listing", err)
			}
			env := testEnv(t, s)
			s.Close()
			_, err = OpenDB(ctx, openTestNotes(t, env), s.notebook, t.TempDir(), nil)
			check("opening the notebook", err)
		})
	}
}
