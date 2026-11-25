package workspace

import (
	"context"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// A note's fmn-id survives a rename made outside the workspace, as an
// import's, and no other note may take it.
func TestNoteIdentitySurvivesRenameAndRejectsCollision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, err := NewNoteID()
	if err != nil {
		t.Fatal(err)
	}
	content, err := WithNoteID([]byte("# One\n"), id)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Save(ctx, "one.md", content, "", "author")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d.Content), "fmn-id:") {
		t.Fatalf("new note has no stable identity: %s", d.Content)
	}
	moved := commitFixture(t, s, notestore.FileChange{Path: "renamed.md", From: "one.md"})
	got, err := s.Read(ctx, "renamed.md", "")
	if err != nil || string(got.Content) != string(d.Content) || got.Revision != moved {
		t.Fatalf("rename lost identity: %v", err)
	}
	if f, err := s.notes.Stat(ctx, gitobj.ID{}, "renamed.md"); err != nil || f.EntryID != id {
		t.Fatal("the renamed note's entry:", f, err)
	}
	if _, err = s.Save(ctx, "collision.md", d.Content, "", "author"); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	if _, err = s.Save(ctx, "renamed.md", []byte(strings.Replace(string(d.Content), id, strings.Repeat("d", 32), 1)), moved, "author"); err == nil {
		t.Fatal("changed identity accepted")
	}
	copy, err := WithNoteID([]byte("# Copy\n"), strings.Repeat("e", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, "copy.md", copy, "", "author"); err != nil {
		t.Fatal("a new identity:", err)
	}
}

func TestReservedIdentitySpellingsCannotBypassValidation(t *testing.T) {
	s := newTestStore(t)
	for _, key := range []string{"\"fmn-id\"", "'fmn-id'", " fmn-id"} {
		if _, err := s.Save(context.Background(), "invalid.md", []byte("---\n"+key+": invalid\n---\nText"), "", "author"); err == nil {
			t.Fatalf("accepted reserved key %q", key)
		}
	}
	if _, err := s.Save(context.Background(), "legacy.md", []byte("---\nunclosed legacy metadata\nA prose mention of fmn-id: remains editable"), "", "author"); err != nil {
		t.Fatal("reserved-field validation rejected legacy prose", err)
	}
}

// Identities a fixture committed past the workspace, as an untrusted import
// may, still bind the notes that hold them.
func TestCommittedIdentitiesBindLaterSaves(t *testing.T) {
	s := newTestStore(t)
	content, _ := WithNoteID([]byte("# Imported\n"), strings.Repeat("a", 32))
	head := commitFixture(t, s, fixture("one.md", string(content)))
	if _, err := s.Save(context.Background(), "two.md", content, "", "author"); err == nil {
		t.Fatal("a second note took a committed identity")
	}
	if _, err := s.Save(context.Background(), "one.md", []byte(string(content)+"More.\n"), head, "author"); err != nil {
		t.Fatal("the note holding the identity:", err)
	}
}
