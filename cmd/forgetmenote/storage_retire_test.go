package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/workspace"
)

type retireLine struct {
	Notebook  string   `json:"notebook"`
	Status    string   `json:"status"`
	Files     []string `json:"files"`
	Head      string   `json:"head"`
	Commits   int      `json:"commits"`
	Rewritten int      `json:"rewritten"`
	Trees     int      `json:"trees"`
	Blobs     int      `json:"blobs"`
	Error     string   `json:"error"`
}

type retireSummary struct {
	Summary   map[string]int `json:"summary"`
	Notebooks int            `json:"notebooks"`
	DryRun    bool           `json:"dryRun"`
}

// retire runs admin storage retire with args and returns each notebook's
// line, the summary and the command's error.
func retire(t *testing.T, data string, args ...string) (map[string]retireLine, retireSummary, error) {
	t.Helper()
	var out bytes.Buffer
	err := runAdmin(append([]string{"storage", "retire", "--data", data}, args...), nil, &out)
	lines := map[string]retireLine{}
	var summary retireSummary
	for _, raw := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var l retireLine
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

// previousFiles lists which of notebook id's previous release files are in
// data.
func previousFiles(data, id string) []string {
	var present []string
	for _, name := range append(previousReleaseFiles, retiredFiles) {
		if _, err := os.Lstat(filepath.Join(data, "projects", id, name)); err == nil {
			present = append(present, name)
		}
	}
	return present
}

// Retire removes a notebook's previous release files only with -yes, and
// only once the store verifiably holds its repository's history: after new
// revisions and after a purge that rewrote it, too. It keeps every file of a
// notebook whose store lacks a commit of the repository, whose repository
// has refs the import left out, or whose files hold work the history lacks,
// and refuses while a gateway serves.
func TestStorageRetireRemovesOnlyWhatTheStoreHolds(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	db := instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	project := func(id string) string { return filepath.Join(data, "projects", id) }
	heads := map[string]string{}
	for _, id := range []string{"opened", "moved", "purged", "behind", "dirty", "tagged"} {
		heads[id] = previousNotebook(t, data, id)
	}
	// A branch inside HEAD's history is held; an annotated tag's object is not.
	tagged := filepath.Join(project("tagged"), "workspace")
	cutoverGitRun(t, tagged, "branch", "draft", "HEAD~1")
	cutoverGitRun(t, tagged, "tag", "--annotate", "--message", "Release", "v1")
	previousLiveFiles(t, data, "opened")
	os.MkdirAll(filepath.Join(project("opened"), "captures"), 0o700)
	os.WriteFile(filepath.Join(project("opened"), "captures", "receipt.json"), []byte("{}"), 0o600)
	os.MkdirAll(project("fresh"), 0o700)
	os.MkdirAll(filepath.Join(project("leftover"), retiredFiles, "workspace"), 0o700)
	os.WriteFile(filepath.Join(project("leftover"), retiredFiles, "workspace", "idea.md"), []byte("left\n"), 0o600)
	var out bytes.Buffer
	if err := runAdmin([]string{"storage", "import", "--data", data}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}

	// After the cutover: the notebook takes a revision, a purge rewrites
	// another's history, the previous release commits to a third, a file
	// appears in a fourth's working tree, and a stash in tagged's.
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
	ada := gitobj.Signature{Name: "Ada", Email: "ada@example.com", When: 1700000100, TZ: "+0000"}
	if _, err := notes.Notebook("moved").Apply(context.Background(), notestore.ChangeSet{Subject: "Update later.md", Author: ada,
		Files: []notestore.FileChange{{Path: "later.md", Content: []byte("# Later\n")}}}); err != nil {
		t.Fatal(err)
	}
	purge := func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
		raw, err := content()
		if err != nil || !bytes.Contains(raw, []byte("An idea")) {
			return nil, true, err
		}
		return bytes.ReplaceAll(raw, []byte("An idea"), []byte("[purged]")), true, nil
	}
	if _, err := notes.Notebook("purged").Rewrite(context.Background(), 2, "purge-1", purge, notestore.RewriteOptions{Actor: "owner", Reason: "a test", Scope: map[string]any{"kind": "text"}}); err != nil {
		t.Fatal(err)
	}
	behind := filepath.Join(project("behind"), "workspace")
	os.WriteFile(filepath.Join(behind, "late.md"), []byte("late\n"), 0o600)
	cutoverGitRun(t, behind, "add", "late.md")
	cutoverGitRun(t, behind, "commit", "--quiet", "-m", "Late")
	os.WriteFile(filepath.Join(project("dirty"), "workspace", "draft.md"), []byte("unsaved\n"), 0o600)
	os.WriteFile(filepath.Join(tagged, "idea.md"), []byte("stashed\n"), 0o600)
	cutoverGitRun(t, tagged, "stash", "--quiet")

	all := map[string][]string{}
	for _, id := range []string{"opened", "moved", "purged", "behind", "dirty", "tagged", "fresh", "leftover"} {
		all[id] = previousFiles(data, id)
	}
	if strings.Join(all["opened"], " ") != "workspace collaboration captures" {
		t.Fatal("the previous release's files of opened:", all["opened"])
	}
	unchanged := func(when string) {
		t.Helper()
		for id, want := range all {
			if got := previousFiles(data, id); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("%s: %s has %v, not %v", when, id, got, want)
			}
		}
	}

	// A dry run, the default, verifies and removes nothing.
	for _, args := range [][]string{nil, {"-dry-run"}} {
		lines, summary, err := retire(t, data, args...)
		if err == nil || !strings.Contains(err.Error(), "failed for 3 of 8 notebooks") {
			t.Fatal("a dry run with notebooks that fail:", err)
		}
		for _, id := range []string{"opened", "moved", "leftover"} {
			if l := lines[id]; l.Status != "would remove" || l.Error != "" || len(l.Files) != len(all[id]) {
				t.Fatalf("%s: %+v", id, l)
			}
		}
		if l := lines["opened"]; l.Head != heads["opened"] || l.Commits != 3 || l.Rewritten != 0 || l.Trees != 1 || l.Blobs != 1 {
			t.Fatalf("what opened's verification compared: %+v", l)
		}
		if l := lines["moved"]; l.Head != heads["moved"] || l.Commits != 3 {
			t.Fatalf("a notebook with revisions after the import: %+v", l)
		}
		if l := lines["purged"]; l.Status != "would remove" || l.Head != heads["purged"] || l.Commits != 1 || l.Rewritten != 2 {
			t.Fatalf("a notebook a purge rewrote: %+v", l)
		}
		if l := lines["behind"]; l.Status != "kept" || !strings.Contains(l.Error, "of Git HEAD's history is not in the notebook") {
			t.Fatalf("a notebook whose store lacks a commit: %+v", l)
		}
		if l := lines["dirty"]; l.Status != "kept" || !strings.Contains(l.Error, "the working tree has files its Git history lacks") {
			t.Fatalf("a notebook with an uncommitted file: %+v", l)
		}
		if l := lines["tagged"]; l.Status != "kept" || !strings.Contains(l.Error, "the repository has refs the store lacks, refs/stash, refs/tags/v1:") {
			t.Fatalf("a notebook with a stash and an annotated tag: %+v", l)
		}
		if l := lines["fresh"]; l.Status != "skipped: no previous release files" {
			t.Fatalf("a notebook without previous release files: %+v", l)
		}
		want := map[string]int{"would remove": 4, "kept": 3, "skipped: no previous release files": 1}
		if summary.Notebooks != 8 || !summary.DryRun || len(summary.Summary) != len(want) {
			t.Fatalf("summary %+v", summary)
		}
		for status, n := range want {
			if summary.Summary[status] != n {
				t.Fatalf("summary %+v", summary)
			}
		}
		unchanged("after a dry run")
	}
	if _, _, err := retire(t, data, "-dry-run", "-yes"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatal("both -dry-run and -yes:", err)
	}

	leave, err := db.KeepPresence()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := retire(t, data, "-yes"); err == nil || !strings.Contains(err.Error(), "gateways are serving") {
		t.Fatal("retire beside a serving gateway:", err)
	}
	leave()
	unchanged("after retire was refused")

	lines, summary, err := retire(t, data, "-yes")
	if err == nil || !strings.Contains(err.Error(), "failed for 3 of 8 notebooks") {
		t.Fatal("retire with notebooks that fail:", err)
	}
	for _, id := range []string{"opened", "moved", "purged", "leftover"} {
		if l := lines[id]; l.Status != "removed" || l.Error != "" {
			t.Fatalf("%s: %+v", id, l)
		}
		if left := previousFiles(data, id); len(left) != 0 {
			t.Fatalf("%s still has %v", id, left)
		}
		if _, err := os.Stat(project(id)); err != nil {
			t.Fatal("the notebook's state directory:", err)
		}
	}
	for _, id := range []string{"behind", "dirty", "tagged"} {
		if l := lines[id]; l.Status != "kept" {
			t.Fatalf("%s: %+v", id, l)
		}
		if got := previousFiles(data, id); strings.Join(got, " ") != strings.Join(all[id], " ") {
			t.Fatalf("%s lost files: %v", id, got)
		}
	}
	if _, err := os.Stat(filepath.Join(behind, ".git", "HEAD")); err != nil {
		t.Fatal("the kept repository:", err)
	}
	if summary.DryRun || summary.Summary["removed"] != 4 || summary.Summary["kept"] != 3 {
		t.Fatalf("summary %+v", summary)
	}
	if head, err := notes.Notebook("opened").Head(context.Background()); err != nil || head.Commit.String() != heads["opened"] {
		t.Fatal("the stored notebook after retire:", head, err)
	}

	lines, _, err = retire(t, data, "-yes", "-notebook", "opened")
	if err != nil || lines["opened"].Status != "skipped: no previous release files" {
		t.Fatal("retire again:", lines["opened"], err)
	}
}

// Retire keeps a notebook whose working tree has no repository but holds
// files, and one the notes store lacks.
func TestStorageRetireKeepsFilesNothingVerifies(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	previousNotebook(t, data, "absent")
	orphan := filepath.Join(data, "projects", "orphan", "workspace")
	os.MkdirAll(orphan, 0o700)
	os.WriteFile(filepath.Join(orphan, "idea.md"), []byte("an idea\n"), 0o600)

	lines, _, err := retire(t, data, "-yes")
	if err == nil {
		t.Fatal("retired files nothing verifies")
	}
	if l := lines["absent"]; l.Status != "kept" || !strings.Contains(l.Error, "the notes store does not hold this notebook") {
		t.Fatalf("a notebook the store lacks: %+v", l)
	}
	if l := lines["orphan"]; l.Status != "kept" || !strings.Contains(l.Error, "has no Git repository") {
		t.Fatalf("a working tree without its repository: %+v", l)
	}
	for _, path := range []string{filepath.Join(data, "projects", "absent", "workspace", ".git"), filepath.Join(orphan, "idea.md")} {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			t.Fatal("removed", path)
		}
	}
}
