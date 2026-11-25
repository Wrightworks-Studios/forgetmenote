package workspace

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

func sourceContext() context.Context {
	return attribution.With(context.Background(), attribution.Identity{ID: "alice", Name: "Alice", Email: "alice@local.invalid", Source: "mcp", Client: "Claude"})
}

// sourceFixture is a capture with every field fixed, so both backends
// write the same note for it.
func sourceFixture(id, text string) SourceCapture {
	return SourceCapture{Title: "Study " + id[:4], Original: []byte("<p>" + text + "</p>"), Text: text + "\n", Record: SourceRecord{ID: id, URL: "https://example.com/" + id[:4], MIME: "text/html", Method: "uploaded", CapturedAt: "2026-11-22T00:00:00Z"}}
}

// An archived source imports, refreshes and verifies with its note and
// original, refuses each bad request for its own reason, replays an
// operation, and commits with the Git backend's messages and identities.
func TestDatabaseSourcesImportRefreshAndVerify(t *testing.T) {
	g := newTestStore(t)
	ctx := sourceContext()
	a, b, c, d := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32), strings.Repeat("d", 32)
	existing, err := g.ImportSource(ctx, sourceFixture(a, "First edition"), "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	saveNote(t, g, existing.Path, string(existing.Content)+"\nA reader's note.\n")
	saveNote(t, g, "notes/plain.md", "# Plain\n")
	saveNote(t, g, "library.md", "# A file, not a folder\n")
	saveNote(t, g, "references/"+b+".md", "# An ordinary note\n")
	saveNote(t, g, "locked/a.md", "# Locked\n")
	if _, err := g.SetLock(context.Background(), "locked", true, headOf(t, g), "owner"); err != nil {
		t.Fatal(err)
	}
	commitFixture(t, g, notestore.FileChange{Path: "linked", Content: []byte("notes"), Mode: gitobj.ModeSymlink})
	changed, err := g.ImportSource(ctx, sourceFixture(d, "Changed original"), "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	saveNote(t, g, ParseSource(changed.Content).Original, "<p>Replaced</p>")
	removed, err := g.ImportSource(ctx, sourceFixture(strings.Repeat("e", 32), "Removed original"), "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	commitFixture(t, g, notestore.FileChange{Path: ParseSource(removed.Content).Original})
	op := sourceFixture(strings.Repeat("f", 32), "Operation")
	op.OperationID = "drive-file-version"
	replayed, err := g.ImportSource(ctx, op, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := g
	head := headOf(t, s)

	folder := func(f string) *string { return &f }
	long := sourceFixture(c, "Long operation")
	long.OperationID = strings.Repeat("o", 161)
	anonymous := sourceFixture(c, "No ID")
	anonymous.OperationID, anonymous.Record.ID = "no-id", ""
	invalid := sourceFixture(c, "Invalid")
	invalid.Text = "\x00"
	empty := sourceFixture(c, "Empty")
	empty.Original = nil
	ftp := sourceFixture(c, "FTP")
	ftp.Record.URL = "ftp://example.com/x"
	unknown := sourceFixture(c, "Unknown")
	unknown.Record.MIME = "application/x-unknown"
	untitled := sourceFixture(c, "Untitled")
	untitled.Title = " "
	badID := sourceFixture(c, "Bad ID")
	badID.Record.ID = "zz"
	opChanged := op
	opChanged.Text = "Different\n"
	fresh := sourceFixture(c, "Fresh")
	refresh := sourceFixture(a, "Second edition")
	type importCase struct {
		capture                 SourceCapture
		target, expected, title string
		folder                  *string
		class                   error
	}
	for name, ic := range map[string]importCase{
		"no original":              {capture: empty},
		"invalid text":             {capture: invalid},
		"a long operation":         {capture: long},
		"an operation without ID":  {capture: anonymous},
		"an invalid URL":           {capture: ftp},
		"no title":                 {capture: untitled},
		"an unknown format":        {capture: unknown},
		"a refresh with a folder":  {capture: refresh, target: existing.Path, expected: head, folder: folder("sources")},
		"a refresh without base":   {capture: refresh, target: existing.Path, class: ErrConflict},
		"a refresh of a file":      {capture: refresh, target: "library.txt", expected: head, class: ErrConflict},
		"a refresh of no revision": {capture: refresh, target: existing.Path, expected: strings.Repeat("0", 39) + "1", class: ErrConflict},
		"a refresh of a note":      {capture: refresh, target: "notes/plain.md", expected: head},
		"a stale refresh":          {capture: refresh, target: existing.Path, expected: existing.Revision, class: ErrConflict},
		"a refresh of nothing":     {capture: refresh, target: "notes/missing.md", expected: head, class: os.ErrNotExist},
		"a new import with a base": {capture: fresh, expected: head, class: ErrConflict},
		"an invalid ID":            {capture: badID},
		"an unclean folder":        {capture: fresh, folder: folder("a/../b"), class: ErrForbidden},
		"a hidden folder":          {capture: fresh, folder: folder(".hidden"), class: ErrForbidden},
		"a file as folder":         {capture: fresh, folder: folder("library.md")},
		"a linked folder":          {capture: fresh, folder: folder("linked"), class: ErrForbidden},
		"a locked folder":          {capture: fresh, folder: folder("locked"), class: ErrForbidden},
		"the Trash":                {capture: fresh, folder: folder("Trash"), class: ErrForbidden},
		"an imported ID":           {capture: sourceFixture(a, "Again"), folder: folder("elsewhere"), class: ErrConflict},
		"an occupied path":         {capture: sourceFixture(b, "Occupied"), folder: folder("references"), class: ErrConflict},
		"a replay":                 {capture: op},
		"a changed replay":         {capture: opChanged, class: ErrConflict},
	} {
		got, err := s.ImportSource(ctx, ic.capture, ic.target, ic.expected, ic.title, ic.folder)
		if name == "a replay" {
			if err != nil || got.Revision != replayed.Revision || string(got.Content) != string(replayed.Content) {
				t.Errorf("a replay read %+v %v", got, err)
			}
		} else if err == nil || ic.class != nil && !errors.Is(err, ic.class) {
			t.Errorf("import of %s: %v, want %v", name, err, ic.class)
		}
	}
	if headOf(t, s) != head {
		t.Fatal("a refused import committed")
	}
	if _, err := s.VerifySource(ctx, existing.Path, strings.Repeat("0", 39)+"1"); err == nil {
		t.Error("verified at an unknown revision")
	}
	if v, _ := s.VerifySource(ctx, changed.Path, ""); v.OriginalStatus != "changed" || v.TextStatus != "match" {
		t.Error("a changed original verified", v)
	}
	if v, _ := s.VerifySource(ctx, removed.Path, ""); v.OriginalStatus != "unavailable" {
		t.Error("a removed original verified", v)
	}

	// A new import and a refresh commit their note and original, with the
	// Git backend's messages.
	committed := func(what string, got Document, err error, dir string) {
		t.Helper()
		if err != nil || !strings.HasPrefix(got.Path, dir) || got.Revision != headOf(t, s) || got.Version != got.Revision {
			t.Fatalf("%s %+v %v", what, got, err)
		}
	}
	got, err := s.ImportSource(ctx, fresh, "", "", "Fresh title", folder("notes"))
	committed("new import", got, err, "notes/")
	committedAs(t, s, "Archive source: Fresh title", attribution.From(ctx))
	original := ParseSource(got.Content).Original
	if o, err := s.Read(ctx, original, ""); err != nil || string(o.Content) != string(fresh.Original) {
		t.Fatal("the original was not committed with its note", err)
	}
	got, err = s.ImportSource(ctx, refresh, existing.Path, headOf(t, s), "", nil)
	committed("refresh", got, err, existing.Path)
	if !strings.Contains(string(got.Content), "Second edition") {
		t.Fatalf("the refresh committed %q", got.Content)
	}
	committedAs(t, s, "Archive source: Study aaaa", attribution.From(ctx))
	verified, err := s.VerifySource(ctx, existing.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	if verified.OriginalStatus != "match" || verified.TextStatus != "match" {
		t.Fatal("the refresh does not verify", verified)
	}
}

// errRefused stands for any refusal in a table of expected outcomes.
var errRefused = errors.New("refused")

// Project instructions are written only through WriteInstructions, within
// their budget, scope and revision checks, with the Git backend's commits.
func TestDatabaseInstructionsKeepTheirChecks(t *testing.T) {
	s := newTestStore(t)
	ctx := sourceContext()
	both := func(what string, call func(*Store) (Document, error), class error) {
		t.Helper()
		_, err := call(s)
		if class == nil && err != nil || class != nil && class != errRefused && !errors.Is(err, class) || class == errRefused && err == nil {
			t.Errorf("%s: %v, want %v", what, err, class)
		}
	}
	scoped := func(control string) context.Context {
		return WithMutationScope(ctx, MutationScope{Version: 1, Content: ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}, Control: control})
	}
	both("oversized", func(s *Store) (Document, error) {
		return s.WriteInstructions(ctx, []byte(strings.Repeat("x", 65537)), "", "owner")
	}, errRefused)
	both("an ordinary save", func(s *Store) (Document, error) {
		return s.Save(ctx, "AGENTS.md", []byte("# Instructions\n"), "", "owner")
	}, ErrForbidden)
	both("a scope for another control", func(s *Store) (Document, error) {
		return s.WriteInstructions(scoped(""), []byte("# Instructions\n"), "", "owner")
	}, ErrForbidden)
	both("a first write", func(s *Store) (Document, error) {
		return s.WriteInstructions(scoped("set_project_instructions"), []byte("# Instructions\n"), "", "owner")
	}, nil)
	committedAs(t, s, "Update AGENTS.md", attribution.From(ctx))
	both("a write as if it were the first", func(s *Store) (Document, error) {
		return s.WriteInstructions(ctx, []byte("# Second\n"), "", "owner")
	}, ErrConflict)
	both("a write of an unknown revision", func(s *Store) (Document, error) {
		return s.WriteInstructions(ctx, []byte("# Second\n"), strings.Repeat("0", 39)+"1", "owner")
	}, ErrConflict)
	both("an unchanged write", func(s *Store) (Document, error) {
		return s.WriteInstructions(ctx, []byte("# Instructions\n"), headOf(t, s), "owner")
	}, nil)
	both("a second write", func(s *Store) (Document, error) {
		return s.WriteInstructions(ctx, []byte("# Second\n"), headOf(t, s), "owner")
	}, nil)
	committedAs(t, s, "Update AGENTS.md", attribution.From(ctx))
	if doc, err := s.Read(ctx, "AGENTS.md", ""); err != nil || string(doc.Content) != "# Second\n" {
		t.Fatal("instructions not written", doc, err)
	}
}

// findingCopy copies in with evidence of its own: AddFinding pins the
// evidence it is given in place.
func findingCopy(in FindingInput) FindingInput {
	in.Evidence = append([]SourceEvidence(nil), in.Evidence...)
	return in
}

// The review inbox lists the open findings, stale ones marked and hidden
// evidence withheld, before and after a restart with an exclusion; findings
// are added and decided with their checks and the Git backend's commits.
func TestDatabaseFindingsReviewAddAndDecide(t *testing.T) {
	g := newTestStore(t)
	ctx := context.Background()
	note := saveNote(t, g, "a.md", "# A\n\nA claim.\n")
	evidence := saveNote(t, g, "b.md", "# B\n\nThe evidence.\n")
	saveNote(t, g, "secret/c.md", "# Secret\n")
	steady := saveNote(t, g, "d.md", "# D\n")
	add := func(s *Store, in FindingInput, actor string) ReviewFinding {
		t.Helper()
		f, err := s.AddFinding(ctx, in, actor)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	head := headOf(t, g)
	pinned := add(g, FindingInput{Key: "pinned", Path: note.Path, Title: "Check", Text: "Compare the evidence", Revision: head, Hash: ContentHash(note.Content), Evidence: []SourceEvidence{{Path: evidence.Path}, {URL: "https://example.com/paper", Explanation: "The paper"}}}, "agent")
	add(g, FindingInput{Key: "hidden", Path: "d.md", Title: "Hidden evidence", Text: "Uses a secret", Revision: headOf(t, g), Hash: ContentHash(steady.Content), Evidence: []SourceEvidence{{Path: "secret/c.md"}}}, "agent")
	add(g, FindingInput{Key: "excluded", Path: "secret/c.md", Title: "Excluded", Text: "In an excluded folder", Revision: headOf(t, g), Hash: ContentHash([]byte("# Secret\n"))}, "agent")
	decided := add(g, FindingInput{Key: "decided", Path: "d.md", Title: "Decided", Text: "Already reviewed", Revision: headOf(t, g), Hash: ContentHash(steady.Content)}, "agent")
	discussion, err := g.Comments(ctx, "d.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.DecideFinding(ctx, "d.md", decided.Comment.ID, discussion.Revision, "agent", "dismiss", false); err != nil {
		t.Fatal(err)
	}
	commented, err := g.AddComment(ctx, "d.md", "An ordinary comment", headOf(t, g), "reader")
	if err != nil {
		t.Fatal(err)
	}
	ordinary := commented.Comments[len(commented.Comments)-1]
	saveNote(t, g, evidence.Path, "# B\n\nChanged evidence.\n")
	saveNote(t, g, "e.md", "# E\n")
	copied, err := g.Read(ctx, commentPath(note.Path), "")
	if err != nil {
		t.Fatal(err)
	}
	commitFixture(t, g, fixture(commentPath("e.md"), "not a discussion"), fixture(commentPath("f.md"), string(copied.Content)),
		fixture("comments/nested/x.json", string(copied.Content)), fixture("comments/readme.md", "# Not a discussion\n"))
	s := reopenTestStoreExcluding(t, g, []string{"secret"})

	got, err := s.Findings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale := map[string]bool{}
	for _, f := range got {
		stale[f.Comment.Finding.Key] = f.Comment.Finding.Stale
		if f.Comment.Finding.Key == "hidden" && f.Comment.Finding.Evidence[0].Explanation != "Evidence unavailable" {
			t.Errorf("hidden evidence was disclosed: %+v", f.Comment.Finding.Evidence)
		}
	}
	if _, listed := stale["excluded"]; listed || len(got) != 3 || !stale["pinned"] || !stale["hidden"] || stale["decided"] {
		t.Fatalf("the inbox %+v", got)
	}
	s = reopenTestStore(t, s)
	if again, err := s.Findings(ctx); err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("the inbox after a restart: %+v %v\nwas %+v", again, err, got)
	}

	revision := got[0].Revision
	for name, d := range map[string]struct {
		path, id, revision, actor, decision string
		canEdit                             bool
	}{
		"a stale revision":        {note.Path, pinned.Comment.ID, head, "agent", "resolve", false},
		"another author":          {note.Path, pinned.Comment.ID, revision, "reader", "resolve", false},
		"an unknown decision":     {note.Path, pinned.Comment.ID, revision, "agent", "approve", false},
		"an unknown finding":      {note.Path, "unknown", revision, "agent", "resolve", false},
		"an ordinary comment":     {"d.md", ordinary.ID, revision, "agent", "resolve", true},
		"an excluded note":        {"secret/c.md", pinned.Comment.ID, revision, "agent", "resolve", true},
		"a note without findings": {"b.md", pinned.Comment.ID, revision, "agent", "resolve", true},
		"a copied discussion":     {"f.md", pinned.Comment.ID, revision, "agent", "resolve", true},
	} {
		if _, err := s.DecideFinding(ctx, d.path, d.id, d.revision, d.actor, d.decision, d.canEdit); err == nil {
			t.Errorf("deciding with %s succeeded", name)
		}
	}
	input := FindingInput{Key: "pinned", Path: note.Path, Title: "Check", Text: "Compare the evidence", Revision: head, Hash: ContentHash(note.Content), Evidence: []SourceEvidence{{Path: evidence.Path}, {URL: "https://example.com/paper", Explanation: "The paper"}}}
	for name, in := range map[string]FindingInput{
		"a replay":         input,
		"a changed replay": {Key: "pinned", Path: note.Path, Title: "Changed", Text: "Changed", Revision: head, Hash: input.Hash},
		"no key":           {Path: note.Path, Title: "Check", Text: "Text", Revision: revision, Hash: input.Hash},
		"a stale hash":     {Key: "stale", Path: note.Path, Title: "Check", Text: "Text", Revision: revision, Hash: ContentHash([]byte("other"))},
		"no revision":      {Key: "unpinned", Path: note.Path, Title: "Check", Text: "Text", Hash: input.Hash},
		"a local URL":      {Key: "local", Path: note.Path, Title: "Check", Text: "Text", Revision: revision, Hash: input.Hash, Evidence: []SourceEvidence{{URL: "file:///etc/passwd"}}},
		"stale evidence":   {Key: "evidence", Path: note.Path, Title: "Check", Text: "Text", Revision: revision, Hash: input.Hash, Evidence: []SourceEvidence{{Path: evidence.Path, Revision: revision, Hash: ContentHash(evidence.Content)}}},
		"hidden evidence":  {Key: "hidden-evidence", Path: note.Path, Title: "Check", Text: "Text", Revision: revision, Hash: input.Hash, Evidence: []SourceEvidence{{Path: "secret/c.md"}}},
		"an excluded note": {Key: "excluded-note", Path: "secret/c.md", Title: "Check", Text: "Text", Revision: revision, Hash: ContentHash([]byte("# Secret\n"))},
	} {
		got, err := s.AddFinding(ctx, findingCopy(in), "agent")
		if name == "a replay" {
			if err != nil || got.Comment.ID != pinned.Comment.ID {
				t.Errorf("a replay answered %+v %v", got, err)
			}
		} else if err == nil {
			t.Errorf("adding %s succeeded", name)
		}
	}
	if _, err := s.AddFinding(ctx, FindingInput{Key: "unknown", Path: note.Path, Title: "Check", Text: "Text", Revision: strings.Repeat("0", 39) + "1", Hash: input.Hash}, "agent"); err == nil {
		t.Error("a finding pinned to an unknown revision was added")
	}
	if headOf(t, s) != revision {
		t.Fatal("a refused finding committed")
	}

	// A new finding and a decision commit the discussion, pinning the
	// evidence at the revision the finding names.
	fresh := FindingInput{Key: "fresh", Path: "d.md", Title: "Fresh", Text: "A fresh finding", Revision: revision, Hash: ContentHash(steady.Content), Evidence: []SourceEvidence{{Path: evidence.Path}}}
	added, err := s.AddFinding(ctx, findingCopy(fresh), "agent")
	if err != nil || added.Revision != headOf(t, s) || added.Comment.Finding.Evidence[0].Path != evidence.Path || added.Comment.Finding.Evidence[0].Revision != revision || added.Comment.Finding.RequestHash == "" {
		t.Fatalf("adding %+v %+v %v", added, added.Comment.Finding, err)
	}
	committedAs(t, s, "Update "+commentPath("d.md"), attribution.From(ctx))
	gotDecided, err := s.DecideFinding(ctx, "d.md", added.Comment.ID, headOf(t, s), "editor", "resolve", true)
	if err != nil || !gotDecided.Comment.Resolved || gotDecided.Comment.Finding.Dismissed || gotDecided.Revision != headOf(t, s) {
		t.Fatalf("deciding %+v %v", gotDecided, err)
	}
	committedAs(t, s, "Update "+commentPath("d.md"), attribution.From(ctx))
	inbox, err := s.Findings(ctx)
	if err != nil || len(inbox) != 4 {
		t.Fatal("the inbox after deciding", inbox, err)
	}
}
