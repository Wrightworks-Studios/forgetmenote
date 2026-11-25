package workspace

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
)

// organizationFixture is a notebook with what organization actions change:
// a note to move with an image link, a discussion and a backlink,
// duplicates with a backlink, and notes to tag and relate.
func organizationFixture(r *recording) {
	t, s := r.t, r.s
	for _, f := range [][2]string{
		{"notes/m.md", "# Original\n[image](../assets/picture.png)\n"},
		{"dest/index.md", "# Destination\n"},
		{"backlink.md", "[note](notes/m.md)\n"},
		{"a.md", "# Shared fact\nWorkshop begins at 09:00.\n"},
		{"b.md", "# Shared fact\nWorkshop begins at 09:00.\n"},
		{"links.md", "[current](b.md)\n"},
		{"tag.md", "# Tag me\n"},
		{"relate.md", "# Relate\n"},
		{"target.md", "# Target\n"},
	} {
		orgSave(t, s, f[0], f[1])
	}
	if _, err := s.AddComment(context.Background(), "notes/m.md", "Discussion moves with the note", headOf(t, s), "reader"); err != nil {
		t.Fatal(err)
	}
}

// organize records an organization action and returns its result.
func (r *recording) organize(name string, a OrganizationAction, apply bool) OrganizationResult {
	result, _ := r.step(name, func(s *Store) (any, error) { return s.Organize(organizationTestContext(), a, apply) }).(OrganizationResult)
	return result
}

// undo records the undo of a, applied at revision, and returns its result.
func (r *recording) undo(name, id string, a OrganizationAction, revision string, later ...OrganizationUndo) OrganizationResult {
	u := OrganizationUndo{ID: id, Action: a, Actor: "organizer-test", Revision: revision, Later: later}
	result, _ := r.step(name, func(s *Store) (any, error) { return s.UndoOrganization(organizationTestContext(), u) }).(OrganizationResult)
	return result
}

// The results organizationSteps applied.
type organizationOutcome struct {
	move, duplicate, undoDuplicate, undoMove OrganizationResult
}

func organizationSteps(r *recording) organizationOutcome {
	t, s, ctx := r.t, r.s, organizationTestContext()
	var out organizationOutcome
	tags := OrganizationAction{ID: "run:tags", Kind: "tags", Path: "tag.md", Tags: []string{"workshop"}, Allowed: []string{"tag.md"}, Sources: []MutationSource{orgSource(t, s, "tag.md")}}
	r.organize("a tag preview", tags, false)
	r.organize("tags", tags, true)
	r.organize("a replay", tags, true)
	r.organize("links", OrganizationAction{ID: "run:links", Kind: "links", Path: "relate.md", Target: "target.md", Allowed: []string{"relate.md"}, Sources: []MutationSource{orgSource(t, s, "relate.md"), orgSource(t, s, "target.md")}}, true)

	move := OrganizationAction{ID: "run:move", Kind: "move", Path: "notes/m.md", Target: "dest/m.md", Allowed: []string{"notes/m.md", "backlink.md"}, Folders: []string{"dest"}, Sources: []MutationSource{orgSource(t, s, "notes/m.md")}}
	r.organize("a move preview", move, false)
	out.move = r.organize("a move", move, true)
	r.step("the moved note's discussion", func(s *Store) (any, error) { return s.Comments(ctx, "dest/m.md") })

	duplicate := OrganizationAction{ID: "run:duplicate", Kind: "duplicate", Path: "b.md", Target: "a.md", Allowed: []string{"b.md", "links.md"}, Sources: []MutationSource{orgSource(t, s, "b.md"), orgSource(t, s, "a.md")}}
	out.duplicate = r.organize("a duplicate", duplicate, true)
	r.step("a receipt", func(s *Store) (any, error) { return s.OrganizationReceipt(ctx, duplicate, "organizer-test") })
	r.step("a receipt of an action never applied", func(s *Store) (any, error) {
		return s.OrganizationReceipt(ctx, OrganizationAction{ID: "run:never"}, "organizer-test")
	})

	out.undoDuplicate = r.undo("undoing the duplicate", "run:undo-duplicate", duplicate, out.duplicate.Revision)
	r.step("the Trash after the undo", func(s *Store) (any, error) {
		listing, err := s.ListTrash(ctx)
		listing.Confirmation = ""
		return listing, err
	})
	out.undoMove = r.undo("undoing the move", "run:undo-move", move, out.move.Revision)
	r.undo("an undo replay", "run:undo-move", move, out.move.Revision)
	r.step("current sources", func(s *Store) (any, error) {
		return nil, s.OrganizationSources(ctx, []MutationSource{orgSource(t, s, "notes/m.md"), orgSource(t, s, "a.md")})
	})
	return out
}

