package workspace

import (
	"context"
	"strings"
	"testing"
)

// purgeAll purges text from the whole notebook.
func purgeAll(t *testing.T, s *Store, text string) PurgeResult {
	t.Helper()
	ctx := context.Background()
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: text}, nil)
	if err != nil || plan.Blocked != "" || plan.RevisionsRewritten == 0 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "a leaked key", nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// The Trash records' histories, as TrashPolicy lists them for history
// search, name the revisions standing for their versions after each purge
// that rewrote them, through successive purges: each one reads, and none
// is a revision the notebook no longer has.
func TestTrashPolicyListsTheRevisionsPurgesRewrote(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	first, second := purgeSecret(t), purgeSecret(t)
	saveNote(t, s, "keys.md", "Keys "+first+" and "+second+".\n")
	saveNote(t, s, "a.md", "# A\n")
	saveNote(t, s, "a.md", "# A, again\n")
	if _, err := s.TrashNotes(ctx, []string{"a.md"}, headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
	emptyTrash(t, s)
	saveNote(t, s, "b.md", "# B\n")
	if _, err := s.TrashNotes(ctx, []string{"b.md"}, headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{first, second} {
		purgeAll(t, s, secret)
		policy, err := s.TrashPolicy(ctx)
		if err != nil || len(policy.Records) != 2 {
			t.Fatal(policy, err)
		}
		listed := 0
		for _, record := range policy.Records {
			for p, revisions := range record.History {
				for _, rev := range revisions {
					listed++
					doc, err := s.Read(ctx, p, rev)
					if err != nil || !strings.HasPrefix(string(doc.Content), "# ") {
						t.Fatalf("after purging %s, the record of %s lists %s at %s: %q %v", secret, record.OriginalPath, p, rev, doc.Content, err)
					}
				}
			}
		}
		if listed != 3 {
			t.Fatal("the records list", listed, "revisions")
		}
	}
}

// A gateway that cached a version it read before another gateway purged it
// no longer serves it once it has heard of the rewritten history, before
// any announcement of the purge reaches it: it refuses the old revision as
// the purging gateway does.
func TestPurgeEndsAnotherGatewaysCachedReadOfAPurgedVersion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	secret := purgeSecret(t)
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	first := save(t, s, "a.md", noteWithID(id, "Key "+secret+".\n"), "")
	save(t, s, "a.md", noteWithID(id, "No key.\n"), first.Revision)
	other := openGateway(t, s)
	waitHead(t, other, headOf(t, s))
	if doc, err := other.Read(ctx, "a.md", first.Revision); err != nil || !strings.Contains(string(doc.Content), secret) {
		t.Fatal("the version before the purge:", err)
	}
	purgeAll(t, s, secret)
	waitHead(t, other, headOf(t, s))
	_, purging := s.Read(ctx, "a.md", first.Revision)
	if purging == nil {
		t.Fatal("the purging gateway reads the purged revision")
	}
	if doc, err := other.Read(ctx, "a.md", first.Revision); err == nil || err.Error() != purging.Error() {
		t.Fatalf("the other gateway read the purged revision: %q %v, the purging gateway %v", doc.Content, err, purging)
	}
}
