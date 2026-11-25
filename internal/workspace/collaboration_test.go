package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"

	"github.com/reearth/ygo/crdt"
)

// liveTestStore is a notebook kept in the notes store with one note, and an
// agent's attributed context.
func liveTestStore(t *testing.T) (*Store, context.Context, Document) {
	t.Helper()
	s := newTestStore(t)
	ctx := attribution.With(context.Background(), attribution.Identity{ID: "alice", Name: "Alice", Email: "alice@example.test", Source: "mcp", Client: "agent-a"})
	doc, err := s.Save(ctx, "note.md", []byte("😀 alpha\n\nbeta\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, doc
}

// browserDoc is a browser's Yjs document, loaded from a live note's update.
func browserDoc(state []byte) (*crdt.Doc, error) {
	doc := crdt.New(crdt.WithGC(false))
	doc.GetText("markdown")
	if len(state) > 0 {
		if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

func liveDB(s *Store) *dbNotebook { return s.backend }

// reopenLiveStore restarts s as reopenTestStore does, recovers its pending
// live edits as its first write does, and reads each recovered document.
func reopenLiveStore(t *testing.T, s *Store) (*Store, error) {
	t.Helper()
	again := reopenTestStore(t, s)
	return again, recoverLive(context.Background(), again)
}

// recoverLive holds s's stored documents with pending edits, as a write's
// clean does, then reads each, reconciling it with Git.
func recoverLive(ctx context.Context, s *Store) error {
	n := liveDB(s)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.liveRecover(ctx); err != nil {
		return err
	}
	for path := range n.live {
		if _, err := n.liveRecord(ctx, path); err != nil {
			return err
		}
	}
	return nil
}

// liveStored is everything the notes store keeps of path's live document
// and of the notebook's live receipts.
func liveStored(t *testing.T, s *Store, path string) string {
	t.Helper()
	ctx := context.Background()
	f, err := s.notes.Stat(ctx, gitobj.ID{}, path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.notes.LiveState(ctx, f.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := s.notes.Receipts(ctx, liveReceiptKind)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		State    notestore.LiveState
		Receipts []notestore.Receipt
	}{st, receipts})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// noteRevisions are the revisions that changed path, newest first.
func noteRevisions(t *testing.T, s *Store, path string) []notestore.CommitInfo {
	t.Helper()
	history, err := s.notes.History(context.Background(), notestore.HistoryQuery{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return history
}

// revisionMessage is the full commit message of revision, "" meaning the
// head.
func revisionMessage(t *testing.T, s *Store, revision string) string {
	t.Helper()
	ctx := context.Background()
	if revision == "" {
		head, err := s.notes.Head(ctx)
		if err != nil {
			t.Fatal(err)
		}
		revision = head.Commit.String()
	}
	id, err := gitobj.ParseID(revision)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.notes.Commit(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return info.Commit.Message
}

// failLiveRecords makes the notes store refuse to record a live document's
// snapshot or updates, as a lost primary would right after a checkpoint
// committed, until the returned function runs.
func failLiveRecords(t *testing.T, s *Store) func() {
	t.Helper()
	return pgtest.FailTable(t, testEnv(t, s).db, "live_docs")
}

func TestLiveEditsWaitForExplicitCheckpointAcrossRestart(t *testing.T) {
	s, ctx, original := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	patch := TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "explicit-save", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "durable pending"}}}
	accepted, err := s.LivePatch(ctx, patch)
	if err != nil {
		t.Fatal(err)
	}
	assertPending := func(store *Store) {
		t.Helper()
		// Exercise idle time beyond the former background checkpoint interval.
		time.Sleep(2500 * time.Millisecond)
		status, err := store.Status(ctx)
		if err != nil || status.Dirty || status.Revision != original.Revision {
			t.Fatalf("pending status changed Git: %+v %v", status, err)
		}
		committed, err := store.Read(ctx, "note.md", "")
		if err != nil || committed.Revision != original.Revision || string(committed.Content) != string(original.Content) {
			t.Fatalf("live editing advanced Git without Save: %+v %v", committed, err)
		}
		if history := noteRevisions(t, store, "note.md"); len(history) != 1 {
			t.Fatalf("pending edit added history: %d", len(history))
		}
		live, err := store.LiveRead(ctx, "note.md")
		if err != nil || !live.Pending || live.Content != accepted.Content || live.LiveRevision != accepted.LiveRevision {
			t.Fatalf("accepted pending state changed: %+v %v", live, err)
		}
	}
	assertPending(s)
	// Even before a restart recovers live edits, their first use finds them.
	reopened := reopenTestStore(t, s)
	assertPending(reopened)
	if _, err = reopened.LivePatch(ctx, patch); err != nil {
		t.Fatal("identical retry after restart", err)
	}
	saved, err := reopened.LiveCheckpoint(ctx, "note.md")
	if err != nil || saved.Pending || saved.Content != accepted.Content || saved.Revision == original.Revision {
		t.Fatalf("explicit checkpoint failed: %+v %v", saved, err)
	}
	retried, err := reopened.LiveCheckpoint(ctx, "note.md")
	if err != nil || retried.Revision != saved.Revision {
		t.Fatalf("checkpoint retry changed Git: %+v %v", retried, err)
	}
	if history := noteRevisions(t, reopened, "note.md"); len(history) != 2 {
		t.Fatalf("expected exactly one new version: %d", len(history))
	}
	committed, err := reopened.Read(ctx, "note.md", "")
	if err != nil || string(committed.Content) != accepted.Content {
		t.Fatalf("explicit checkpoint lost accepted content: %+v %v", committed, err)
	}
}

func TestLiveDisjointPatchesOverlapAndReplay(t *testing.T) {
	s, ctx, git := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	patches := []TargetedPatch{
		{Path: "note.md", Base: base.LiveRevision, OperationID: "one", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "ALPHA"}}},
		{Path: "note.md", Base: base.LiveRevision, OperationID: "two", Edits: []TextEdit{{Start: 10, End: 14, Old: "beta", Text: "BETA"}}},
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, patch := range patches {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.LivePatch(ctx, patch); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	live, err := s.LiveRead(ctx, "note.md")
	if err != nil || live.Content != "😀 ALPHA\n\nBETA\n" || !live.Pending || live.Revision != git.Revision {
		t.Fatalf("live=%+v err=%v", live, err)
	}
	if _, err = s.LivePatch(ctx, patches[0]); err != nil {
		t.Fatal("identical retry", err)
	}
	patches[0].Edits[0].Text = "different"
	if _, err = s.LivePatch(ctx, patches[0]); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry accepted: %v", err)
	}
	patches[0].OperationID = "overlap"
	if _, err = s.LivePatch(ctx, patches[0]); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlap accepted: %v", err)
	}
	if _, err = s.Save(ctx, "note.md", []byte("clobber"), git.Revision, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy save overwrote live: %v", err)
	}
	checkpoint, err := s.LiveCheckpoint(ctx, "note.md")
	if err != nil || checkpoint.Pending {
		t.Fatalf("checkpoint: %+v %v", checkpoint, err)
	}
	committed, err := s.Read(ctx, "note.md", "")
	if err != nil || string(committed.Content) != live.Content {
		t.Fatalf("Git differs: %+v %v", committed, err)
	}
	if log := revisionMessage(t, s, ""); !strings.Contains(log, "Collaboration-Receipt:") || !strings.Contains(log, "agent-a") {
		t.Fatalf("missing authenticated audit: %s", log)
	}
	if _, err = s.LivePatch(ctx, patches[1]); err != nil {
		t.Fatal("retry after checkpoint", err)
	}
}

// A checkpoint commit is the one the Git backend made: its subject, the
// collaboration identity, the operation trailers and one receipt trailer for
// each accepted operation, in order, byte for byte.
func TestLiveCheckpointCommitKeepsTheGitBackendsFormat(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	live, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "ALPHA"}, {Start: 3, End: 8, Old: "ALPHA", Text: "ALPHA!"}} {
		if live, err = s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: live.LiveRevision, OperationID: edit.Old, Edits: []TextEdit{edit}}); err != nil {
			t.Fatal(err)
		}
	}
	v := *s.live["note.md"]
	op := dbLiveCheckpointIdentity(&v)
	checkpoint, err := s.LiveCheckpoint(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	want := "Checkpoint live edits to note.md\n\nGitnotes-Source: collaboration\nGitnotes-Operation: " + op.ID + "\nGitnotes-Request: " + op.Request
	for _, id := range v.Uncommitted {
		raw, _ := json.Marshal(struct {
			Operation string      `json:"operation"`
			Receipt   liveReceipt `json:"receipt"`
		}{id, v.Receipts[id]})
		want += "\nCollaboration-Receipt: " + string(raw)
	}
	want += "\n"
	if message := revisionMessage(t, s, checkpoint.Revision); len(v.Uncommitted) != 2 || message != want {
		t.Fatalf("checkpoint message:\n%q\nwant\n%q", message, want)
	}
	id, _ := gitobj.ParseID(checkpoint.Revision)
	info, err := s.notes.Commit(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	author := info.Commit.Author.Name + " <" + info.Commit.Author.Email + ">"
	committer := info.Commit.Committer.Name + " <" + info.Commit.Committer.Email + ">"
	if author != "ForgetMeNote collaboration <forgetmenote@localhost>" || committer != "ForgetMeNote <forgetmenote@localhost>" {
		t.Fatalf("checkpoint identities: %q %q", author, committer)
	}
}

func TestLivePendingRestartAndFailedGitCheckpoint(t *testing.T) {
	s, ctx, original := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	restore := failCommits(t, s)
	patch := TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "durable", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "durable"}}}
	accepted, err := s.LivePatch(ctx, patch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LiveCheckpoint(ctx, "note.md"); err == nil {
		t.Fatal("checkpoint unexpectedly succeeded")
	}
	before, err := s.Read(ctx, "note.md", "")
	if err != nil || string(before.Content) != string(original.Content) {
		t.Fatal("failed checkpoint changed Git")
	}
	s2, err := reopenLiveStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	live, err := s2.LiveRead(ctx, "note.md")
	if err != nil || live.Content != accepted.Content || !live.Pending {
		t.Fatalf("pending lost: %+v %v", live, err)
	}
	restore()
	if _, err = s2.LivePatch(ctx, patch); err != nil {
		t.Fatal("restart retry", err)
	}
	if _, err = s2.LiveCheckpoint(ctx, "note.md"); err != nil {
		t.Fatal(err)
	}
	if committed, err := s2.Read(ctx, "note.md", ""); err != nil || string(committed.Content) != accepted.Content {
		t.Fatal("checkpoint lost content", err)
	}
}

