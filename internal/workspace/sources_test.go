package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// sourceLocks commits the lock policy as SetLock writes it, with these
// rules.
func sourceLocks(t *testing.T, s *Store, paths ...string) {
	t.Helper()
	raw, err := json.Marshal(lockPolicy{Version: 1, Paths: append([]string{}, paths...)})
	if err != nil {
		t.Fatal(err)
	}
	commitFixture(t, s, notestore.FileChange{Path: locksPath, Content: raw})
}

func TestSourceOriginalBeyondNoteBudgetReadsButBoundsNotes(t *testing.T) {
	t.Setenv("FMN_LIMIT_FILE_BYTES", "1024")
	t.Setenv("FMN_LIMIT_SOURCE_BYTES", "8192")
	s := newTestStore(t)
	ctx := t.Context()
	capture := SourceCapture{Title: "Larger original", Original: bytes.Repeat([]byte("x"), 4096), Text: "Amber evidence.\n", Record: SourceRecord{MIME: "application/pdf", Method: "uploaded"}}
	doc, err := s.ImportSource(ctx, capture, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	record := ParseSource(doc.Content)
	if record == nil {
		t.Fatal("missing source record")
	}
	read, err := s.Read(ctx, record.Original, doc.Revision)
	if err != nil || !bytes.Equal(read.Content, capture.Original) {
		t.Fatal("source read used ordinary note limit", err)
	}
	if _, err = s.Save(ctx, "ordinary.md", capture.Original, "", ""); err == nil {
		t.Fatal("source read budget widened ordinary note writes")
	}
}

func TestSourceAtomicRefreshAndHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := attribution.With(context.Background(), attribution.Identity{ID: "alice", Name: "Alice", Email: "alice@local.invalid", Source: "browser"})
	capture := SourceCapture{Title: "Study", Original: []byte("<p>Original evidence</p>"), Text: "## Page 1\n\nOriginal evidence\n", Record: SourceRecord{MIME: "text/html", Method: "uploaded", ImportedBy: "impersonator"}}
	first, err := s.ImportSource(ctx, capture, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	record := ParseSource(first.Content)
	if record == nil || record.ImportedBy != "alice" {
		t.Fatal("untrusted attribution", record)
	}
	verification, err := s.VerifySource(ctx, first.Path, first.Revision)
	if err != nil || !strings.Contains(verification.TextState, "matches") || !strings.Contains(verification.OriginalState, "matches") {
		t.Fatal(verification, err)
	}
	original, _ := s.Read(ctx, record.Original, first.Revision)
	if string(original.Content) != string(capture.Original) {
		t.Fatal("original changed")
	}
	revision, _ := gitobj.ParseID(first.Revision)
	info, err := s.notes.Commit(ctx, revision)
	if err != nil || len(info.Changes) != 2 {
		t.Fatal("the import is not one revision of the note and its original", info.Changes, err)
	}
	if commit := commitRecord(info); commit.AuthorName != "Alice" || commit.UserID != "alice" || commit.Message != "Archive source: Study" {
		t.Fatal("bad import history", commit)
	}
	capture.Original = []byte("<p>Updated evidence</p>")
	capture.Text = "Updated evidence\n"
	second, err := s.ImportSource(ctx, capture, first.Path, first.Revision, "New edition", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ImportSource(ctx, capture, first.Path, first.Revision, "", nil); !errors.Is(err, ErrConflict) {
		t.Fatal("stale refresh", err)
	}
	old, _ := s.Read(ctx, first.Path, first.Revision)
	if string(old.Content) != string(first.Content) {
		t.Fatal("history overwritten")
	}
	now := ParseSource(second.Content)
	if now.ID != record.ID || now.Original == record.Original {
		t.Fatal("capture identity/version lost")
	}
	oldOriginal, _ := s.Read(ctx, record.Original, "")
	if string(oldOriginal.Content) != string(original.Content) {
		t.Fatal("prior original was replaced")
	}
	sourceLocks(t, s, first.Path)
	if _, err = s.ImportSource(ctx, capture, first.Path, second.Revision, "", nil); !errors.Is(err, ErrForbidden) {
		t.Fatal("locked source refreshed", err)
	}
	sourceLocks(t, s)
	before, _ := s.Status(ctx)
	listed, _ := s.List(ctx)
	restore := failCommits(t, s)
	if _, err = s.ImportSource(ctx, capture, "", "", "", nil); err == nil {
		t.Fatal("commit unexpectedly succeeded")
	}
	restore()
	state, err := s.Status(ctx)
	after, _ := s.List(ctx)
	if err != nil || state.Revision != before.Revision || len(after) != len(listed) {
		t.Fatal("failed import left files", state, err)
	}
}
func TestSourceRejectsSymlinkAndDetectsAlteredText(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	capture := SourceCapture{Title: "Evidence", Original: []byte("html"), Text: "Text\n", Record: SourceRecord{MIME: "text/html"}}
	doc, err := s.ImportSource(ctx, capture, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(doc.Content), "Text\n", "Edited text\n", 1)
	newer, err := s.Save(ctx, doc.Path, []byte(edited), doc.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.VerifySource(ctx, newer.Path, "")
	if err != nil || v.TextState != "Extracted text changed" {
		t.Fatal(v, err)
	}
	s2 := newTestStore(t)
	commitFixture(t, s2, notestore.FileChange{Path: "sources", Content: []byte(t.TempDir()), Mode: gitobj.ModeSymlink})
	if _, err = s2.ImportSource(ctx, capture, "", "", "", nil); !errors.Is(err, ErrForbidden) {
		t.Fatal("followed source symlink", err)
	}
}

func TestSourceOriginalMoveRepairsProvenance(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.ImportSource(ctx, SourceCapture{Title: "Source", Original: []byte("html"), Text: "Evidence\n", Record: SourceRecord{MIME: "text/html"}}, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	record := ParseSource(doc.Content)
	if _, err = s.Move(ctx, "assets/sources/"+record.ID, "assets/library", doc.Revision, ""); err != nil {
		t.Fatal(err)
	}
	moved, err := s.Read(ctx, doc.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	record = ParseSource(moved.Content)
	if record == nil || record.Original != "assets/library/"+record.SHA256+".html" {
		t.Fatal("provenance link lost", string(moved.Content))
	}
	v, err := s.VerifySource(ctx, doc.Path, "")
	if err != nil || !strings.Contains(v.OriginalState, "matches") {
		t.Fatal(v, err)
	}
}

func TestSourceFolderLocksConflictsAndRename(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	capture := SourceCapture{Title: "Evidence", Original: []byte("Original"), Text: "Original\n", Record: SourceRecord{MIME: "text/plain", ID: strings.Repeat("a", 32)}}
	folder := "references"
	ordinary, err := s.Save(ctx, folder+"/"+capture.Record.ID+".md", []byte("Human note"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ImportSource(ctx, capture, "", "", "", &folder); !errors.Is(err, ErrConflict) {
		t.Fatal("overwrote existing note", err)
	}
	check, _ := s.Read(ctx, ordinary.Path, "")
	if string(check.Content) != "Human note" {
		t.Fatal("existing note changed")
	}
	capture.Record.ID = strings.Repeat("b", 32)
	sourceLocks(t, s, folder)
	if _, err = s.ImportSource(ctx, capture, "", "", "", &folder); !errors.Is(err, ErrForbidden) {
		t.Fatal("locked destination accepted", err)
	}
	sourceLocks(t, s)
	source, err := s.ImportSource(ctx, capture, "", "", "", &folder)
	if err != nil {
		t.Fatal(err)
	}
	// The folder is renamed, as a Move commits it.
	commitFixture(t, s, notestore.FileChange{Path: "library/" + strings.Repeat("a", 32) + ".md", From: ordinary.Path}, notestore.FileChange{Path: "library/" + strings.Repeat("b", 32) + ".md", From: source.Path})
	folder = "library"
	capture.Record.ID = strings.Repeat("c", 32)
	if _, err = s.ImportSource(ctx, capture, "", "", "", &folder); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Path, "sources/") {
			t.Fatal("default folder recreated", entry.Path)
		}
	}
	// A regular file cannot serve as a destination directory.
	folder = "library/" + strings.Repeat("a", 32) + ".md"
	capture.Record.ID = strings.Repeat("d", 32)
	before, _ := s.Status(ctx)
	if _, err = s.ImportSource(ctx, capture, "", "", "", &folder); err == nil {
		t.Fatal("file accepted as folder")
	}
	after, _ := s.Status(ctx)
	if after.Dirty || after.Revision != before.Revision {
		t.Fatal("failed import mutated workspace")
	}
}
