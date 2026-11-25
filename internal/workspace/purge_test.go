package workspace

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
)

func purgeSecret(t testing.TB) string {
	t.Helper()
	raw := make([]byte, 12)
	rand.Read(raw)
	return "sk-live-" + hex.EncodeToString(raw)
}

func noteWithID(id, body string) []byte {
	return []byte("---\nfmn-id: " + id + "\n---\n\n" + body)
}

func save(t testing.TB, s *Store, path string, content []byte, expected string) Document {
	t.Helper()
	doc, err := s.Save(t.Context(), path, content, expected, "test")
	if err != nil {
		t.Fatal(path, err)
	}
	return doc
}

func readAt(t testing.TB, s *Store, path, revision string) string {
	t.Helper()
	doc, err := s.Read(t.Context(), path, revision)
	if err != nil {
		t.Fatal(path, revision, err)
	}
	return string(doc.Content)
}

// resolved is the revision a purge turned revision into.
func resolved(t testing.TB, s *Store, path, revision string) (string, bool) {
	t.Helper()
	next, purged, err := s.ResolvePurgedRevision(t.Context(), path, revision)
	if err != nil {
		t.Fatal("resolve", revision, err)
	}
	return next, purged
}

// storedBytes is every byte of every row db's org has in any table, as a
// binary COPY gives them, and of every file under dirs.
func storedBytes(t testing.TB, db *pg.DB, dirs ...string) []byte {
	t.Helper()
	ctx := t.Context()
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.columns WHERE table_schema = 'public' AND column_name = 'org' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		tables = append(tables, name)
	}
	rows.Close()
	var out bytes.Buffer
	for _, table := range tables {
		if _, err := conn.Conn().PgConn().CopyTo(ctx, &out, `COPY (SELECT * FROM "`+table+`" WHERE org = '`+db.Org+`') TO STDOUT WITH (FORMAT binary)`); err != nil {
			t.Fatal(table, err)
		}
	}
	for _, dir := range dirs {
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				raw, _ := os.ReadFile(p)
				out.Write(raw)
			}
			return nil
		})
	}
	return out.Bytes()
}

// A text purge of selected notes plans every version of them that holds the
// text, reports where else it appears, and needs the plan's confirmation and
// a reason. It replaces the text in those versions only; references to the
// old revisions resolve to their replacements.
func TestPurgeRedactsTextFromSelectedNotesAndReportsTheRest(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	save(t, s, "c.md", []byte("Unrelated.\n"), "")
	first := save(t, s, "a.md", noteWithID(id, "Key "+secret+".\n"), "")
	other := save(t, s, "b.md", []byte("Also "+secret+".\n"), "")
	second := save(t, s, "a.md", noteWithID(id, "Key "+secret+", again.\n"), other.Revision)
	third := save(t, s, "a.md", noteWithID(id, "No key.\n"), second.Revision)

	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret, Paths: []string{"a.md"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Notes) != 1 || plan.Notes[0].Path != "a.md" || !slices.Equal(plan.Notes[0].Versions, []string{second.Version, first.Version}) {
		t.Fatalf("notes %+v", plan.Notes)
	}
	if len(plan.Elsewhere) != 1 || plan.Elsewhere[0].Path != "b.md" || plan.Elsewhere[0].Versions != 1 {
		t.Fatalf("elsewhere %+v", plan.Elsewhere)
	}
	if plan.Revision != third.Revision || plan.From != first.Revision || plan.RevisionsRewritten != 4 || plan.Versions != 2 || plan.Blocked != "" || plan.ExportForcePush || plan.Confirmation == "" {
		t.Fatalf("plan %+v", plan)
	}
	if _, err := s.ExecutePurge(ctx, plan, strings.Repeat("0", 64), "owner", "a leaked key", nil); !errors.Is(err, ErrPurgeConfirmation) {
		t.Fatal("a wrong confirmation:", err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", " ", nil); err == nil {
		t.Fatal("a purge without a reason")
	}
	tampered := plan
	tampered.Scope.Paths = []string{"b.md"}
	if _, err := s.ExecutePurge(ctx, tampered, plan.Confirmation, "owner", "a leaked key", nil); !errors.Is(err, ErrPurgeConfirmation) {
		t.Fatal("a confirmation of another plan:", err)
	}

	result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "a leaked key", nil)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx)
	if err != nil || result.Revision != status.Revision || result.Rewritten != 4 || result.Purge == "" || result.Export {
		t.Fatalf("result %+v, status %+v %v", result, status, err)
	}
	firstAfter, purged := resolved(t, s, "a.md", first.Revision)
	if purged || readAt(t, s, "a.md", firstAfter) != string(noteWithID(id, "Key [purged].\n")) {
		t.Fatal("the first version:", firstAfter, purged)
	}
	secondAfter, _ := resolved(t, s, "a.md", second.Revision)
	if readAt(t, s, "a.md", secondAfter) != string(noteWithID(id, "Key [purged], again.\n")) {
		t.Fatal("the second version")
	}
	if readAt(t, s, "b.md", "") != "Also "+secret+".\n" || readAt(t, s, "a.md", "") != string(noteWithID(id, "No key.\n")) {
		t.Fatal("a file outside the purge changed")
	}
	if same, _ := resolved(t, s, "c.md", status.Revision); same != status.Revision {
		t.Fatal("a current revision resolves elsewhere")
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "a leaked key", nil); !errors.Is(err, ErrConflict) {
		t.Fatal("a plan of the history before the purge:", err)
	}
	record, err := s.notes.Purge(ctx, result.Purge)
	if err != nil || bytes.Contains(record.Scope, []byte(secret)) || record.Actor != "owner" {
		t.Fatalf("record %+v %v", record, err)
	}
	var summary map[string]any
	if json.Unmarshal(record.Scope, &summary) != nil || summary["kind"] != PurgeText {
		t.Fatal("scope:", string(record.Scope))
	}
}