func TestLiveRestartPreservesExcludedNotes(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "pending"}[pending], func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			const path, content = "private/hidden.md", "hidden evidence\n"
			if _, err := s.Save(ctx, path, []byte(content), "", ""); err != nil {
				t.Fatal(err)
			}
			before, err := s.LiveRead(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if pending {
				before, err = s.LivePatch(ctx, TargetedPatch{Path: path, Base: before.LiveRevision, OperationID: "excluded-pending", Edits: []TextEdit{{Start: 0, End: 6, Old: "hidden", Text: "accepted hidden"}}})
				if err != nil || !before.Pending {
					t.Fatalf("accept pending text: %+v %v", before, err)
				}
			}
			env := testEnv(t, s)
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenDB(ctx, openTestNotes(t, env), s.notebook, filepath.Dir(s.Root), []string{"private"})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err = recoverLive(ctx, reopened); err != nil {
				t.Fatalf("browse exclusions prevented live recovery: %v", err)
			}
			after, err := reopened.LiveRead(ctx, path)
			if err != nil || after.Content != before.Content || after.LiveRevision != before.LiveRevision || after.Pending != pending {
				t.Fatalf("excluded live state lost: %+v %v", after, err)
			}
			if pending {
				checkpoint, err := reopened.LiveCheckpoint(ctx, path)
				if err != nil || checkpoint.Pending || checkpoint.Content != before.Content || checkpoint.Revision == before.Revision {
					t.Fatalf("excluded accepted text did not checkpoint: %+v %v", checkpoint, err)
				}
			}
			committed, err := reopened.Read(ctx, path, "")
			if err != nil || string(committed.Content) != before.Content {
				t.Fatalf("excluded Git content lost: %+v %v", committed, err)
			}
			if entries, err := reopened.List(ctx); err != nil || len(entries) != 0 {
				t.Fatalf("excluded note appeared in browse: %+v %v", entries, err)
			}
			if entries, err := reopened.Search(ctx, "evidence"); err != nil || len(entries) != 0 {
				t.Fatalf("excluded note appeared in search: %+v %v", entries, err)
			}
			if snapshot, err := reopened.Snapshot(ctx, ""); err != nil || len(snapshot.Files) != 0 {
				t.Fatalf("excluded note appeared in search snapshot: %+v %v", snapshot, err)
			}
		})
	}
}

