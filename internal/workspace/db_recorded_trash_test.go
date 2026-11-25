package workspace

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
)

// findInTrash is what a search that includes the Trash finds in it, named
// and sorted.
func findInTrash(r *recording, s *Store, query, tag, status, path string) (any, error) {
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "include", Drafts: "include"}, Collection: "all"}
	found, err := s.FindScoped(context.Background(), query, tag, status, path, true, 0, scope)
	trash := []Entry{}
	for _, e := range found.Entries {
		if e.TrashID != "" {
			trash = append(trash, e)
		}
	}
	return r.sorted(trash), err
}

// trashMutationSteps moves notes into the Trash, restores them, empties it
// and searches it, from an empty notebook, with the refusals between.
func trashMutationSteps(r *recording) {
	t := r.t
	ctx, user := context.Background(), attributed()
	for _, note := range []struct{ path, content string }{
		{"notes/a.md", "---\ntitle: Titled A\ntags: [keep]\nstatus: draft\n---\n# A\n\nSee [b](b.md) and [other](../other.md).\n"},
		{"notes/b.md", "# B\n\nA needle in [a](a.md).\n"},
		{"other.md", "# Other\n\nAnother needle.\n"},
		{"untitled.md", "Only text.\n"},
		{"locked/c.md", "# C\n"},
		{commentPath("notes/a.md"), `[{"finding":{"key":"k","path":"notes/a.md","title":"Check"},"id":"f1","author":"agent","text":"Check this","created":"2026-11-22T00:00:00Z","resolved":false}]`},
	} {
		r.step("save "+note.path, func(s *Store) (any, error) {
			return s.Save(ctx, note.path, []byte(note.content), "", "writer")
		})
	}
	r.step("comment", func(s *Store) (any, error) {
		d, err := s.Comments(ctx, "notes/a.md")
		if err != nil {
			return d, err
		}
		return s.AddComment(user, "notes/a.md", "Keep this discussion", d.Revision, "reviewer", CommentOptions{ReplyTo: "f1"})
	})
	r.step("lock", func(s *Store) (any, error) {
		return s.SetLock(user, "locked", true, headOf(t, s), "owner")
	})
	many := make([]string, 51)
	for i := range many {
		many[i] = fmt.Sprintf("n%02d.md", i)
	}
	for _, c := range []struct {
		name  string
		paths []string
	}{
		{"no notes", []string{}},
		{"51 notes", many},
		{"twice", []string{"notes/a.md", "notes/a.md"}},
		{"guidance", []string{"AGENTS.md"}},
		{"the Trash", []string{"Trash/x.md"}},
		{"locked", []string{"locked/c.md"}},
		{"missing", []string{"notes/a.md", "missing.md"}},
		{"a folder", []string{"notes"}},
		{"not a note", []string{commentPath("notes/a.md")}},
		{"invalid", []string{"../a.md"}},
	} {
		r.step("trash "+c.name, func(s *Store) (any, error) {
			return s.TrashNotes(ctx, c.paths, headOf(t, s), "owner")
		})
	}
	r.step("trash stale", func(s *Store) (any, error) {
		return s.TrashNotes(ctx, []string{"notes/a.md"}, "", "owner")
	})
	trashed, _ := r.step("trash", func(s *Store) (any, error) {
		return s.TrashNotes(user, []string{"notes/a.md", "other.md", "untitled.md"}, headOf(t, s), "owner")
	}).(TrashResult)
	if len(trashed.Entries) != 3 {
		t.Fatalf("trashed %+v", trashed)
	}
	var listing TrashListing
	list := func(s *Store) (any, error) {
		var err error
		listing, err = s.ListTrash(ctx)
		if err == nil && listing.Confirmation == "" {
			t.Error("a listing without a confirmation")
		}
		shown := listing
		shown.Confirmation = ""
		return shown, err
	}
	r.step("list", list)
	r.step("policy", func(s *Store) (any, error) { return s.TrashPolicy(ctx) })
	for _, q := range [][4]string{{"needle"}, {"", "keep"}, {"", "", "DRAFT"}, {"", "", "", "Trash/"}, {"notes/a"}, {"titled a"}, {"", "", "", "notes"}, {"only text"}} {
		r.step(fmt.Sprintf("search %q", q), func(s *Store) (any, error) { return findInTrash(r, s, q[0], q[1], q[2], q[3]) })
	}
	first := trashed.Entries[0]
	r.step("discussion in the Trash", func(s *Store) (any, error) { return s.Comments(ctx, first.Path) })
	r.step("comment in the Trash", func(s *Store) (any, error) {
		return s.AddComment(ctx, first.Path, "No", headOf(t, s), "reviewer")
	})
	r.step("edit in the Trash", func(s *Store) (any, error) {
		return s.Save(ctx, first.Path, []byte("changed"), headOf(t, s), "writer")
	})
	for _, c := range []struct{ name, to string }{
		{"a folder", "notes"},
		{"invalid", "a/../b.md"},
		{"into the Trash", "Trash/x.md"},
		{"into a Trash folder", "Trash/sub/x.md"},
		{"locked", "locked/a.md"},
		{"not a note", "x.txt"},
		{"occupied", "notes/b.md"},
		{"onto guidance", "AGENTS.md"},
	} {
		r.step("restore "+c.name, func(s *Store) (any, error) {
			return s.RestoreTrash(ctx, first.ID, c.to, headOf(t, s), "owner")
		})
	}
	r.step("restore unknown", func(s *Store) (any, error) {
		return s.RestoreTrash(ctx, strings.Repeat("0", 32), "", headOf(t, s), "owner")
	})
	r.step("restore stale", func(s *Store) (any, error) {
		return s.RestoreTrash(ctx, first.ID, "restored/a.md", "", "owner")
	})
	r.step("restore", func(s *Store) (any, error) {
		return s.RestoreTrash(user, first.ID, "restored/a.md", headOf(t, s), "owner")
	})
	r.step("restore twice", func(s *Store) (any, error) {
		return s.RestoreTrash(ctx, first.ID, "again.md", headOf(t, s), "owner")
	})
	r.step("restore to the original path", func(s *Store) (any, error) {
		return s.RestoreTrash(ctx, trashed.Entries[1].ID, "", headOf(t, s), "owner")
	})
	r.step("restored note", func(s *Store) (any, error) { return s.Read(ctx, "restored/a.md", "") })
	r.step("restored discussion", func(s *Store) (any, error) { return s.Comments(ctx, "restored/a.md") })
	r.step("trash again", func(s *Store) (any, error) {
		return s.TrashNotes(ctx, []string{"restored/a.md"}, headOf(t, s), "owner")
	})
	r.step("policy after a restore", func(s *Store) (any, error) { return s.TrashPolicy(ctx) })
	r.step("list again", list)
	stale := listing
	r.step("trash one more", func(s *Store) (any, error) {
		return s.TrashNotes(ctx, []string{"notes/b.md"}, headOf(t, s), "owner")
	})
	r.step("empty unconfirmed", func(s *Store) (any, error) { return s.EmptyTrash(ctx, headOf(t, s), "") })
	r.step("empty with a stale confirmation", func(s *Store) (any, error) {
		return s.EmptyTrash(ctx, headOf(t, s), stale.Confirmation)
	})
	r.step("list before emptying", list)
	r.step("empty another revision", func(s *Store) (any, error) {
		return s.EmptyTrash(ctx, stale.Revision, listing.Confirmation)
	})
	r.step("empty", func(s *Store) (any, error) { return s.EmptyTrash(user, listing.Revision, listing.Confirmation) })
	r.step("list emptied", list)
	r.step("empty an empty Trash", func(s *Store) (any, error) {
		return s.EmptyTrash(ctx, listing.Revision, listing.Confirmation)
	})
	r.step("restore emptied", func(s *Store) (any, error) {
		return s.RestoreTrash(ctx, trashed.Entries[2].ID, "", headOf(t, s), "owner")
	})
	r.step("policy after emptying", func(s *Store) (any, error) { return s.TrashPolicy(ctx) })
	r.step("search emptied", func(s *Store) (any, error) { return findInTrash(r, s, "", "", "", "") })
}