// Organization actions, their previews, replays, receipts and undos make
// the revisions and results the Git backend made: the same retagged,
// related, moved and Trashed notes, rewritten backlinks, moved discussions,
// Trash records, subjects and operation trailers. A moved or Trashed note
// keeps its entry, and so does one an undo brings back.
func TestDatabaseOrganizationMatchesGit(t *testing.T) {
	r := newRecording(t, newTestStore(t), "organization")
	organizationFixture(r)
	r.begin()
	s, ctx := r.s, organizationTestContext()
	moved, _ := s.notes.Stat(ctx, gitobj.ID{}, "notes/m.md")
	trashed, _ := s.notes.Stat(ctx, gitobj.ID{}, "b.md")
	out := organizationSteps(r)
	checkRecorded(t, "organization", r)

	if out.duplicate.Trash == nil {
		t.Fatal("the duplicate has no Trash entry")
	}
	for _, c := range []struct {
		revision, path string
		want           string
	}{
		{out.move.Revision, "dest/m.md", moved.EntryID},
		{out.duplicate.Revision, out.duplicate.Trash.Path, trashed.EntryID},
		{out.undoDuplicate.Revision, "b.md", trashed.EntryID},
		{out.undoMove.Revision, "notes/m.md", moved.EntryID},
	} {
		if f, err := s.notes.Stat(ctx, mustParse(t, c.revision), c.path); err != nil || f.EntryID != c.want {
			t.Errorf("%s at %s is entry %q, want %q: %v", c.path, c.revision, f.EntryID, c.want, err)
		}
	}
}

// An organizationRefusal is an action refused on a notebook with two
// duplicate notes, a backlink to one and a folder, after setup.
type organizationRefusal struct {
	name   string
	setup  func(r *recording)
	action func(r *recording) OrganizationAction
}

