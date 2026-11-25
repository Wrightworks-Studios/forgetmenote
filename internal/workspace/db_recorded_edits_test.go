package workspace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// mutateContext signs the revisions a scenario makes, so its attribution
// trailers are the ones recorded.
func mutateContext() context.Context {
	return attribution.With(context.Background(), attribution.Identity{ID: "alice", Name: "Alice", Email: "alice@example.test", Source: "mcp", Client: "agent-a"})
}

// outsideLinks commits a symbolic link to outside the notebook at each of
// paths.
func outsideLinks(t *testing.T, s *Store, paths ...string) {
	t.Helper()
	var changes []notestore.FileChange
	for _, path := range paths {
		changes = append(changes, notestore.FileChange{Path: path, Content: []byte("outside-target"), Mode: gitobj.ModeSymlink})
	}
	commitFixture(t, s, changes...)
}

// moveFixture is a notebook with what a move or a copy carries along or
// rewrites: relative links in and out, reference definitions, code that
// must stay as it is, a discussion with a finding, a lock, covers, a
// source's original, a pinned historical link, a note with an fmn-id and
// symbolic links.
func moveFixture(r *recording) {
	t, s := r.t, r.s
	ctx := mutateContext()
	id, _ := NewNoteID()
	kept, _ := WithNoteID([]byte("# Kept\n\nAn identity.\n"), id)
	for _, f := range []struct{ path, content string }{
		{"old/a.md", "# An original note\n\nSeveral lines of original content.\n\n[Other](../other.md#section)\n![Image](media/pic.png)\n"},
		{"old/locked.md", "# Keep this text unchanged\n"},
		{"old/kept.md", string(kept)},
		{"old/media/pic.png", "image bytes"},
		{"other.md", "# References\n\n[Read](old/a.md#heading)\n\n[ref]: old/a.md \"A title\"\n\n`[code](old/a.md)`\n\n```md\n[code](old/a.md)\n```\n"},
		{"assets/set/image.png", "image"},
		{"assets/set/note.md", "---\ncover: assets/set/image.png\ncustom: keep\n---\n# Note\nExact body."},
		{"outside.md", "---\ncover: assets/set/image.png\n---\nOutside."},
		{"sources/a.md", "# Source\n"},
		{"locked/child.md", "Protected text\n"},
	} {
		saveNote(t, s, f.path, f.content)
	}
	pinned := headOf(t, s)
	saveNote(t, s, "summary.md", "[Evidence](sources/a.md?fmn-rev="+pinned+"#source)\n")
	if _, err := s.AddComment(ctx, "old/a.md", "Feedback", headOf(t, s), "reader"); err != nil {
		t.Fatal(err)
	}
	discussion, err := s.Comments(ctx, "old/a.md")
	if err != nil {
		t.Fatal(err)
	}
	finding := Comment{ID: "f1", Author: "agent", Text: "It needs a source.", Created: "2026-11-22T00:00:00Z", Finding: &Finding{Key: "k1", Path: "old/a.md", Title: "Check the claim"}}
	raw, _ := json.MarshalIndent(append(discussion.Comments, finding), "", "  ")
	saveNote(t, s, commentPath("old/a.md"), string(raw))
	for _, path := range []string{"old/locked.md", "locked"} {
		if _, err = s.SetLock(ctx, path, true, headOf(t, s), "owner"); err != nil {
			t.Fatal(err)
		}
	}
	saveNote(t, s, "box/note.md", "# Boxed\n")
	outsideLinks(t, s, "linked", "box/inner")
}

