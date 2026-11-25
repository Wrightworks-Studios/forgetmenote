package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBackupRestoresIndependentRepository(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	_, err := s.Save(ctx, "notes/a.md", []byte("# Keep"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "assets/image.bin", []byte{0, 255, 10, 13, 0}, "", "test"); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "backup.bundle")
	if err = s.Backup(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if out, err := exec.Command("git", "clone", bundle, target).CombinedOutput(); err != nil {
		t.Fatalf("clone: %s %v", out, err)
	}
	raw, err := os.ReadFile(filepath.Join(target, "notes/a.md"))
	if err != nil || string(raw) != "# Keep" {
		t.Fatalf("restore: %s %v", raw, err)
	}

	media, err := os.ReadFile(filepath.Join(target, "assets/image.bin"))
	if err != nil || string(media) != string([]byte{0, 255, 10, 13, 0}) {
		t.Fatal("binary attachment lost in bundle")
	}
}
