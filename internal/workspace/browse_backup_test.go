package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"forgetmenote/internal/gitexport"
)

func TestBoundedExportStopsAtLimit(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "a.md", []byte("export test"), "", ""); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "export.bundle")
	if err := s.BackupLimited(ctx, target, 32); !errors.Is(err, gitexport.ErrBundleLimit) {
		t.Fatal("oversize export:", err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Size() > 32 {
		t.Fatal(info, err)
	}
}
