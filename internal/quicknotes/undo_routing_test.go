package quicknotes

import (
	"context"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/workspace"
)

func automaticUndoFixture(t *testing.T) (*Service, *catalog.Catalog, string, *decision.Service, *atomic.Int32, Capture) {
	t.Helper()
	s, c, root := fixture(t)
	stopScanWorker(t, s)
	routeTarget(t, c)
	calls := new(atomic.Int32)
	judge := testJudge(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); routeReply(w, r, "destination_000") })
	s.ConfigureJudgments(judge, false)
	settings, err := s.Settings("alice")
	if err != nil {
		t.Fatal(err)
	}
	settings.Mode = "automatic"
	settings.CloudConsent = true
	settings.ApprovedDestinations, err = s.Destinations(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateSettings("alice", settings); err != nil {
		t.Fatal(err)
	}
	capture, err := s.Capture(context.Background(), "alice", "automatic-undo", "# Planning\nKeep the original private capture.")
	if err != nil {
		t.Fatal(err)
	}
	s.scan(s.ctx)
	list, err := s.List(context.Background(), "alice")
	if err != nil || len(list) != 1 || list[0].Status != "filed" || calls.Load() != 1 {
		t.Fatal("initial automatic filing failed", list, err, calls.Load())
	}
	return s, c, root, judge, calls, capture
}