// Moving notes into the Trash, restoring them, emptying it and searching it
// answer, refuse and commit as they did on the Git backend, step by step
// from an empty notebook: the same results and errors, the same revisions
// with their subjects and attribution, and the same notes, discussions and
// Trash index.
func TestDatabaseTrashMutationsMatchGit(t *testing.T) {
	r := newRecording(t, newTestStore(t), "trash mutations")
	r.begin()
	trashMutationSteps(r)
	checkRecorded(t, "trash-mutations", r)
}

// commentWriteSteps adds, replies to, resolves and deletes comments, from
// an empty notebook, with the refusals between.
func commentWriteSteps(r *recording) {
	t := r.t
	ctx, user := context.Background(), attributed()
	note := "notes/a.md"
	r.step("a note", func(s *Store) (any, error) {
		return s.Save(ctx, note, []byte("---\ntitle: Private metadata\n---\n# Heading\n\nA **bright star** shines. A star.\n"), "", "writer")
	})
	r.step("another note", func(s *Store) (any, error) {
		return s.Save(ctx, "notes/b.md", []byte("# B\n"), "", "writer")
	})
	var d Discussion
	discussion := func(got Discussion, err error) (any, error) {
		if err == nil {
			d = got
		}
		return got, err
	}
	r.step("no discussion", func(s *Store) (any, error) { return discussion(s.Comments(ctx, note)) })
	for _, c := range []struct {
		name string
		add  func(s *Store) (Discussion, error)
	}{
		{"ambiguous passage", func(s *Store) (Discussion, error) {
			return s.AddComment(ctx, note, "Which?", d.Revision, "reviewer", CommentOptions{Anchor: &CommentAnchor{Quote: "star"}})
		}},
		{"anchored reply", func(s *Store) (Discussion, error) {
			return s.AddComment(ctx, note, "Both", d.Revision, "reviewer", CommentOptions{Anchor: &CommentAnchor{Quote: "bright star"}, ReplyTo: "x"})
		}},
		{"empty text", func(s *Store) (Discussion, error) { return s.AddComment(ctx, note, " \n", d.Revision, "reviewer") }},
		{"long text", func(s *Store) (Discussion, error) {
			return s.AddComment(ctx, note, strings.Repeat("x", 10001), d.Revision, "reviewer")
		}},
		{"missing note", func(s *Store) (Discussion, error) {
			return s.AddComment(ctx, "missing.md", "Missing", d.Revision, "reviewer")
		}},
		{"missing thread", func(s *Store) (Discussion, error) {
			return s.AddComment(ctx, note, "Reply", d.Revision, "reviewer", CommentOptions{ReplyTo: "missing"})
		}},
		{"not a note", func(s *Store) (Discussion, error) {
			return s.AddComment(ctx, "notes", "Folder", d.Revision, "reviewer")
		}},
		{"stale revision", func(s *Store) (Discussion, error) { return s.AddComment(ctx, note, "Stale", "", "reviewer") }},
		{"two option sets", func(s *Store) (Discussion, error) {
			return s.AddComment(ctx, note, "Two", d.Revision, "reviewer", CommentOptions{}, CommentOptions{})
		}},
	} {
		r.step(c.name, func(s *Store) (any, error) { return c.add(s) })
	}
	r.step("anchored thread", func(s *Store) (any, error) {
		return discussion(s.AddComment(user, note, "Check this passage", d.Revision, "reviewer", CommentOptions{Anchor: &CommentAnchor{Quote: "bright star", Prefix: "Heading A ", Suffix: " shines."}}))
	})
	r.step("reply with an anchor", func(s *Store) (any, error) {
		return s.AddComment(ctx, note, "Both", d.Revision, "second", CommentOptions{Anchor: &CommentAnchor{Quote: "bright star"}, ReplyTo: d.Comments[0].ID})
	})
	r.step("reply", func(s *Store) (any, error) {
		return discussion(s.AddComment(user, note, "Reply without editing", d.Revision, "second", CommentOptions{ReplyTo: d.Comments[0].ID}))
	})
	r.step("second thread", func(s *Store) (any, error) {
		return discussion(s.AddComment(ctx, note, "A second thread", d.Revision, "other"))
	})
	r.step("resolve another's thread", func(s *Store) (any, error) {
		return s.ResolveComment(ctx, note, d.Comments[0].ID, d.Revision, "other", false)
	})
	r.step("resolve a missing thread", func(s *Store) (any, error) {
		return s.ResolveComment(ctx, note, "missing", d.Revision, "other", true)
	})
	r.step("resolve stale", func(s *Store) (any, error) {
		return s.ResolveComment(ctx, note, d.Comments[1].ID, "", "other", false)
	})
	r.step("resolve", func(s *Store) (any, error) {
		return discussion(s.ResolveComment(user, note, d.Comments[1].ID, d.Revision, "other", false))
	})
	r.step("reply to a resolved thread", func(s *Store) (any, error) {
		return s.AddComment(ctx, note, "Late", d.Revision, "second", CommentOptions{ReplyTo: d.Comments[1].ID})
	})
	r.step("delete another's reply", func(s *Store) (any, error) {
		return s.DeleteComment(ctx, note, d.Comments[0].ID, d.Comments[0].Replies[0].ID, d.Revision, "reviewer", false)
	})
	r.step("delete a thread with replies", func(s *Store) (any, error) {
		return discussion(s.DeleteComment(user, note, d.Comments[0].ID, "", d.Revision, "reviewer", false))
	})
	r.step("delete a placeholder", func(s *Store) (any, error) {
		return s.DeleteComment(ctx, note, d.Comments[0].ID, "", d.Revision, "reviewer", true)
	})
	r.step("delete a missing reply", func(s *Store) (any, error) {
		return s.DeleteComment(ctx, note, d.Comments[0].ID, "missing", d.Revision, "reviewer", true)
	})
	r.step("delete stale", func(s *Store) (any, error) {
		return s.DeleteComment(ctx, note, d.Comments[1].ID, "", "", "other", false)
	})
	r.step("moderate the last reply", func(s *Store) (any, error) {
		return discussion(s.DeleteComment(ctx, note, d.Comments[0].ID, d.Comments[0].Replies[0].ID, d.Revision, "moderator", true))
	})
	r.step("delete another's thread", func(s *Store) (any, error) {
		return s.DeleteComment(ctx, note, d.Comments[0].ID, "", d.Revision, "reviewer", false)
	})
	r.step("delete the last thread", func(s *Store) (any, error) {
		return discussion(s.DeleteComment(ctx, note, d.Comments[0].ID, "", d.Revision, "other", false))
	})
	r.step("lock the folder", func(s *Store) (any, error) {
		return s.SetLock(user, "notes", true, d.Revision, "owner")
	})
	r.step("comment on a locked note", func(s *Store) (any, error) {
		return s.AddComment(ctx, note, "Still open for discussion", headOf(t, s), "reviewer")
	})
	r.step("the discussion", func(s *Store) (any, error) { return s.Comments(ctx, note) })
}