func TestLiveDirectAccessIgnoresBrowseExclusionsAndPreservesPathSafety(t *testing.T) {
	ctx := context.Background()
	s := newTestStoreExcluding(t, []string{"private"})
	if _, err := s.Save(ctx, "private/hidden.md", []byte("before"), "", ""); err != nil {
		t.Fatal(err)
	}
	base, err := s.LiveRead(ctx, "private/hidden.md")
	if err != nil {
		t.Fatalf("direct live read treated browse exclusion as access control: %v", err)
	}
	if _, err = s.LivePatch(ctx, TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "direct-excluded", Edits: []TextEdit{{Start: 0, End: 6, Old: "before", Text: "after"}}}); err != nil {
		t.Fatal("direct live edit", err)
	}
	if _, err = s.LiveCheckpoint(ctx, base.Path); err != nil {
		t.Fatal("direct checkpoint", err)
	}
	commitFixture(t, s, notestore.FileChange{Path: "alias", Content: []byte("private"), Mode: gitobj.ModeSymlink})
	for _, path := range []string{"AGENTS.md", ".git/config.md", "private/../private/hidden.md", "/private/hidden.md", "private/hidden.txt"} {
		if _, err = s.LiveRead(ctx, path); !errors.Is(err, ErrForbidden) {
			t.Errorf("unsafe or protected live path %q: %v", path, err)
		}
	}
	// The tree never follows a link: a path through one names no note.
	if _, err = s.LiveRead(ctx, "alias/hidden.md"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("live path through a link: %v", err)
	}
}

