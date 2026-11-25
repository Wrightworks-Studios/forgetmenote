package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
)

// emptyTrash empties the Trash with the confirmation ListTrash gives.
func emptyTrash(t *testing.T, s *Store) TrashResult {
	t.Helper()
	ctx := context.Background()
	listing, err := s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.EmptyTrash(ctx, listing.Revision, listing.Confirmation)
	if err != nil {
		t.Fatal("emptying the Trash:", err)
	}
	return result
}

// trashArchivesAt is the Trash archive files at the head, by path.
func trashArchivesAt(t *testing.T, s *Store) map[string][]byte {
	t.Helper()
	files, err := backendOf(s).files(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	archives := map[string][]byte{}
	for _, f := range files {
		if strings.HasPrefix(f.Path, ".gitnotes-trash-") {
			raw, _, err := s.notes.Read(context.Background(), gitobj.ID{}, f.Path)
			if err != nil {
				t.Fatal(err)
			}
			archives[f.Path] = raw
		}
	}
	return archives
}

// Emptying the Trash frees its index for good: past the index's capacity,
// the records of emptied and restored notes move to archive files, so
// every move to the Trash succeeds for the life of the notebook. The
// records stay where history, restores and Trash moves of restored notes
// find them.
func TestEmptyTrashKeepsWorkingPastTheIndexCapacity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	lowerTrashLimits(t, 4, 8<<20, 100000, 1000)
	created := map[string]string{} // each trashed note's first revision, by path
	var ids []string
	var kept TrashEntry // a note restored, then trashed again
	var againID, restored string
	for round := range 5 {
		var paths []string
		for i := range 3 {
			p := fmt.Sprintf("round-%d/note-%d.md", round, i)
			created[p] = saveNote(t, s, p, fmt.Sprintf("# Round %d, note %d\n", round, i)).Revision
			paths = append(paths, p)
		}
		trashed, err := s.TrashNotes(ctx, paths, headOf(t, s), "owner")
		if err != nil {
			t.Fatalf("round %d: moving notes to the Trash: %v", round, err)
		}
		for _, entry := range trashed.Entries {
			ids = append(ids, entry.ID)
		}
		if round == 1 {
			// Restore one note; its record becomes a tombstone the next
			// compaction archives, and trashing it again later continues
			// its history from there.
			kept = trashed.Entries[0]
			result, err := s.RestoreTrash(ctx, kept.ID, "", headOf(t, s), "owner")
			if err != nil {
				t.Fatal(err)
			}
			restored = result.Revision
		}
		if round == 3 {
			edited := saveNote(t, s, kept.OriginalPath, "# Kept, edited\n")
			again, err := s.TrashNotes(ctx, []string{kept.OriginalPath}, headOf(t, s), "owner")
			if err != nil {
				t.Fatal(err)
			}
			history, err := trashedHistory(s, again.Entries[0].ID)
			if want := map[string][]string{kept.OriginalPath: {edited.Revision, restored, created[kept.OriginalPath]}}; err != nil || !reflect.DeepEqual(history, want) {
				t.Errorf("a note trashed again after its record was archived: %v %v, want %v", history, err, want)
			}
			againID = again.Entries[0].ID
			ids = append(ids, againID)
		}
		emptyTrash(t, s)
	}

	archives := trashArchivesAt(t, s)
	if len(archives) < 2 {
		t.Fatalf("archives: %d", len(archives))
	}
	for p, raw := range archives {
		if !bytes.Contains(raw, []byte(`"emptied": true`)) && !bytes.Contains(raw, []byte(`"restored": true`)) {
			t.Errorf("%s holds no tombstone: %s", p, raw)
		}
	}
	main, _, err := s.notes.Read(ctx, gitobj.ID{}, trashIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(main), `"id"`) > 4 {
		t.Errorf("the index holds more than its capacity: %s", main)
	}

	// The Trash policy, which history search filters with, still has every
	// record and the history each recorded.
	policy, err := s.TrashPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]TrashRecord{}
	for _, r := range policy.Records {
		records[r.ID] = r
	}
	if len(records) != len(ids) || len(policy.Records) != len(ids) {
		t.Fatalf("the policy has %d records, %d distinct, want %d", len(policy.Records), len(records), len(ids))
	}
	for _, id := range ids {
		r, ok := records[id]
		if !ok || !r.Emptied && !r.Restored || len(r.History[r.OriginalPath]) == 0 {
			t.Errorf("record %s: %+v", id, r)
		}
		if r.ID != againID && !reflect.DeepEqual(r.History, map[string][]string{r.OriginalPath: {created[r.OriginalPath]}}) {
			t.Errorf("record %s's history: %v", id, r.History)
		}
	}
	// A restore of an archived record, emptied or restored, finds nothing
	// to restore, as it did before.
	for _, id := range []string{ids[0], kept.ID} {
		if _, err := s.RestoreTrash(ctx, id, "", headOf(t, s), "owner"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("restoring archived record %s: %v", id, err)
		}
	}
	// The notes' histories still read.
	for p := range created {
		history, err := s.History(ctx, p)
		if err != nil || len(history) == 0 {
			t.Errorf("%s's history: %v %v", p, history, err)
		}
	}
	// The archives survive a restart with cold caches.
	again := reopenTestStore(t, s)
	if policy, err := again.TrashPolicy(ctx); err != nil || len(policy.Records) != len(ids) {
		t.Errorf("the policy after a restart: %d records, %v", len(policy.Records), err)
	}
}