// Adding, replying to, resolving and deleting comments answer, refuse and
// commit as they did on the Git backend, step by step from an empty
// notebook: the same discussions, errors, revisions with their messages and
// attribution, and the same files.
func TestDatabaseCommentWritesMatchGit(t *testing.T) {
	r := newRecording(t, newTestStore(t), "comment writes")
	r.begin()
	commentWriteSteps(r)
	checkRecorded(t, "comment-writes", r)
}

// listingFixture is a notebook with what listings tell apart: notes with
// and without metadata, containers valid and not, a symbolic link, an
// executable note, an asset, an excluded folder, instructions and a lock.
func listingFixture(r *recording) {
	commitTree(r.t, r.s,
		treeChange{path: "Inbox/first.md", content: "---\ntitle: First\ntags: [one, Two]\nstatus: active\n---\n# Heading\n\nBody text.\n"},
		treeChange{path: "Inbox/second.md", content: "# Second\n\nMore body.\n"},
		treeChange{path: "Inbox/deep/third.md", content: "---\nfmn-container: draft\n---\n# Third\n"},
		treeChange{path: "archive.md", content: "---\nfmn-container: archive\n---\nArchived\n"},
		treeChange{path: "broken.md", content: "---\ntags: [broken\n---\nBroken\n"},
		treeChange{path: "invalid.md", content: "---\nfmn-container: future\n---\nRepair\n"},
		treeChange{path: "same-a.md", content: "# Same\n"},
		treeChange{path: "same-b.md", content: "# Same\n"},
		treeChange{path: "run.md", content: "# Run\n", mode: gitobj.ModeExecutable},
		treeChange{path: "link.md", content: "Inbox/first.md", mode: gitobj.ModeSymlink},
		treeChange{path: "assets/image.png", content: "\x89PNG\r\n"},
		treeChange{path: "private/hidden.md", content: "# Hidden\n"},
		treeChange{path: "AGENTS.md", content: "# Instructions\n"},
		treeChange{path: locksPath, content: `{"version":1,"paths":["Inbox/deep"]}`},
	)
}

