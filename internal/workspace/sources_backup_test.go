package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/attribution"
)

// A restore keeps a source original the note budget would refuse, and still
// refuses an ordinary file of that size.
func TestSourceOriginalBeyondNoteBudgetSurvivesRestore(t *testing.T) {
	t.Setenv("FMN_LIMIT_FILE_BYTES", "1024")
	t.Setenv("FMN_LIMIT_SOURCE_BYTES", "8192")
	s := backupTestStore(t)
	ctx := t.Context()
	original := bytes.Repeat([]byte("x"), 4096)
	path := "assets/sources/" + strings.Repeat("d", 32) + "/" + ContentHash(original) + ".pdf"
	if !SourceOriginalPath(path) || MaxReadBytes(path) < len(original) {
		t.Fatal("not a source original's path:", path)
	}
	// The original as an archived source stores it, past the note budget.
	revision := commitFixture(t, s, fixture(path, string(original)))
	read, err := s.Read(ctx, path, revision)
	if err != nil || !bytes.Equal(read.Content, original) {
		t.Fatal("source read used ordinary note limit", err)
	}
	restored := restore(t, s)
	raw, err := restored.Read(ctx, path, "")
	if err != nil || !bytes.Equal(raw.Content, original) {
		t.Fatal("restore changed original", err)
	}
	commitFixture(t, s, fixture("assets/large.bin", string(original)))
	notes := newTestNotes(t)
	if err = importFile(t, notes, "restored", backupFile(t, s)); !errors.Is(err, ErrBundle) || !strings.Contains(err.Error(), "budget") {
		t.Fatal("restore admitted a file over the note budget:", err)
	}
	absent(t, notes, "restored")
}

// A standalone Git bundle contains every original version and source record.
func TestBackupKeepsEverySourceOriginal(t *testing.T) {
	s := backupTestStore(t)
	ctx := attribution.With(context.Background(), attribution.Identity{ID: "alice", Name: "Alice", Email: "alice@local.invalid", Source: "browser"})
	capture := SourceCapture{Title: "Study", Original: []byte("<p>Original evidence</p>"), Text: "## Page 1\n\nOriginal evidence\n", Record: SourceRecord{MIME: "text/html", Method: "uploaded"}}
	first, err := s.ImportSource(ctx, capture, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	record := ParseSource(first.Content)
	capture.Original = []byte("<p>Updated evidence</p>")
	capture.Text = "Updated evidence\n"
	if _, err = s.ImportSource(ctx, capture, first.Path, first.Revision, "New edition", nil); err != nil {
		t.Fatal(err)
	}
	original := Document{Content: []byte("<p>Original evidence</p>")}
	bundle := backupFile(t, s)
	clone := filepath.Join(t.TempDir(), "restored")
	if out, e := exec.Command("git", "clone", bundle, clone).CombinedOutput(); e != nil {
		t.Fatal(e, string(out))
	}
	raw, e := os.ReadFile(filepath.Join(clone, record.Original))
	if e != nil || string(raw) != string(original.Content) {
		t.Fatal("original absent from restore", e)
	}
}