// A text purge of the whole notebook covers every text file and commit
// message. It refuses to run when a file name holds the text, and reports
// binary files it cannot redact; without either, nothing it stores keeps
// the text.
func TestPurgeOfTheWholeNotebookRedactsEveryTextFileAndMessage(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	save(t, s, "a.md", noteWithID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "Key "+secret+".\n"), "")
	big := strings.Repeat("A long paragraph of the manuscript.\n\n", 600) + "The code is " + secret + ".\n\n" + strings.Repeat("More of the book.\n\n", 400)
	save(t, s, "book.md", []byte(big), "")
	commitFixture(t, s, fixture(commentPath("a.md"), `[{"id":"c1","body":"The key `+secret+` leaked"}]`+"\n"))
	if _, err := s.notes.Apply(ctx, notestore.ChangeSet{Subject: "Note " + secret, Author: fixtureAuthor, AllowEmpty: true}); err != nil {
		t.Fatal(err)
	}
	commitFixture(t, s, fixture("assets/scan.png", "\x89PNG\x00"+secret))
	scope := PurgeScope{Kind: PurgeText, Text: secret}
	plan, err := s.PreparePurge(ctx, scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Unredactable) != 1 || plan.Unredactable[0].Path != "assets/scan.png" || plan.Messages != 1 || len(plan.Notes) != 3 || len(plan.Elsewhere) != 0 {
		t.Fatalf("plan %+v", plan)
	}
	// The binary file goes, and a file named after the text arrives.
	head, _ := s.Status(ctx)
	if _, err := s.notes.Apply(ctx, notestore.ChangeSet{Subject: "Rename", Author: fixtureAuthor, Files: []notestore.FileChange{{Path: "assets/scan.png"}, fixture("keys/"+secret+".md", "x\n")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); !errors.Is(err, ErrConflict) {
		t.Fatal("a plan made before the notebook changed:", err, head)
	}
	plan, err = s.PreparePurge(ctx, scope, nil)
	if err != nil || plan.Blocked == "" {
		t.Fatal("a file name holding the text:", plan.Blocked, err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); err == nil {
		t.Fatal("a blocked purge ran")
	}
	status, _ := s.Status(ctx)
	if _, err := s.notes.Apply(ctx, notestore.ChangeSet{Subject: "Rename back", Author: fixtureAuthor, Files: []notestore.FileChange{{Path: "keys/note.md", From: "keys/" + secret + ".md"}}}); err != nil {
		t.Fatal(err, status)
	}
	// The name stays in the history before the rename; only purging that
	// note whole removes it.
	if plan, err = s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: "keys/note.md"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); err != nil {
		t.Fatal(err)
	}
	if plan, err = s.PreparePurge(ctx, scope, nil); err != nil || plan.Blocked != "" || len(plan.Unredactable) != 1 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	// The binary version must go whole too.
	if plan, err = s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: "assets/scan.png"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); err != nil {
		t.Fatal(err)
	}
	if plan, err = s.PreparePurge(ctx, scope, nil); err != nil || len(plan.Unredactable) != 0 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	env := testEnv(t, s)
	if !bytes.Contains(storedBytes(t, env.db, env.blobs.Path()), []byte(secret)) {
		t.Fatal("the scan does not see the stored secret")
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(storedBytes(t, env.db, env.blobs.Path(), filepath.Dir(s.Root)), []byte(secret)) {
		t.Fatal("the secret is still stored")
	}
	if !strings.Contains(readAt(t, s, "book.md", ""), "The code is [purged].") {
		t.Fatal("the manuscript")
	}
	if plan, err = s.PreparePurge(ctx, scope, nil); err != nil || len(plan.Notes) != 0 || plan.RevisionsRewritten != 0 {
		t.Fatalf("a plan after the purge: %+v %v", plan, err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); err == nil {
		t.Fatal("a purge of nothing ran")
	}
}

// A whole-note purge removes the note from every version wherever it was,
// with its discussion and its Trash record, and reports other files with the
// same content, which keep it. A reference to one of its versions resolves
// as purged.
func TestPurgeOfANoteRemovesItsHistoryDiscussionAndTrashRecord(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const id, trashID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccc"
	content := noteWithID(id, "Customer record.\n")
	created := save(t, s, "x.md", content, "")
	save(t, s, "y.md", []byte("Kept.\n"), "")
	commitFixture(t, s, fixture(commentPath("x.md"), `[{"id":"c1","body":"About the customer"}]`+"\n"), fixture("z.md", string(content)))
	trashPath := "Trash/" + trashID + ".md"
	index := TrashIndex{Version: 1, Records: []TrashRecord{{TrashEntry: TrashEntry{ID: trashID, Path: trashPath, OriginalPath: "x.md", Title: "Customer", DeletedAt: "2026-11-22T00:00:00+00:00", DeletedBy: "ada"},
		History: map[string][]string{"x.md": {created.Revision}}}}}
	changes := map[string][]byte{}
	if err := stageTrashIndex(index, 0, changes); err != nil {
		t.Fatal(err)
	}
	trashed := commitFixture(t, s, notestore.FileChange{Path: trashPath, From: "x.md"}, notestore.FileChange{Path: commentPath(trashPath), From: commentPath("x.md")},
		fixture(trashIndexPath, string(changes[trashIndexPath])))

	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: trashPath}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Notes) != 1 || plan.Notes[0].Path != trashPath || len(plan.Notes[0].Versions) != 2 || plan.From != created.Revision {
		t.Fatalf("plan %+v", plan)
	}
	if len(plan.Shared) != 1 || plan.Shared[0].Path != "z.md" {
		t.Fatalf("shared %+v", plan.Shared)
	}
	if plan.ReferencesToRemap != 1 {
		t.Fatal("references:", plan.ReferencesToRemap)
	}
	result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "customer data", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{created.Revision, trashed} {
		next, _ := resolved(t, s, "", old)
		rev, _ := parseRevision(next)
		files, err := s.notes.Files(ctx, rev)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if f.EntryID == id || f.Path == trashPath || f.Path == commentPath("x.md") || f.Path == commentPath(trashPath) {
				t.Fatal("the purged note remains:", f.Path)
			}
		}
	}
	if _, purged := resolved(t, s, "x.md", created.Revision); !purged {
		t.Fatal("a pin to the purged note")
	}
	if raw, _, err := s.notes.Read(ctx, gitobj.ID{}, trashIndexPath); err != nil || bytes.Contains(raw, []byte(trashID)) || bytes.Contains(raw, []byte("Customer")) {
		t.Fatal("the Trash index keeps the record:", string(raw), err)
	}
	listing, err := s.TrashPolicy(ctx)
	if err != nil || len(listing.Records) != 0 {
		t.Fatal("Trash:", listing, err)
	}
	if readAt(t, s, "z.md", "") != string(content) || readAt(t, s, "y.md", "") != "Kept.\n" {
		t.Fatal("another file changed")
	}
	if result.Rewritten != plan.RevisionsRewritten {
		t.Fatal(result)
	}
	reopenTestStore(t, s)
}