func TestLiveYjsConcurrentUpdatesUnicodeAndMalformedRollback(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := browserDoc(base.Update)
	b, _ := browserDoc(base.Update)
	a.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, 3, "first ", nil) })
	b.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, 10, "second ", nil) })
	for i, doc := range []*crdt.Doc{a, b} {
		id := string(rune('a' + i))
		if _, err = s.LiveApply(ctx, LiveUpdate{Path: "note.md", Generation: strings.Split(base.LiveRevision, ":")[0], OperationID: id, Update: crdt.EncodeStateAsUpdateV1(doc, nil)}); err != nil {
			t.Fatal(err)
		}
	}
	live, err := s.LiveRead(ctx, "note.md")
	if err != nil || live.Content != "😀 first alpha\n\nsecond beta\n" {
		t.Fatalf("merge: %q %v", live.Content, err)
	}
	if _, err = s.LiveApply(ctx, LiveUpdate{Path: "note.md", Generation: strings.Split(base.LiveRevision, ":")[0], OperationID: "invalid", Update: []byte{255, 255, 255}}); err == nil {
		t.Fatal("accepted malformed update")
	}
	foreign := crdt.New()
	foreign.Transact(func(tx *crdt.Transaction) { tx.GetText("secret").Insert(tx, 0, "hidden", nil) })
	if _, err = s.LiveApply(ctx, LiveUpdate{Path: "note.md", Generation: strings.Split(base.LiveRevision, ":")[0], OperationID: "foreign", Update: crdt.EncodeStateAsUpdateV1(foreign, nil)}); err == nil {
		t.Fatal("accepted foreign root")
	}
	after, _ := s.LiveRead(ctx, "note.md")
	if after.LiveRevision != live.LiveRevision || after.Content != live.Content {
		t.Fatal("rejected update mutated live state")
	}
	bad := TargetedPatch{Path: "note.md", Base: after.LiveRevision, OperationID: "half-surrogate", Edits: []TextEdit{{Start: 1, End: 2, Old: "�", Text: "x"}}}
	if _, err = s.LivePatch(ctx, bad); err == nil {
		t.Fatal("accepted split surrogate")
	}
}

