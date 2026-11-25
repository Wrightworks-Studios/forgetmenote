package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

type checkLine struct {
	Notebook       string   `json:"notebook"`
	Catalog        bool     `json:"catalog"`
	Compared       bool     `json:"compared"`
	Status         string   `json:"status"`
	Head           string   `json:"head"`
	StoreHead      string   `json:"storeHead"`
	LaterRevisions int      `json:"laterRevisions"`
	Commits        int      `json:"commits"`
	Export         string   `json:"export"`
	Files          int      `json:"files"`
	Refused        int      `json:"refused"`
	Notes          int      `json:"notes"`
	NotImported    []string `json:"notImported"`
	Retired        string   `json:"retired"`
	Error          string   `json:"error"`
}

// checkSummary is check's last line: it counts each status (summary), as
// retire's last line does, and the notebooks compared, passed, failed and
// skipped (counts).
type checkSummary struct {
	Summary   map[string]int `json:"summary"`
	Counts    map[string]int `json:"counts"`
	Notebooks int            `json:"notebooks"`
	Passed    bool           `json:"passed"`
}

// counted is whether summary counts compared, passed, failed and skipped
// notebooks.
func (summary checkSummary) counted(compared, passed, failed, skipped int) bool {
	want := map[string]int{"compared": compared, "passed": passed, "failed": failed, "skipped": skipped}
	return maps.Equal(summary.Counts, want)
}

// check runs admin storage check and returns each notebook's line, the
// summary and the command's error.
func check(t *testing.T, data string, args ...string) (map[string]checkLine, checkSummary, error) {
	t.Helper()
	var out bytes.Buffer
	err := runAdmin(append([]string{"storage", "check", "--data", data}, args...), nil, &out)
	lines := map[string]checkLine{}
	var summary checkSummary
	for _, raw := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var l checkLine
		if e := json.Unmarshal(raw, &l); e != nil {
			t.Fatal(e, string(raw))
		}
		if l.Notebook == "" {
			if e := json.Unmarshal(raw, &summary); e != nil {
				t.Fatal(e, string(raw))
			}
			continue
		}
		lines[l.Notebook] = l
	}
	return lines, summary, err
}

