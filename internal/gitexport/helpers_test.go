package gitexport

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var fmnID = regexp.MustCompile(`(?m)\A---\n(?:.*\n)*?fmn-id: ([0-9a-f]{32})\n`)

// declaredID is the identity a note's front matter declares, as the
// workspace's fmn-id rules give it for well-formed notes.
func declaredID(path string, content []byte) string {
	if !strings.HasSuffix(path, ".md") {
		return ""
	}
	if m := fmnID.FindSubmatch(content); m != nil {
		return string(m[1])
	}
	return ""
}

// noteID refuses an fmn-id line that is not 32 lowercase hex characters.
func noteID(content []byte) (string, error) {
	if id := declaredID("x.md", content); id != "" {
		return id, nil
	}
	if strings.HasPrefix(string(content), "---\n") && strings.Contains(string(content), "\nfmn-id:") {
		return "", errors.New("fmn-id must be 32 lowercase hexadecimal characters")
	}
	return "", nil
}

// allowed is the notebook file allow-list of the workspace's import.
func allowed(p string) bool {
	switch {
	case p == ".gitnotes-locks.json" || p == ".gitnotes-trash.json":
		return true
	case strings.HasPrefix(p, "comments/"):
		name := strings.TrimSuffix(strings.TrimPrefix(p, "comments/"), ".json")
		return len(name) == 64 && strings.HasSuffix(p, ".json") && strings.Trim(name, "0123456789abcdef") == ""
	case strings.HasPrefix(p, "assets/"), strings.HasSuffix(p, ".md"):
		for _, part := range strings.Split(p, "/") {
			if strings.HasPrefix(part, ".") {
				return false
			}
		}
		return true
	}
	return false
}

func testLimits(t testing.TB) Limits {
	return Limits{Allowed: allowed, NoteID: noteID, Staging: t.TempDir()}
}

type env struct {
	t     testing.TB
	ctx   context.Context
	store *notestore.Store
	ex    *Exporter
}

func newEnv(t testing.TB) *env {
	t.Helper()
	db := pgtest.Org(t)
	blobs, err := blob.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	store, err := notestore.Open(context.Background(), db, blobs, notestore.Options{ImportEntryID: declaredID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	e := &env{t: t, ctx: context.Background(), store: store}
	e.ex = e.exporter(t.TempDir(), Options{})
	return e
}

func (e *env) exporter(dir string, o Options) *Exporter {
	e.t.Helper()
	if o.Logf == nil {
		o.Logf = e.t.Logf
	}
	ex, err := New(e.store, dir, o)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(ex.Close)
	return ex
}

var (
	ada = gitobj.Signature{Name: "Ada Author", Email: "ada@example.com", When: 1700000000, TZ: "+0100"}
	bob = gitobj.Signature{Name: "Bob", Email: "bob@example.com", When: 1700000100, TZ: "-0500"}
)

func (e *env) create(id string) *notestore.Notebook {
	e.t.Helper()
	if _, err := e.store.Create(e.ctx, id, ada); err != nil {
		e.t.Fatal(err)
	}
	return e.store.Notebook(id)
}

func apply(t testing.TB, n *notestore.Notebook, cs notestore.ChangeSet) notestore.CommitResult {
	t.Helper()
	if cs.Subject == "" {
		cs.Subject = "Change notes"
	}
	if cs.Author.Name == "" {
		cs.Author = ada
	}
	r, err := n.Apply(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func write(path, content string) notestore.FileChange {
	return notestore.FileChange{Path: path, Content: []byte(content)}
}

func note(id, title, body string) string {
	return "---\nfmn-id: " + id + "\ntitle: " + title + "\n---\n\n" + body
}

// history writes a notebook history with the shapes the export must keep:
// notes with identities, a large note, assets, an executable, nested
// directories, comments, locks, Trash, renames, deletions and trailers.
func history(t *testing.T, n *notestore.Notebook) notestore.Head {
	t.Helper()
	sig := ada
	step := func(subject string, files ...notestore.FileChange) notestore.Head {
		sig.When += 60
		return apply(t, n, notestore.ChangeSet{Author: sig, Subject: subject, Files: files,
			Trailers: []gitobj.Trailer{{Key: "Gitnotes-Source", Value: "mcp"}, {Key: "Gitnotes-User", Value: "ada"}}}).Head
	}
	big := strings.Repeat("A paragraph of the manuscript, long enough to matter.\n\n", 3000)
	step("Start the notebook",
		write("AGENTS.md", "Follow the notebook rules.\n"),
		write("Inbox/idea.md", note("0123456789abcdef0123456789abcdef", "Idea", "An idea.\n")),
		write("Projects/book.md", note("11111111111111111111111111111111", "Book", big)),
		notestore.FileChange{Path: "assets/scan.png", Content: []byte(strings.Repeat("\x89PNG\x00\x01binary", 500))},
		notestore.FileChange{Path: "assets/tool.sh", Content: []byte("#!/bin/sh\n"), Mode: gitobj.ModeExecutable})
	step("Discuss and lock",
		write("comments/"+strings.Repeat("ab", 32)+".json", `{"comments":[]}`+"\n"),
		write(".gitnotes-locks.json", `{"locks":["Projects/book.md"]}`+"\n"))
	step("Grow the book", write("Projects/book.md", note("11111111111111111111111111111111", "Book", strings.Replace(big, "manuscript", "novel", 7))))
	step("Move \"Inbox/idea.md\" to \"Projects/Ideas/idea.md\"", notestore.FileChange{Path: "Projects/Ideas/idea.md", From: "Inbox/idea.md",
		Content: []byte(note("0123456789abcdef0123456789abcdef", "Idea", "An idea, moved.\n"))})
	step("Move notes to Trash", notestore.FileChange{Path: "Trash/scan-note.md", Content: []byte(note("22222222222222222222222222222222", "Scan", "Old.\n"))},
		write(".gitnotes-trash.json", `{"entries":[]}`+"\n"))
	return step("Empty Trash", notestore.FileChange{Path: "Trash/scan-note.md"})
}

// git runs Git in dir, a work tree or a Git directory, without user or
// system configuration.
func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := gitErr(dir, nil, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

func gitErr(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=tester@example.com", "GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=tester@example.com",
		"GIT_AUTHOR_DATE=1700000000 +0000", "GIT_COMMITTER_DATE=1700000000 +0000")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func sortedFields(s string) []string {
	f := strings.Fields(s)
	slices.Sort(f)
	return f
}

// objectFiles lists the loose object files of a repository by relative path.
// Background maintenance may prune a loose object or its fan-out directory
// while the walk runs; what vanished is not there. The objects directory
// itself must exist.
func objectFiles(t *testing.T, repo string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	objects := filepath.Join(repo, "objects")
	err := filepath.WalkDir(objects, func(p string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && p != objects {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(objects, p)
		if len(rel) == 41 && rel[2] == '/' {
			raw, err := os.ReadFile(p)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			out[filepath.ToSlash(rel)] = raw
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustID(t *testing.T, s string) gitobj.ID {
	t.Helper()
	id, err := gitobj.ParseID(strings.TrimSpace(s))
	if err != nil {
		t.Fatal(err)
	}
	return id
}
