package notestore_test

import (
	"errors"
	"io/fs"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// DeleteIncarnation deletes a notebook only while it is the incarnation its
// creator read, and leaves one deleted and created again since.
func TestDeleteIncarnationLeavesANotebookCreatedAgain(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	first := e.create("research")
	apply(t, first, notestore.ChangeSet{Files: []notestore.FileChange{write("plan.md", "# First\n")}})
	_, old, err := first.StoredIncarnation(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(e.ctx, "research"); err != nil {
		t.Fatal(err)
	}
	again := e.create("research")
	apply(t, again, notestore.ChangeSet{Files: []notestore.FileChange{write("plan.md", "# Again\n")}})
	head, current, err := again.StoredIncarnation(e.ctx)
	if err != nil || current <= old {
		t.Fatalf("the notebook created again is incarnation %d after %d: %v", current, old, err)
	}
	if err := e.store.DeleteIncarnation(e.ctx, "research", old); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleting the earlier incarnation: %v", err)
	}
	if got := read(t, again, gitobj.ID{}, "plan.md"); got != "# Again\n" {
		t.Fatalf("the notebook created again holds %q", got)
	}
	if stored, _ := again.Head(e.ctx); stored != head {
		t.Fatalf("the notebook created again is at %v, was at %v", stored, head)
	}
	if err := e.store.DeleteIncarnation(e.ctx, "research", current); err != nil {
		t.Fatal(err)
	}
	if _, err := again.Head(e.ctx); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("its own incarnation was not deleted:", err)
	}
	if err := e.store.DeleteIncarnation(e.ctx, "research", current); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleting it twice: %v", err)
	}
}