// commitAs commits changes straight into s's notebook with subject, and
// returns the head.
func commitAs(t testing.TB, s *Store, subject string, changes ...notestore.FileChange) string {
	t.Helper()
	r, err := s.notes.Apply(t.Context(), notestore.ChangeSet{Subject: subject, Author: fixtureAuthor, Files: changes, AllowEmpty: len(changes) == 0})
	if err != nil {
		t.Fatal(err)
	}
	return r.Head.Commit.String()
}

// fileAt is path's content at what a purge made of revision, and whether it
// is there.
func fileAt(t testing.TB, s *Store, revision, path string) (string, bool) {
	t.Helper()
	next, _ := resolved(t, s, "", revision)
	id, err := parseRevision(next)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.notes.Read(t.Context(), id, path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(path, err)
	}
	return string(raw), true
}

// jsonSecret is a secret with every character JSON escapes, as a password
// can be.
func jsonSecret(t testing.TB) string {
	return `pw"Tr0ub&dor<3>\` + purgeSecret(t)
}

// jsonForms are the ways a JSON file can store text: as json.Marshal
// writes it, with and without HTML escaping, quotes stripped.
func jsonForms(text string) [][]byte {
	html, _ := json.Marshal(text)
	var plain bytes.Buffer
	enc := json.NewEncoder(&plain)
	enc.SetEscapeHTML(false)
	enc.Encode(text)
	return [][]byte{[]byte(text), html[1 : len(html)-1], bytes.TrimSuffix(plain.Bytes(), []byte("\"\n"))[1:]}
}

// A text purge finds its text in a JSON file as JSON stores it, escaped,
// and redacts it there: a password holding a quote, a backslash, an
// ampersand and angle brackets leaves no copy in a note, its discussion or a
// JSON attachment written without HTML escaping, by text or by pattern. Each
// file keeps the escaping its writer used.
func TestPurgeRedactsTextJSONStoresEscaped(t *testing.T) {
	discussion := func(text string) string {
		raw, err := json.MarshalIndent([]Comment{{ID: "c1", Author: "ada", Text: "Tom & Jerry: the password is " + text + ", rotate it"}}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return string(raw) + "\n"
	}
	config := func(password string) string {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.Encode(map[string]string{"password": password, "user": "ada & co"})
		return b.String()
	}
	for _, byPattern := range []bool{false, true} {
		ctx := t.Context()
		s := newTestStore(t)
		secret := jsonSecret(t)
		const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		save(t, s, "a.md", noteWithID(id, "Password: "+secret+"\n"), "")
		commitFixture(t, s, fixture(commentPath("a.md"), discussion(secret)), fixture("assets/config.json", config(secret)))
		scope := PurgeScope{Kind: PurgeText, Text: secret}
		if byPattern {
			scope = PurgeScope{Kind: PurgeText, Pattern: regexp.QuoteMeta(secret)}
		}
		plan, err := s.PreparePurge(ctx, scope, nil)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Blocked != "" || len(plan.Notes) != 3 || len(plan.Unredactable) != 0 || len(plan.Elsewhere) != 0 {
			t.Fatalf("pattern %v: plan %+v", byPattern, plan)
		}
		if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "a leaked password", nil); err != nil {
			t.Fatal(err)
		}
		if got, _, err := s.notes.Read(ctx, gitobj.ID{}, commentPath("a.md")); err != nil || string(got) != discussion(purgedText) {
			t.Fatalf("pattern %v: the discussion %s %v", byPattern, got, err)
		}
		if got := readAt(t, s, "assets/config.json", ""); got != config(purgedText) {
			t.Fatalf("pattern %v: the attachment %s", byPattern, got)
		}
		stored := storedBytes(t, testEnv(t, s).db, testEnv(t, s).blobs.Path())
		for _, form := range jsonForms(secret) {
			if bytes.Contains(stored, form) {
				t.Fatalf("pattern %v: %q is still stored", byPattern, form)
			}
		}
	}
}

// A whole-note purge removes the discussion the note had at each of its
// paths, and only while the note was there: a note that later took one of
// its old paths keeps its own discussion in every version.
func TestPurgeOfANoteKeepsAnotherNotesDiscussionAtAnOldPath(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const a, b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	aComments, bComments := `[{"id":"c1","text":"About A"}]`+"\n", `[{"id":"c2","text":"About B"}]`+"\n"
	bMore := `[{"id":"c2","text":"About B"},{"id":"c3","text":"More on B"}]` + "\n"
	created := commitAs(t, s, "Update x.md", notestore.FileChange{Path: "x.md", Content: noteWithID(a, "A.\n"), EntryID: a}, fixture(commentPath("x.md"), aComments))
	moved := commitAs(t, s, `Move "x.md" to "y.md"`, notestore.FileChange{Path: "y.md", From: "x.md"}, notestore.FileChange{Path: commentPath("y.md"), From: commentPath("x.md")})
	taken := commitAs(t, s, "Update x.md", notestore.FileChange{Path: "x.md", Content: noteWithID(b, "B.\n"), EntryID: b}, fixture(commentPath("x.md"), bComments))
	edited := commitAs(t, s, "Comment on x.md", fixture(commentPath("x.md"), bMore))

	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: "y.md"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Blocked != "" || len(plan.Notes) != 1 || plan.Notes[0].Path != "y.md" || len(plan.Shared) != 0 {
		t.Fatalf("plan %+v", plan)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "customer data", nil); err != nil {
		t.Fatal(err)
	}
	for _, rev := range []string{created, moved, taken, edited} {
		for _, p := range []string{"y.md", commentPath("y.md")} {
			if _, ok := fileAt(t, s, rev, p); ok {
				t.Fatal("the purged note keeps", p)
			}
		}
	}
	for rev, want := range map[string]string{created: "", moved: "", taken: bComments, edited: bMore} {
		got, ok := fileAt(t, s, rev, commentPath("x.md"))
		if want == "" && ok || want != "" && got != want {
			t.Fatalf("the discussion at x.md at %s: %q %v, want %q", rev, got, ok, want)
		}
	}
	if got, _ := fileAt(t, s, edited, "x.md"); got != string(noteWithID(b, "B.\n")) {
		t.Fatal("the other note:", got)
	}
}

// subjectAt is the first line of the message of what a purge made of
// revision.
func subjectAt(t testing.TB, s *Store, revision string) string {
	t.Helper()
	next, _ := resolved(t, s, "", revision)
	id, _ := parseRevision(next)
	info, err := s.notes.Commit(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	subject, _, _ := strings.Cut(info.Commit.Message, "\n")
	return subject
}

// A whole-note purge removes the note's path, and its discussion's, from
// the commit messages that name it while the note is there, bare or quoted,
// and counts them in its plan. A message naming another file keeps it, even
// one that later took the note's old path, or whose path ends with it.
func TestPurgeOfANoteRedactsItsPathFromMessages(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const id = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const first, last = "Jane.md", "Patients/Jane Doe.md"
	created := save(t, s, first, noteWithID(id, "Diagnosis.\n"), "")
	edited := save(t, s, first, noteWithID(id, "Diagnosis, revised.\n"), created.Revision)
	other := save(t, s, "Old/Jane.md", []byte("Another Jane.\n"), "")
	moved := commitAs(t, s, fmt.Sprintf("Move %q to %q", first, last), notestore.FileChange{Path: last, From: first})
	checkpoint := commitAs(t, s, "Checkpoint live edits to "+last, fixture(last, string(noteWithID(id, "Diagnosis, live.\n"))))
	commented := commitAs(t, s, "Update "+commentPath(last), fixture(commentPath(last), `[{"id":"c1","text":"Seen"}]`+"\n"))
	typo := commitAs(t, s, "Fix a typo in "+last+", again", fixture(last, string(noteWithID(id, "Diagnosis, fixed.\n"))))
	taken := save(t, s, first, []byte("Someone else.\n"), "")

	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: last}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Messages != 6 {
		t.Fatalf("plan %+v", plan)
	}
	result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "personal data", nil)
	if err != nil {
		t.Fatal(err)
	}
	for rev, want := range map[string]string{
		created.Revision: "Update [purged]", edited.Revision: "Update [purged]", moved: `Move "[purged]" to "[purged]"`,
		checkpoint: "Checkpoint live edits to [purged]", commented: "Update [purged]", typo: "Fix a typo in [purged], again",
		other.Revision: "Update Old/Jane.md", taken.Revision: "Update Jane.md",
	} {
		if got := subjectAt(t, s, rev); got != want {
			t.Errorf("subject %q, want %q", got, want)
		}
	}
	if stored := storedBytes(t, testEnv(t, s).db, testEnv(t, s).blobs.Path()); bytes.Contains(stored, []byte("Jane Doe")) || bytes.Contains(stored, []byte(commentPath(last))) {
		t.Fatal("the note's path is still stored")
	}
	record, err := s.notes.Purge(ctx, result.Purge)
	var summary purgeSummary
	if err != nil || json.Unmarshal(record.Scope, &summary) != nil || summary.Messages != 6 {
		t.Fatal("summary:", string(record.Scope), err)
	}
}

// A whole-note purge removes the note's lock from every version of the
// locks file that names it while the note is there, and keeps every other
// lock.
func TestPurgeOfANoteRemovesItsLock(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const id = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const note = "Patients/Jane Doe.md"
	policy := func(paths ...string) string {
		raw, _ := json.MarshalIndent(lockPolicy{1, paths}, "", "  ")
		return string(raw) + "\n"
	}
	save(t, s, "Other.md", []byte("Kept.\n"), "")
	created := commitAs(t, s, "Update "+note, notestore.FileChange{Path: note, Content: noteWithID(id, "Diagnosis.\n"), EntryID: id})
	locked := commitAs(t, s, "Lock", fixture(locksPath, policy("Other.md", note)))
	edited := commitAs(t, s, "Fixture", fixture("Other.md", "Kept, edited.\n"))
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: note}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Shared) != 0 || plan.Blocked != "" {
		t.Fatalf("plan %+v", plan)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "personal data", nil); err != nil {
		t.Fatal(err)
	}
	for _, rev := range []string{locked, edited} {
		if got, _ := fileAt(t, s, rev, locksPath); got != policy("Other.md") {
			t.Fatalf("the locks at %s: %q", rev, got)
		}
	}
	if _, ok := fileAt(t, s, created, locksPath); ok {
		t.Fatal("a locks file before the lock")
	}
	if bytes.Contains(storedBytes(t, testEnv(t, s).db, testEnv(t, s).blobs.Path()), []byte("Jane Doe")) {
		t.Fatal("the note's path is still stored")
	}

	// A lock the head keeps after the note left its path may be another
	// note's there: it stays, and the plan reports it.
	s = newTestStore(t)
	commitAs(t, s, "Update a.md", notestore.FileChange{Path: "a.md", Content: noteWithID(id, "A.\n"), EntryID: id})
	commitAs(t, s, "Lock", fixture(locksPath, policy("a.md")))
	commitAs(t, s, `Move "a.md" to "b.md"`, notestore.FileChange{Path: "b.md", From: "a.md"})
	commitAs(t, s, "Update a.md", fixture("a.md", "Another note.\n"))
	if plan, err = s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: "b.md"}, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Shared, []PurgeOccurrence{{Path: locksPath, Kind: "name", Versions: 1}}) {
		t.Fatalf("plan %+v", plan)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "personal data", nil); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.notes.Read(ctx, gitobj.ID{}, locksPath); err != nil || string(got) != policy("a.md") {
		t.Fatal("the other note's lock:", string(got), err)
	}
}

func TestRedactPathsNamesOnlyWholePaths(t *testing.T) {
	for message, want := range map[string]string{
		"Update a.md":                     "Update [purged]",
		"Update data.md":                  "Update data.md",
		"Update Old/a.md":                 "Update Old/a.md",
		"Update a.md.bak":                 "Update a.md.bak",
		"Saved a.md.":                     "Saved [purged].",
		"Saved (a.md), then b.md":         "Saved ([purged]), then b.md",
		`Move "a.md" to "q\"x.md"`:        `Move "[purged]" to "[purged]"`,
		"Edit a.md and a.md\n\nBody a.md": "Edit [purged] and [purged]\n\nBody [purged]",
	} {
		if got := redactPaths(message, []string{"a.md", `q"x.md`}); got != want {
			t.Errorf("%q: %q, want %q", message, got, want)
		}
	}
}