// Without tombstones to archive, a full index refuses: as capacity while
// notes in the Trash fill it, which emptying the Trash or restoring them
// releases, and as invalid input when the notes one request moves would
// not fit even in an empty Trash.
func TestAFullTrashRefusesUntilEmptied(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	lowerTrashLimits(t, 4, 8<<20, 100000, 1000)
	var paths []string
	for i := range 5 {
		p := fmt.Sprintf("note-%d.md", i)
		saveNote(t, s, p, "# Note\n")
		paths = append(paths, p)
	}
	head := headOf(t, s)
	if _, err := s.TrashNotes(ctx, paths, head, "owner"); !errors.Is(err, ErrInvalid) || errorText(err) != "Trash transaction exceeds the Trash index; move fewer notes at a time" {
		t.Errorf("moving more notes than the index holds: %v", err)
	}
	if _, err := s.TrashNotes(ctx, paths[:4], head, "owner"); err != nil {
		t.Fatal(err)
	}
	full := headOf(t, s)
	if _, err := s.TrashNotes(ctx, paths[4:], full, "owner"); !errors.Is(err, ErrCapacity) || errorText(err) != "Trash index capacity reached; empty the Trash or restore notes first" {
		t.Errorf("a full Trash: %v", err)
	}
	if now := headOf(t, s); now != full {
		t.Fatalf("a refusal wrote %s", now)
	}
	emptyTrash(t, s)
	if _, err := s.TrashNotes(ctx, paths[4:], headOf(t, s), "owner"); err != nil {
		t.Error("after emptying the Trash:", err)
	}
}

