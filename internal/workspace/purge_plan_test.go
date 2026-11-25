package workspace

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// A text purge's plan lists the commits whose author or committer holds
// the text, as an "author" occurrence counting them: a rewrite keeps every
// commit's author and committer, so the purge does not erase them there.
// A plan that finds the text only there rewrites nothing and cannot run.
func TestPurgePlanListsTheTextInCommitAuthors(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	const email = "jane.doe.review@example.com"
	save(t, s, "a.md", []byte("Contact "+email+" for the key.\n"), "")
	if _, err := s.notes.Apply(ctx, notestore.ChangeSet{Subject: "Jane's edit", Author: gitobj.NewSignature("Jane Doe", email, time.Now()), Files: []notestore.FileChange{fixture("b.md", "# B\n")}}); err != nil {
		t.Fatal(err)
	}
	author := PurgeOccurrence{Kind: "author", Versions: 1}
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: email}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Blocked != "" || len(plan.Notes) != 1 || !slices.Contains(plan.Elsewhere, author) {
		t.Fatalf("plan: notes %+v elsewhere %+v blocked %q", plan.Notes, plan.Elsewhere, plan.Blocked)
	}
	unlisted := purgeWork{PurgePlan: plan}
	unlisted.Elsewhere = slices.DeleteFunc(slices.Clone(plan.Elsewhere), func(o PurgeOccurrence) bool { return o.Kind == "author" })
	if unlisted.confirmation(s.notebook) == plan.Confirmation {
		t.Fatal("the confirmation does not bind the authors the plan lists")
	}

	plan, err = s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: "Jane Doe"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.From != "" || len(plan.Notes) != 0 || !slices.Equal(plan.Elsewhere, []PurgeOccurrence{author}) {
		t.Fatalf("a plan of a name only an author holds: %+v", plan)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "personal data", nil); !errors.Is(err, ErrPurgeInvalid) {
		t.Fatal("a purge of text only authors hold:", err)
	}
}

// A whole-note purge's plan lists the other notes whose versions may still
// name the note in a link, as "link" occurrences counting those versions:
// a Markdown link to its file name, as written or URL-escaped, or a wiki
// link to its name. They keep its name, which only a text purge removes;
// a mention of another file is not one.
func TestPurgePlanOfANoteListsTheNotesThatLinkToIt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	save(t, s, "Patients/Jane Doe.md", noteWithID("dddddddddddddddddddddddddddddddd", "Jane's record.\n"), "")
	save(t, s, "index.md", []byte("See [patient](Patients/Jane%20Doe.md).\n"), "")
	save(t, s, "angle.md", []byte("See [patient](<Patients/Jane Doe.md>).\n"), "")
	save(t, s, "wiki.md", []byte("See [[Jane Doe]].\n"), "")
	save(t, s, "other.md", []byte("See [x](Patients/John%20Doe.md), [[Jane Doe Junior]] and Doe.md.\n"), "")
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: "Patients/Jane Doe.md"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []PurgeOccurrence{{Path: "angle.md", Kind: "link", Versions: 1}, {Path: "index.md", Kind: "link", Versions: 1}, {Path: "wiki.md", Kind: "link", Versions: 1}}
	if !slices.Equal(plan.Elsewhere, want) || plan.Blocked != "" {
		t.Fatalf("elsewhere %+v, blocked %q", plan.Elsewhere, plan.Blocked)
	}

	// A move rewrites the link to the note's new path: both versions of
	// the linking note name it.
	s = newTestStore(t)
	note := save(t, s, "Jane.md", noteWithID("dddddddddddddddddddddddddddddddd", "Jane's record.\n"), "")
	save(t, s, "index.md", []byte("See [patient](Jane.md).\n"), "")
	if _, err := s.Move(ctx, "Jane.md", "Patients/Jane Doe.md", headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
	if got := readAt(t, s, "index.md", ""); got != "See [patient](Patients/Jane%20Doe.md).\n" {
		t.Fatalf("the move left index.md as %q", got)
	}
	plan, err = s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: "Patients/Jane Doe.md"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []PurgeOccurrence{{Path: "index.md", Kind: "link", Versions: 2}}; !slices.Equal(plan.Elsewhere, want) {
		t.Fatalf("elsewhere after a move %+v (note %s)", plan.Elsewhere, note.Revision)
	}
}