// listingSteps reads the notebook's listings, collections, folders, scoped
// searches and visibility.
func listingSteps(r *recording) {
	ctx := context.Background()
	r.step("list", func(s *Store) (any, error) { return s.List(ctx) })
	for _, collection := range []string{"", "all", "notes", "drafts", "archives", "other"} {
		r.step("collection "+collection, func(s *Store) (any, error) { return s.ListCollection(ctx, collection) })
		for _, folder := range []string{"", "Inbox", "Inbox/", "Inbox/deep", "missing", "../x"} {
			r.step("browse "+folder+" in "+collection, func(s *Store) (any, error) {
				b, err := s.BrowseCollection(ctx, folder, collection)
				b.Revision = ""
				return b, err
			})
		}
	}
	r.step("collections", func(s *Store) (any, error) {
		c, err := s.Collections(ctx)
		c.Revision = ""
		return c, err
	})
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "lower", Drafts: "exclude"}, Collection: "all"}
	r.step("find scoped", func(s *Store) (any, error) {
		e, err := s.FindScoped(ctx, "body", "", "", "", false, 0, scope)
		e.Revision = ""
		return e, err
	})
	r.step("browse scoped", func(s *Store) (any, error) {
		b, err := s.BrowseScoped(ctx, "Inbox", scope)
		b.Revision = ""
		return b, err
	})
	r.step("visible history paths", func(s *Store) (any, error) {
		return s.VisibleHistoryPaths(ctx, []string{"Inbox/first.md", "private/hidden.md", "assets/image.png", ".git/config", "Trash/x.md"})
	})
	r.step("visibility", func(s *Store) (any, error) { return s.HistoryVisibilityID(ctx) })
}

// Listings, collections, folders, scoped searches and history visibility
// answer as they did on the Git backend for the same tree; an unknown
// collection and a folder outside the notebook are refused.
func TestDatabaseListingsMatchGit(t *testing.T) {
	r := newRecording(t, newTestStoreExcluding(t, []string{"private"}), "listings")
	listingFixture(r)
	r.begin()
	listingSteps(r)
	checkRecorded(t, "listings", r)
	for _, step := range r.steps {
		if step.Name == "collection other" && step.Error == "" {
			t.Error("listed an unknown collection")
		}
		if strings.HasPrefix(step.Name, "browse ../x ") && (len(step.Is) != 1 || step.Is[0] != "forbidden") {
			t.Errorf("%s: %+v, want a forbidden error", step.Name, step)
		}
	}
}
