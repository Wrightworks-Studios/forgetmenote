package quicknotes

import (
	"context"
	"errors"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) (*Service, *catalog.Catalog, string) {
	t.Helper()
	root := t.TempDir()
	c, err := catalog.Open(filepath.Join(root, "catalog"), nil, pgtest.Dir(t, filepath.Join(root, "catalog")))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(pgtest.Dir(t, filepath.Join(root, "catalog")), c)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); c.Close() })
	return s, c, root
}
func TestCaptureSaveFirstPrivateAndIdempotent(t *testing.T) {
	s, c, _ := fixture(t)
	ctx := context.Background()
	captured, err := s.Capture(ctx, "alice", "request-one", "# Private capture\n")
	if err != nil {
		t.Fatalf("capture without destination failed: %v", err)
	}
	if captured.Status != "inbox" || captured.Version == "" {
		t.Fatalf("capture not saved: %+v", captured)
	}
	if c.CanCreateProjects("alice") {
		t.Fatal("capture granted ordinary creation authority")
	}
	if _, err = c.Access("bob", captured.Project, "read"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("capture disclosed", err)
	}
	again, err := s.Capture(ctx, "alice", "request-one", "# Private capture\n")
	if err != nil || again.ID != captured.ID || again.Version != captured.Version {
		t.Fatal("capture retry duplicated", again, err)
	}
	if _, err = s.Capture(ctx, "alice", "request-one", "different"); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("operation reused for different content", err)
	}
	settings, err := s.Settings("alice")
	if err != nil || settings.Mode != "suggest" || settings.CloudConsent {
		t.Fatal("default settings", settings, err)
	}
	list, err := s.List(ctx, "alice")
	if err != nil || len(list) != 1 || list[0].Title != "Private capture" {
		t.Fatal("recent capture missing", list, err)
	}
}
func TestCaptureFileUndoAndNewerEditGuard(t *testing.T) {
	s, c, _ := fixture(t)
	ctx := context.Background()
	captured, err := s.Capture(ctx, "alice", "capture", "capture")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Create(catalog.Bootstrap, "target", "Shared target"); err != nil {
		t.Fatal(err)
	}
	if err = c.Member(catalog.Bootstrap, "target", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	target, _ := c.Access("alice", "target", "owner")
	if _, err = target.Save(ctx, "existing.md", []byte("existing"), "", "alice"); err != nil {
		t.Fatal(err)
	}
	filed, err := s.File(ctx, "alice", captured.ID, Destination{Project: "target"}, captured.Version)
	if err != nil || filed.Status != "filed" || !filed.CanUndo {
		t.Fatal("filing", filed, err)
	}
	undone, err := s.Undo(ctx, "alice", captured.ID)
	if err != nil || undone.Status != "inbox" || undone.Project != captured.Project {
		t.Fatal("undo", undone, err)
	}
	filed, err = s.File(ctx, "alice", captured.ID, Destination{Project: "target"}, undone.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = target.Save(ctx, filed.Path, []byte("newer work"), filed.Version, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Undo(ctx, "alice", captured.ID); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("Undo accepted newer target revision", err)
	}
	now, err := target.Read(ctx, filed.Path, "")
	if err != nil || string(now.Content) != "newer work" {
		t.Fatal("newer work overwritten", now, err)
	}
}
func TestFiledCaptureRetryReturnsCommittedReceipt(t *testing.T) {
	s, c, _ := fixture(t)
	ctx := context.Background()
	saved, err := s.Capture(ctx, "alice", "file-retry", "capture")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Create(catalog.Bootstrap, "target", "Target"); err != nil {
		t.Fatal(err)
	}
	if err = c.Member(catalog.Bootstrap, "target", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	filed, err := s.File(ctx, "alice", saved.ID, Destination{Project: "target"}, saved.Version)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.File(ctx, "alice", saved.ID, Destination{Project: "target"}, saved.Version)
	if err != nil || replay.Version != filed.Version {
		t.Fatalf("lost acknowledgement not replayable: %+v %v", replay, err)
	}
}

// Gateways share one database: a capture made through one is listed by the
// other, and a write computed from a stale read conflicts instead of
// overwriting the other gateway's change.
func TestGatewaysShareCapturesAndRefuseStaleWrites(t *testing.T) {
	a, c, root := fixture(t)
	b, err := Open(pgtest.Dir(t, filepath.Join(root, "catalog")), c)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	captured, err := a.Capture(ctx, "alice", "shared-capture", "# Shared\n")
	if err != nil {
		t.Fatal(err)
	}
	if list, err := b.List(ctx, "alice"); err != nil || len(list) != 1 || list[0].ID != captured.ID {
		t.Fatal("another gateway does not list the capture", list, err)
	}
	_, account, err := a.actor(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.readRecord(account.ID, captured.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.readRecord(account.ID, captured.ID)
	if err != nil {
		t.Fatal(err)
	}
	first.Error = "first"
	if err = a.writeRecord(&first); err != nil {
		t.Fatal(err)
	}
	second.Error = "second"
	if err = b.writeRecord(&second); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("a stale capture write overwrote another gateway's change:", err)
	}
	if stored, err := b.readRecord(account.ID, captured.ID); err != nil || stored.Error != "first" {
		t.Fatal("the stored capture changed", stored.Error, err)
	}
	settings, err := a.Settings("alice")
	if err != nil {
		t.Fatal(err)
	}
	next := settings
	next.Mode, next.Generation = "manual", settings.Generation+1
	if err = a.writeSettings(account.ID, settings.Generation, next); err != nil {
		t.Fatal(err)
	}
	next.Mode = "automatic"
	if err = b.writeSettings(account.ID, settings.Generation, next); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("stale settings overwrote another gateway's change:", err)
	}
	if stored, err := b.Settings("alice"); err != nil || stored.Mode != "manual" {
		t.Fatal("the stored settings changed", stored, err)
	}
}