// A checkpoint that committed but could not record so in its live document,
// as after a crash between the two, is recognized: retrying it makes no
// second commit, and its receipt survives later writes.
func TestLiveCheckpointCrashReceiptAndOtherNotes(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	p := TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "noop", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "alpha"}}}
	if _, err = s.LivePatch(ctx, p); err != nil {
		t.Fatal(err)
	}
	restore := failLiveRecords(t, s)
	if _, err = s.LiveCheckpoint(ctx, "note.md"); err == nil {
		t.Fatal("checkpoint recorded in a live document that refuses writes")
	}
	restore()
	state, _ := s.Status(ctx)
	if !strings.HasPrefix(revisionMessage(t, s, state.Revision), "Checkpoint live edits to note.md\n") {
		t.Fatal("the interrupted checkpoint did not commit")
	}
	if _, err = s.LiveCheckpoint(ctx, "note.md"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Status(ctx)
	if state.Revision != after.Revision {
		t.Fatal("replayed checkpoint made duplicate commit")
	}
	base, _ = s.LiveRead(ctx, "note.md")
	if base.Pending {
		t.Fatal("recorded checkpoint left the edit pending")
	}
	p.Base = base.LiveRevision
	p.OperationID = "pending"
	p.Edits[0].Text = "pending"
	if _, err = s.LivePatch(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, "other.md", []byte("independent"), "", ""); err != nil {
		t.Fatal("other note blocked", err)
	}
	if _, err = s.LivePatch(ctx, p); err != nil {
		t.Fatal("receipt lost after ordinary write", err)
	}
}

func TestLiveExplicitCheckpointAndLockRecheck(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, _ := s.LiveRead(ctx, "note.md")
	p := TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "explicit", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "checkpointed"}}}
	if _, err := s.LivePatch(ctx, p); err != nil {
		t.Fatal(err)
	}
	if live, err := s.LiveCheckpoint(ctx, "note.md"); err != nil || live.Pending {
		t.Fatalf("explicit checkpoint failed: %+v %v", live, err)
	}
	commitFixture(t, s, fixture(locksPath, `{"version":1,"paths":["note.md"]}`))
	p.OperationID = "locked"
	if _, err := s.LivePatch(ctx, p); !errors.Is(err, ErrForbidden) {
		t.Fatalf("lock not rechecked: %v", err)
	}
}