// A discussion file that served the purged note and then, left at its old
// path, another note holds both notes' comments; the plan is blocked rather
// than keep the purged note's comments or drop the other's.
func TestPurgeOfANoteIsBlockedByADiscussionTwoNotesShared(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const a, b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	commitAs(t, s, "Update x.md", notestore.FileChange{Path: "x.md", Content: noteWithID(a, "A.\n"), EntryID: a}, fixture(commentPath("x.md"), `[{"id":"c1","text":"About A"}]`+"\n"))
	commitAs(t, s, `Move "x.md" to "y.md"`, notestore.FileChange{Path: "y.md", From: "x.md"})
	commitAs(t, s, "Update x.md", notestore.FileChange{Path: "x.md", Content: noteWithID(b, "B.\n"), EntryID: b},
		fixture(commentPath("x.md"), `[{"id":"c1","text":"About A"},{"id":"c2","text":"About B"}]`+"\n"))
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: "y.md"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Blocked, commentPath("x.md")) {
		t.Fatalf("plan %+v", plan)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "customer data", nil); err == nil {
		t.Fatal("a blocked purge ran")
	}
}

// A purge of versions replaces just those versions with a placeholder that
// keeps the note's identity; a reference to one resolves as purged, and to
// any other as its rewritten revision.
func TestPurgeOfVersionsReplacesThemWithAPlaceholder(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	const id = "dddddddddddddddddddddddddddddddd"
	v1 := save(t, s, "v.md", noteWithID(id, "Draft.\n"), "")
	v2 := save(t, s, "v.md", noteWithID(id, "Pasted "+secret+".\n"), v1.Revision)
	v3 := save(t, s, "v.md", noteWithID(id, "Clean again.\n"), v2.Revision)
	if _, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeVersions, Path: "v.md", Versions: []string{strings.Repeat("e", 40)}}, nil); err == nil {
		t.Fatal("a version the note never had")
	}
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeVersions, Path: "v.md", Versions: []string{v2.Version}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Notes) != 1 || !slices.Equal(plan.Notes[0].Versions, []string{v2.Version}) || plan.RevisionsRewritten != 2 {
		t.Fatalf("plan %+v", plan)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "pasted a key", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolved(t, s, "v.md", v1.Revision); got != v1.Revision || readAt(t, s, "v.md", got) != string(noteWithID(id, "Draft.\n")) {
		t.Fatal("the version before the purge:", got)
	}
	next, purged := resolved(t, s, "v.md", v2.Revision)
	if !purged || readAt(t, s, "v.md", next) != string(noteWithID(id, "[purged]\n")) {
		t.Fatal("the purged version:", purged, readAt(t, s, "v.md", next))
	}
	if next, purged := resolved(t, s, "v.md", v3.Revision); purged || readAt(t, s, "v.md", next) != string(noteWithID(id, "Clean again.\n")) {
		t.Fatal("the version after:", purged)
	}
	if _, _, err := s.ResolvePurgedRevision(ctx, "v.md", strings.Repeat("f", 40)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("an unknown revision:", err)
	}
	// Only a note purge removes files: a path the revision never had is not
	// one a purge removed.
	if _, purged, err := s.ResolvePurgedRevision(ctx, "never.md", v2.Revision); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a path the revision never had:", purged, err)
	}
}

