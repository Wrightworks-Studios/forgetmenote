package workspace

import (
	"context"
	"errors"
	"sync"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

func TestLockChangeFailurePreservesPolicy(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "notes/a.md", []byte("original"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	locked, err := s.SetLock(ctx, "notes", true, doc.Revision, "admin")
	if err != nil {
		t.Fatal(err)
	}
	before := committedPolicy(t, s)
	restore := failCommits(t, s)
	if _, err = s.SetLock(ctx, "notes", false, locked.Revision, "admin"); err == nil {
		t.Fatal("expected commit failure")
	}
	restore()
	if after := committedPolicy(t, s); after != before {
		t.Fatal("failed unlock changed policy")
	}
	status, err := s.Status(ctx)
	if err != nil || status.Dirty || status.Revision != locked.Revision {
		t.Fatalf("failed unlock damaged history: %+v %v", status, err)
	}
	if _, err = s.Save(ctx, doc.Path, []byte("no"), locked.Revision, "writer"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("failed unlock took effect: %v", err)
	}
	if state, err := reopenTestStore(t, s).Locks(ctx, doc.Path); err != nil || state.Revision != locked.Revision || state.Covering(doc.Path) != "notes" {
		t.Fatalf("the lock after a restart: %+v %v", state, err)
	}
}
func TestConcurrentLockAndSaveHaveOneWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "notes/a.md", []byte("original"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := s.SetLock(ctx, "notes", true, doc.Revision, "admin")
		results <- err
	}()
	go func() {
		defer wg.Done()
		_, err := s.Save(ctx, doc.Path, []byte("revised"), doc.Revision, "writer")
		results <- err
	}()
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent operation winners=%d", wins)
	}
	state, err := s.Locks(ctx, doc.Path)
	if err != nil {
		t.Fatal(err)
	}
	now, err := s.Read(ctx, doc.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Rules) > 0 && string(now.Content) != "original" {
		t.Fatal("lock and save both took effect")
	}
}
func TestMalformedOrSymlinkPolicyFailsClosed(t *testing.T) {
	for _, kind := range []string{"malformed", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			lockManuscript(t, s)
			ctx := context.Background()
			if kind == "malformed" {
				commitFixture(t, s, fixture(locksPath, `{"version":999,"paths":[]}`))
			} else {
				commitFixture(t, s, notestore.FileChange{Path: locksPath, Content: []byte("../outside"), Mode: gitobj.ModeSymlink})
			}
			if _, err := s.Save(ctx, "new.md", []byte("no"), "", "test"); err == nil {
				t.Fatal("invalid policy allowed a write")
			}
		})
	}
}