func TestLiveOperationIDIsNotebookWideWhilePending(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	if _, err := s.Save(ctx, "other.md", []byte("original"), "", ""); err != nil {
		t.Fatal(err)
	}
	a, _ := s.LiveRead(ctx, "note.md")
	b, _ := s.LiveRead(ctx, "other.md")
	first := TargetedPatch{Path: a.Path, Base: a.LiveRevision, OperationID: "shared-id", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "changed"}}}
	if _, err := s.LivePatch(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := TargetedPatch{Path: b.Path, Base: b.LiveRevision, OperationID: "shared-id", Edits: []TextEdit{{Start: 0, End: 8, Old: "original", Text: "changed other"}}}
	if _, err := s.LivePatch(ctx, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("operation accepted on another path: %v", err)
	}
	for _, path := range []string{a.Path, b.Path} {
		v, err := s.LiveCheckpoint(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := s.Read(ctx, path, "")
		if err != nil || v.Pending || v.Content != string(doc.Content) {
			t.Fatalf("false checkpoint: %+v %+v %v", v, doc, err)
		}
	}
	if _, err := s.LivePatch(ctx, first); err != nil {
		t.Fatal("exact retry:", err)
	}
	if _, err := s.LivePatch(ctx, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("permanent receipt overwritten: %v", err)
	}
}

func TestLiveInsertionRejectsDeletedGapButRebasesIndependentChange(t *testing.T) {
	s, ctx, git := liveTestStore(t)
	if _, err := s.Save(ctx, "note.md", []byte("alpha beta"), git.Revision, ""); err != nil {
		t.Fatal(err)
	}
	base, _ := s.LiveRead(ctx, "note.md")
	insert := TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "insert", Edits: []TextEdit{{Start: 5, End: 5, Text: " AGENT"}}}
	if _, err := s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "rewrite", Edits: []TextEdit{{Start: 0, End: 10, Old: "alpha beta", Text: "human rewrite"}}}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.LiveRead(ctx, "note.md")
	if _, err := s.LivePatch(ctx, insert); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleted insertion gap accepted: %v", err)
	}
	after, _ := s.LiveRead(ctx, "note.md")
	if after.LiveRevision != before.LiveRevision || after.Content != before.Content {
		t.Fatal("rejected insertion mutated document")
	}
	base, _ = s.LiveRead(ctx, "note.md")
	insert.Base = base.LiveRevision
	insert.OperationID = "independent-insert"
	insert.Edits[0].Start = 5
	insert.Edits[0].End = 5
	if _, err := s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "independent-tail", Edits: []TextEdit{{Start: 6, End: 13, Old: "rewrite", Text: "text"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LivePatch(ctx, insert); err != nil {
		t.Fatal("independent insertion failed:", err)
	}
	after, _ = s.LiveRead(ctx, "note.md")
	if after.Content != "human AGENT text" {
		t.Fatal(after.Content)
	}
}

func TestLiveCheckpointRejectsMarkerWithoutExpectedContent(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, _ := s.LiveRead(ctx, "note.md")
	if _, err := s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "marker-content", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "accepted"}}}); err != nil {
		t.Fatal(err)
	}
	op := dbLiveCheckpointIdentity(s.live["note.md"])
	if _, err := s.notes.Apply(ctx, notestore.ChangeSet{Subject: "Synthetic mismatched checkpoint", Author: fixtureAuthor, OperationID: op.ID, RequestID: op.Request, AllowEmpty: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LiveCheckpoint(ctx, "note.md"); !errors.Is(err, ErrConflict) {
		t.Fatalf("false checkpoint succeeded: %v", err)
	}
	live, err := s.LiveRead(ctx, "note.md")
	if err != nil || !live.Pending || !strings.Contains(live.Content, "accepted") {
		t.Fatalf("pending state lost: %+v %v", live, err)
	}
}

func TestLivePatchRequiresOriginalTargetItems(t *testing.T) {
	for _, source := range []string{"browser", "agent"} {
		for _, row := range []struct {
			name, content  string
			change, target TextEdit
			want           string
			conflict       bool
		}{
			{"deleted repeated target", "alpha alpha", TextEdit{Start: 0, End: 6, Old: "alpha "}, TextEdit{Start: 0, End: 5, Old: "alpha", Text: "AGENT"}, "alpha", true},
			{"identical replacement", "alpha alpha", TextEdit{Start: 0, End: 5, Old: "alpha", Text: "alpha"}, TextEdit{Start: 0, End: 5, Old: "alpha", Text: "AGENT"}, "alpha alpha", true},
			{"identical interior replacement", "alpha alpha", TextEdit{Start: 1, End: 4, Old: "lph", Text: "lph"}, TextEdit{Start: 0, End: 5, Old: "alpha", Text: "AGENT"}, "alpha alpha", true},
			{"partial interior deletion", "aaaaaa", TextEdit{Start: 1, End: 2, Old: "a"}, TextEdit{Start: 0, End: 4, Old: "aaaa", Text: "AGENT"}, "aaaaa", true},
			{"independent repeated prefix deletion", "alpha alpha", TextEdit{Start: 0, End: 6, Old: "alpha "}, TextEdit{Start: 6, End: 11, Old: "alpha", Text: "AGENT"}, "AGENT", false},
			{"independent tail replacement", "alpha alpha", TextEdit{Start: 6, End: 11, Old: "alpha", Text: "beta"}, TextEdit{Start: 0, End: 5, Old: "alpha", Text: "AGENT"}, "AGENT beta", false},
		} {
			t.Run(source+"/"+row.name, func(t *testing.T) {
				s, ctx, saved := liveTestStore(t)
				if _, err := s.Save(ctx, "note.md", []byte(row.content), saved.Revision, ""); err != nil {
					t.Fatal(err)
				}
				base, err := s.LiveRead(ctx, "note.md")
				if err != nil {
					t.Fatal(err)
				}
				if source == "browser" {
					browser, err := browserDoc(base.Update)
					if err != nil {
						t.Fatal(err)
					}
					browser.Transact(func(tx *crdt.Transaction) {
						text := tx.GetText("markdown")
						text.Delete(tx, row.change.Start, row.change.End-row.change.Start)
						if row.change.Text != "" {
							text.Insert(tx, row.change.Start, row.change.Text, nil)
						}
					})
					_, err = s.LiveApply(ctx, LiveUpdate{Path: base.Path, Generation: strings.Split(base.LiveRevision, ":")[0], OperationID: "browser-change", Update: crdt.EncodeStateAsUpdateV1(browser, nil)})
					if err != nil {
						t.Fatal(err)
					}
				} else if _, err = s.LivePatch(ctx, TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "earlier-agent-change", Edits: []TextEdit{row.change}}); err != nil {
					t.Fatal(err)
				}
				before, err := s.LiveRead(ctx, base.Path)
				if err != nil {
					t.Fatal(err)
				}
				stored := liveStored(t, s, base.Path)
				_, err = s.LivePatch(ctx, TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "stale-target", Edits: []TextEdit{row.target}})
				if row.conflict && !errors.Is(err, ErrConflict) {
					t.Fatalf("original target was deleted/replaced: expected conflict, got %v", err)
				}
				if !row.conflict && err != nil {
					t.Fatalf("independent target did not rebase: %v", err)
				}
				after, err := s.LiveRead(ctx, base.Path)
				if err != nil || after.Content != row.want {
					t.Fatalf("live text: %q, want %q (%v)", after.Content, row.want, err)
				}
				if row.conflict && (after.LiveRevision != before.LiveRevision || liveStored(t, s, base.Path) != stored) {
					t.Fatal("rejected target changed accepted state or receipts")
				}
				if _, err = s.LiveCheckpoint(ctx, base.Path); err != nil {
					t.Fatal(err)
				}
				committed, err := s.Read(ctx, base.Path, "")
				if err != nil || string(committed.Content) != row.want {
					t.Fatalf("checkpoint text: %q (%v)", committed.Content, err)
				}
			})
		}
	}
}

