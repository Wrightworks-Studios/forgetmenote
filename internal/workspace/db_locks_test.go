package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

// committedPolicy is the lock policy committed at s's head.
func committedPolicy(t *testing.T, s *Store) string {
	t.Helper()
	raw, _, err := s.notes.Read(context.Background(), gitobj.ID{}, locksPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// attributed is a request by a signed-in browser user.
func attributed() context.Context {
	return attribution.With(context.Background(), attribution.Identity{ID: "u-1", Name: "Ada Lovelace", Email: "ada@example.com", Source: "browser"})
}

// SetLock commits the Git backend's policy file and revision, "Update
// .gitnotes-locks.json" with the request's attribution, keeps rules sorted
// and bounded, and refuses what its original refuses.
func TestDatabaseSetLockCommitsThePolicy(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "notes/a.md", []byte("# A\n"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetLock(ctx, "Trash/x.md", true, doc.Revision, "admin"); !errors.Is(err, ErrForbidden) {
		t.Error("locked the Trash:", err)
	}
	if _, err = s.SetLock(ctx, "notes/../a.md", true, doc.Revision, "admin"); !errors.Is(err, ErrForbidden) {
		t.Error("locked an invalid path:", err)
	}
	if _, err = s.Locks(ctx, ".git"); !errors.Is(err, ErrForbidden) {
		t.Error("listed the rules of an invalid path:", err)
	}
	if _, err = s.SetLock(ctx, "notes", true, "", "admin"); !errors.Is(err, ErrConflict) {
		t.Error("locked from a stale revision:", err)
	}
	if same, err := s.SetLock(ctx, "notes", false, doc.Revision, "admin"); err != nil || same.Revision != doc.Revision || len(same.Rules) != 0 {
		t.Fatalf("an unchanged policy: %+v %v", same, err)
	}
	for i, path := range []string{"notes", "manuscript/draft.md", "alpha"} {
		state, err := s.Locks(ctx, "")
		if err != nil || len(state.Rules) != i {
			t.Fatalf("rules before %s: %+v %v", path, state, err)
		}
		if _, err = s.SetLock(attributed(), path, true, state.Revision, "admin"); err != nil {
			t.Fatal(path, err)
		}
	}
	all, err := s.Locks(ctx, "")
	if err != nil || len(all.Rules) != 3 || all.Rules[0].Path != "alpha" || all.Rules[2].Path != "notes" || all.Revision != headOf(t, s) {
		t.Fatalf("all rules: %+v %v", all, err)
	}
	one, err := s.Locks(ctx, "manuscript/draft.md")
	if err != nil || one.Path != "manuscript/draft.md" || len(one.Rules) != 1 || one.Revision != all.Revision {
		t.Fatalf("the rules of one path: %+v %v", one, err)
	}
	info, err := s.notes.Commit(ctx, mustParse(t, all.Revision))
	if err != nil || commitSubject(info.Commit.Message) != "Update .gitnotes-locks.json" || trailerValue(info.Trailers, "Gitnotes-User") != "u-1" || trailerValue(info.Trailers, "Gitnotes-Source") != "browser" || info.Commit.Author.Name != "Ada Lovelace" || info.Commit.Committer.Name != "ForgetMeNote" || len(info.Changes) != 1 || info.Changes[0].Path != locksPath {
		t.Fatalf("the lock revision: %+v %v", info, err)
	}
	if got := committedPolicy(t, s); got != "{\n  \"version\": 1,\n  \"paths\": [\n    \"alpha\",\n    \"manuscript/draft.md\",\n    \"notes\"\n  ]\n}\n" {
		t.Fatalf("the committed policy: %q", got)
	}

	paths := make([]string, 256)
	for i := range paths {
		paths[i] = fmt.Sprintf("p%03d", i)
	}
	full := commitFixture(t, s, fixture(locksPath, `{"version":1,"paths":["`+strings.Join(paths, `","`)+`"]}`))
	if _, err = s.SetLock(ctx, "one-more", true, full, "admin"); errorText(err) != "project lock limit reached" {
		t.Error("a 257th rule:", err)
	}
	if state, err := s.SetLock(ctx, paths[0], false, full, "admin"); err != nil || state.Revision == full || len(state.Rules) != 0 {
		t.Error("an unlock in a full policy:", state, err)
	}
}

// Rules are the policy committed at the head: one another gateway commits
// takes effect, even one this store parsed before it changed the rules.
func TestDatabaseLocksFollowPoliciesOtherGatewaysCommit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "notes/a.md", []byte("original"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	policy := "{\n  \"version\": 1,\n  \"paths\": [\n    \"notes\"\n  ]\n}\n"
	commitFixture(t, s, fixture(locksPath, policy))
	locked, err := s.Locks(ctx, doc.Path)
	if err != nil || locked.Covering(doc.Path) != "notes" {
		t.Fatalf("a committed lock: %+v %v", locked, err)
	}
	unlocked, err := s.SetLock(ctx, "notes", false, locked.Revision, "admin")
	if err != nil || len(unlocked.Rules) != 0 || unlocked.Revision == locked.Revision {
		t.Fatalf("unlock: %+v %v", unlocked, err)
	}
	relocked := commitFixture(t, s, fixture(locksPath, policy))
	state, err := s.Locks(ctx, doc.Path)
	if err != nil || state.Revision != relocked || state.Covering(doc.Path) != "notes" {
		t.Fatalf("the lock committed again elsewhere: %+v %v", state, err)
	}
	if _, err = s.Save(ctx, doc.Path, []byte("no"), relocked, "writer"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a lock committed elsewhere was ignored: %v", err)
	}
}

// A lock on one gateway and a save of the note it covers on another, from
// the same revision, have one winner, even before the saving gateway hears
// of the lock.
func TestDatabaseLockAndSaveOnTwoGatewaysHaveOneWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	other, err := OpenDB(ctx, openTestNotes(t, testEnv(t, s)), "notes", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for i := range 10 {
		path := fmt.Sprintf("notes%d/a.md", i)
		doc, err := s.Save(ctx, path, []byte("original"), "", "test")
		if err != nil {
			t.Fatal(err)
		}
		for {
			if read, err := other.Read(ctx, path, ""); err == nil && read.Revision == doc.Revision {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if _, err := s.SetLock(ctx, fmt.Sprintf("notes%d", i), true, doc.Revision, "admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := other.Save(ctx, path, []byte("revised"), doc.Revision, "writer"); !errors.Is(err, ErrConflict) && !errors.Is(err, ErrForbidden) {
			t.Fatalf("round %d: a save past another gateway's lock: %v", i, err)
		}
		if now, err := s.Read(ctx, path, ""); err != nil || string(now.Content) != "original" {
			t.Fatalf("round %d: the locked note: %q %v", i, now.Content, err)
		}
	}
}

// A save that checked the lock rules before another gateway's lock reached
// it, and reads the head it commits on only after, is refused: it cannot
// commit past a lock it never checked.
func TestDatabaseSaveRefusesALockHeardAfterItsCheck(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	db := testEnv(t, s).db
	saving, err := OpenDB(ctx, openTestNotes(t, testEnv(t, s)), "notes", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer saving.Close()
	doc, err := s.Save(ctx, "notes/a.md", []byte("original"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	waitHeard(t, saving, doc.Revision)
	// The saving gateway checks the rules at doc.Revision, then waits on
	// the notebook's authority record, which every write reads next.
	waiting, release := holdReads(t, db, "notebook_authority")
	saved := make(chan error, 1)
	go func() {
		_, err := saving.Save(ctx, doc.Path, []byte("revised"), doc.Revision, "writer")
		saved <- err
	}()
	waiting(1)
	locked := commitFixture(t, s, fixture(locksPath, "{\n  \"version\": 1,\n  \"paths\": [\n    \"notes\"\n  ]\n}\n"))
	waitHeard(t, saving, locked)
	release()
	if err := <-saved; !errors.Is(err, ErrConflict) && !errors.Is(err, ErrForbidden) {
		t.Fatalf("a save past a lock it heard of after its check: %v", err)
	}
	if now, err := s.Read(ctx, doc.Path, ""); err != nil || string(now.Content) != "original" || now.Revision != locked {
		t.Fatalf("the locked note: %q %s %v", now.Content, now.Revision, err)
	}
	if _, err := saving.Save(ctx, doc.Path, []byte("revised"), locked, "writer"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("the save again from the locked revision: %v", err)
	}
}

// Every revision commits only on a base that holds the lock rules its checks
// read: rules another gateway committed in between fail it as a conflict,
// whichever paths they cover, and once the rules are read again they apply.
// A live checkpoint commits edits accepted before the lock, and is exempt.
func TestDatabaseRevisionsCommitOnTheLockRulesTheyChecked(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	n := s.backend
	doc, err := s.Save(ctx, "notes/a.md", []byte("original"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.loadLocks(ctx); err != nil {
		t.Fatal(err)
	}
	locked := commitFixture(t, s, fixture(locksPath, "{\n  \"version\": 1,\n  \"paths\": [\n    \"elsewhere\"\n  ]\n}\n"))
	revise := []pathChange{{Path: doc.Path, Content: []byte("revised")}}
	if _, err := n.applyChanges(ctx, locked, "Update "+doc.Path, revise); !errors.Is(err, ErrConflict) {
		t.Fatalf("a revision on rules its checks never read: %v", err)
	}
	checkpoint := context.WithValue(ctx, collaborationAuditKey{}, "")
	if _, err := n.applyChanges(checkpoint, locked, "Checkpoint live edits to "+doc.Path, revise); err != nil {
		t.Fatalf("a checkpoint on rules its acceptance never read: %v", err)
	}
	if err := n.loadLocks(ctx); err != nil {
		t.Fatal(err)
	}
	head, err := n.head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.applyChanges(ctx, head, "Update "+doc.Path, []pathChange{{Path: doc.Path, Content: []byte("again")}}); err != nil {
		t.Fatalf("a revision on the rules read again: %v", err)
	}
}

// waitHeard waits until s's notes store has heard of head.
func waitHeard(t *testing.T, s *Store, head string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if h, err := s.notes.Head(context.Background()); err == nil && h.Commit.String() == head {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the store never heard of %s", head)
		}
	}
}

// holdReads makes every statement that reads table in db's database wait,
// as a stalled disk would, until release runs or the test ends; waiting is
// pgtest.WaitingFor this hold. The workspace tests run one at a time, so no
// other test's statement reads the held table.
func holdReads(t *testing.T, db *pg.DB, table string) (waiting func(n int) int, release func()) {
	t.Helper()
	ctx := context.Background()
	hold, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { hold.Rollback(ctx) }) }
	t.Cleanup(release)
	if _, err := hold.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	return pgtest.WaitingFor(t, db, pgtest.Backend(t, hold)), release
}
