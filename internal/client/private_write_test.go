package client

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Saved sessions and edit drafts survive a crash only once their directory is synced.
func TestPrivateWriteSyncsDirectoryAfterRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	original := syncPrivateDirectory
	t.Cleanup(func() { syncPrivateDirectory = original })
	synced := []string{}
	syncPrivateDirectory = func(dir string) error {
		if raw, err := os.ReadFile(path); err != nil || string(raw) != "{}" {
			t.Error("directory synced before the rename:", err)
		}
		synced = append(synced, dir)
		return nil
	}
	if err := privateWrite(path, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != filepath.Dir(path) {
		t.Fatalf("directory syncs = %q", synced)
	}
	failure := errors.New("injected directory sync failure")
	syncPrivateDirectory = func(string) error { return failure }
	if err := privateWrite(path, []byte("[]")); !errors.Is(err, failure) {
		t.Fatal("directory sync failure was not reported:", err)
	}
}