func TestLiveRecoveryRejectsUnrelatedMatchingGit(t *testing.T) {
	for _, marker := range []string{"absent", "wrong operation", "wrong request", "earlier valid checkpoint"} {
		t.Run(marker, func(t *testing.T) {
			s, ctx, _ := liveTestStore(t)
			base, err := s.LiveRead(ctx, "note.md")
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := s.LivePatch(ctx, TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "attributed-operation", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "accepted"}}})
			if err != nil {
				t.Fatal(err)
			}
			op := dbLiveCheckpointIdentity(s.live[base.Path])
			external := notestore.ChangeSet{Subject: "Unrelated operator replacement", Author: fixtureAuthor, Files: []notestore.FileChange{fixture(base.Path, accepted.Content)}}
			switch marker {
			case "wrong operation":
				external.OperationID, external.RequestID = ContentHash([]byte("unrelated operation")), op.Request
			case "wrong request":
				external.OperationID, external.RequestID = op.ID, ContentHash([]byte("unrelated request"))
			case "earlier valid checkpoint":
				// The checkpoint commits, but its document never records it.
				restore := failLiveRecords(t, s)
				if _, err = s.LiveCheckpoint(ctx, base.Path); err == nil {
					t.Fatal("checkpoint recorded in a live document that refuses writes")
				}
				restore()
				commitFixture(t, s, fixture(base.Path, "different external text"))
			}
			if _, err = s.notes.Apply(ctx, external); err != nil {
				t.Fatal(err)
			}
			pending := liveStored(t, s, base.Path)
			head, err := s.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if current, err := s.LiveRead(ctx, base.Path); !errors.Is(err, ErrConflict) {
				t.Fatalf("unrelated matching Git cleared live audit: pending=%v err=%v", current.Pending, err)
			}
			if _, err = s.LiveCheckpoint(ctx, base.Path); !errors.Is(err, ErrConflict) {
				t.Fatalf("unrelated matching Git checkpoint: %v", err)
			}
			if liveStored(t, s, base.Path) != pending {
				t.Fatal("recovery changed pending state or receipts")
			}
			status, err := s.Status(ctx)
			if err != nil || status.Revision != head.Revision {
				t.Fatal("rejected recovery changed Git")
			}
			if _, err = reopenLiveStore(t, s); !errors.Is(err, ErrConflict) {
				t.Fatalf("restart recovery accepted unrelated matching Git: %v", err)
			}
			if liveStored(t, s, base.Path) != pending {
				t.Fatal("failed restart changed pending state")
			}
		})
	}
}