// commitFiles writes files into the working tree work and commits them with
// subject.
func commitFiles(t *testing.T, work, subject string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(work, path)), 0o700)
		if err := os.WriteFile(filepath.Join(work, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cutoverGitRun(t, work, "add", "--all")
	cutoverGitRun(t, work, "commit", "--quiet", "--allow-empty", "-m", subject)
}

func noteWith(id, title, body string) string {
	return "---\nfmn-id: " + id + "\ntitle: " + title + "\n---\n\n" + body
}

// richHistory adds to the notebook repository work the shapes whose
// history the check must follow as the import does: a note without an
// fmn-id moved to Trash and back with edits (a rename Git detects), a note
// rewritten under a new name with its fmn-id (a rename only the identity
// shows), a note Git pairs with a deleted one although it declares another
// identity, a note rewritten as it moved (only the Move subject ties it), a
// branch that renames a note main deletes, merged in, more revisions of one note than its history lists,
// and files the read path refuses: a hidden file and an executable.
func richHistory(t *testing.T, work string) {
	t.Helper()
	plan := "# Plan\n\n" + strings.Repeat("A step of the plan.\n", 12)
	commitFiles(t, work, "Update plan.md", map[string]string{"plan.md": plan})
	os.MkdirAll(filepath.Join(work, "Trash"), 0o700)
	cutoverGitRun(t, work, "mv", "plan.md", "Trash/plan.md")
	commitFiles(t, work, `Move "plan.md" to "Trash/plan.md"`, map[string]string{"Trash/plan.md": plan + "Trashed.\n"})
	cutoverGitRun(t, work, "mv", "Trash/plan.md", "plan.md")
	commitFiles(t, work, "Restore note from Trash", map[string]string{"plan.md": plan + "Restored.\n"})

	commitFiles(t, work, "Update draft.md", map[string]string{"draft.md": noteWith("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "Draft", "A first draft.\n")})
	cutoverGitRun(t, work, "rm", "--quiet", "draft.md")
	commitFiles(t, work, "Rewrite the draft", map[string]string{"final.md": noteWith("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "Final", strings.Repeat("Entirely new text.\n", 20))})

	body := strings.Repeat("Shared paragraph of the old note.\n", 20)
	commitFiles(t, work, "Update old.md", map[string]string{"old.md": noteWith("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "Old", body)})
	cutoverGitRun(t, work, "rm", "--quiet", "old.md")
	commitFiles(t, work, "Replace old.md", map[string]string{"new.md": noteWith("cccccccccccccccccccccccccccccccc", "New", body+"One more line.\n")})

	commitFiles(t, work, "Update notes.md", map[string]string{"notes.md": "# Notes\n\nShort.\n"})
	cutoverGitRun(t, work, "rm", "--quiet", "notes.md")
	commitFiles(t, work, `Move "notes.md" to "Archive/notes.md"`, map[string]string{"Archive/notes.md": strings.Repeat("Rewritten while it moved.\n", 10)})

	// The branch renames shared.md, which main deletes meanwhile: the import
	// takes the rename from the files of the branch's own parent.
	commitFiles(t, work, "Update shared.md", map[string]string{"shared.md": "# Shared\n\nA note without an identity, renamed on a branch.\n"})
	cutoverGitRun(t, work, "checkout", "--quiet", "-b", "side")
	cutoverGitRun(t, work, "mv", "shared.md", "kept.md")
	commitFiles(t, work, "Rename shared.md", map[string]string{"side.md": "# Side\n"})
	cutoverGitRun(t, work, "checkout", "--quiet", "main")
	cutoverGitRun(t, work, "rm", "--quiet", "shared.md")
	commitFiles(t, work, "Update main.md", map[string]string{"main.md": "# Main\n"})
	merge := exec.Command("git", "-C", work, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "merge", "--quiet", "--no-ff", "side")
	merge.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=ForgetMeNote", "GIT_COMMITTER_EMAIL=forgetmenote@localhost")
	if out, err := merge.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("rename/delete")) {
		t.Fatalf("merging the branch: %v\n%s", err, out)
	}
	commitFiles(t, work, "Merge side", nil) // keeps kept.md
	cutoverGitRun(t, work, "branch", "--quiet", "-D", "side")

	for i := range 55 {
		commitFiles(t, work, "Update journal.md", map[string]string{"journal.md": fmt.Sprintf("# Journal\n\nEntry %d.\n", i)})
	}
	commitFiles(t, work, "Add a hidden file", map[string]string{".hidden.md": "hidden\n"})
	os.MkdirAll(filepath.Join(work, "assets"), 0o700)
	if err := os.WriteFile(filepath.Join(work, "assets", "tool.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	commitFiles(t, work, "Add an executable", nil)
}

// instanceState fingerprints everything a check could change: every file
// under data, and the rows of the notes store and the control plane.
func instanceState(t *testing.T, db *pg.DB, data string) string {
	t.Helper()
	var state strings.Builder
	err := filepath.WalkDir(data, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&state, "%s %v %d", path, info.Mode(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			state.WriteString(" " + hex.EncodeToString(sum[:]))
		}
		state.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"notebook_heads", "revisions", "revision_changes", "entries", "git_trees", "file_versions", "live_docs", "live_updates", "documents", "operation_receipts", "notebook_authority", "leases"} {
		var sum *string
		if err := db.Pool.QueryRow(context.Background(), `SELECT md5(string_agg(t::text, ',' ORDER BY t::text)) FROM `+table+` t`).Scan(&sum); err != nil {
			t.Fatal(table, err)
		}
		if sum != nil {
			fmt.Fprintf(&state, "%s %s\n", table, *sum)
		}
	}
	return state.String()
}

// Right after a correct cutover, check passes for every notebook in the
// catalog and on disk, following each note's history as the import does,
// and changes nothing; it runs beside a serving gateway, which the other
// cutover commands refuse. Retire then removes the previous release's files
// at once, the new release starts without them, and check reports them
// retired and fails, as it has nothing left to compare.
func TestStorageCheckPassesACorrectCutoverThenRetireRemovesTheOldFiles(t *testing.T) {
	data, file := cutoverInstance(t)
	db := instanceDatabase(t, file)
	heads := map[string]string{}
	previousNotebook(t, data, "default")
	richHistory(t, filepath.Join(data, "projects", "default", "workspace"))
	work := filepath.Join(data, "projects", "default", "workspace")
	heads["default"] = cutoverGitRun(t, work, "rev-parse", "HEAD")
	commits, _ := strconv.Atoi(cutoverGitRun(t, work, "rev-list", "--count", "HEAD"))
	heads["loose"] = previousNotebook(t, data, "loose") // on disk, not in the catalog
	os.MkdirAll(filepath.Join(data, "projects", "empty"), 0o700)
	importAll(t, data)
	var out bytes.Buffer
	if err := runAdmin([]string{"storage", "export", "--data", data}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}

	before := instanceState(t, db, data)
	lines, summary, err := check(t, data)
	if err != nil {
		t.Fatal(err, lines)
	}
	for id, head := range heads {
		l := lines[id]
		if l.Status != "passed" || !l.Compared || l.Error != "" || l.Head != head || l.Export != filepath.Join(data, "exports", id+".git") || l.Catalog != (id == "default") {
			t.Fatalf("%s: %+v", id, l)
		}
	}
	// Files: idea.md, plan.md, final.md, new.md, Archive/notes.md, kept.md,
	// side.md, main.md, journal.md, .hidden.md and assets/tool.sh; the last
	// two refused.
	if l := lines["default"]; l.Files != 9 || l.Refused != 2 || l.Notes != 9 || l.Commits != commits || commits != 73 {
		t.Fatalf("what default's check compared: %+v", l)
	}
	if l := lines["empty"]; l.Status != "skipped: no notebook repository" || l.Compared || l.Error != "" {
		t.Fatalf("a directory without a repository: %+v", l)
	}
	if !summary.Passed || summary.Notebooks != 3 || !summary.counted(2, 2, 0, 1) || summary.Summary["passed"] != 2 || summary.Summary["skipped: no notebook repository"] != 1 {
		t.Fatalf("summary %+v", summary)
	}
	// A notebook named that does not exist fails, and so does a check that
	// compared no notebook at all.
	lines, summary, err = check(t, data, "-notebook", "nosuch")
	if l := lines["nosuch"]; err == nil || l.Status != "failed" || l.Compared ||
		l.Error != "no such notebook: the catalog does not list it, and neither the notes store nor the data directory holds it" || summary.Passed || !summary.counted(0, 0, 1, 0) {
		t.Fatal("check of a notebook that does not exist:", err, l, summary)
	}
	lines, summary, err = check(t, data, "-notebook", "empty")
	if l := lines["empty"]; err == nil || !strings.Contains(err.Error(), "storage check compared no notebook: it found 1, and none has a previous release's repository to compare with (1 skipped: no notebook repository), so it proves nothing") ||
		l.Status != "skipped: no notebook repository" || summary.Passed || !summary.counted(0, 0, 0, 1) {
		t.Fatal("check that compared no notebook:", err, l, summary)
	}
	if after := instanceState(t, db, data); after != before {
		t.Fatal("check changed the instance")
	}

	// Beside the new release, serving: the catalog's and the exporter's
	// directory locks are held and a gateway is present.
	r, _, err := serve(t, data, file)
	if err != nil {
		t.Fatal(err)
	}
	leave, err := db.KeepPresence()
	if err != nil {
		t.Fatal(err)
	}
	if lines, _, err := check(t, data, "-notebook", "default"); err != nil || lines["default"].Status != "passed" {
		t.Fatal("check beside a serving gateway:", err, lines["default"])
	}
	if _, _, err := retire(t, data, "-yes"); err == nil || !strings.Contains(err.Error(), "gateways are serving") {
		t.Fatal("retire beside a serving gateway:", err)
	}
	leave()
	r.Close()

	retired, _, err := retire(t, data, "-yes")
	if err != nil {
		t.Fatal(err, retired)
	}
	for _, id := range []string{"default", "loose"} {
		if l := retired[id]; l.Status != "removed" {
			t.Fatalf("%s: %+v", id, l)
		}
		if left := previousFiles(data, id); len(left) != 0 {
			t.Fatalf("%s still has %v", id, left)
		}
	}
	if _, store, err := serve(t, data, file); err != nil {
		t.Fatal("serving the retired notebooks:", err)
	} else if doc, err := store.Read(t.Context(), "journal.md", ""); err != nil || !strings.Contains(string(doc.Content), "Entry 54.") {
		t.Fatalf("journal.md after retire: %q %v", doc.Content, err)
	}
	// Retire removed the repositories check compares with: it says so for
	// each notebook, and fails, as it compared nothing.
	lines, summary, err = check(t, data)
	if err == nil || !strings.Contains(err.Error(), "storage check compared no notebook: it found 3, and none has a previous release's repository to compare with (1 skipped: no notebook repository, 2 skipped: retired), so it proves nothing") {
		t.Fatal("check after retire:", err)
	}
	for _, id := range []string{"default", "loose"} {
		if l := lines[id]; l.Status != "skipped: retired" || l.Retired == "" || l.Compared || l.Error != "" {
			t.Fatalf("%s after retire: %+v", id, l)
		}
	}
	if summary.Passed || !summary.counted(0, 0, 0, 3) {
		t.Fatalf("summary after retire %+v", summary)
	}

	// The storage v2 release before this one retired without recording it.
	// Check then takes each repository of a notebook the catalog lists for
	// one that went missing, and fails, as the runbook says.
	if _, err := db.Pool.Exec(t.Context(), `DELETE FROM documents WHERE kind = 'storage-retired'`); err != nil {
		t.Fatal(err)
	}
	lines, summary, err = check(t, data)
	if err == nil || err.Error() != "storage check failed for 1 of 3 notebooks" {
		t.Fatal("check after a retire that recorded nothing:", err)
	}
	missing := "its previous release repository, " + filepath.Join(data, "projects", "default", "workspace") + ", is missing, and retire never removed it"
	if l := lines["default"]; l.Status != "failed" || l.Retired != "" || !strings.HasPrefix(l.Error, missing) {
		t.Fatalf("default after a retire that recorded nothing: %+v", l)
	}
	for _, id := range []string{"loose", "empty"} {
		if l := lines[id]; l.Status != "skipped: no notebook repository" || l.Error != "" {
			t.Fatalf("%s after a retire that recorded nothing: %+v", id, l)
		}
	}
	if summary.Passed || !summary.counted(0, 0, 1, 2) || summary.Summary["failed"] != 1 || summary.Summary["skipped: no notebook repository"] != 2 {
		t.Fatalf("summary after a retire that recorded nothing %+v", summary)
	}
}

// Check never passes without comparing a notebook with its repository: not
// in a data directory without the repositories, whose notebooks the catalog
// lists and the store holds although retire never removed them, nor for a
// cataloged notebook whose repository went missing. A -notebook that does
// not exist, and one directory without a repository, fail in the check of
// the correct cutover above.
func TestStorageCheckFailsWhenItComparesNothing(t *testing.T) {
	data, _ := cutoverInstance(t)
	heads := map[string]string{"default": previousNotebook(t, data, "default")}
	os.MkdirAll(filepath.Join(data, "projects", "empty"), 0o700)
	importAll(t, data)
	var out bytes.Buffer
	if err := runAdmin([]string{"storage", "export", "--data", data}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}
	if lines, summary, err := check(t, data); err != nil || lines["default"].Status != "passed" || !summary.Passed || !summary.counted(1, 1, 0, 1) {
		t.Fatal("the check of the cutover:", err, lines, summary)
	}

	missing := "its previous release repository, %s, is missing, and retire never removed it"
	other := t.TempDir()
	lines, summary, err := check(t, other)
	if err == nil || !strings.Contains(err.Error(), "storage check failed for 1 of 1 notebooks") {
		t.Fatal("a data directory without projects/:", err)
	}
	if l := lines["default"]; l.Status != "failed" || l.Compared || !strings.Contains(l.Error, fmt.Sprintf(missing, filepath.Join(other, "projects", "default", "workspace"))) {
		t.Fatalf("default in a data directory without projects/: %+v", l)
	}
	if summary.Passed || !summary.counted(0, 0, 1, 0) {
		t.Fatalf("summary %+v", summary)
	}

	// The repository went missing before any retire: not one retire removed.
	work := filepath.Join(data, "projects", "default", "workspace")
	if err := os.Rename(work, filepath.Join(data, "moved")); err != nil {
		t.Fatal(err)
	}
	lines, summary, err = check(t, data)
	if err == nil || summary.Passed || !summary.counted(0, 0, 1, 1) {
		t.Fatal("a missing repository:", err, summary)
	}
	if l := lines["default"]; l.Status != "failed" || l.Retired != "" || !strings.Contains(l.Error, fmt.Sprintf(missing, work)) {
		t.Fatalf("a missing repository: %+v", l)
	}
	if err := os.Rename(filepath.Join(data, "moved"), work); err != nil {
		t.Fatal(err)
	}
	if lines, _, err := check(t, data, "-notebook", "default"); err != nil || lines["default"].Head != heads["default"] {
		t.Fatal("the repository back:", err, lines)
	}
}

// Each kind of difference between a notebook's repository and what the new
// release serves fails the check with a message that names it, deep in a
// note's history too. A notebook that took revisions after the cutover is
// proved at Git HEAD, with an export that has not caught up with it yet.
func TestStorageCheckFailsOnEachDifference(t *testing.T) {
	data, file := cutoverInstance(t)
	db := instanceDatabase(t, file)
	ids := []string{"default", "ahead", "other", "lost", "byte", "model", "history", "deep", "behind", "unexported", "dirty",
		"saved", "savedhistory", "savedunexported", "savedbehind", "savedforked"}
	heads := map[string]string{}
	for _, id := range ids {
		heads[id] = previousNotebook(t, data, id)
	}
	commitFiles(t, filepath.Join(data, "projects", "model", "workspace"), "Update other.md", map[string]string{"other.md": "# Another note\n"})
	heads["model"] = cutoverGitRun(t, filepath.Join(data, "projects", "model", "workspace"), "rev-parse", "HEAD")
	// deep: idea.md has more history entries than the server lists, and
	// than the first page of revisions the store's history walk reads.
	deep := filepath.Join(data, "projects", "deep", "workspace")
	deepAdded := cutoverGitRun(t, deep, "rev-parse", "HEAD~1")
	for i := range 70 {
		commitFiles(t, deep, "Update idea.md", map[string]string{"idea.md": noteWith("0123456789abcdef0123456789abcdef", "Idea", fmt.Sprintf("Revision %d.\n", i))})
	}
	heads["deep"] = cutoverGitRun(t, deep, "rev-parse", "HEAD")
	importAll(t, data)
	var out bytes.Buffer
	if err := runAdmin([]string{"storage", "export", "--data", data}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}
	project := func(id string) string { return filepath.Join(data, "projects", id) }
	ctx := context.Background()
	sql := func(query string, args ...any) {
		t.Helper()
		if tag, err := db.Pool.Exec(ctx, query, args...); err != nil || tag.RowsAffected() == 0 {
			t.Fatal(query, tag, err)
		}
	}
	hexID := func(id string) []byte {
		sha, err := gitobj.ParseID(id)
		if err != nil {
			t.Fatal(err)
		}
		return sha[:]
	}

	// ahead: the previous release committed after the import.
	commitFiles(t, filepath.Join(project("ahead"), "workspace"), "Update late.md", map[string]string{"late.md": "late\n"})
	// other: the repository's branch moved to another history.
	other := filepath.Join(project("other"), "workspace")
	cutoverGitRun(t, other, "update-ref", "refs/heads/main", cutoverGitRun(t, other, "commit-tree", "-m", "Elsewhere", "HEAD^{tree}"))
	// lost: the store lost a commit of the history.
	lost := cutoverGitRun(t, filepath.Join(project("lost"), "workspace"), "rev-parse", "HEAD~1")
	sql(`DELETE FROM revisions WHERE notebook = 'lost' AND commit_sha = $1`, hexID(lost))
	// byte: a byte of idea.md at the head changed in the store.
	ideaBlob := cutoverGitRun(t, filepath.Join(project("byte"), "workspace"), "rev-parse", "HEAD:idea.md")
	sql(`UPDATE file_versions SET inline = overlay(inline PLACING 'X'::bytea FROM 60) WHERE blob_sha = $1`, hexID(ideaBlob))
	// model: the store's files at its head name other.md's content for idea.md.
	otherBlob := cutoverGitRun(t, filepath.Join(project("model"), "workspace"), "rev-parse", "HEAD:other.md")
	sql(`UPDATE entries SET blob_sha = $1 WHERE notebook = 'model' AND path = 'idea.md'`, hexID(otherBlob))
	// history: the store lost the change that added Inbox/idea.md.
	sql(`DELETE FROM revision_changes WHERE notebook = 'history' AND path = 'Inbox/idea.md' AND op = 'add'`)
	// deep: the same, its 72nd history entry, older than the newest 50.
	sql(`DELETE FROM revision_changes WHERE notebook = 'deep' AND path = 'Inbox/idea.md' AND op = 'add'`)
	// behind: the export lags one commit.
	cutoverGitRun(t, filepath.Join(data, "exports", "behind.git"), "update-ref", "refs/heads/main", heads["behind"]+"~1")
	// unexported: no export.
	if err := os.RemoveAll(filepath.Join(data, "exports", "unexported.git")); err != nil {
		t.Fatal(err)
	}
	// dirty: a file appeared in the old working tree.
	os.WriteFile(filepath.Join(project("dirty"), "workspace", "draft.md"), []byte("unsaved\n"), 0o600)
	// saved and the rest: the notebook took two revisions, which its export
	// does not have yet; the saved ones then differ as above at Git HEAD.
	withNotes(t, data, db, func(notes *notestore.Store) {
		ada := gitobj.Signature{Name: "Ada", Email: "ada@example.com", When: 1700000100, TZ: "+0000"}
		for _, id := range []string{"saved", "savedhistory", "savedunexported", "savedbehind", "savedforked"} {
			for _, content := range []string{"# Later\n", "# Later still\n"} {
				if _, err := notes.Notebook(id).Apply(ctx, notestore.ChangeSet{Subject: "Update later.md", Author: ada,
					Files: []notestore.FileChange{{Path: "later.md", Content: []byte(content)}}}); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	sql(`DELETE FROM revision_changes WHERE notebook = 'savedhistory' AND path = 'Inbox/idea.md' AND op = 'add'`)
	if err := os.RemoveAll(filepath.Join(data, "exports", "savedunexported.git")); err != nil {
		t.Fatal(err)
	}
	cutoverGitRun(t, filepath.Join(data, "exports", "savedbehind.git"), "update-ref", "refs/heads/main", heads["savedbehind"]+"~1")
	// savedforked: the export's branch holds HEAD but is no revision of the
	// notebook.
	forked := filepath.Join(data, "exports", "savedforked.git")
	fork := cutoverGitRun(t, forked, "commit-tree", "-p", heads["savedforked"], "-m", "Elsewhere", heads["savedforked"]+"^{tree}")
	cutoverGitRun(t, forked, "update-ref", "refs/heads/main", fork)

	before := instanceState(t, db, data)
	lines, summary, err := check(t, data)
	if err == nil || !strings.Contains(err.Error(), "storage check failed for 14 of 16 notebooks") {
		t.Fatal("a check with differences:", err)
	}
	if summary.Passed || !summary.counted(16, 2, 14, 0) || summary.Summary["passed"] != 1 || summary.Summary["passed at Git HEAD"] != 1 {
		t.Fatalf("summary %+v", summary)
	}
	if l := lines["default"]; l.Status != "passed" {
		t.Fatalf("default: %+v", l)
	}
	if l := lines["saved"]; l.Status != "passed at Git HEAD" || l.Head != heads["saved"] || l.StoreHead == "" || l.StoreHead == l.Head || l.LaterRevisions != 2 || l.Files != 1 || l.Notes != 1 {
		t.Fatalf("saved: %+v", l)
	}
	for id, want := range map[string]string{
		"ahead":           "history: Git HEAD " + cutoverGitRun(t, filepath.Join(project("ahead"), "workspace"), "rev-parse", "HEAD") + " has 1 commit the notes store lacks; its head is " + heads["ahead"],
		"other":           "history: the notes store's head " + heads["other"] + " is not Git HEAD",
		"lost":            "history: the notebook does not match its Git history: Git object " + lost + " is not in the notebook",
		"byte":            "history: content " + ideaBlob + " is corrupt",
		"model":           "history: the notes store cannot read the notebook's head: notebook model: entries disagree with tree at idea.md",
		"history":         "note histories: idea.md has 1 history entry in the store and 2 in Git, and entry 2 is nothing in the store but " + cutoverGitRun(t, filepath.Join(project("history"), "workspace"), "rev-parse", "HEAD~1") + " (Inbox/idea.md) in Git",
		"deep":            "note histories: idea.md has 71 history entries in the store and 72 in Git, and entry 72 is nothing in the store but " + deepAdded + " (Inbox/idea.md) in Git",
		"behind":          "export: the export is 1 commit behind Git HEAD " + heads["behind"],
		"unexported":      "export: " + filepath.Join(data, "exports", "unexported.git") + " does not exist",
		"dirty":           "the notebook repository has uncommitted changes",
		"savedhistory":    "note histories: idea.md has 1 history entry in the store and 2 in Git, and entry 2 is nothing in the store but " + cutoverGitRun(t, filepath.Join(project("savedhistory"), "workspace"), "rev-parse", "HEAD~1") + " (Inbox/idea.md) in Git",
		"savedunexported": "export: " + filepath.Join(data, "exports", "savedunexported.git") + " does not exist",
		"savedbehind":     "export: the export is 1 commit behind Git HEAD " + heads["savedbehind"],
		"savedforked":     "export: the notebook does not match its Git history: the export's branch is at " + fork + ", not ",
	} {
		if l := lines[id]; l.Status != "failed" || !strings.Contains(l.Error, want) {
			t.Fatalf("%s: %+v\nwant %q", id, l, want)
		}
	}
	if after := instanceState(t, db, data); after != before {
		t.Fatal("a failing check changed the instance")
	}
}

// The file comparison reports a file whose served bytes are not the blob Git
// HEAD's tree names, and a file the read path refuses although the previous
// release served it; it counts what both refuse.
func TestStorageCheckComparesServedBytesWithGit(t *testing.T) {
	data, file := cutoverInstance(t)
	db := instanceDatabase(t, file)
	work := filepath.Join(data, "projects", "default", "workspace")
	previousNotebook(t, data, "default")
	commitFiles(t, work, "Add a hidden file", map[string]string{".hidden.md": "hidden\n", "other.md": "# Other\n"})
	importAll(t, data)
	withNotes(t, data, db, func(notes *notestore.Store) {
		ctx := t.Context()
		nb := notes.Notebook("default")
		head, err := nb.Head(ctx)
		if err != nil {
			t.Fatal(err)
		}
		s, err := workspace.OpenDB(ctx, notes, "default", filepath.Join(data, "projects", "default"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		files, err := gitFiles(ctx, filepath.Join(work, ".git"), head.Commit)
		if err != nil || len(files) != 3 {
			t.Fatal(files, err)
		}
		served, refused, wrong, err := checkServedFiles(ctx, nb, s, files, head.Commit, true)
		if err != nil || served != 2 || refused != 1 || len(wrong) != 0 {
			t.Fatal("the files as imported:", served, refused, wrong, err)
		}
		// Git HEAD names other bytes for idea.md, and other.md at a path the
		// read path refuses.
		for i, f := range files {
			switch f.path {
			case "idea.md":
				files[i].blob = gitobj.BlobID([]byte("Other bytes.\n"))
			case "other.md":
				files[i].path = "missing.md"
			}
		}
		served, refused, wrong, err = checkServedFiles(ctx, nb, s, files, head.Commit, true)
		if err != nil || served != 0 || refused != 1 || len(wrong) != 1 {
			t.Fatal("the files compared with another tree:", served, refused, wrong, err)
		}
		for _, want := range []string{"files: ", "idea.md is blob ", "idea.md reads as ", "bytes that differ from Git HEAD's", "missing.md is not in the store's files at its head", "missing.md does not read", "other.md is in the store's files at its head but not in Git HEAD's tree"} {
			if !strings.Contains(wrong[0], want) {
				t.Fatalf("%q lacks %q", wrong[0], want)
			}
		}
	})
}

// A repository older than the one the import read, from an older copy of
// the data directory or a HEAD moved back, holds only commits the store
// holds, as does the repository of a notebook that took revisions after the
// start. Check tells the two apart by the commit the import recorded, and
// fails for the older one; without that record, as an import by an earlier
// build left, it cannot, and fails too.
func TestStorageCheckFailsForARepositoryOlderThanTheImport(t *testing.T) {
	data, file := cutoverInstance(t)
	db := instanceDatabase(t, file)
	previousNotebook(t, data, "default")
	work := filepath.Join(data, "projects", "default", "workspace")
	commitFiles(t, work, "Update keep.md", map[string]string{"keep.md": "# Keep\n"})
	imported := cutoverGitRun(t, work, "rev-parse", "HEAD")
	importAll(t, data)
	var out bytes.Buffer
	if err := runAdmin([]string{"storage", "export", "--data", data}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}

	// The repository moved back one commit, and no one wrote after the start.
	cutoverGitRun(t, work, "reset", "--quiet", "--hard", "HEAD~1")
	cutoverGitRun(t, work, "reflog", "expire", "--expire=now", "--all")
	older := cutoverGitRun(t, work, "rev-parse", "HEAD")
	lines, summary, err := check(t, data)
	want := "history: Git HEAD " + older + " is not " + imported + ", the commit the import imported"
	if err == nil || summary.Passed || !summary.counted(1, 0, 1, 0) {
		t.Fatal("an older repository:", err, lines, summary)
	}
	if l := lines["default"]; l.Status != "failed" || !strings.Contains(l.Error, want) {
		t.Fatalf("an older repository: %+v\nwant %q", l, want)
	}

	// Back at the imported commit, with a revision written after the start.
	cutoverGitRun(t, work, "reset", "--quiet", "--hard", imported)
	withNotes(t, data, db, func(notes *notestore.Store) {
		ada := gitobj.Signature{Name: "Ada", Email: "ada@example.com", When: 1700000100, TZ: "+0000"}
		if _, err := notes.Notebook("default").Apply(t.Context(), notestore.ChangeSet{Subject: "Update later.md", Author: ada,
			Files: []notestore.FileChange{{Path: "later.md", Content: []byte("# Later\n")}}}); err != nil {
			t.Fatal(err)
		}
	})
	if lines, summary, err := check(t, data); err != nil || lines["default"].Status != "passed at Git HEAD" || lines["default"].LaterRevisions != 1 || !summary.Passed {
		t.Fatal("the imported repository with a later revision:", err, lines, summary)
	}

	// An import by an earlier build recorded no commit.
	if _, err := db.Pool.Exec(t.Context(), `DELETE FROM documents WHERE kind = 'storage-imported'`); err != nil {
		t.Fatal(err)
	}
	lines, summary, err = check(t, data)
	if l := lines["default"]; err == nil || summary.Passed || l.Status != "failed" || !strings.Contains(l.Error, "history: the import recorded no commit it imported") {
		t.Fatalf("no record of the imported commit: %v %+v %+v", err, l, summary)
	}
}

// With no notebook in the catalog and no projects/ in the data directory,
// check has nothing to compare, and fails.
func TestStorageCheckFailsWithNoNotebookAtAll(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	databaseURLFile(t, true)
	data := t.TempDir()
	lines, summary, err := check(t, data)
	if err == nil || !strings.Contains(err.Error(), "storage check compared no notebook: it found none, as the catalog lists none and the data directory's projects/ holds none") {
		t.Fatal("no notebook at all:", err)
	}
	if len(lines) != 0 || summary.Passed || summary.Notebooks != 0 || !summary.counted(0, 0, 0, 0) {
		t.Fatal("no notebook at all:", lines, summary)
	}
}