// moveSteps moves and copies folders, notes and assets of moveFixture.
func moveSteps(r *recording) {
	ctx := mutateContext()
	for _, step := range []struct {
		name     string
		from, to string
		copy     bool
	}{
		{"a folder", "old", "new/nested", false},
		{"a copied folder", "new/nested", "copies/nested", true},
		{"a copied asset folder", "assets/set", "assets/copy", true},
		{"an asset folder", "assets/set", "assets/moved", false},
		{"a folder pinned links name", "sources", "archive", false},
		{"a note with a pinned link", "summary.md", "notes/summary.md", false},
		{"a note out of a locked folder", "locked/child.md", "child.md", false},
		{"a copied note with an identity", "new/nested/kept.md", "kept-copy.md", true},
	} {
		r.step(step.name, func(s *Store) (any, error) {
			if step.copy {
				return s.Duplicate(ctx, step.from, step.to, headOf(r.t, s), "owner")
			}
			return s.Move(ctx, step.from, step.to, headOf(r.t, s), "owner")
		})
	}
}

// Moves and copies make the revisions the Git backend made: the same
// rewritten links, covers, discussions with their findings, lock policies,
// fresh fmn-ids for copies, commit subjects, trailers and identities, and
// the same results; a moved note and its discussion keep their entries, so
// their histories follow them.
func TestDatabaseMovesMatchGit(t *testing.T) {
	r := newRecording(t, newTestStore(t), "moves")
	moveFixture(r)
	r.begin()
	s, ctx := r.s, mutateContext()
	note, _ := s.notes.Stat(ctx, gitobj.ID{}, "old/a.md")
	kept, _ := s.notes.Stat(ctx, gitobj.ID{}, "old/kept.md")
	discussion, _ := s.notes.Stat(ctx, gitobj.ID{}, commentPath("old/a.md"))
	moveSteps(r)
	checkRecorded(t, "moves", r)

	for _, c := range []struct {
		path string
		was  notestore.File
	}{{"new/nested/a.md", note}, {"new/nested/kept.md", kept}, {commentPath("new/nested/a.md"), discussion}} {
		f, err := s.notes.Stat(ctx, gitobj.ID{}, c.path)
		if err != nil || f.EntryID != c.was.EntryID {
			t.Errorf("%s is entry %q, want the moved file's %q: %v", c.path, f.EntryID, c.was.EntryID, err)
		}
	}
	for _, path := range []string{"copies/nested/a.md", "kept-copy.md"} {
		f, err := s.notes.Stat(ctx, gitobj.ID{}, path)
		if err != nil || f.EntryID == note.EntryID || f.EntryID == kept.EntryID {
			t.Errorf("the copy %s shares its source's entry: %v", path, err)
		}
	}
	history := entryHistory(t, s, "new/nested/a.md")
	if len(history) != 2 || history[0].Path != "new/nested/a.md" || history[len(history)-1].Path != "old/a.md" {
		t.Errorf("the moved note's history: %+v", history)
	}
	for _, version := range history {
		if _, err := s.Read(ctx, version.Path, version.Revision); err != nil {
			t.Errorf("history version %s at %s: %v", version.Path, version.Revision, err)
		}
	}
	if copied := entryHistory(t, s, "copies/nested/a.md"); len(copied) != 1 {
		t.Errorf("the copy's history: %+v", copied)
	}
	if rules := lockRulesOf(t, s, "child.md"); len(rules) != 1 || rules[0].Path != "child.md" {
		t.Errorf("a note leaving a locked folder lost its lock: %+v", rules)
	}
}

