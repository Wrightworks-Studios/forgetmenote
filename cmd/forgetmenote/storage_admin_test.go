package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/workspace"
)

// cutoverGitRun runs Git for a test without user or system configuration,
// and without automatic maintenance, which newer Git runs in the background
// and which would repack the repository under the command a test runs next.
func cutoverGitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "commit.gpgsign=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C",
		"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_COMMITTER_NAME=ForgetMeNote", "GIT_COMMITTER_EMAIL=forgetmenote@localhost",
		"GIT_AUTHOR_DATE=1700000000 +0100", "GIT_COMMITTER_DATE=1700000000 +0100")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// previousNotebook writes a notebook as the previous release stored it: a
// Git working tree under data/projects/ID/workspace.
func previousNotebook(t *testing.T, data, id string) string {
	t.Helper()
	work := filepath.Join(data, "projects", id, "workspace")
	if err := os.MkdirAll(filepath.Join(work, "Inbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	cutoverGitRun(t, work, "init", "--quiet", "--initial-branch=main")
	cutoverGitRun(t, work, "commit", "--quiet", "--allow-empty", "-m", "Initialize notes notebook")
	note := "---\nfmn-id: 0123456789abcdef0123456789abcdef\ntitle: Idea\n---\n\nAn idea in " + id + ".\n"
	os.WriteFile(filepath.Join(work, "Inbox/idea.md"), []byte(note), 0o600)
	cutoverGitRun(t, work, "add", "--all")
	cutoverGitRun(t, work, "commit", "--quiet", "-m", "Update Inbox/idea.md", "-m", "Gitnotes-Source: mcp")
	cutoverGitRun(t, work, "mv", "Inbox/idea.md", "idea.md")
	cutoverGitRun(t, work, "commit", "--quiet", "-m", `Move "Inbox/idea.md" to "idea.md"`)
	return cutoverGitRun(t, work, "rev-parse", "HEAD")
}

type storageLine struct {
	Notebook    string   `json:"notebook"`
	Status      string   `json:"status"`
	Head        string   `json:"head"`
	Commits     int      `json:"commits"`
	Error       string   `json:"error"`
	NotImported []string `json:"notImported"`
	Repository  string   `json:"repository"`
}

func storageLines(t *testing.T, out *bytes.Buffer) map[string]storageLine {
	t.Helper()
	lines := map[string]storageLine{}
	for dec := json.NewDecoder(out); dec.More(); {
		var l storageLine
		if err := dec.Decode(&l); err != nil {
			t.Fatal(err)
		}
		lines[l.Notebook] = l
	}
	return lines
}

// The cutover imports every notebook of a stopped instance with its exact
// commits, verifies it, and never imports over a notebook the store has;
// then the export reproduces each notebook's original head.
func TestStorageCutoverImportsVerifiesAndExports(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	db := instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	heads := map[string]string{"alpha": previousNotebook(t, data, "alpha"), "beta": previousNotebook(t, data, "beta")}
	alpha := filepath.Join(data, "projects", "alpha", "workspace")
	cutoverGitRun(t, alpha, "branch", "draft", "HEAD~1")
	cutoverGitRun(t, alpha, "checkout", "--quiet", "--orphan", "stray")
	cutoverGitRun(t, alpha, "commit", "--quiet", "--allow-empty", "-m", "Unrelated")
	cutoverGitRun(t, alpha, "checkout", "--quiet", "main")
	os.MkdirAll(filepath.Join(data, "projects", "empty"), 0o700)

	var out bytes.Buffer
	if err := runAdmin([]string{"storage", "import", "--data", data}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}
	lines := storageLines(t, &out)
	for id, head := range heads {
		if l := lines[id]; l.Status != "imported" || l.Head != head || l.Commits != 3 || l.Error != "" {
			t.Fatalf("%s: %+v", id, l)
		}
	}
	if l := lines["alpha"]; strings.Join(l.NotImported, " ") != "refs/heads/stray" {
		t.Fatal("refs the import left out:", l.NotImported)
	}
	if lines["empty"].Status != "skipped: no notebook repository" {
		t.Fatal(lines["empty"])
	}
	blobs, err := blob.OpenDir(data)
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	store, err := notestore.Open(context.Background(), db, blobs, notestore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	idea, err := store.Notebook("alpha").Stat(context.Background(), gitobj.ID{}, "idea.md")
	if err != nil || idea.EntryID != "0123456789abcdef0123456789abcdef" {
		t.Fatal("the moved note keeps its fmn-id:", idea, err)
	}

	// Again: verified, not imported over.
	out.Reset()
	if err := runAdmin([]string{"storage", "import", "--data", data, "--notebook", "beta"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if l := storageLines(t, &out)["beta"]; l.Status != "already imported" || l.Head != heads["beta"] {
		t.Fatal(l)
	}
	out.Reset()
	if err := runAdmin([]string{"storage", "verify", "--data", data}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if l := storageLines(t, &out)["alpha"]; l.Status != "verified" || l.Head != heads["alpha"] {
		t.Fatal(l)
	}

	out.Reset()
	if err := runAdmin([]string{"storage", "export", "--data", data}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}
	lines = storageLines(t, &out)
	for id, head := range heads {
		l := lines[id]
		if l.Status != "exported" || l.Head != head || l.Repository != filepath.Join(data, "exports", id+".git") {
			t.Fatalf("%s: %+v", id, l)
		}
		if got := cutoverGitRun(t, l.Repository, "rev-parse", "main"); got != head {
			t.Fatalf("%s exported at %s, not %s", id, got, head)
		}
		cutoverGitRun(t, l.Repository, "fsck", "--strict", "--no-dangling")
	}
	if lines["empty"].Status != "skipped: not in the notebook store" {
		t.Fatal(lines["empty"])
	}

	// A new commit in the old repository no longer verifies, and is never
	// imported over the stored notebook.
	os.WriteFile(filepath.Join(alpha, "late.md"), []byte("late\n"), 0o600)
	cutoverGitRun(t, alpha, "add", "late.md")
	cutoverGitRun(t, alpha, "commit", "--quiet", "-m", "Late")
	out.Reset()
	if err := runAdmin([]string{"storage", "verify", "--data", data, "--notebook", "alpha"}, nil, &out); err == nil {
		t.Fatal("verified a notebook whose repository moved on")
	}
	out.Reset()
	if err := runAdmin([]string{"storage", "import", "--data", data, "--notebook", "alpha"}, nil, &out); err == nil || !strings.Contains(storageLines(t, &out)["alpha"].Error, "never imported over") {
		t.Fatal("imported over a stored notebook:", err)
	}
}

// A note without an fmn-id that was moved with edits, here to Trash and back,
// keeps one history after the cutover: each revision names where the note was
// then, as the previous release's history did, so it opens at that revision.
func TestStorageImportedHistoryFollowsRenamedNotes(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	db := instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	previousNotebook(t, data, "alpha")
	work := filepath.Join(data, "projects", "alpha", "workspace")
	plan := "# Plan\n\n" + strings.Repeat("A step of the plan.\n", 12)
	os.WriteFile(filepath.Join(work, "plan.md"), []byte(plan), 0o600)
	cutoverGitRun(t, work, "add", "plan.md")
	cutoverGitRun(t, work, "commit", "--quiet", "-m", "Update plan.md")
	os.MkdirAll(filepath.Join(work, "Trash"), 0o700)
	cutoverGitRun(t, work, "mv", "plan.md", "Trash/plan.md")
	os.WriteFile(filepath.Join(work, "Trash", "plan.md"), []byte(plan+"Trashed.\n"), 0o600)
	cutoverGitRun(t, work, "commit", "--quiet", "--all", "-m", `Move "plan.md" to "Trash/plan.md"`)
	cutoverGitRun(t, work, "mv", "Trash/plan.md", "plan.md")
	os.WriteFile(filepath.Join(work, "plan.md"), []byte(plan+"Restored.\n"), 0o600)
	cutoverGitRun(t, work, "commit", "--quiet", "--all", "-m", "Restore note from Trash")
	revisions := strings.Fields(cutoverGitRun(t, work, "log", "--format=%H", "-3"))

	var out bytes.Buffer
	if err := runAdmin([]string{"storage", "import", "--data", data}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}
	blobs, err := blob.OpenDir(data)
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	notes, err := notestore.Open(context.Background(), db, blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		t.Fatal(err)
	}
	defer notes.Close()
	s, err := workspace.OpenDB(context.Background(), notes, "alpha", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	history, err := s.History(context.Background(), "plan.md")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i, c := range history {
		got = append(got, c.Path)
		if i < len(revisions) && c.Revision != revisions[i] {
			t.Fatalf("history entry %d is %s, Git's %s", i, c.Revision, revisions[i])
		}
		if _, err := s.Read(context.Background(), c.Path, c.Revision); err != nil {
			t.Errorf("history entry %d names %s, which does not open at %s: %v", i, c.Path, c.Revision, err)
		}
	}
	if want := []string{"plan.md", "Trash/plan.md", "plan.md"}; !slices.Equal(got, want) {
		t.Fatalf("history paths %q, want %q", got, want)
	}
}

// previousLiveFiles copies into notebook id the live editing files the
// previous release (the Git backend, removed since) wrote for idea.md, as
// recorded in testdata/previous-live/ID: its collaboration snapshot, append
// log and receipts.
//   - opened: a live note nobody edited;
//   - checkpointed: two edits that release checkpointed into Git, whose
//     commit previousLiveFiles adds to the notebook's history;
//   - logged: one accepted edit in the append log of a version 2 snapshot;
//   - compacted: 129 edits, more than the log holds, so the snapshot itself
//     lists them as uncommitted;
//   - rolledback: one edit, then that release's rollback to the release
//     before it, which writes a version 0 snapshot;
//   - unknown: a note nobody edited, whose snapshot a test may rewrite.
func previousLiveFiles(t *testing.T, data, id string) {
	t.Helper()
	project := filepath.Join(data, "projects", id)
	if err := os.CopyFS(project, os.DirFS(filepath.Join("testdata", "previous-live", id))); err != nil {
		t.Fatal(err)
	}
	if id == "checkpointed" {
		work := filepath.Join(project, "workspace")
		note := "---\nfmn-id: 0123456789abcdef0123456789abcdef\ntitle: Idea\n---\n\nAn edit 1 in checkpointed.\n"
		if err := os.WriteFile(filepath.Join(work, "idea.md"), []byte(note), 0o600); err != nil {
			t.Fatal(err)
		}
		cutoverGitRun(t, work, "commit", "--quiet", "--all", "-m", "Checkpoint live edits to idea.md", "-m", "Gitnotes-Source: collaboration")
	}
}

// The import refuses a notebook whose old storage holds work its history
// lacks, and leaves nothing of it in the store; it refuses a running
// instance. Live edits are left as the previous release stores them.
func TestStorageImportRefusesUnsettledNotebooks(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	db := instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	refused := []string{"dirty", "journal", "logged", "compacted", "rolledback", "unknown"}
	for _, id := range append([]string{"opened", "checkpointed"}, refused...) {
		previousNotebook(t, data, id)
	}
	project := func(id string) string { return filepath.Join(data, "projects", id) }
	os.WriteFile(filepath.Join(project("dirty"), "workspace", "draft.md"), []byte("unsaved\n"), 0o600)
	os.WriteFile(filepath.Join(project("journal"), "write-journal.json"), []byte("{}"), 0o600)
	// A live note nobody edited, and live edits checkpointed into Git, are
	// settled; an empty append log left before its first snapshot is too.
	// Edits in an append log, edits a full log compacted into its snapshot
	// and a rolled back snapshot's edits are not.
	for _, id := range []string{"opened", "checkpointed", "logged", "compacted", "rolledback", "unknown"} {
		previousLiveFiles(t, data, id)
	}
	os.WriteFile(filepath.Join(project("opened"), "collaboration", strings.Repeat("0", 64)+".jsonl"), nil, 0o600)
	unknown, _ := filepath.Glob(filepath.Join(project("unknown"), "collaboration", "*.json"))
	if len(unknown) != 1 {
		t.Fatal("live snapshots:", unknown)
	}
	os.WriteFile(unknown[0], []byte(`{"version":3,"data":{"path":"idea.md"}}`), 0o600)
	for id, want := range map[string]string{"logged": `"sequence":1`, "compacted": `"uncommitted":["`, "rolledback": `"uncommitted":["`} {
		snapshots, _ := filepath.Glob(filepath.Join(project(id), "collaboration", "*.json*"))
		var all []byte
		for _, name := range snapshots {
			raw, _ := os.ReadFile(name)
			all = append(all, raw...)
		}
		if !bytes.Contains(all, []byte(want)) {
			t.Fatalf("%s does not store its edit as expected (%s): %s", id, want, all)
		}
	}

	var out bytes.Buffer
	err := runAdmin([]string{"storage", "import", "--data", data}, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "6 of 8") {
		t.Fatal("unsettled notebooks:", err, out.String())
	}
	lines := storageLines(t, &out)
	for id, want := range map[string]string{"dirty": "uncommitted changes", "journal": "interrupted write",
		"logged": "live edits are not checkpointed (idea.md)", "compacted": "live edits are not checkpointed (idea.md)",
		"rolledback": "live edits are not checkpointed (idea.md)", "unknown": "unknown collaboration storage version 3"} {
		if !strings.Contains(lines[id].Error, want) {
			t.Fatalf("%s: %+v, want %q", id, lines[id], want)
		}
	}
	for _, id := range []string{"opened", "checkpointed"} {
		if lines[id].Status != "imported" || lines[id].Error != "" {
			t.Fatalf("%s: %+v", id, lines[id])
		}
	}
	blobs, _ := blob.OpenDir(data)
	defer blobs.Close()
	store, err := notestore.Open(context.Background(), db, blobs, notestore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if content, _, err := store.Notebook("checkpointed").Read(context.Background(), gitobj.ID{}, "idea.md"); err != nil || !bytes.Contains(content, []byte("An edit 1 in checkpointed.")) {
		t.Fatalf("the checkpointed edit was not imported: %q %v", content, err)
	}
	for _, id := range refused {
		if _, err := store.Notebook(id).Head(context.Background()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s was stored: %v", id, err)
		}
	}

	leave, err := db.KeepPresence()
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	if err := runAdmin([]string{"storage", "verify", "--data", data}, nil, &out); err == nil || !strings.Contains(err.Error(), "gateways are serving") {
		t.Fatal("a cutover beside a serving gateway:", err)
	}
}

// An export that no longer matches its notebook is never repaired by an
// ordinary export; export -recreate replaces it with a fresh one and keeps
// the purge flag the backup has not pushed.
func TestStorageExportRecreatesADivergedExport(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	head := previousNotebook(t, data, "alpha")
	var out bytes.Buffer
	for _, command := range []string{"import", "export"} {
		if err := runAdmin([]string{"storage", command, "--data", data}, nil, &out); err != nil {
			t.Fatal(command, err, out.String())
		}
	}
	export := filepath.Join(data, "exports", "alpha.git")
	stray := cutoverGitRun(t, export, "commit-tree", "-m", "Stray", head+"^{tree}")
	cutoverGitRun(t, export, "update-ref", "refs/heads/main", stray)
	flag := `{"purge":"purge-1","head":"` + head + `","replaces":["` + stray + `"]}`
	os.WriteFile(filepath.Join(export, "forgetmenote-purge.json"), []byte(flag), 0o644)

	out.Reset()
	if err := runAdmin([]string{"storage", "export", "--data", data}, nil, &out); err == nil || !strings.Contains(storageLines(t, &out)["alpha"].Error, "only a purge may rebuild it") {
		t.Fatal("exported over a diverged export:", err)
	}
	out.Reset()
	if err := runAdmin([]string{"storage", "export", "--data", data, "-recreate"}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}
	if l := storageLines(t, &out)["alpha"]; l.Status != "recreated" || l.Head != head || l.Repository != export {
		t.Fatalf("%+v", l)
	}
	if got := cutoverGitRun(t, export, "rev-parse", "main"); got != head {
		t.Fatalf("recreated at %s, not %s", got, head)
	}
	if err := exec.Command("git", "-C", export, "cat-file", "-e", stray).Run(); err == nil {
		t.Fatal("the recreated export still holds the stray commit")
	}
	cutoverGitRun(t, export, "fsck", "--strict", "--no-dangling")
	kept, err := os.ReadFile(filepath.Join(export, "forgetmenote-purge.json"))
	var purge struct {
		Purge    string   `json:"purge"`
		Head     string   `json:"head"`
		Replaces []string `json:"replaces"`
	}
	if err != nil || json.Unmarshal(kept, &purge) != nil || purge.Purge != "purge-1" || purge.Head != head || strings.Join(purge.Replaces, " ") != stray {
		t.Fatalf("the purge flag did not carry over: %s %v", kept, err)
	}
}

// The import ends by gathering the statistics of the rows it stored, which
// autovacuum gathers only later: without them a note's history right after
// the cutover is planned as a scan of the notebook's changes per revision.
func TestStorageImportGathersStatistics(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	db := instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	previousNotebook(t, data, "alpha")
	var out bytes.Buffer
	if err := runAdmin([]string{"storage", "import", "--data", data}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}
	for _, table := range []string{"revisions", "revision_changes", "entries", "git_trees"} {
		var analyzed bool
		if err := db.Pool.QueryRow(context.Background(), `SELECT last_analyze IS NOT NULL FROM pg_stat_user_tables WHERE relname = $1`, table).Scan(&analyzed); err != nil || !analyzed {
			t.Fatalf("%s has no statistics after the import: %v", table, err)
		}
	}
}
