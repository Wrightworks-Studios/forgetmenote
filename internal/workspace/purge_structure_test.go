package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// structureNotebook is a notebook whose own files all hold their
// structure: a Trash index and two archives (at an index capacity of two
// records) with the records of a restored note and of emptied ones, a note
// in the Trash, and a note that is locked and commented on.
type structureNotebook struct {
	s        *Store
	trashed  TrashEntry // the note in the Trash
	revision string     // a revision its Trash record's history lists
}

func newStructureNotebook(t *testing.T) structureNotebook {
	t.Helper()
	ctx := context.Background()
	s := newTestStore(t)
	lowerTrashLimits(t, 2, 8<<20, 100000, 1000)
	saveNote(t, s, "back.md", "# Back\n")
	back, err := s.TrashNotes(ctx, []string{"back.md"}, headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreTrash(ctx, back.Entries[0].ID, "", back.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		p := fmt.Sprintf("gone-%d.md", i)
		saveNote(t, s, p, "# Gone\n")
		if _, err := s.TrashNotes(ctx, []string{p}, headOf(t, s), "owner"); err != nil {
			t.Fatal(err)
		}
		emptyTrash(t, s)
	}
	kept := saveNote(t, s, "kept.md", "# Kept\n")
	trashed, err := s.TrashNotes(ctx, []string{"kept.md"}, kept.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(trashArchivesAt(t, s)) == 0 {
		t.Fatal("the fixture has no Trash archive")
	}
	doc := save(t, s, "a.md", noteWithID("a1b2c3d4e5f6a7b84302c1d2e3f4a5b6", "Locked.\n"), "")
	if _, err := s.AddComment(ctx, "a.md", "A remark.", doc.Revision, "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLock(ctx, "a.md", true, headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
	return structureNotebook{s: s, trashed: trashed.Entries[0], revision: kept.Revision}
}

// revisionSources counts the revisions of s's notebook the store has a
// Gitnotes-Source for.
func revisionSources(t *testing.T, s *Store) int {
	t.Helper()
	db := testEnv(t, s).db
	var n int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM revisions WHERE org = $1 AND notebook = $2 AND source IS NOT NULL`, db.Org, s.notebook).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// checkNotebookWorks reopens the structure notebook as a restarted process
// would and uses each of its own files: it lists, saves, reads the
// comments, lists, restores to and empties the Trash, and still knows where
// every revision came from.
func checkNotebookWorks(t *testing.T, nb structureNotebook, sources int) {
	t.Helper()
	ctx := context.Background()
	s := reopenTestStore(t, nb.s)
	if _, err := s.List(ctx); err != nil {
		t.Fatal("list:", err)
	}
	saved, err := s.Save(ctx, "after.md", []byte("# After\n"), "", "test")
	if err != nil {
		t.Fatal("save:", err)
	}
	if _, err := s.Save(ctx, "a.md", []byte("Unlocked?\n"), saved.Revision, "test"); !errors.Is(err, ErrForbidden) {
		t.Fatal("a save of the locked note:", err)
	}
	discussion, err := s.Comments(ctx, "a.md")
	if err != nil || len(discussion.Comments) != 1 || discussion.Comments[0].Text != "A remark." || discussion.Comments[0].Author != "ada" {
		t.Fatalf("comments: %+v %v", discussion.Comments, err)
	}
	listing, err := s.ListTrash(ctx)
	if err != nil || len(listing.Entries) != 1 || listing.Entries[0].ID != nb.trashed.ID {
		t.Fatalf("Trash: %+v %v", listing, err)
	}
	if _, err := s.RestoreTrash(ctx, nb.trashed.ID, "", listing.Revision, "owner"); err != nil {
		t.Fatal("restore:", err)
	}
	if _, err := s.TrashNotes(ctx, []string{"after.md"}, headOf(t, s), "owner"); err != nil {
		t.Fatal("Trash move:", err)
	}
	emptyTrash(t, s)
	if got := revisionSources(t, s); got < sources {
		t.Fatalf("revisions with a source: %d, before the purge %d", got, sources)
	}
}

// A text purge never redacts the notebook's own structure: text that only a
// key, an ID, a revision, a flag or a trailer of the notebook's own files
// holds blocks a plan for the whole notebook, which would otherwise leave
// files the notebook refuses to open, list or save with. Purging the notes
// that hold it still runs, and the notebook works after either.
func TestPurgeOfTextInTheNotebooksStructureIsBlocked(t *testing.T) {
	ctx := context.Background()
	words := []string{"version", "records", "history", "paths", "emptied", "restored", "originalPath", "text", "fmn-id", "Gitnotes-Source", "revision"}
	for _, word := range words {
		t.Run(word, func(t *testing.T) {
			nb := newStructureNotebook(t)
			s := nb.s
			if word == "revision" {
				// A short code that a revision in a Trash record's history
				// holds by chance.
				word = nb.revision[10:16]
			}
			save(t, s, "words.md", []byte("The word "+word+" is here.\n"), "")
			sources := revisionSources(t, s)
			plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: word}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Blocked == "" {
				t.Fatalf("a purge of %q from the whole notebook is not blocked: %+v", word, plan.Notes)
			}
			if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "test", nil); !errors.Is(err, ErrPurgeBlocked) {
				t.Fatal("a blocked purge:", err)
			}
			plan, err = s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: word, Paths: []string{"words.md"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if word == "Gitnotes-Source" {
				// Every commit message's trailers hold it.
				if plan.Blocked == "" {
					t.Fatal("a purge of a trailer key is not blocked")
				}
			} else {
				if plan.Blocked != "" || len(plan.Notes) != 1 || plan.Notes[0].Path != "words.md" {
					t.Fatalf("a purge of %q from the note that holds it: %+v", word, plan)
				}
				if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "test", nil); err != nil {
					t.Fatal(err)
				}
				if got := readAt(t, s, "words.md", ""); got != "The word [purged] is here.\n" {
					t.Fatalf("words.md: %q", got)
				}
			}
			checkNotebookWorks(t, nb, sources)
		})
	}
}

// Text that a note's fmn-id holds by chance blocks the purge, which would
// otherwise change the note's identity and leave it unsavable.
func TestPurgeOfTextInAnFmnIDIsBlocked(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	const id = "a1b2c3d4e5f6a7b84302c1d2e3f4a5b6"
	doc := save(t, s, "a.md", noteWithID(id, "The code f6a7b84302c1 is here.\n"), "")
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: "f6a7b84302c1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Blocked, "a.md") || !strings.Contains(plan.Blocked, "fmn-id") {
		t.Fatalf("a purge of text in an fmn-id: %q", plan.Blocked)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "test", nil); !errors.Is(err, ErrPurgeBlocked) {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "a.md", noteWithID(id, "Saved.\n"), doc.Revision, "test"); err != nil {
		t.Fatal(err)
	}
}

// A text purge still redacts what people wrote in the notebook's own files:
// a Trash record's title and who deleted the note, a comment's text,
// author and quoted passage, and the subject of a commit message, whose
// trailers stay as they are; the notebook works afterwards and no stored
// byte keeps the text.
func TestPurgeRedactsTextPeopleWroteInTheNotebooksOwnFiles(t *testing.T) {
	ctx := context.Background()
	nb := newStructureNotebook(t)
	s := nb.s
	secret := purgeSecret(t)
	note := save(t, s, "leak.md", []byte("# Plan "+secret+"\n\nThe passage "+secret+" here.\n"), "")
	anchor := &CommentAnchor{Quote: "passage " + secret, Prefix: "The ", Suffix: " here."}
	if _, err := s.AddComment(ctx, "leak.md", "About "+secret+".", note.Revision, "ada "+secret, CommentOptions{Anchor: anchor}); err != nil {
		t.Fatal(err)
	}
	saveNote(t, s, "other.md", "# Other\n")
	trashed, err := s.TrashNotes(ctx, []string{"other.md"}, headOf(t, s), "owner "+secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.notes.Apply(ctx, notestore.ChangeSet{Subject: "Leak " + secret, Author: fixtureAuthor, AllowEmpty: true,
		Trailers: []gitobj.Trailer{{Key: "Gitnotes-Source", Value: "web"}, {Key: "Collaboration-Receipt", Value: `{"actor":{"name":"` + secret + `"}}`}}}); err != nil {
		t.Fatal(err)
	}
	sources := revisionSources(t, s)
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Blocked != "" || plan.Messages != 1 {
		t.Fatalf("plan %+v", plan)
	}
	for _, p := range []string{"leak.md", commentPath("leak.md"), trashIndexPath} {
		if !slices.ContainsFunc(plan.Notes, func(f PurgeFile) bool { return f.Path == p }) {
			t.Fatalf("the plan does not redact %s: %+v", p, plan.Notes)
		}
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "a leaked key", nil); err != nil {
		t.Fatal(err)
	}
	env := testEnv(t, s)
	if bytes.Contains(storedBytes(t, env.db, env.blobs.Path()), []byte(secret)) {
		t.Fatal("the secret is still stored")
	}
	listing, err := s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(listing.Entries, func(e TrashEntry) bool { return e.ID == trashed.Entries[0].ID })
	if i < 0 || listing.Entries[i].DeletedBy != "owner [purged]" || listing.Entries[i].Title != "Other" {
		t.Fatalf("the Trash: %+v", listing.Entries)
	}
	discussion, err := s.Comments(ctx, "leak.md")
	if err != nil || len(discussion.Comments) != 1 {
		t.Fatal(discussion, err)
	}
	c := discussion.Comments[0]
	if c.Text != "About [purged]." || c.Author != "ada [purged]" || c.Anchor == nil || c.Anchor.Quote != "passage [purged]" || c.Anchor.Prefix != "The " || c.ID == "" || c.Anchor.Revision == "" {
		t.Fatalf("the comment: %+v %+v", c, c.Anchor)
	}
	head, err := s.notes.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.notes.Commit(ctx, head.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if commitSubject(info.Commit.Message) != "Leak [purged]" || !slices.Contains(info.Trailers, gitobj.Trailer{Key: "Gitnotes-Source", Value: "web"}) ||
		!slices.Contains(info.Trailers, gitobj.Trailer{Key: "Collaboration-Receipt", Value: `{"actor":{"name":"[purged]"}}`}) {
		t.Fatalf("the message: %q %+v", info.Commit.Message, info.Trailers)
	}
	if got := revisionSources(t, s); got != sources {
		t.Fatalf("revisions with a source: %d, before the purge %d", got, sources)
	}
	s = reopenTestStore(t, s)
	if _, err := s.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "after.md", []byte("# After\n"), "", "test"); err != nil {
		t.Fatal(err)
	}
	listing, err = s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EmptyTrash(ctx, listing.Revision, listing.Confirmation); err != nil {
		t.Fatal(err)
	}
}

// A commit message's redaction keeps the trailers this service writes: the
// Gitnotes trailers whole, and the key of a live edit's receipt. It redacts
// the subject, the body, a receipt's value and every other line, even one
// that looks like a trailer, its key included; what it cannot redact stays
// for the plan to block on.
func TestRedactMessageKeepsTheTrailersTheStoreReads(t *testing.T) {
	m := purgeMatcher{text: []byte("jane")}
	message := "Fix jane's note\n\nAsked by jane.\n\nGitnotes-Source: web\nGitnotes-User: jane\nSigned-off-by: Jane <jane@example.com>\n  jane continued\n"
	want := "Fix [purged]'s note\n\nAsked by [purged].\n\nGitnotes-Source: web\nGitnotes-User: jane\nSigned-off-by: Jane <[purged]@example.com>\n  [purged] continued\n"
	if got := m.redactMessage(message); got != want || !sameTrailers(message, got) {
		t.Fatalf("redacted %q", got)
	}
	for _, c := range []struct {
		text, message string
		// kept reports text a purge cannot redact, which blocks it.
		same, kept bool
	}{
		{"Signed-off-by", "Subject\n\nSigned-off-by: Jane\n", true, false},
		{"Jane", "Imported\n\nJane: please keep these\n", true, false},
		{"Gitnotes-Source", "Subject\n\nGitnotes-Source: web\n", true, true},
		{"web", "Subject on the web\n\nGitnotes-Source: web\n", true, true},
		{"Collaboration-Receipt", "Checkpoint\n\nGitnotes-Source: collaboration\nCollaboration-Receipt: {\"operation\":\"x\"}\n", true, true},
		{"operation", "Checkpoint\n\nGitnotes-Source: collaboration\nCollaboration-Receipt: {\"operation\":\"x\"}\n", true, false},
		{"Subject", "Subject: no trailers\n", true, false},
	} {
		m := purgeMatcher{text: []byte(c.text)}
		got := m.redactMessage(c.message)
		if sameTrailers(c.message, got) != c.same {
			t.Errorf("%q in %q: %q changes the trailers", c.text, c.message, got)
		}
		if kept := m.count([]byte(got)) > 0; kept != c.kept {
			t.Errorf("%q in %q: redacted %q, keeping it: %v", c.text, c.message, got, kept)
		}
	}
	// A pattern that would join the body to the trailers changes them.
	m = purgeMatcher{re: regexp.MustCompile(`\.\n\n`)}
	if got := m.redactMessage("Subject\n\nBody.\n\nGitnotes-Source: web\n"); sameTrailers("Subject\n\nBody.\n\nGitnotes-Source: web\n", got) {
		t.Fatalf("joined %q keeps the trailers", got)
	}
}

// A line of a commit message that only looks like a trailer, as an imported
// commit may have, is what someone wrote: a purge of a note redacts it in
// the message as text, its key included, rather than refusing to run.
func TestPurgeRedactsTextThatOnlyLooksLikeATrailer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	save(t, s, "a.md", []byte("Jane wrote this.\n"), "")
	notes := openTestNotes(t, testEnv(t, s)).Notebook("notes")
	head, err := notes.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := notes.Apply(ctx, notestore.ChangeSet{ExpectHead: &head.Commit, Files: []notestore.FileChange{{Path: "b.md", Content: []byte("Imported.\n")}},
		Author: gitobj.NewSignature("Importer", "importer@example.test", time.Now()), Subject: "Import notes", Body: "Jane: please keep these"})
	if err != nil {
		t.Fatal(err)
	}
	s = reopenTestStore(t, s)
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: "Jane", Paths: []string{"a.md"}}, nil)
	if err != nil || plan.Blocked != "" || plan.Messages != 1 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "a name", nil)
	if err != nil {
		t.Fatal(err)
	}
	rewritten, purged, err := s.ResolvePurgedRevision(ctx, "", imported.Head.Commit.String())
	if err != nil || purged || rewritten != result.Revision {
		t.Fatalf("the imported revision maps to %s (%v %v), want %s", rewritten, purged, err, result.Revision)
	}
	id, _ := gitobj.ParseID(rewritten)
	info, err := openTestNotes(t, testEnv(t, s)).Notebook("notes").Commit(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if message := info.Commit.Message; !strings.Contains(message, "[purged]: please keep these") || strings.Contains(message, "Jane") {
		t.Fatalf("the rewritten message: %q", message)
	}
}
