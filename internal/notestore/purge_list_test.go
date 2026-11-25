package notestore_test

import (
	"testing"

	"forgetmenote/internal/notestore"
)

// Every purge a notebook records is listed, by notebook or for the whole
// org, with who ran it, why and what it covered.
func TestRecordedPurgesListsEveryNotebooksPurges(t *testing.T) {
	const secret = "sk-live-0123456789-LEAKED"
	e := newEnv(t, notestore.Options{})
	for _, id := range []string{"a", "b"} {
		n := e.create(id)
		apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("Inbox/keys.md", "The key is "+secret+".\n")}})
		apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("Inbox/after.md", "After.\n")}})
		if _, err := n.Rewrite(e.ctx, 1, "purge-"+id, redact(secret), purgeOptions()); err != nil {
			t.Fatal(err)
		}
	}
	all, err := notestore.RecordedPurges(e.ctx, e.db, "")
	if err != nil || len(all) != 2 || all[0].Notebook != "a" || all[0].ID != "purge-a" || all[1].ID != "purge-b" || all[1].Actor != "owner" || all[1].Reason != "a leaked key" || all[1].FromSeq != 1 || len(all[1].Scope) == 0 {
		t.Fatalf("every purge: %+v %v", all, err)
	}
	one, err := notestore.RecordedPurges(e.ctx, e.db, "b")
	if err != nil || len(one) != 1 || one[0].ID != "purge-b" {
		t.Fatalf("notebook b's purges: %+v %v", one, err)
	}
}
