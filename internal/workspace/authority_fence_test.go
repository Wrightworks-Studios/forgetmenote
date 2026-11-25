package workspace

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/pg/pgtest"
)

// stalledGatewaySave has gateway B, which knows a's head, check a save of a
// note large enough to be staged before its transaction, and holds the save
// in that staging, as a slow blob store would, until release.
func stalledGatewaySave(t *testing.T, a *Store) (head string, release func(), done <-chan error) {
	t.Helper()
	ctx := context.Background()
	owned, err := a.Save(ctx, "owned.md", []byte("owner bytes"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenDB(ctx, openTestNotes(t, testEnv(t, a)), "notes", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	for {
		if current, err := b.Status(ctx); err == nil && current.Revision == owned.Revision {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waiting, release := pgtest.HoldWrites(t, testEnv(t, a).db, "content_blocks")
	result := make(chan error, 1)
	go func() {
		_, err := b.Save(ctx, "big.md", []byte(strings.Repeat("A large note.\n", 2000)), "", "")
		result <- err
	}()
	waiting(1)
	return owned.Revision, release, result
}

// A save another gateway checked before a handoff completed fails rather
// than committing into the GitHub notebook, so the return handoff erases no
// acknowledged save.
func TestSaveCheckedBeforeAHandoffCannotCommitAfterIt(t *testing.T) {
	a := newTestStore(t)
	ctx := context.Background()
	head, release, saved := stalledGatewaySave(t, a)
	if _, err := a.PrepareAuthority(ctx, AuthorityRequest{ID: filingHandoff, Generation: 1, From: "forgetmenote", To: "github", Revision: head}); err != nil {
		t.Fatal(err)
	}
	state, err := a.FinishAuthority(ctx, filingHandoff, true)
	if err != nil || state.Type != "github" || state.Committed != head {
		t.Fatalf("handoff: %+v %v", state, err)
	}
	release()
	if err := <-saved; !errors.Is(err, ErrReadOnly) {
		t.Fatal("a save committed into a notebook handed to GitHub:", err)
	}
	fresh := reopenTestStore(t, a)
	if current := headOf(t, fresh); current != state.Committed {
		t.Fatalf("the GitHub notebook's head moved from %s to %s", state.Committed, current)
	}
	if _, err := fresh.Read(ctx, "big.md", ""); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the refused save is in the notebook:", err)
	}
}

// A save another gateway checked before a handoff was prepared fails, and
// the handoff completes.
func TestSaveCheckedBeforeAHandoffCannotCommitIntoItsPreparation(t *testing.T) {
	a := newTestStore(t)
	ctx := context.Background()
	head, release, saved := stalledGatewaySave(t, a)
	if _, err := a.PrepareAuthority(ctx, AuthorityRequest{ID: filingHandoff, Generation: 1, From: "forgetmenote", To: "github", Revision: head}); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-saved; !errors.Is(err, ErrForbidden) {
		t.Fatal("a save committed into a prepared handoff:", err)
	}
	if state, err := a.FinishAuthority(ctx, filingHandoff, true); err != nil || state.Type != "github" || state.Committed != head {
		t.Fatalf("the handoff after the refused save: %+v %v", state, err)
	}
}