func assertUndoHeld(t *testing.T, s *Service, c *catalog.Catalog, id string, calls *atomic.Int32) {
	t.Helper()
	for range 3 {
		s.scan(s.ctx)
	}
	// Eligibility may have been checked before Undo completed. The locked
	// background execution path must independently honor the durable hold.
	if _, err := s.sort(context.Background(), "alice", id, false); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(context.Background(), "alice")
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Status != "inbox" || calls.Load() != 1 {
		t.Fatalf("Undo was overridden: captures=%+v calls=%d error=%v", list, calls.Load(), err)
	}
	if list[0].Project != list[0].InboxProject || list[0].Suggestion != nil {
		t.Fatal("Undo did not retain the private Inbox", list[0])
	}
	target, err := c.Access("alice", "target", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = target.Read(context.Background(), list[0].InboxPath, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("undone capture republished to shared target", err)
	}
}

func TestAutomaticUndoStaysInInboxAcrossSettingsAndRestart(t *testing.T) {
	s, c, root, judge, calls, capture := automaticUndoFixture(t)
	undone, err := s.Undo(context.Background(), "alice", capture.ID)
	if err != nil || undone.Status != "inbox" {
		t.Fatal("Undo failed", undone, err)
	}
	assertUndoHeld(t, s, c, capture.ID, calls)
	settings, err := s.Settings("alice")
	if err != nil {
		t.Fatal(err)
	}
	settings.NotifyDestination = !settings.NotifyDestination
	if _, err = s.UpdateSettings("alice", settings); err != nil {
		t.Fatal(err)
	}
	inbox, err := c.Access("alice", undone.Project, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = inbox.Save(context.Background(), "bookkeeping.md", []byte("unrelated notebook commit"), "", "alice"); err != nil {
		t.Fatal(err)
	}
	assertUndoHeld(t, s, c, capture.ID, calls)
	s.Close()
	reopened, err := Open(pgtest.Dir(t, filepath.Join(root, "catalog")), c)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.Start(); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stopScanWorker(t, reopened)
	reopened.ConfigureJudgments(judge, false)
	assertUndoHeld(t, reopened, c, capture.ID, calls)
	filed, err := reopened.Sort(context.Background(), "alice", capture.ID)
	if err != nil || filed.Status != "filed" || calls.Load() != 2 {
		t.Fatal("explicit sorting did not release the hold", filed, err, calls.Load())
	}
}

func TestUndoHoldAllowsExplicitFilingAndChangedContent(t *testing.T) {
	for _, action := range []string{"file", "edit"} {
		t.Run(action, func(t *testing.T) {
			s, c, _, _, calls, capture := automaticUndoFixture(t)
			undone, err := s.Undo(context.Background(), "alice", capture.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertUndoHeld(t, s, c, capture.ID, calls)
			settings, err := s.Settings("alice")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.file(context.Background(), "alice", capture.ID, Destination{Project: "target"}, undone.Version, true, settings.Generation); !errors.Is(err, workspace.ErrConflict) {
				t.Fatal("automatic filing bypassed Undo hold", err)
			}
			switch action {
			case "file":
				if _, err = s.File(context.Background(), "alice", capture.ID, Destination{Project: "target"}, undone.Version); err != nil {
					t.Fatal("explicit filing blocked by hold", err)
				}
			case "edit":
				inbox, err := c.Access("alice", undone.Project, "owner")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = inbox.Save(context.Background(), undone.Path, []byte("# Planning\nChanged capture content becomes new routing work."), undone.Version, "alice"); err != nil {
					t.Fatal(err)
				}
			}
			s.scan(s.ctx)
			list, err := s.List(context.Background(), "alice")
			if err != nil || len(list) != 1 || list[0].Status != "filed" {
				t.Fatal("new filing choice did not progress", list, err)
			}
			wantCalls := int32(1)
			if action == "edit" {
				wantCalls = 2
			}
			if calls.Load() != wantCalls {
				t.Fatal("unexpected judgments for explicit choice", calls.Load(), wantCalls)
			}
			account, err := c.Account("alice")
			if err != nil {
				t.Fatal(err)
			}
			record, err := s.readRecord(account.ID, capture.ID)
			if err != nil || record.RoutingHoldHash != "" {
				t.Fatal("superseded hold not cleared", err)
			}
		})
	}
}

func TestInterruptedUndoRecoveryKeepsRoutingHold(t *testing.T) {
	s, c, root, judge, calls, capture := automaticUndoFixture(t)
	ctx := context.Background()
	if _, err := c.Access("alice", "target", "owner"); err != nil {
		t.Fatal(err)
	}
	restore := failNotebookCommits(t, s.db, "target")
	if _, err := s.Undo(ctx, "alice", capture.ID); err == nil {
		t.Fatal("refused source commit unexpectedly completed")
	}
	account, err := c.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.readRecord(account.ID, capture.ID)
	if err != nil || pending.Pending == nil || !pending.Pending.Undo || pending.Status != "undoing" {
		t.Fatal("Undo did not retain its durable transfer", pending.Status, err)
	}
	inbox, err := c.Access("alice", capture.InboxProject, "owner")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := inbox.Read(ctx, capture.InboxPath, "")
	if err != nil {
		t.Fatal("fault did not occur after Inbox receipt commit", err)
	}
	restore()
	s.Close()
	c.Close()
	reopenedCatalog, err := catalog.Open(filepath.Join(root, "catalog"), nil, pgtest.Dir(t, filepath.Join(root, "catalog")))
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedCatalog.Close()
	reopened, err := Open(pgtest.Dir(t, filepath.Join(root, "catalog")), reopenedCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.Start(); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stopScanWorker(t, reopened)
	reopened.ConfigureJudgments(judge, false)
	// The first scan reconciles the durable Undo, subsequent scans must honor its
	// recovered hold and must not buy another judgment or republish the note.
	assertUndoHeld(t, reopened, reopenedCatalog, capture.ID, calls)
	list, err := reopened.List(ctx, "alice")
	if err != nil || len(list) != 1 || list[0].Version != restored.Version {
		t.Fatal("Undo recovery duplicated its restored commit", list, err)
	}
	replay, err := reopened.Recover(ctx, "alice", capture.ID)
	if err != nil || replay.Version != restored.Version {
		t.Fatal("Undo recovery lost its exact receipt", replay, err)
	}
	if _, err = reopened.Sort(ctx, "alice", capture.ID); err != nil {
		t.Fatal("explicit sort after recovered Undo failed", err)
	}
	list, err = reopened.List(ctx, "alice")
	if err != nil || len(list) != 1 || list[0].Status != "filed" || calls.Load() != 2 {
		t.Fatal("explicit sort did not supersede recovered hold", list, err, calls.Load())
	}
}