func TestLiveRecoveryRecognizesAttributedInterruptedCheckpoint(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	patch := TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "interrupted-checkpoint", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "accepted"}}}
	accepted, err := s.LivePatch(ctx, patch)
	if err != nil {
		t.Fatal(err)
	}
	op := dbLiveCheckpointIdentity(s.live[base.Path])
	restore := failLiveRecords(t, s)
	if _, err = s.LiveCheckpoint(ctx, base.Path); err == nil {
		t.Fatal("checkpoint recorded in a live document that refuses writes")
	}
	restore()
	checkpoint, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head := commitFixture(t, s, fixture("other.md", "unrelated note"))
	recovered, err := reopenLiveStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	live, err := recovered.LiveRead(ctx, base.Path)
	if err != nil || live.Pending || live.Content != accepted.Content || live.LiveRevision != accepted.LiveRevision || live.Version != checkpoint.Revision {
		t.Fatalf("interrupted checkpoint recovery: %+v %v", live, err)
	}
	if _, err = recovered.LivePatch(ctx, patch); err != nil {
		t.Fatalf("recovered identical retry: %v", err)
	}
	patch.Edits[0].Text = "changed payload"
	if _, err = recovered.LivePatch(ctx, patch); !errors.Is(err, ErrConflict) {
		t.Fatalf("recovered changed retry: %v", err)
	}
	status, err := recovered.Status(ctx)
	if err != nil || status.Revision != head {
		t.Fatal("recovery or retry duplicated the Git commit")
	}
	audit := revisionMessage(t, recovered, checkpoint.Revision)
	if !strings.Contains(audit, "Gitnotes-Operation: "+op.ID) || !strings.Contains(audit, "Gitnotes-Request: "+op.Request) || !strings.Contains(audit, "Collaboration-Receipt:") || !strings.Contains(audit, "agent-a") {
		t.Fatalf("checkpoint audit missing: %s", audit)
	}
}

func TestLivePatchKeepsOriginalTargetAcrossMergedBrowserInsertions(t *testing.T) {
	s, ctx, saved := liveTestStore(t)
	if _, err := s.Save(ctx, "note.md", []byte("alpha"), saved.Revision, ""); err != nil {
		t.Fatal(err)
	}
	initial, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	browser, err := browserDoc(initial.Update)
	if err != nil {
		t.Fatal(err)
	}
	generation := strings.Split(initial.LiveRevision, ":")[0]
	browser.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, 2, "x", nil) })
	base, err := s.LiveApply(ctx, LiveUpdate{Path: initial.Path, Generation: generation, OperationID: "insert-x", Update: crdt.EncodeStateAsUpdateV1(browser, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if base.Content != "alxpha" {
		t.Fatal(base.Content)
	}
	browser.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, 3, "p", nil) })
	if _, err = s.LiveApply(ctx, LiveUpdate{Path: initial.Path, Generation: generation, OperationID: "insert-duplicate-p", Update: crdt.EncodeStateAsUpdateV1(browser, nil)}); err != nil {
		t.Fatal(err)
	}
	patched, err := s.LivePatch(ctx, TargetedPatch{Path: initial.Path, Base: base.LiveRevision, OperationID: "replace-original-p", Edits: []TextEdit{{Start: 3, End: 4, Old: "p", Text: "AGENT"}}})
	if err != nil || patched.Content != "alxpAGENTha" {
		t.Fatalf("patch targeted newly inserted identical text: %q (%v)", patched.Content, err)
	}
}

func TestContainerInvalidLiveCheckpointRetainsPendingText(t *testing.T) {
	s, ctx, doc := liveTestStore(t)
	base, _ := s.LiveRead(ctx, doc.Path)
	text := "---\nfmn-container: future\n---\n"
	accepted, err := s.LivePatch(ctx, TargetedPatch{Path: doc.Path, Base: base.LiveRevision, OperationID: "invalid-container", Edits: []TextEdit{{Start: 0, End: 0, Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LiveCheckpoint(ctx, doc.Path); err == nil {
		t.Fatal("invalid checkpoint accepted")
	}
	live, err := s.LiveRead(ctx, doc.Path)
	if err != nil || !live.Pending || live.Content != accepted.Content {
		t.Fatalf("lost pending text: %+v %v", live, err)
	}
	current, err := s.Read(ctx, doc.Path, "")
	if err != nil || string(current.Content) != string(doc.Content) {
		t.Fatalf("invalid text reached Git: %+v %v", current, err)
	}
	reopened, err := reopenLiveStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	live, err = reopened.LiveRead(ctx, doc.Path)
	if err != nil || !live.Pending || live.Content != accepted.Content {
		t.Fatalf("restart lost rejected pending text: %+v %v", live, err)
	}
}