func organizationRefusals() []organizationRefusal {
	duplicate := OrganizationAction{ID: "run:1", Kind: "duplicate", Path: "b.md", Target: "a.md", Allowed: []string{"b.md", "links.md"}}
	sources := func(r *recording, paths ...string) []MutationSource {
		var out []MutationSource
		for _, p := range paths {
			out = append(out, orgSource(r.t, r.s, p))
		}
		return out
	}
	withSources := func(r *recording) OrganizationAction {
		a := duplicate
		a.Sources = sources(r, "b.md", "a.md")
		return a
	}
	move := func(target, folder string, allowed ...string) func(r *recording) OrganizationAction {
		return func(r *recording) OrganizationAction {
			return OrganizationAction{ID: "run:1", Kind: "move", Path: "b.md", Target: target, Folders: []string{folder}, Allowed: allowed, Sources: sources(r, "b.md")}
		}
	}
	return []organizationRefusal{
		{"a changed retained note", nil, func(r *recording) OrganizationAction {
			a := withSources(r)
			a.Sources[1].Hash = ContentHash([]byte("# Changed\n"))
			return a
		}},
		{"a collision", func(r *recording) { orgSave(r.t, r.s, "dest/b.md", "Occupied\n") }, move("dest/b.md", "dest", "b.md", "links.md")},
		{"a missing folder", nil, move("gone/b.md", "gone", "b.md", "links.md")},
		{"a folder that is a note", nil, move("a.md/b.md", "a.md", "b.md", "links.md")},
		{"a folder that is a nested note", nil, move("dest/index.md/b.md", "dest/index.md", "b.md", "links.md")},
		{"a folder that is a nested symbolic link", func(r *recording) { outsideLinks(r.t, r.s, "dest/inner") }, move("dest/inner/b.md", "dest/inner", "b.md", "links.md")},
		{"a folder that is a symbolic link", func(r *recording) { outsideLinks(r.t, r.s, "linked") }, move("linked/b.md", "linked", "b.md", "links.md")},
		{"a locked backlink", func(r *recording) {
			if _, err := r.s.SetLock(context.Background(), "links.md", true, headOf(r.t, r.s), "test"); err != nil {
				r.t.Fatal(err)
			}
		}, withSources},
		{"a backlink out of scope", nil, func(r *recording) OrganizationAction {
			a := withSources(r)
			a.Allowed = []string{"b.md"}
			return a
		}},
		{"an expected backlink missing", nil, func(r *recording) OrganizationAction {
			a := withSources(r)
			a.Expected = []MutationSource{}
			return a
		}},
		{"different metadata", func(r *recording) { orgSave(r.t, r.s, "b.md", "---\ntags: [unique]\n---\n# Shared\nFact 1.\n") }, withSources},
		{"a relative resource", func(r *recording) {
			orgSave(r.t, r.s, "nested/b.md", "# Shared\n[image](image.png)\n")
			orgSave(r.t, r.s, "a.md", "# Shared\n[image](image.png)\n")
		}, func(r *recording) OrganizationAction {
			a := duplicate
			a.Path, a.Allowed = "nested/b.md", []string{"nested/b.md"}
			a.Sources = sources(r, "nested/b.md", "a.md")
			return a
		}},
		{"a traversal", nil, move("../b.md", "..", "b.md")},
		{"an unknown kind", nil, func(r *recording) OrganizationAction {
			a := withSources(r)
			a.Kind = "empty_trash"
			return a
		}},
		{"a stale source", nil, func(r *recording) OrganizationAction {
			a := withSources(r)
			a.Sources[0].Revision = strings.Repeat("a", 40)
			return a
		}},
		{"a historical source", nil, func(r *recording) OrganizationAction {
			a := withSources(r)
			a.Sources[0].Historical = true
			return a
		}},
		{"a source in a container", func(r *recording) {
			orgSave(r.t, r.s, "b.md", "---\nfmn-container: archive\n---\n# Shared\nFact 1.\n")
		}, withSources},
		{"no ID", nil, func(r *recording) OrganizationAction {
			a := withSources(r)
			a.ID = ""
			return a
		}},
		{"a path not allowed", nil, func(r *recording) OrganizationAction {
			a := withSources(r)
			a.Allowed = []string{"links.md"}
			return a
		}},
		{"a retained note that sorts later", nil, func(r *recording) OrganizationAction {
			a := duplicate
			a.Path, a.Target, a.Allowed = "a.md", "b.md", []string{"a.md"}
			a.Sources = sources(r, "a.md", "b.md")
			return a
		}},
		{"a duplicate with a discussion", func(r *recording) {
			if _, err := r.s.AddComment(context.Background(), "b.md", "Keep me", headOf(r.t, r.s), "reader"); err != nil {
				r.t.Fatal(err)
			}
		}, withSources},
		{"a duplicate with a source's provenance", func(r *recording) {
			source := "---\nsource_id: 0123456789abcdef0123456789abcdef\nsource_sha256: " + strings.Repeat("ab", 32) + "\nsource_original: assets/sources/original.pdf\n---\n# Shared fact\nWorkshop begins at 09:00.\n"
			orgSave(r.t, r.s, "b.md", source)
			orgSave(r.t, r.s, "a.md", source)
		}, withSources},
		{"an unrewritable CommonMark backlink", func(r *recording) { orgSave(r.t, r.s, "links.md", "[duplicate \\]](b.md)\n") }, withSources},
		{"an unsupported Trash link rewrite", func(r *recording) {
			orgSave(r.t, r.s, "a.md", "# Same\n[image \\]](assets/picture.png)\n")
			orgSave(r.t, r.s, "b.md", "# Same\n[image \\]](assets/picture.png)\n")
		}, func(r *recording) OrganizationAction {
			return OrganizationAction{ID: "escaped:1", Kind: "duplicate", Path: "b.md", Target: "a.md", Allowed: []string{"b.md"}, Sources: sources(r, "b.md", "a.md")}
		}},
		{"a wiki link to move", func(r *recording) { orgSave(r.t, r.s, "b.md", "# Shared\n[[a]]\n") }, move("dest/b.md", "dest", "b.md", "links.md")},
		{"an HTML link to move", func(r *recording) { orgSave(r.t, r.s, "b.md", "# Shared\n<a href=\"a.md\">a</a>\n") }, move("dest/b.md", "dest", "b.md", "links.md")},
		{"an HTML image to move", func(r *recording) { orgSave(r.t, r.s, "b.md", "# Shared\n<img src=\"a.png\">\n") }, move("dest/b.md", "dest", "b.md", "links.md")},
		{"a fragment backlink to a different body", func(r *recording) {
			orgSave(r.t, r.s, "a.md", "# Shared fact\nWorkshop begins at 09:00.\n\nMore.\n")
			orgSave(r.t, r.s, "links.md", "[current](b.md#shared-fact)\n")
		}, withSources},
		{"pending live edits", nil, func(r *recording) OrganizationAction {
			r.s.mu.Lock()
			r.s.live["a.md"] = &liveRecord{Path: "a.md", Uncommitted: []string{"op"}}
			r.s.mu.Unlock()
			return withSources(r)
		}},
	}
}