// Content another notebook of the org stores too stays stored for it, and
// the plan reports that notebook: a note purge among the files that keep
// what it removes, a text purge among the places its text also is.
func TestPurgePlanReportsContentAnotherNotebookShares(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	content := noteWithID(id, "Key "+secret+".\n")
	save(t, s, "a.md", content, "")
	save(t, s, "b.md", []byte("Unrelated.\n"), "")
	if _, err := s.noteStore.Create(ctx, "other", fixtureAuthor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.noteStore.Notebook("other").Apply(ctx, notestore.ChangeSet{Subject: "Copy", Author: fixtureAuthor, Files: []notestore.FileChange{{Path: "copy.md", Content: content}}}); err != nil {
		t.Fatal(err)
	}
	want := []PurgeOccurrence{{Path: "other", Kind: "notebook", Versions: 1}}
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, nil)
	if err != nil || !slices.Equal(plan.Elsewhere, want) {
		t.Fatalf("text plan %+v %v", plan, err)
	}
	if plan, err = s.PreparePurge(ctx, PurgeScope{Kind: PurgeNote, Path: "a.md"}, nil); err != nil || !slices.Equal(plan.Shared, want) {
		t.Fatalf("note plan %+v %v", plan, err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := s.noteStore.Notebook("other").Read(ctx, gitobj.ID{}, "copy.md"); err != nil || !bytes.Equal(raw, content) {
		t.Fatal("the other notebook's copy:", err)
	}
}

// A purge of text that only pending live edits hold rewrites no revision,
// but resets the live document and records the purge: who ran it, why and
// what it covered.
func TestPurgeOfTextOnlyLiveEditsHoldIsRecorded(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	doc := save(t, s, "a.md", []byte("Clean.\n"), "")
	f, err := s.notes.Stat(ctx, gitobj.ID{}, "a.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.notes.LiveAppend(ctx, f.EntryID, 0, []byte("typed "+secret), "ada"); err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RevisionsRewritten != 0 || !slices.Equal(plan.Live, []PurgeLive{{Path: "a.md", Pending: true}}) {
		t.Fatalf("plan %+v", plan)
	}
	result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "typed a key", nil)
	if err != nil || result.Rewritten != 0 || result.LiveReset != 1 || result.Revision != doc.Revision {
		t.Fatal(result, err)
	}
	record, err := s.notes.Purge(ctx, result.Purge)
	if err != nil || record.Actor != "owner" || record.Reason != "typed a key" || record.FromSeq != 0 {
		t.Fatalf("record %+v %v", record, err)
	}
	var summary purgeSummary
	if json.Unmarshal(record.Scope, &summary) != nil || summary.Kind != PurgeText || summary.Live != 1 {
		t.Fatal("scope:", string(record.Scope))
	}
	if st, err := s.notes.LiveState(ctx, f.EntryID); err != nil || st.Epoch != 1 || len(st.Updates) != 0 {
		t.Fatalf("live %+v %v", st, err)
	}
	if bytes.Contains(storedBytes(t, testEnv(t, s).db, testEnv(t, s).blobs.Path()), []byte(secret)) {
		t.Fatal("the secret is still stored")
	}
}

func TestPurgeRefusesInvalidScopes(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	save(t, s, "a.md", []byte("x\n"), "")
	for _, c := range []struct {
		scope PurgeScope
		want  error
	}{
		{PurgeScope{}, ErrPurgeInvalid},
		{PurgeScope{Kind: PurgeText}, ErrPurgeInvalid},
		{PurgeScope{Kind: PurgeText, Text: "x", Pattern: "x"}, ErrPurgeInvalid},
		{PurgeScope{Kind: PurgeText, Pattern: "x*"}, ErrPurgeInvalid},
		{PurgeScope{Kind: PurgeText, Pattern: "("}, ErrPurgeInvalid},
		{PurgeScope{Kind: PurgeNote, Path: "missing.md"}, fs.ErrNotExist},
		{PurgeScope{Kind: PurgeNote, Path: trashIndexPath}, ErrForbidden},
		{PurgeScope{Kind: PurgeVersions, Path: "a.md"}, ErrPurgeInvalid},
		{PurgeScope{Kind: PurgeVersions, Path: "a.md", Versions: []string{"x"}}, ErrPurgeInvalid},
		{PurgeScope{Kind: PurgeText, Text: "x", Paths: []string{"missing.md"}}, fs.ErrNotExist},
		// What the placeholder holds would stay.
		{PurgeScope{Kind: PurgeText, Text: "purge"}, ErrPurgeInvalid},
		{PurgeScope{Kind: PurgeText, Pattern: "[a-z]+"}, ErrPurgeInvalid},
	} {
		if _, err := s.PreparePurge(ctx, c.scope, nil); !errors.Is(err, c.want) {
			t.Errorf("scope %+v: %v, want %v", c.scope, err, c.want)
		}
	}
	// Text the placeholder forms again with what follows it.
	save(t, s, "b.md", []byte("d]xx\n"), "")
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: "d]x"}, nil)
	if err != nil || plan.Blocked == "" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); !errors.Is(err, ErrPurgeBlocked) || !errors.Is(err, ErrPurgeInvalid) {
		t.Fatal("a blocked plan:", err)
	}
	if plan, err = s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: "absent"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); !errors.Is(err, ErrPurgeInvalid) {
		t.Fatal("a plan that removes nothing:", err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "", "leak", nil); !errors.Is(err, ErrPurgeInvalid) {
		t.Fatal("a purge without who runs it:", err)
	}
}