// A purge of a note removes its record from the Trash archives as it does
// from the index, and leaves the other records there.
func TestPurgeRemovesAnArchivedTrashRecord(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	lowerTrashLimits(t, 3, 8<<20, 100000, 1000)
	for _, p := range []string{"secret.md", "other.md", "kept.md", "next.md"} {
		saveNote(t, s, p, "# "+p+"\n")
	}
	trashed, err := s.TrashNotes(ctx, []string{"secret.md", "other.md", "kept.md"}, headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	// Two restored notes' records are archived while kept.md stays in the
	// Trash, so the archive holds what no version of the index did.
	for _, entry := range trashed.Entries[:2] {
		if _, err := s.RestoreTrash(ctx, entry.ID, "", headOf(t, s), "owner"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.TrashNotes(ctx, []string{"next.md"}, headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
	archives := trashArchivesAt(t, s)
	if len(archives) != 1 || !bytes.Contains(archives[".gitnotes-trash-1.json"], []byte(trashed.Entries[0].ID)) {
		t.Fatalf("archives: %q", archives)
	}

	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: "secret.md"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "customer data", nil); err != nil {
		t.Fatal(err)
	}
	archive := trashArchivesAt(t, s)[".gitnotes-trash-1.json"]
	if bytes.Contains(archive, []byte(trashed.Entries[0].ID)) || bytes.Contains(archive, []byte("secret.md")) || !bytes.Contains(archive, []byte(trashed.Entries[1].ID)) {
		t.Errorf("the archive after the purge: %s", archive)
	}
	policy, err := s.TrashPolicy(ctx)
	if err != nil || len(policy.Records) != 3 {
		t.Errorf("the policy after the purge: %+v %v", policy, err)
	}
	// Moves to the Trash keep working on the rewritten history.
	saveNote(t, s, "after.md", "# After\n")
	if _, err := s.TrashNotes(ctx, []string{"after.md"}, headOf(t, s), "owner"); err != nil {
		t.Error("after the purge:", err)
	}
}

// An organization that moves a duplicate to a full Trash archives the
// tombstones as any Trash move does; its receipt lists only the notes it
// changed, and its undo restores the duplicate.
func TestOrganizationArchivesTombstonesOfAFullTrash(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	lowerTrashLimits(t, 2, 8<<20, 100000, 1000)
	for _, p := range []string{"old-1.md", "old-2.md"} {
		saveNote(t, s, p, "# Old\n")
	}
	if _, err := s.TrashNotes(ctx, []string{"old-1.md", "old-2.md"}, headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
	emptyTrash(t, s)
	for _, p := range []string{"a.md", "b.md"} {
		orgSave(t, s, p, "# Shared fact\nWorkshop begins at 09:00.\n")
	}
	a := OrganizationAction{ID: "run:1", Kind: "duplicate", Path: "b.md", Target: "a.md", Sources: []MutationSource{orgSource(t, s, "b.md"), orgSource(t, s, "a.md")}, Allowed: []string{"b.md"}}
	preview, err := s.Organize(ctx, a, false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Organize(ctx, a, true)
	if err != nil || result.Trash == nil {
		t.Fatal(result, err)
	}
	for _, files := range [][]OrganizationFile{preview.Files, result.Files} {
		for _, f := range files {
			if trashFile(f.Path) {
				t.Errorf("the preview or receipt lists %s", f.Path)
			}
		}
	}
	if len(preview.Files) != len(result.Files) {
		t.Errorf("the preview lists %d files, the receipt %d", len(preview.Files), len(result.Files))
	}
	if archives := trashArchivesAt(t, s); len(archives) != 1 {
		t.Fatalf("archives: %d", len(archives))
	}
	receipt, err := s.OrganizationReceipt(ctx, a, "organizer-test")
	if err != nil || !reflect.DeepEqual(receipt.Files, result.Files) || receipt.Trash == nil || receipt.Trash.ID != result.Trash.ID {
		t.Errorf("the receipt: %+v %v, applied %+v", receipt, err, result)
	}
	if _, err := s.UndoOrganization(ctx, OrganizationUndo{ID: "run:undo1", Action: a, Actor: "organizer-test", Revision: result.Revision}); err != nil {
		t.Fatal("undoing the duplicate:", err)
	}
	if _, err := s.Read(ctx, "b.md", ""); err != nil {
		t.Error("the duplicate after the undo:", err)
	}
}

// A handoff from GitHub brings a notebook's Trash archives with its index:
// they validate, the stage writes them, and the notebook reads every record
// and keeps moving notes to its Trash.
func TestAHandoffFromGitHubBringsTheTrashArchives(t *testing.T) {
	ctx := context.Background()
	lowerTrashLimits(t, 2, 8<<20, 100000, 1000)
	source := newTestStore(t)
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		saveNote(t, source, p, "# "+p+"\n")
		if _, err := source.TrashNotes(ctx, []string{p}, headOf(t, source), "owner"); err != nil {
			t.Fatal(err)
		}
		emptyTrash(t, source)
	}
	want, err := source.TrashPolicy(ctx)
	if err != nil || len(want.Records) != 3 || len(trashArchivesAt(t, source)) == 0 {
		t.Fatal("the source's Trash:", want, err)
	}
	snapshot, err := source.NotebookSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestStore(t)
	doc := saveNote(t, s, "note.md", "# Note\n")
	away, back := "0123456789abcdef0123456789abcdef", "1123456789abcdef0123456789abcdef"
	if _, err := s.PrepareAuthority(ctx, AuthorityRequest{ID: away, Generation: 1, From: "forgetmenote", To: "github", Revision: doc.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishAuthority(ctx, away, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareAuthority(ctx, AuthorityRequest{ID: back, Generation: 2, From: "github", To: "forgetmenote", Revision: doc.Revision}); err != nil {
		t.Fatal(err)
	}
	snapshot.Revision = doc.Revision
	if _, err := s.StageAuthority(ctx, back, snapshot); err != nil {
		t.Fatal("staging a snapshot with Trash archives:", err)
	}
	if _, err := s.FinishAuthority(ctx, back, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.TrashPolicy(ctx)
	if err != nil || !reflect.DeepEqual(got.Records, want.Records) {
		t.Errorf("the Trash after the handoff: %+v %v, want %+v", got.Records, err, want.Records)
	}
	saveNote(t, s, "d.md", "# d\n")
	if _, err := s.TrashNotes(ctx, []string{"d.md"}, headOf(t, s), "owner"); err != nil {
		t.Error("moving a note to the Trash after the handoff:", err)
	}
}