// moveRefusalFixture is moveFixture with a lone note that a locked note
// links to, a file that is no note and a discussion without its note.
func moveRefusalFixture(r *recording) {
	moveFixture(r)
	t, s := r.t, r.s
	saveNote(t, s, "ref.md", "[A](lone.md)\n")
	saveNote(t, s, "lone.md", "# Lone\n")
	saveNote(t, s, "plain.txt", "text\n")
	saveNote(t, s, commentPath("orphan.md"), "[]")
	if _, err := s.SetLock(context.Background(), "ref.md", true, headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
}

func moveRefusalSteps(r *recording) {
	ctx := mutateContext()
	for _, c := range []struct {
		name, from, to string
		copy, stale    bool
	}{
		{name: "an escape", from: "old", to: "../escape"},
		{name: "a folder into itself", from: "old", to: "old/child"},
		{name: "onto a note", from: "old", to: "other.md"},
		{name: "a hidden path", from: "old", to: ".git/config"},
		{name: "a stale revision", from: "old", to: "fresh", stale: true},
		{name: "a missing source", from: "missing", to: "fresh"},
		{name: "a note losing .md", from: "lone.md", to: "lone.txt"},
		{name: "a file that is no note", from: "plain.txt", to: "plain2.txt"},
		{name: "into a note", from: "lone.md", to: "lone.md/child.md", copy: true},
		{name: "onto itself", from: "lone.md", to: "lone.md"},
		{name: "a locked backlink", from: "lone.md", to: "moved.md"},
		{name: "into the Trash", from: "lone.md", to: "Trash/x.md"},
		{name: "through a symbolic link", from: "lone.md", to: "linked/lone.md"},
		{name: "through a nested symbolic link", from: "lone.md", to: "box/inner/lone.md"},
		{name: "into a nested note", from: "lone.md", to: "box/note.md/lone.md"},
		{name: "onto a nested symbolic link", from: "lone.md", to: "box/inner"},
		{name: "a symbolic link", from: "linked", to: "elsewhere"},
		{name: "a folder with a symbolic link", from: "box", to: "elsewhere"},
		{name: "a copy through a symbolic link", from: "lone.md", to: "linked/lone.md", copy: true},
		{name: "a copy into a nested note", from: "lone.md", to: "box/note.md/lone.md", copy: true},
		{name: "a copied folder with a symbolic link", from: "box", to: "elsewhere", copy: true},
		{name: "onto a path with a discussion", from: "old/a.md", to: "orphan.md"},
	} {
		r.refuse(c.name, func(s *Store) error {
			expected := headOf(r.t, s)
			if c.stale {
				expected = strings.Repeat("a", 40)
			}
			if c.copy {
				_, err := s.Duplicate(ctx, c.from, c.to, expected, "owner")
				return err
			}
			_, err := s.Move(ctx, c.from, c.to, expected, "owner")
			return err
		})
	}
}

// Moves and copies are refused as the Git backend refused them, and change
// nothing.
func TestDatabaseMoveRefusalsMatchGit(t *testing.T) {
	r := newRecording(t, newTestStore(t), "move refusals")
	moveRefusalFixture(r)
	r.begin()
	moveRefusalSteps(r)
	checkRecorded(t, "move-refusals", r)
}

func linkedDiscussionsFixture(r *recording) {
	saveNote(r.t, r.s, "a.md", "# A\n")
	outsideLinks(r.t, r.s, "comments")
}

func linkedDiscussionsSteps(r *recording) {
	r.refuse("a linked discussions folder", func(s *Store) error {
		_, err := s.Move(mutateContext(), "a.md", "b.md", headOf(r.t, s), "owner")
		return err
	})
	// A copy carries no discussion, so it was made.
	r.step("a copy", func(s *Store) (any, error) {
		return s.Duplicate(mutateContext(), "a.md", "b.md", headOf(r.t, s), "owner")
	})
}

// A note whose discussions folder is a symbolic link does not move, as in
// the Git backend, and nothing changes; it is copied without a discussion.
func TestDatabaseMoveLinkedDiscussionsRefusedAsGit(t *testing.T) {
	r := newRecording(t, newTestStore(t), "linked discussions")
	linkedDiscussionsFixture(r)
	r.begin()
	linkedDiscussionsSteps(r)
	checkRecorded(t, "linked-discussions", r)
}

func propertiesFixture(r *recording) {
	t, s := r.t, r.s
	saveNote(t, s, "Archives/note.md", "---\r\nstatus: draft\r\ncustom:\r\n  nested: keep\r\n---\r\n# Exact text\r\n[Link](other.md)\r\n")
	saveNote(t, s, "props.md", "---\ntitle: 'Old title'\nstatus: provisional\ntags:\n  - Fire\n  - 'old tongue'\ncustom:\n  nested: [keep, exactly]\n# keep this comment\n---\n\n# Human text\nExact body.\n")
	saveNote(t, s, "assets/cover.png", "image")
	saveNote(t, s, "locked.md", "# Locked\n")
	commitFixture(t, s, fixture("invalid.md", "---\nfmn-container: future\n---\nBody\n"))
	if _, err := s.SetLock(context.Background(), "locked.md", true, headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
}

func propertiesSteps(r *recording) {
	ctx := mutateContext()
	base := headOf(r.t, r.s)
	for _, container := range []string{"draft", "archive", "archive", ""} {
		r.step("container "+container, func(s *Store) (any, error) {
			return s.SetContainer(ctx, "Archives/note.md", container, headOf(r.t, s), "editor")
		})
	}
	cover := "assets/cover.png"
	for _, m := range []Metadata{
		{Title: "New title", Status: "canon", Tags: []string{"Fire", "research", "fire"}},
		{Title: "New title", Status: "canon", Tags: []string{"Fire", "research"}, Cover: &cover},
		{Title: "New title", Status: "canon", Tags: []string{"Fire", "research"}},
	} {
		r.step("properties", func(s *Store) (any, error) {
			return s.SetMetadata(ctx, "props.md", m, headOf(r.t, s), "writer")
		})
	}
	missing := "assets/missing.png"
	bad := "covers/x.png"
	for _, c := range []struct {
		name string
		op   func(*Store) error
	}{
		{"an unknown container", func(s *Store) error {
			_, err := s.SetContainer(ctx, "props.md", "future", headOf(r.t, s), "editor")
			return err
		}},
		{"a container outside the notebook", func(s *Store) error {
			_, err := s.SetContainer(ctx, "../outside.md", "archive", headOf(r.t, s), "editor")
			return err
		}},
		{"a container of instructions", func(s *Store) error {
			_, err := s.SetContainer(ctx, "AGENTS.md", "archive", headOf(r.t, s), "editor")
			return err
		}},
		{"a container of a missing note", func(s *Store) error {
			_, err := s.SetContainer(ctx, "missing.md", "archive", headOf(r.t, s), "editor")
			return err
		}},
		{"an invalid container to replace", func(s *Store) error {
			_, err := s.SetContainer(ctx, "invalid.md", "archive", headOf(r.t, s), "editor")
			return err
		}},
		{"a stale container", func(s *Store) error {
			_, err := s.SetContainer(ctx, "props.md", "archive", base, "editor")
			return err
		}},
		{"a locked container", func(s *Store) error {
			_, err := s.SetContainer(ctx, "locked.md", "archive", headOf(r.t, s), "editor")
			return err
		}},
		{"stale properties", func(s *Store) error {
			_, err := s.SetMetadata(ctx, "props.md", Metadata{Title: "Stale"}, base, "writer")
			return err
		}},
		{"locked properties", func(s *Store) error {
			_, err := s.SetMetadata(ctx, "locked.md", Metadata{Title: "Locked"}, headOf(r.t, s), "writer")
			return err
		}},
		{"a missing cover", func(s *Store) error {
			_, err := s.SetMetadata(ctx, "props.md", Metadata{Cover: &missing}, headOf(r.t, s), "writer")
			return err
		}},
		{"a cover outside assets", func(s *Store) error {
			_, err := s.SetMetadata(ctx, "props.md", Metadata{Cover: &bad}, headOf(r.t, s), "writer")
			return err
		}},
		{"a multi-line title", func(s *Store) error {
			_, err := s.SetMetadata(ctx, "props.md", Metadata{Title: "two\nlines"}, headOf(r.t, s), "writer")
			return err
		}},
		{"properties of a file that is no note", func(s *Store) error {
			_, err := s.SetMetadata(ctx, "assets/cover.png", Metadata{Title: "x"}, headOf(r.t, s), "writer")
			return err
		}},
	} {
		r.refuse(c.name, c.op)
	}
}

// SetContainer and SetMetadata make the revisions the Git backend made,
// refuse what it refused, and keep every other byte of a note.
func TestDatabasePropertiesMatchGit(t *testing.T) {
	r := newRecording(t, newTestStore(t), "properties")
	propertiesFixture(r)
	r.begin()
	propertiesSteps(r)
	checkRecorded(t, "properties", r)
}

// groupFixture is a notebook for edit groups: notes to edit, a note to
// delete with a discussion, a locked folder, a reverted note and a nested
// symbolic link.
func groupFixture(r *recording) {
	t, s := r.t, r.s
	for _, f := range [][2]string{
		{"a.md", "A\n"}, {"b.md", "B\n"}, {"notes/discard.md", "# Recover this note\n\n[A](../a.md)\n"}, {"manuscript/chapter.md", "# Chapter\n"},
	} {
		saveNote(t, s, f[0], f[1])
	}
	if _, err := s.AddComment(context.Background(), "notes/discard.md", "Discussion goes to the Trash too", headOf(t, s), "reader"); err != nil {
		t.Fatal(err)
	}
	lockManuscript(t, s)
	saveNote(t, s, "reverted.md", "Original\n")
	saveNote(t, s, "box/note.md", "# Boxed\n")
	outsideLinks(t, s, "box/inner")
}

// groupSteps applies a group, replays it, reads its receipt and is refused
// the groups the Git backend refused; it returns the group's result.
func groupSteps(r *recording) GroupResult {
	t, ctx := r.t, mutateContext()
	base := headOf(t, r.s)
	edit := func(path, content string, del bool) NoteEdit {
		doc, err := r.s.Read(ctx, path, "")
		if err != nil {
			t.Fatal(err)
		}
		return NoteEdit{Path: path, Content: content, Revision: base, Hash: ContentHash(doc.Content), Delete: del}
	}
	group := EditGroup{ID: "review-1", Summary: "Connect related notes", Edits: []NoteEdit{edit("a.md", "A2\n", false), edit("b.md", "B2\n", false), {Path: "new.md", Content: "# New\n"}, edit("notes/discard.md", "", true)}}
	applied, _ := r.step("a group", func(s *Store) (any, error) { return s.ApplyGroup(ctx, group) }).(GroupResult)
	r.step("the Trash", func(s *Store) (any, error) {
		listing, err := s.ListTrash(ctx)
		listing.Confirmation = ""
		return listing, err
	})
	r.step("a replay", func(s *Store) (any, error) { return s.ApplyGroup(ctx, group) })
	r.step("a receipt", func(s *Store) (any, error) { return s.GroupReceipt(ctx, group, "alice") })
	r.step("a receipt of a group never applied", func(s *Store) (any, error) {
		return s.GroupReceipt(ctx, EditGroup{ID: "never"}, "alice")
	})
	r.step("change a note", func(s *Store) (any, error) { return saveNote(t, s, "reverted.md", "Changed\n"), nil })
	r.step("revert it", func(s *Store) (any, error) { return saveNote(t, s, "reverted.md", "Original\n"), nil })
	for _, c := range []struct {
		name  string
		group func(*Store) EditGroup
	}{
		{"a changed retry", func(s *Store) EditGroup { g := group; g.Summary = "changed retry"; return g }},
		{"a stale group", func(s *Store) EditGroup { g := group; g.ID = "stale"; return g }},
		{"a changed then reverted note", func(s *Store) EditGroup {
			return EditGroup{ID: "reverted", Summary: "Must keep the conflict", Edits: []NoteEdit{{Path: "reverted.md", Revision: base, Hash: ContentHash([]byte("Original\n")), Content: "Stale replacement\n"}}}
		}},
		{"a new note that exists", func(s *Store) EditGroup {
			return EditGroup{ID: "exists", Summary: "Create", Edits: []NoteEdit{{Path: "a.md", Content: "x"}}}
		}},
		{"an unknown base", func(s *Store) EditGroup {
			return EditGroup{ID: "unknown", Summary: "Edit", Edits: []NoteEdit{{Path: "a.md", Revision: strings.Repeat("b", 40), Hash: ContentHash([]byte("A2\n")), Content: "x"}}}
		}},
		{"a locked note", func(s *Store) EditGroup {
			return EditGroup{ID: "locked", Summary: "Two notes", Edits: []NoteEdit{{Path: "a.md", Content: "x", Revision: headOf(t, s), Hash: ContentHash([]byte("A2\n"))}, {Path: "manuscript/new.md", Content: "blocked"}}}
		}},
		{"a malformed container", func(s *Store) EditGroup {
			return EditGroup{ID: "container", Summary: "Reject", Edits: []NoteEdit{{Path: "a.md", Revision: headOf(t, s), Hash: ContentHash([]byte("A2\n")), Content: "---\nfmn-container: draft\nfmn-container: archive\n---\nAfter"}}}
		}},
		{"a duplicate path", func(s *Store) EditGroup {
			return EditGroup{ID: "twice", Summary: "Twice", Edits: []NoteEdit{{Path: "x.md", Content: "1"}, {Path: "x.md", Content: "2"}}}
		}},
		{"instructions", func(s *Store) EditGroup {
			return EditGroup{ID: "agents", Summary: "Agents", Edits: []NoteEdit{{Path: "AGENTS.md", Content: "x"}}}
		}},
		{"no summary", func(s *Store) EditGroup { g := group; g.ID, g.Summary = "nosummary", " "; return g }},
		{"invalid text", func(s *Store) EditGroup {
			return EditGroup{ID: "nul", Summary: "Nul", Edits: []NoteEdit{{Path: "nul.md", Content: "a\x00b"}}}
		}},
		{"a deleted note that never was", func(s *Store) EditGroup {
			return EditGroup{ID: "never", Summary: "Delete", Edits: []NoteEdit{{Path: "never.md", Delete: true}}}
		}},
		{"a new note through a nested symbolic link", func(s *Store) EditGroup {
			return EditGroup{ID: "nested-link", Summary: "Nested", Edits: []NoteEdit{{Path: "box/inner/new.md", Content: "# New\n"}}}
		}},
		{"a new note in a nested note", func(s *Store) EditGroup {
			return EditGroup{ID: "nested-note", Summary: "Nested", Edits: []NoteEdit{{Path: "box/note.md/new.md", Content: "# New\n"}}}
		}},
	} {
		r.refuse(c.name, func(s *Store) error {
			_, err := s.ApplyGroup(ctx, c.group(s))
			return err
		})
	}
	return applied
}

// Edit groups make the revisions and results the Git backend made: edits,
// new notes and deletions into the Trash as one revision with the group's
// summary and operation trailers, replays of the same group, and conflicts
// for a changed retry or a stale base. A deleted note and its discussion
// keep their entries in the Trash.
func TestDatabaseGroupsMatchGit(t *testing.T) {
	r := newRecording(t, newTestStore(t), "groups")
	groupFixture(r)
	r.begin()
	s, ctx := r.s, mutateContext()
	discard, _ := s.notes.Stat(ctx, gitobj.ID{}, "notes/discard.md")
	discussion, _ := s.notes.Stat(ctx, gitobj.ID{}, commentPath("notes/discard.md"))
	applied := groupSteps(r)
	checkRecorded(t, "groups", r)

	listing, err := s.ListTrash(ctx)
	if err != nil || len(listing.Entries) != 1 {
		t.Fatal(listing, err)
	}
	trashed := listing.Entries[0].Path
	at := mustParse(t, applied.Revision)
	if f, err := s.notes.Stat(ctx, at, trashed); err != nil || f.EntryID != discard.EntryID {
		t.Errorf("the deleted note's entry is %q, want %q: %v", f.EntryID, discard.EntryID, err)
	}
	if f, err := s.notes.Stat(ctx, at, commentPath(trashed)); err != nil || f.EntryID != discussion.EntryID {
		t.Errorf("the deleted note's discussion is entry %q, want %q: %v", f.EntryID, discussion.EntryID, err)
	}
}
