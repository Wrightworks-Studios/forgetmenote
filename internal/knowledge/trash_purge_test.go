package knowledge

import (
	"context"
	"testing"

	"forgetmenote/internal/workspace"
)

// purgeText purges text from the whole notebook, as an Owner does, and has
// e forget what it derived from the notebook before.
func purgeText(t *testing.T, e *Engine, text string) {
	t.Helper()
	ctx := context.Background()
	plan, err := e.Store.PreparePurge(ctx, workspace.PurgeScope{Kind: workspace.PurgeText, Text: text}, nil)
	if err != nil || plan.Blocked != "" || plan.RevisionsRewritten == 0 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	result, err := e.Store.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "a leaked key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Purged(ctx, result.Purge, nil, true); err != nil {
		t.Fatal(err)
	}
}

// historyMatches is what a history search for text finds, with and without
// the Trash.
func historyMatches(t *testing.T, e *Engine, text string) (int, int) {
	t.Helper()
	counts := [2]int{}
	for i, include := range []bool{false, true} {
		query := historyQuery(text)
		query.IncludeTrash = include
		result, err := e.HybridSearch(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range result.Matches {
			if include && m.TrashID == "" {
				t.Fatalf("a Trash match without its Trash ID: %+v", m)
			}
		}
		counts[i] = len(result.Matches)
	}
	return counts[0], counts[1]
}

func emptyTheTrash(t *testing.T, s *workspace.Store) {
	t.Helper()
	ctx := context.Background()
	listing, err := s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EmptyTrash(ctx, listing.Revision, listing.Confirmation); err != nil {
		t.Fatal(err)
	}
}

// A note emptied from the Trash stays out of history search after a purge
// of other text rewrote the revisions its Trash record lists, and a note
// still in the Trash is found only with the Trash, under its Trash ID.
func TestTrashHidesHistoryAfterAPurgeRewroteIt(t *testing.T) {
	ctx := context.Background()
	s := newNotebook(t, nil)
	const secret = "sk-live-trash-0123456789abcdef"
	if _, err := s.Save(ctx, "keys.md", []byte("# Keys\nThe key is "+secret+".\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	emptied, err := s.Save(ctx, "a.md", []byte("# Alpha\nAn observation.\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := s.Save(ctx, "b.md", []byte("# Beta\nA remark.\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(database(t, s), s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TrashNotes(ctx, []string{"a.md"}, kept.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	emptyTheTrash(t, s)
	state, _ := s.Status(ctx)
	if _, err := s.TrashNotes(ctx, []string{"b.md"}, state.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	if without, with := historyMatches(t, e, "observation"); without != 0 || with != 0 {
		t.Fatalf("before the purge, the emptied note: %d, %d with the Trash", without, with)
	}
	purgeText(t, e, secret)
	if without, with := historyMatches(t, e, "observation"); without != 0 || with != 0 {
		t.Fatalf("after the purge, the emptied note: %d, %d with the Trash (saved at %s)", without, with, emptied.Revision)
	}
	if without, with := historyMatches(t, e, "remark"); without != 0 || with == 0 {
		t.Fatalf("after the purge, the note in the Trash: %d, %d with the Trash", without, with)
	}
}

// A note restored from the Trash, then trashed again after a purge, has a
// Trash record that lists the revisions standing for its old ones, so once
// emptied it stays out of history search.
func TestTrashHidesHistoryOfANoteTrashedAgainAfterAPurge(t *testing.T) {
	ctx := context.Background()
	s := newNotebook(t, nil)
	const secret = "sk-live-trash-fedcba9876543210"
	if _, err := s.Save(ctx, "keys.md", []byte("# Keys\nThe key is "+secret+".\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Save(ctx, "a.md", []byte("# Alpha\nAn observation.\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(database(t, s), s)
	if err != nil {
		t.Fatal(err)
	}
	trashed, err := s.TrashNotes(ctx, []string{"a.md"}, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := s.RestoreTrash(ctx, trashed.Entries[0].ID, "", trashed.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	edited, err := s.Save(ctx, "a.md", []byte("# Alpha\nAn observation, edited.\n"), restored.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	purgeText(t, e, secret)
	state, _ := s.Status(ctx)
	again, err := s.TrashNotes(ctx, []string{"a.md"}, state.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := s.TrashPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range policy.Records {
		if record.ID != again.Entries[0].ID {
			continue
		}
		for p, revisions := range record.History {
			for _, rev := range revisions {
				if _, err := s.Read(ctx, p, rev); err != nil {
					t.Errorf("the record lists %s at %s: %v", p, rev, err)
				}
			}
		}
	}
	emptyTheTrash(t, s)
	if without, with := historyMatches(t, e, "observation"); without != 0 || with != 0 {
		t.Fatalf("the emptied note: %d, %d with the Trash (edited at %s)", without, with, edited.Revision)
	}
}
