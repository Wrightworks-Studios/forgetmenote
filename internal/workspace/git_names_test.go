package workspace

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostileNames are folder names Git's fsck refuses as spellings of .git
// (hasDotgit) on every platform: NTFS's short name git~1 and its spellings,
// a stream name after a colon, and .git behind a code point HFS+ ignores.
var hostileNames = []string{"git~1", "GIT~1 ", "git~1.", "git~1. .", "git~1:x", "\u200c.git", ".gI\u200cT", ".git ", ".GIT:$DATA"}

// fsckBundle copies the bundle at path into a new repository without
// checking its objects, as a backup reader would, and runs git fsck --strict
// on it.
func fsckBundle(t testing.TB, path string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.Command("git", "-c", "transfer.fsckObjects=false", "clone", "--quiet", "--bare", path, repo).CombinedOutput(); err != nil {
		t.Fatalf("git clone of the backup: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", repo, "fsck", "--strict", "--no-dangling", "--no-progress").CombinedOutput(); err != nil {
		t.Fatalf("git fsck --strict of the backup: %v: %s", err, out)
	}
}

// No write path stores a name or a message Git refuses: Save (and so an
// upload), Move, Duplicate and an edit group each refuse a folder Git's fsck
// takes for .git, and a group refuses a summary with a NUL, so the
// notebook's backup passes git fsck --strict and restores.
func TestWritesRefuseNamesAndMessagesGitRefuses(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	note := saveNote(t, s, "note.md", "# Note\n")
	for _, name := range hostileNames {
		if _, err := s.Save(ctx, name+"/x.md", []byte("# X\n"), "", "writer"); !errors.Is(err, ErrForbidden) {
			t.Errorf("Save into %q: %v", name, err)
		}
		if _, err := s.Save(ctx, "assets/"+name+"/photo.png", []byte("png"), "", "writer"); !errors.Is(err, ErrForbidden) {
			t.Errorf("upload into %q: %v", name, err)
		}
		if _, err := s.Move(ctx, "note.md", name+"/note.md", headOf(t, s), "writer"); !errors.Is(err, ErrForbidden) {
			t.Errorf("Move into %q: %v", name, err)
		}
		if _, err := s.Duplicate(ctx, "note.md", name+"/copy.md", headOf(t, s), "writer"); !errors.Is(err, ErrForbidden) {
			t.Errorf("Duplicate into %q: %v", name, err)
		}
		group := EditGroup{ID: "group " + name, Summary: "Add a note", Edits: []NoteEdit{{Path: name + "/n.md", Content: "# N\n"}}}
		if _, err := s.ApplyGroup(ctx, group); !errors.Is(err, ErrForbidden) {
			t.Errorf("group writing into %q: %v", name, err)
		}
	}
	group := EditGroup{ID: "nul summary", Summary: "Fix\x00typo", Edits: []NoteEdit{{Path: "note.md", Content: "# Fixed\n", Revision: note.Revision, Hash: ContentHash(note.Content)}}}
	if _, err := s.ApplyGroup(ctx, group); !errors.Is(err, ErrInvalid) {
		t.Errorf("group with a NUL in its summary: %v", err)
	}
	if head := headOf(t, s); head != note.Revision {
		t.Fatalf("a refused write moved the head from %s to %s", note.Revision, head)
	}
	// A name Git keeps, close to a refused one, is still a folder.
	if _, err := s.Save(ctx, "git~2/x.md", []byte("# Kept\n"), "", "writer"); err != nil {
		t.Fatal(err)
	}
	backup := backupFile(t, s)
	fsckBundle(t, backup)
	if err := importFile(t, newTestNotes(t), "restored", backup); err != nil {
		t.Fatal("the notebook's own backup does not restore:", err)
	}
}

// A revision's summary reaches its commit message through commitMessage,
// which refuses a NUL whatever sent it.
func TestCommitMessageRefusesNUL(t *testing.T) {
	if _, _, err := commitMessage(context.Background(), "Import notebook from x\x00 at y"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

// A handoff installs no snapshot holding a name Git refuses.
func TestHandoffSnapshotRefusesNamesGitRefuses(t *testing.T) {
	for _, name := range hostileNames {
		snapshot := NotebookSnapshot{Revision: strings.Repeat("a", 40), Files: map[string][]byte{name + "/x.md": []byte("# X\n")}}
		if err := ValidateNotebookSnapshot(context.Background(), snapshot); !errors.Is(err, ErrForbidden) {
			t.Errorf("snapshot with %q: %v", name, err)
		}
	}
}
