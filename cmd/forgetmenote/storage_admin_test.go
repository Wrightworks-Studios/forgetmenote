package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
)

// gitRun runs Git for a test without user or system configuration, and
// without automatic maintenance, which newer Git runs in the background and
// which would repack the repository under the command a test runs next.
func gitRun(t *testing.T, dir string, args ...string) string {
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

type storageLine struct {
	Notebook   string `json:"notebook"`
	Status     string `json:"status"`
	Head       string `json:"head"`
	Commits    int    `json:"commits"`
	Error      string `json:"error"`
	Repository string `json:"repository"`
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

// admin storage export writes and verifies the export of every notebook the
// catalog lists, whether or not it has a state directory, and refuses an
// export that diverged from its notebook until -recreate writes it again from
// the notes store, keeping a purge's flag for the backup worker.
func TestStorageExportRecreatesADivergedExport(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	db := instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	c, err := catalog.Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := c.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	saved, err := store.Save(t.Context(), "idea.md", []byte("# Idea\n"), "", "test")
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	head := saved.Revision
	if err = os.RemoveAll(filepath.Join(data, "projects")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAdmin([]string{"storage", "export", "--data", data}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}
	export := filepath.Join(data, "exports", "default.git")
	if l := storageLines(t, &out)["default"]; l.Status != "exported" || l.Head != head || l.Repository != export {
		t.Fatalf("%+v", l)
	}
	stray := gitRun(t, export, "commit-tree", "-m", "Stray", head+"^{tree}")
	gitRun(t, export, "update-ref", "refs/heads/main", stray)
	flag := `{"purge":"purge-1","head":"` + head + `","replaces":["` + stray + `"]}`
	os.WriteFile(filepath.Join(export, "forgetmenote-purge.json"), []byte(flag), 0o644)

	out.Reset()
	if err := runAdmin([]string{"storage", "export", "--data", data}, nil, &out); err == nil || !strings.Contains(storageLines(t, &out)["default"].Error, "only a purge may rebuild it") {
		t.Fatal("exported over a diverged export:", err)
	}
	out.Reset()
	if err := runAdmin([]string{"storage", "export", "--data", data, "-recreate"}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}
	if l := storageLines(t, &out)["default"]; l.Status != "recreated" || l.Head != head || l.Repository != export {
		t.Fatalf("%+v", l)
	}
	if got := gitRun(t, export, "rev-parse", "main"); got != head {
		t.Fatalf("recreated at %s, not %s", got, head)
	}
	if err := exec.Command("git", "-C", export, "cat-file", "-e", stray).Run(); err == nil {
		t.Fatal("the recreated export still holds the stray commit")
	}
	gitRun(t, export, "fsck", "--strict", "--no-dangling")
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