// The same plan always gets the same confirmation, even when two files the
// purge lists once had the same path.
func TestPurgePlansListInOneOrder(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	for i := range 6 {
		id := strings.Repeat(string(rune('a'+i)), 32)
		first := save(t, s, "p.md", noteWithID(id, secret+"\n"), "")
		commitFixture(t, s, notestore.FileChange{Path: "p.md"})
		save(t, s, "q.md", []byte(secret+" "+first.Revision+"\n"), "")
		commitFixture(t, s, notestore.FileChange{Path: "q.md"})
	}
	var confirmation string
	for range 20 {
		plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Notes) != 12 {
			t.Fatal("notes:", plan.Notes)
		}
		if confirmation != "" && plan.Confirmation != confirmation {
			t.Fatal("the same plan got two confirmations")
		}
		confirmation = plan.Confirmation
	}
}

// With the instance's exporter, the plan says the export will be
// force-pushed, and the purge rebuilds it: the branch names the rewritten
// head, the purge flag authorizes replacing the remote branch, and no object
// holds the text.
func TestExecutePurgeRebuildsTheGitExport(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	first := save(t, s, "a.md", []byte("Key "+secret+".\n"), "")
	save(t, s, "b.md", []byte("Kept.\n"), "")
	ex, err := gitexport.New(s.noteStore, t.TempDir(), gitexport.Options{Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	old, err := ex.Sync(ctx, s.notebook)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, ex)
	if err != nil || !plan.ExportForcePush {
		t.Fatal("force push:", plan.ExportForcePush, err)
	}
	if unexported, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, nil); err != nil || unexported.ExportForcePush {
		t.Fatal("a plan without an exporter:", unexported.ExportForcePush, err)
	}
	result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", ex)
	if err != nil || !result.Export {
		t.Fatal(result, err)
	}
	repo := ex.Path(s.notebook)
	if branch, err := ex.Branch(s.notebook); err != nil || branch.String() != result.Revision {
		t.Fatal("branch:", branch, err)
	}
	flag, ok, err := gitexport.ReadPurge(repo)
	if err != nil || !ok || flag.Purge != result.Purge || flag.Head.String() != result.Revision || !slices.Contains(flag.Replaces, old.Commit) {
		t.Fatalf("flag %+v %v %v", flag, ok, err)
	}
	cmd := exec.Command("git", "--git-dir="+repo, "cat-file", "--batch-all-objects", "--unordered", "--batch")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	objects, err := cmd.Output()
	if err != nil || bytes.Contains(objects, []byte(secret)) || !bytes.Contains(objects, []byte("Key [purged].")) {
		t.Fatal("export objects:", err)
	}
	if next, _ := resolved(t, s, "a.md", first.Revision); readAt(t, s, "a.md", next) != "Key [purged].\n" {
		t.Fatal("the rewritten version")
	}
}
