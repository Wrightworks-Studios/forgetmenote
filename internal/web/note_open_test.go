package web

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
)

// Opening a note in the browser asks for the notebook listings, status, review
// inbox, the note, its locks, comments and live snapshot. They all describe one
// commit, so Git answers each committed fact once and a reopen reuses it.
func TestNoteOpenGitProcesses(t *testing.T) {
	dir := t.TempDir()
	projects, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { projects.Close() })
	// No index worker: it reindexes after commits in the background, so its
	// Git processes would land in this count whenever it happens to run.
	app, err := New(projects, pgtest.Org(t), dir, "http://localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	for i := range 6 {
		body := fmt.Sprintf(`{"path":"other-%d.md","content":"# Other %d\n","revision":""}`, i, i)
		if w := call(app, "POST", "/api/note", body, testToken); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if w := call(app, "POST", "/api/note", `{"path":"open.md","content":"# Open\n\nBody\n","revision":""}`, testToken); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	processes := countGitProcesses(t)
	open := func() []string {
		for _, path := range []string{
			"/api/me", "/api/projects",
			"/api/notes", "/api/notes?collection=notes", "/api/collections",
			"/api/status", "/api/knowledge/review",
			"/api/note?path=open.md", "/api/locks?path=open.md",
			"/api/comments?path=open.md", "/api/live?path=open.md",
		} {
			if w := call(app, "GET", path, "", testToken); w.Code != 200 {
				t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
			}
		}
		return processes()
	}
	first, again := open(), open()
	t.Logf("first open after a commit: %d Git processes %v", len(first), first)
	t.Logf("reopen at the same commit: %d Git processes %v", len(again), again)
	// First open: the per-commit listing and lock caches, status, the review
	// inbox, one read of the note and one lookup for its absent discussion.
	if len(first) > 9 {
		t.Errorf("first note open ran %d Git processes; each committed fact should be read once: %v", len(first), first)
	}
	// Reopen: status and the review inbox; committed reads are reused.
	if len(again) > 2 {
		t.Errorf("reopening the same note at the same commit ran %d Git processes: %v", len(again), again)
	}
}

// countGitProcesses puts a recording git first on PATH. Each call of the
// returned function reports the subcommands run since the previous call.
func countGitProcesses(t *testing.T) func() []string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexec '" + git + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	seen := 0
	return func() []string {
		raw, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		if len(raw) == 0 {
			lines = nil
		}
		commands := []string{}
		for _, line := range lines[seen:] {
			commands = append(commands, gitSubcommand(strings.Fields(line)))
		}
		seen = len(lines)
		return commands
	}
}

// gitSubcommand skips global options, such as the -c settings every workspace
// call passes, and returns the subcommand.
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "-C":
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return ""
}
