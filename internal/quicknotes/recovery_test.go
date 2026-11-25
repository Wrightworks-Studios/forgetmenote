package quicknotes

import (
	"context"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

func copyStoppedTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("synthetic backup contained a special file")
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestCompleteRestoreRecoversCommittedTargetWithoutDuplicate(t *testing.T) {
	s, c, root := fixture(t)
	s.cancel()
	<-s.done
	routeTarget(t, c)
	ctx, account, err := s.actor(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	captured, err := s.Capture(ctx, "alice", "restore-capture", "# Recover me\nunchanged bytes\n")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.readRecord(account.ID, captured.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := workspace.CaptureMove{ID: "interrupted-filing", From: r.Path, Version: r.Version, Hash: r.Hash, To: r.InboxPath, SourceProject: r.Project, TargetProject: "target"}
	r.Pending = &transfer{Intent: in}
	r.Status = "filing"
	if err = s.writeRecord(&r); err != nil {
		t.Fatal(err)
	}
	source, _ := c.Access("alice", r.Project, "owner")
	target, _ := c.Access("alice", "target", "owner")
	frozen, err := source.CapturePrepare(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := target.CaptureReceive(ctx, in, frozen.Content)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	c.Close()
	restored := t.TempDir()
	copyStoppedTree(t, root, restored)
	// The database is restored with the volume: the stopped original's.
	c2, err := catalog.Open(filepath.Join(restored, "catalog"), nil, pgtest.Dir(t, filepath.Join(root, "catalog")))
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	s2, err := Open(pgtest.Dir(t, filepath.Join(root, "catalog")), c2)
	if err != nil {
		t.Fatal(err)
	}
	if err = s2.Start(); err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	recovered, err := s2.Recover(context.Background(), "alice", captured.ID)
	if err != nil || recovered.Status != "filed" || recovered.Version != committed.Version {
		t.Fatal("committed transfer not recovered", recovered, err)
	}
	target2, _ := c2.Access("alice", "target", "owner")
	notes, err := target2.List(context.Background())
	if err != nil || len(notes) != 1 {
		t.Fatal("restore duplicated target", notes, err)
	}
	source2, _ := c2.Access("alice", captured.Project, "owner")
	if _, err = source2.Read(context.Background(), captured.Path, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source was not completed", err)
	}
	if _, err = source2.Read(context.Background(), captured.Path, captured.Version); err != nil {
		t.Fatal("private original history lost", err)
	}
	undone, err := s2.Undo(context.Background(), "alice", captured.ID)
	if err != nil || undone.Status != "inbox" {
		t.Fatal("restored receipt cannot Undo", undone, err)
	}
}
func TestRecoverUnknownCaptureAcknowledgementAndStaleFiling(t *testing.T) {
	s, c, _ := fixture(t)
	s.cancel()
	<-s.done
	routeTarget(t, c)
	ctx, account, err := s.actor(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Capture(ctx, "alice", "lost-save-ack", "original")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s.readRecord(account.ID, saved.ID)
	r.Status = "saving"
	r.Version = ""
	if err = s.writeRecord(&r); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Recover(ctx, "alice", saved.ID)
	if err != nil || recovered.Version != saved.Version {
		t.Fatal("save acknowledgement unrecoverable", recovered, err)
	}
	r, _ = s.readRecord(account.ID, saved.ID)
	r.Pending = &transfer{Intent: workspace.CaptureMove{ID: "stale-before-reservation", From: r.Path, Version: r.Version, Hash: r.Hash, To: r.InboxPath, SourceProject: r.Project, TargetProject: "target"}}
	r.Status = "filing"
	if err = s.writeRecord(&r); err != nil {
		t.Fatal(err)
	}
	source, _ := c.Access("alice", saved.Project, "owner")
	if _, err = source.Save(ctx, saved.Path, []byte("newer"), saved.Version, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Recover(ctx, "alice", saved.ID); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("stale filing did not conflict", err)
	}
	current, err := s.readRecord(account.ID, saved.ID)
	if err != nil || current.Status != "inbox" || current.Pending != nil {
		t.Fatal("stale filing left reservation", current.Status, err)
	}
	target, _ := c.Access("alice", "target", "read")
	if _, err = target.Read(ctx, saved.Path, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale capture published", err)
	}
}
func TestRevocationBlocksRecoveryAndRecentDisclosure(t *testing.T) {
	s, c, _ := fixture(t)
	routeTarget(t, c)
	ctx := context.Background()
	saved, err := s.Capture(ctx, "alice", "revoke-capture", "private")
	if err != nil {
		t.Fatal(err)
	}
	filed, err := s.File(ctx, "alice", saved.ID, Destination{Project: "target"}, saved.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Member(catalog.Bootstrap, "target", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Undo(ctx, "alice", filed.ID); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("Undo bypassed revocation", err)
	}
	recent, err := s.List(ctx, "alice")
	if err != nil || len(recent) != 0 {
		t.Fatal("recent capture leaked revoked destination", recent, err)
	}
}