// organizationRefusalFixture is the notebook every organizationRefusal
// starts from.
func organizationRefusalFixture(r *recording, c organizationRefusal) {
	for _, p := range []string{"a.md", "b.md"} {
		orgSave(r.t, r.s, p, "# Shared fact\nWorkshop begins at 09:00.\n")
	}
	orgSave(r.t, r.s, "links.md", "[note](b.md)\n")
	orgSave(r.t, r.s, "dest/index.md", "# Folder\n")
	if c.setup != nil {
		c.setup(r)
	}
}

func organizationRefusalSteps(r *recording, c organizationRefusal) {
	r.refuse(c.name, func(s *Store) error {
		_, err := s.Organize(organizationTestContext(), c.action(r), true)
		return err
	})
}

// organizationUndoRefusalSteps applies a duplicate and is refused the undos
// the Git backend refused.
func organizationUndoRefusalSteps(r *recording) {
	t, ctx := r.t, organizationTestContext()
	base := headOf(t, r.s)
	duplicate := OrganizationAction{ID: "run:1", Kind: "duplicate", Path: "b.md", Target: "a.md", Allowed: []string{"b.md", "links.md"}, Sources: []MutationSource{orgSource(t, r.s, "b.md"), orgSource(t, r.s, "a.md")}}
	applied := r.organize("a duplicate", duplicate, true)
	undo := OrganizationUndo{ID: "run:undo", Action: duplicate, Actor: "organizer-test", Revision: applied.Revision}
	for _, c := range []struct {
		name string
		edit func(OrganizationUndo) OrganizationUndo
	}{
		{"another revision", func(u OrganizationUndo) OrganizationUndo { u.Revision = base; return u }},
		{"another action", func(u OrganizationUndo) OrganizationUndo { u.Action.Tags = []string{"x"}; return u }},
		{"no ID", func(u OrganizationUndo) OrganizationUndo { u.ID = ""; return u }},
		{"a later receipt of another run", func(u OrganizationUndo) OrganizationUndo {
			u.Later = []OrganizationUndo{{ID: "other:1", Action: OrganizationAction{ID: "other:1"}}}
			return u
		}},
		{"a later receipt never made", func(u OrganizationUndo) OrganizationUndo {
			u.Later = []OrganizationUndo{{ID: "run:2", Action: OrganizationAction{ID: "run:2"}}}
			return u
		}},
	} {
		r.refuse(c.name, func(s *Store) error {
			_, err := s.UndoOrganization(ctx, c.edit(undo))
			return err
		})
	}
	doc, err := r.s.Read(ctx, "links.md", "")
	if err != nil {
		t.Fatal(err)
	}
	r.step("edit the backlink", func(s *Store) (any, error) { return saveNote(t, s, "links.md", "User changed backlink\n"), nil })
	r.step("revert the edit", func(s *Store) (any, error) { return saveNote(t, s, "links.md", string(doc.Content)), nil })
	r.refuse("an edit since, reverted", func(s *Store) error {
		_, err := s.UndoOrganization(ctx, undo)
		return err
	})
	r.step("edit the backlink again", func(s *Store) (any, error) {
		return saveNote(t, s, "links.md", "User changed backlink\n"), nil
	})
	r.refuse("a newer edit", func(s *Store) error {
		_, err := s.UndoOrganization(ctx, undo)
		return err
	})
}

// Organization actions and undos are refused as the Git backend refused
// them, and change nothing.
func TestDatabaseOrganizationRefusalsMatchGit(t *testing.T) {
	var recordings []*recording
	for _, c := range organizationRefusals() {
		t.Run(c.name, func(t *testing.T) {
			r := newRecording(t, newTestStore(t), c.name)
			recordings = append(recordings, r)
			organizationRefusalFixture(r, c)
			r.begin()
			organizationRefusalSteps(r, c)
		})
	}
	t.Run("undo", func(t *testing.T) {
		r := newRecording(t, newTestStore(t), "undo")
		recordings = append(recordings, r)
		organizationFixture(r)
		r.begin()
		organizationUndoRefusalSteps(r)
	})
	checkRecorded(t, "organization-refusals", recordings...)
}

// organizationUndoAfterLaterSteps applies a duplicate, then more tag
// actions on its rewritten backlink than undoLaterPage, each undone, and
// undoes the duplicate with all, all but one and none of them named.
func organizationUndoAfterLaterSteps(r *recording) {
	t := r.t
	duplicate := OrganizationAction{ID: "run:duplicate", Kind: "duplicate", Path: "b.md", Target: "a.md", Allowed: []string{"b.md", "links.md"}, Sources: []MutationSource{orgSource(t, r.s, "b.md"), orgSource(t, r.s, "a.md")}}
	applied := r.organize("a duplicate", duplicate, true)
	var later []OrganizationUndo
	for i := 1; 2*(i-1) <= undoLaterPage; i++ {
		id := fmt.Sprintf("run:tags-%d", i)
		tags := OrganizationAction{ID: id, Kind: "tags", Path: "links.md", Tags: []string{"workshop"}, Allowed: []string{"links.md"}, Sources: []MutationSource{orgSource(t, r.s, "links.md")}}
		tagged := r.organize("tags on the rewritten backlink", tags, true)
		r.undo("undoing the tags", "run:undo-"+id, tags, tagged.Revision)
		later = append(later, OrganizationUndo{ID: "run:undo-" + id, Action: tags, Actor: "organizer-test", Revision: tagged.Revision})
	}
	undo := OrganizationUndo{ID: "run:undo-duplicate", Action: duplicate, Actor: "organizer-test", Revision: applied.Revision}
	r.refuse("an undo past all but one later action", func(s *Store) error {
		u := undo
		u.Later = later[1:]
		_, err := s.UndoOrganization(organizationTestContext(), u)
		return err
	})
	r.refuse("an undo past a later action", func(s *Store) error {
		_, err := s.UndoOrganization(organizationTestContext(), undo)
		return err
	})
	r.undo("an undo after a later action's undo", undo.ID, duplicate, applied.Revision, later...)
	r.step("the restored duplicate", func(s *Store) (any, error) { return s.Read(organizationTestContext(), "b.md", "") })
}

// An undo restores what its action changed once the same run's later
// actions that changed it since are undone and named as later receipts, as
// in Git, however many there are; without them it is refused.
func TestDatabaseOrganizationUndoAfterLaterActionsMatchesGit(t *testing.T) {
	r := newRecording(t, newTestStore(t), "undo after later actions")
	organizationFixture(r)
	r.begin()
	organizationUndoAfterLaterSteps(r)
	checkRecorded(t, "organization-undo-after-later", r)
}
