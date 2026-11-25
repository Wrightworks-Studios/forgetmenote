package gitobj_test

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
)

// gitEnv isolates Git from the host's configuration.
func gitEnv(t *testing.T) []string {
	home := t.TempDir()
	return append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Ada Author", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_AUTHOR_DATE=1700000000 +0100",
		"GIT_COMMITTER_NAME=Cy Committer", "GIT_COMMITTER_EMAIL=cy@example.com", "GIT_COMMITTER_DATE=1700000100 -0230",
		"LC_ALL=C")
}

type repo struct {
	t   *testing.T
	dir string
	env []string
}

func newRepo(t *testing.T, bare bool) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("the git executable is required:", err)
	}
	r := &repo{t: t, dir: t.TempDir(), env: gitEnv(t)}
	args := []string{"init", "-q", "--initial-branch=main"}
	if bare {
		args = append(args, "--bare")
	}
	r.run(nil, nil, args...)
	return r
}

func (r *repo) run(env []string, stdin []byte, args ...string) string {
	r.t.Helper()
	out, err := r.try(env, stdin, args...)
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (r *repo) try(env []string, stdin []byte, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(slices.Clone(r.env), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out) + stderr.String(), err
	}
	return string(out), nil
}

func (r *repo) id(s string) gitobj.ID {
	r.t.Helper()
	id, err := gitobj.ParseID(strings.TrimSpace(s))
	if err != nil {
		r.t.Fatalf("%q: %v", s, err)
	}
	return id
}

func (r *repo) catFile(kind string, id gitobj.ID) []byte {
	r.t.Helper()
	return []byte(r.run(nil, nil, "cat-file", kind, id.String()))
}

func (r *repo) write(name, content string) {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func TestIDsParseAndPrintAsGitDoes(t *testing.T) {
	r := newRepo(t, false)
	empty := r.id(r.run(nil, nil, "mktree", "--missing"))
	if empty != gitobj.EmptyTree || empty.String() != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Fatalf("empty tree = %s", empty)
	}
	for _, bad := range []string{"", "4b825dc642cb6eb9a060e54bf8d69288fbee490", "4B825DC642CB6EB9A060E54BF8D69288FBEE4904", "4b825dc642cb6eb9a060e54bf8d69288fbee490g"} {
		if _, err := gitobj.ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) succeeded", bad)
		}
	}
	var id gitobj.ID
	if !id.IsZero() || empty.IsZero() {
		t.Fatal("IsZero")
	}
	raw, _ := empty.MarshalText()
	if err := id.UnmarshalText(raw); err != nil || id != empty {
		t.Fatal("text round trip:", id, err)
	}
}

func TestBlobIDMatchesHashObject(t *testing.T) {
	r := newRepo(t, false)
	large := bytes.Repeat([]byte("all work and no play\x00\xff\n"), 50000)
	for _, content := range [][]byte{nil, []byte("hello\n"), []byte("no newline"), {0, 1, 2, 255}, large} {
		want := r.id(r.run(nil, content, "hash-object", "--stdin"))
		if got := gitobj.BlobID(content); got != want {
			t.Errorf("BlobID(%d bytes) = %s, git %s", len(content), got, want)
		}
		if got := gitobj.HashObject("blob", content); got != want {
			t.Errorf("HashObject(blob) = %s, git %s", got, want)
		}
	}
	commit := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a> 1 +0000\ncommitter A <a> 1 +0000\n\nm\n")
	if got, want := gitobj.HashObject("commit", commit), r.id(r.run(nil, commit, "hash-object", "-t", "commit", "--stdin")); got != want {
		t.Errorf("HashObject(commit) = %s, git %s", got, want)
	}
}

func TestEncodeTreeMatchesMktree(t *testing.T) {
	r := newRepo(t, false)
	blob := r.id(r.run(nil, []byte("x"), "hash-object", "-w", "--stdin"))
	other := r.id(r.run(nil, []byte("y"), "hash-object", "-w", "--stdin"))
	// Names that sort differently once a directory is compared as name + "/".
	entries := []gitobj.TreeEntry{
		{Name: "a", Mode: gitobj.ModeTree, ID: gitobj.EmptyTree},
		{Name: "a-b", Mode: gitobj.ModeFile, ID: blob},
		{Name: "a.b", Mode: gitobj.ModeExecutable, ID: other},
		{Name: "a0", Mode: gitobj.ModeFile, ID: blob},
		{Name: "ab", Mode: gitobj.ModeTree, ID: gitobj.EmptyTree},
		{Name: "ab.md", Mode: gitobj.ModeFile, ID: blob},
		{Name: "link", Mode: gitobj.ModeSymlink, ID: other},
		{Name: "module", Mode: gitobj.ModeGitlink, ID: gitobj.ID{1, 2, 3}},
		{Name: "Zeta", Mode: gitobj.ModeFile, ID: blob},
		{Name: "na\u00efve \u2713.md", Mode: gitobj.ModeFile, ID: other},
		{Name: "with space", Mode: gitobj.ModeFile, ID: blob},
	}
	var input strings.Builder
	shuffled := slices.Clone(entries)
	rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	for _, e := range shuffled {
		kind := map[uint32]string{gitobj.ModeTree: "tree", gitobj.ModeGitlink: "commit"}[e.Mode]
		if kind == "" {
			kind = "blob"
		}
		fmt.Fprintf(&input, "%06o %s %s\t%s\n", e.Mode, kind, e.ID, e.Name)
	}
	want := r.id(r.run(nil, []byte(input.String()), "mktree", "--missing"))
	body := gitobj.EncodeTree(shuffled)
	if got := gitobj.HashObject("tree", body); got != want {
		t.Fatalf("tree = %s, git mktree %s", got, want)
	}
	if !bytes.Equal(body, r.catFile("tree", want)) {
		t.Fatal("tree body differs from git cat-file")
	}
	parsed, err := gitobj.ParseTree(body)
	if err != nil {
		t.Fatal(err)
	}
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, gitobj.CompareEntries)
	if !slices.Equal(parsed, sorted) {
		t.Fatalf("parsed %v\nwant %v", parsed, sorted)
	}
	for _, bad := range [][]byte{[]byte("100644 a\x00short"), []byte("10064x a\x00" + strings.Repeat("x", 20)), []byte(" a\x00" + strings.Repeat("x", 20))} {
		if _, err := gitobj.ParseTree(bad); err == nil {
			t.Errorf("ParseTree(%q) succeeded", bad)
		}
	}
	for name, valid := range map[string]bool{"a": true, ".gitnotes-trash.json": true, "": false, ".": false, "..": false, ".git": false, ".GIT": false, "a/b": false, "a\x00": false} {
		if gitobj.ValidName(name) != valid {
			t.Errorf("ValidName(%q) != %v", name, valid)
		}
	}
}

func TestEncodeCommitMatchesCommitTree(t *testing.T) {
	r := newRepo(t, false)
	tree := gitobj.EmptyTree
	r.run(nil, nil, "mktree", "--missing")
	r.run(nil, []byte{}, "hash-object", "-t", "tree", "-w", "--stdin")
	first := r.id(r.run(nil, nil, "commit-tree", tree.String(), "-m", "first"))
	second := r.id(r.run(nil, nil, "commit-tree", tree.String(), "-p", first.String(), "-m", "second\n\nbody line"))
	merge := r.id(r.run([]string{"GIT_AUTHOR_NAME= Mx. Crud, \"Q\" ;", "GIT_AUTHOR_EMAIL=<odd@example.com>"}, nil,
		"commit-tree", tree.String(), "-p", second.String(), "-p", first.String(), "-m", "merge"))
	for _, id := range []gitobj.ID{first, second, merge} {
		body := r.catFile("commit", id)
		c, err := gitobj.ParseCommit(body)
		if err != nil {
			t.Fatalf("%s: %v\n%s", id, err, body)
		}
		if !bytes.Equal(gitobj.EncodeCommit(c), body) || gitobj.HashObject("commit", body) != id {
			t.Fatalf("%s does not round-trip", id)
		}
	}
	c, _ := gitobj.ParseCommit(r.catFile("commit", merge))
	if c.Tree != tree || !slices.Equal(c.Parents, []gitobj.ID{second, first}) || c.Message != "merge\n" || c.Extra != nil {
		t.Fatalf("merge fields: %+v", c)
	}
	if c.Author.Name != gitobj.CleanIdent(" Mx. Crud, \"Q\" ;") || c.Author.Email != gitobj.CleanIdent("<odd@example.com>") {
		t.Fatalf("Git cleaned the author to %q <%q>, CleanIdent to %q <%q>", c.Author.Name, c.Author.Email,
			gitobj.CleanIdent(" Mx. Crud, \"Q\" ;"), gitobj.CleanIdent("<odd@example.com>"))
	}
	if c.Author.When != 1700000000 || c.Author.TZ != "+0100" || c.Committer != (gitobj.Signature{Name: "Cy Committer", Email: "cy@example.com", When: 1700000100, TZ: "-0230"}) {
		t.Fatalf("signatures: %+v %+v", c.Author, c.Committer)
	}
	if got := c.Committer.Time(); got.Unix() != 1700000100 || got.Format("-0700") != "-0230" {
		t.Fatal("committer time:", got)
	}
	sig := gitobj.NewSignature("N", "e", time.Unix(1700000000, 0).In(time.FixedZone("", -(5*3600+45*60))))
	if sig.TZ != "-0545" || sig.When != 1700000000 {
		t.Fatal("NewSignature:", sig)
	}
	// Our encoding of fresh fields is what commit-tree writes for them.
	ours := gitobj.Commit{Tree: tree, Parents: []gitobj.ID{first}, Author: sig, Committer: sig, Message: "ours\n"}
	theirs := r.id(r.run([]string{"GIT_AUTHOR_NAME=N", "GIT_AUTHOR_EMAIL=e", "GIT_AUTHOR_DATE=1700000000 -0545", "GIT_COMMITTER_NAME=N", "GIT_COMMITTER_EMAIL=e", "GIT_COMMITTER_DATE=1700000000 -0545"},
		nil, "commit-tree", tree.String(), "-p", first.String(), "-m", "ours"))
	if got := gitobj.HashObject("commit", gitobj.EncodeCommit(ours)); got != theirs {
		t.Fatalf("encoded commit %s, commit-tree %s", got, theirs)
	}
	for _, bad := range []string{
		"",
		"tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a> 1 +0000\ncommitter A <a> 1 +0000\n",
		"tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A<a> 1 +0000\ncommitter A <a> 1 +0000\n\nm",
		"tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a> 01 +0000\ncommitter A <a> 1 +0000\n\nm",
		"tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\ncommitter A <a> 1 +0000\n\nm",
		"tree 4B825DC642CB6EB9A060E54BF8D69288FBEE4904\nauthor A <a> 1 +0000\ncommitter A <a> 1 +0000\n\nm",
	} {
		if _, err := gitobj.ParseCommit([]byte(bad)); err == nil {
			t.Errorf("ParseCommit(%q) succeeded", bad)
		}
	}
}

// buildHistory makes a repository with merges, an octopus merge, renames,
// modes, nested directories, a non-UTF-8 encoding header and hand-made signed
// and merge-tagged commits, and returns it.
func buildHistory(t *testing.T) *repo {
	t.Helper()
	r := newRepo(t, false)
	commit := func(message string) {
		r.run(nil, nil, "add", "-A")
		r.run(nil, nil, "commit", "-q", "--allow-empty", "-m", message)
	}
	r.write("notes/first.md", "# First\n\nhello\n")
	r.write("AGENTS.md", "instructions\n")
	commit("Add first")
	r.write("notes/deep/er/still.md", "deep\n")
	r.write("assets/image.png", "\x89PNG\r\n\x1a\nbinary")
	os.Symlink("notes/first.md", filepath.Join(r.dir, "link"))
	commit("Add deep files and a link")
	r.run(nil, nil, "checkout", "-q", "-b", "side")
	r.run(nil, nil, "mv", "notes/first.md", "notes/renamed.md")
	r.write("side.md", "side\n")
	commit("Rename on a side branch")
	r.run(nil, nil, "checkout", "-q", "-b", "third", "main")
	r.write("third.sh", "#!/bin/sh\n")
	os.Chmod(filepath.Join(r.dir, "third.sh"), 0o755)
	commit("Add a script\n\nGitnotes-Source: mcp\nGitnotes-User: u1")
	r.run(nil, nil, "checkout", "-q", "main")
	r.write("notes/deep/er/still.md", "deeper\n")
	commit("Edit on main")
	r.run(nil, nil, "merge", "-q", "--no-edit", "side")
	r.write("after.md", "after merge\n")
	commit("After merge")
	r.run(nil, nil, "checkout", "-q", "-b", "fourth", "main~2")
	r.write("fourth.md", "four\n")
	commit("Fourth")
	r.run(nil, nil, "checkout", "-q", "main")
	r.run(nil, nil, "merge", "-q", "--no-edit", "third", "fourth")
	r.run(nil, nil, "-c", "i18n.commitEncoding=ISO-8859-1", "commit", "-q", "--allow-empty", "-m", "Caf\xe9 latin-1")
	r.run(nil, nil, "commit", "-q", "--allow-empty", "-m", "Trailers", "--trailer", "Gitnotes-Source: browser", "--trailer", "Collaboration-Receipt: abc")
	r.run(nil, nil, "checkout", "-q", "-b", "tagged", "HEAD~2")
	r.write("tagged.md", "tagged\n")
	commit("Before tag merge")
	r.run(nil, nil, "tag", "-a", "-m", "release", "v1")
	r.run(nil, nil, "checkout", "-q", "main")
	r.run(nil, nil, "merge", "-q", "--no-ff", "--no-edit", "v1")
	head := r.id(r.run(nil, nil, "rev-parse", "HEAD"))
	tree := r.id(r.run(nil, nil, "rev-parse", "HEAD^{tree}"))
	// A signed commit: Git keeps the gpgsig header verbatim and fsck checks
	// only its shape, so a fixed signature block stands in for a real key.
	signed := fmt.Sprintf("tree %s\nparent %s\nauthor Ada Author <ada@example.com> 1700000200 +0000\ncommitter Ada Author <ada@example.com> 1700000200 +0000\n"+
		"gpgsig -----BEGIN SSH SIGNATURE-----\n U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAg\n \n -----END SSH SIGNATURE-----\n\nSigned commit\n", tree, head)
	id := r.id(r.run(nil, []byte(signed), "hash-object", "-t", "commit", "-w", "--stdin"))
	// Git records a mergetag header only when the merged tag is signed.
	tagged := r.id(r.run(nil, nil, "rev-parse", "tagged"))
	merged := fmt.Sprintf("tree %s\nparent %s\nparent %s\nauthor Ada Author <ada@example.com> 1700000300 +0000\ncommitter Ada Author <ada@example.com> 1700000300 +0000\n"+
		"mergetag object %s\n type commit\n tag v2\n tagger Ada Author <ada@example.com> 1700000250 +0000\n \n signed release\n -----BEGIN PGP SIGNATURE-----\n \n iQEz\n -----END PGP SIGNATURE-----\n\nMerge tag 'v2'\n",
		tree, id, tagged, tagged)
	id = r.id(r.run(nil, []byte(merged), "hash-object", "-t", "commit", "-w", "--stdin"))
	r.run(nil, nil, "update-ref", "refs/heads/main", id.String())
	return r
}

func TestEveryObjectOfARealRepositoryRoundTrips(t *testing.T) {
	r := buildHistory(t)
	r.run(nil, nil, "fsck", "--strict", "--no-dangling")
	commits := strings.Fields(r.run(nil, nil, "rev-list", "--all"))
	merges := strings.Fields(r.run(nil, nil, "rev-list", "--all", "--merges"))
	if len(commits) < 14 || len(merges) < 4 {
		t.Fatalf("%d commits, %d merges", len(commits), len(merges))
	}
	extras := 0
	for _, raw := range commits {
		id := r.id(raw)
		body := r.catFile("commit", id)
		c, err := gitobj.ParseCommit(body)
		if err != nil {
			t.Fatalf("%s: %v\n%s", id, err, body)
		}
		if !bytes.Equal(gitobj.EncodeCommit(c), body) || gitobj.HashObject("commit", body) != id {
			t.Fatalf("%s does not round-trip", id)
		}
		parents := strings.Fields(r.run(nil, nil, "log", "-1", "--format=%P", id.String()))
		if len(parents) != len(c.Parents) {
			t.Fatalf("%s: %d parents, git %d", id, len(c.Parents), len(parents))
		}
		if len(c.Extra) > 0 {
			extras++
		}
		gitTrailers := r.run(nil, nil, "log", "-1", "--format=%(trailers:only,unfold)", id.String())
		var ours strings.Builder
		for _, tr := range gitobj.Trailers(c.Message) {
			fmt.Fprintf(&ours, "%s: %s\n", tr.Key, tr.Value)
		}
		if strings.TrimSpace(ours.String()) != strings.TrimSpace(gitTrailers) {
			t.Fatalf("%s trailers %q, git %q", id, ours.String(), gitTrailers)
		}
	}
	if extras < 3 {
		t.Fatalf("only %d commits carried extra headers (encoding, mergetag, gpgsig)", extras)
	}
	objects := r.run(nil, nil, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype) %(objectname)")
	trees := 0
	for _, line := range strings.Split(strings.TrimSpace(objects), "\n") {
		kind, raw, _ := strings.Cut(line, " ")
		if kind != "tree" {
			continue
		}
		trees++
		id := r.id(raw)
		body := r.catFile("tree", id)
		entries, err := gitobj.ParseTree(body)
		if err != nil {
			t.Fatal(id, err)
		}
		if !bytes.Equal(gitobj.EncodeTree(entries), body) {
			t.Fatalf("tree %s does not round-trip", id)
		}
	}
	if trees < 10 {
		t.Fatalf("only %d trees", trees)
	}
}

func TestWriteLooseObjectsMakeAValidRepository(t *testing.T) {
	r := newRepo(t, true)
	blob, err := gitobj.WriteLoose(r.dir, "blob", []byte("# Note\n"))
	if err != nil {
		t.Fatal(err)
	}
	inner := gitobj.EncodeTree([]gitobj.TreeEntry{{Name: "note.md", Mode: gitobj.ModeFile, ID: blob}})
	innerID, err := gitobj.WriteLoose(r.dir, "tree", inner)
	if err != nil {
		t.Fatal(err)
	}
	root := gitobj.EncodeTree([]gitobj.TreeEntry{{Name: "notes", Mode: gitobj.ModeTree, ID: innerID}, {Name: "notes.md", Mode: gitobj.ModeFile, ID: blob}})
	rootID, err := gitobj.WriteLoose(r.dir, "tree", root)
	if err != nil {
		t.Fatal(err)
	}
	sig := gitobj.Signature{Name: "ForgetMeNote", Email: "forgetmenote@localhost", When: 1700000000, TZ: "+0000"}
	message := gitobj.CleanMessage("Save note", "", []gitobj.Trailer{{Key: "Gitnotes-Source", Value: "browser"}})
	commit := gitobj.EncodeCommit(gitobj.Commit{Tree: rootID, Author: sig, Committer: sig, Message: message})
	commitID, err := gitobj.WriteLoose(r.dir, "commit", commit)
	if err != nil {
		t.Fatal(err)
	}
	again, err := gitobj.WriteLoose(r.dir, "commit", commit)
	if err != nil || again != commitID {
		t.Fatal("rewriting an object:", again, err)
	}
	r.run(nil, nil, "update-ref", "refs/heads/main", commitID.String())
	r.run(nil, nil, "fsck", "--strict", "--no-dangling")
	if got := r.run(nil, nil, "cat-file", "-p", "main:notes/note.md"); got != "# Note\n" {
		t.Fatalf("blob through git: %q", got)
	}
	if got := r.catFile("commit", commitID); !bytes.Equal([]byte(got), commit) {
		t.Fatal("commit through git differs")
	}
	if got := r.run(nil, nil, "log", "-1", "--format=%(trailers:key=Gitnotes-Source,valueonly)", "main"); strings.TrimSpace(got) != "browser" {
		t.Fatalf("trailer through git: %q", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(r.dir, "objects", "*", "tmp_obj_*"))
	if len(leftovers) != 0 {
		t.Fatal("temporary files remain:", leftovers)
	}
}

// messageCases are messages whose trailer handling exercises Git's rules.
var messageCases = []struct {
	subject, body string
	trailers      []gitobj.Trailer
}{
	{"Save note", "", []gitobj.Trailer{{"Gitnotes-Source", "browser"}, {"Gitnotes-User", "u1"}, {"Gitnotes-Client", "c"}}},
	{"Save note", "", nil},
	{"  Spaces around  \t", "\n\n  body  \n\n\n second   \n\n", nil},
	{"Subject", "Body text\n\nKey: existing", []gitobj.Trailer{{"Gitnotes-Source", "mcp"}}},
	{"Subject", "Body text\nKey: not a block alone", []gitobj.Trailer{{"Gitnotes-Source", "mcp"}}},
	{"Subject", "Key:value\nOther :  spaced  \n  continued here", []gitobj.Trailer{{"New", "v"}}},
	{"Subject", "", []gitobj.Trailer{{"A", "1"}, {"A", "1"}, {"a", "1"}, {"A", "2"}, {"B", "1"}, {"A", "2"}}},
	{"Subject", "", []gitobj.Trailer{{"Gitnotes-Operation", "op"}, {"Gitnotes-Request", "op"}}},
	{"Subject", "", []gitobj.Trailer{{"Key", ""}, {"My Key", "v"}, {"K=x", "y"}, {"Ends-", "z"}}},
	{"Subject", "", []gitobj.Trailer{{"Key", "  padded  "}, {"Multi", "line\nvalue"}}},
	{"Subject", "text\n\nSigned-off-by: A <a>\nnot a trailer\nnot either", []gitobj.Trailer{{"Gitnotes-Source", "cli"}}},
	{"Subject", "text\n\nSigned-off-by: A <a>\nnot a trailer\nnot either\nnor this", []gitobj.Trailer{{"Gitnotes-Source", "cli"}}},
	{"Subject", "text\n\n(cherry picked from commit abc)\nfree text", []gitobj.Trailer{{"X", "y"}}},
	{"Subject", "# a comment line\nbody\n# trailing comment", []gitobj.Trailer{{"X", "y"}}},
	{"Subject", "body\n\nKey: v\n# comment in block\nOther: w", []gitobj.Trailer{{"X", "y"}}},
	{"Subject", "body\n---\nKey: after divider", []gitobj.Trailer{{"X", "y"}}},
	{"Subject\nsecond subject line", "Key: v", []gitobj.Trailer{{"X", "y"}}},
	{"Subject", "Conflicts:\n\tfile.md\n", []gitobj.Trailer{{"X", "y"}}},
	{"Subject", "body\n# ------------------------ >8 ------------------------\nKey: below scissors", []gitobj.Trailer{{"X", "y"}}},
	{"Subject\r", "crlf body\r\nKey: v\r", []gitobj.Trailer{{"X", "y\r"}}},
	{"Subject", "body\v\n\fform feed", []gitobj.Trailer{{"X", "\vy"}}},
	{"Gr\u00fc\u00dfe", "\u00c4Key: v\nKey: \u00c4\u00d6", []gitobj.Trailer{{"Key", "\u00e4\u00f6"}, {"KEY", "\u00e4\u00f6"}}},
	{"Subject", "Key: V", []gitobj.Trailer{{"KEY", "v"}, {"Ke", "x"}, {"Key-Longer", "x"}}},
}

// gitCommitMessage returns the message "git commit -m subject [-m body]
// --trailer..." stores.
func gitCommitMessage(t *testing.T, r *repo, subject, body string, trailers []gitobj.Trailer) (string, error) {
	args := []string{"commit", "-q", "--allow-empty", "-m", subject}
	if body != "" {
		args = append(args, "-m", body)
	}
	for _, tr := range trailers {
		args = append(args, "--trailer", tr.Key+": "+tr.Value)
	}
	if out, err := r.try(nil, nil, args...); err != nil {
		return out, err
	}
	raw := r.run(nil, nil, "cat-file", "commit", "HEAD")
	_, message, _ := strings.Cut(raw, "\n\n")
	return message, nil
}

func TestCleanMessageMatchesGitCommit(t *testing.T) {
	r := newRepo(t, false)
	for i, c := range messageCases {
		want, err := gitCommitMessage(t, r, c.subject, c.body, c.trailers)
		if err != nil {
			t.Fatalf("case %d: git commit: %v %s", i, err, want)
		}
		if got := gitobj.CleanMessage(c.subject, c.body, c.trailers); got != want {
			t.Errorf("case %d:\n got %q\nwant %q", i, got, want)
		}
	}
}

func TestTrailersMatchInterpretTrailersParse(t *testing.T) {
	r := newRepo(t, false)
	messages := []string{"Subject\n"}
	for _, c := range messageCases {
		messages = append(messages, c.subject+"\n\n"+c.body+"\n", gitobj.CleanMessage(c.subject, c.body, c.trailers))
	}
	for i, message := range messages {
		want := r.run(nil, []byte(message), "interpret-trailers", "--parse", "--no-divider")
		var got strings.Builder
		for _, tr := range gitobj.Trailers(message) {
			fmt.Fprintf(&got, "%s: %s\n", tr.Key, tr.Value)
		}
		if got.String() != want {
			t.Errorf("message %d %q:\n got %q\nwant %q", i, message, got.String(), want)
		}
	}
}

// Random messages built from the fragments Git's trailer rules treat
// specially must clean up exactly as git commit does.
func TestCleanMessageMatchesGitCommitOnRandomMessages(t *testing.T) {
	r := newRepo(t, false)
	fragments := []string{"text", "Key: v", "key: V", "Other: w", "  continuation", "\tindented", "#comment", "", "   ", "Signed-off-by: A <a>",
		"(cherry picked from commit 0)", "---", "Conflicts:", "\tpath", "a:b", "Long Key: x", "Key=v", "-: dash", "\u00c4\u00d6: \u00fc"}
	keys := []string{"Key", "key", "Other", "Gitnotes-Source", "K", "Long Key", "\u00c4\u00d6", "Signed-off-by"}
	values := []string{"v", "V", "w", "", " x ", "a:b"}
	rng := rand.New(rand.NewPCG(1, 2))
	pick := func(list []string) string { return list[rng.IntN(len(list))] }
	paragraph := func() string {
		var lines []string
		for range rng.IntN(5) {
			lines = append(lines, pick(fragments))
		}
		return strings.Join(lines, "\n")
	}
	compared := 0
	for i := range 150 {
		subject := pick([]string{"Subject", "Subject:", "Key: in subject", " pad "})
		var body []string
		for range rng.IntN(4) {
			body = append(body, paragraph())
		}
		var trailers []gitobj.Trailer
		for range rng.IntN(4) {
			trailers = append(trailers, gitobj.Trailer{Key: pick(keys), Value: pick(values)})
		}
		bodyText := strings.Join(body, "\n\n")
		want, err := gitCommitMessage(t, r, subject, bodyText, trailers)
		if err != nil {
			continue // Git refuses some messages, such as one cleaned to nothing.
		}
		if got := gitobj.CleanMessage(subject, bodyText, trailers); got != want {
			t.Fatalf("random case %d: subject %q body %q trailers %q:\n got %q\nwant %q", i, subject, bodyText, trailers, got, want)
		}
		parsed := r.run(nil, []byte(want), "interpret-trailers", "--parse", "--no-divider")
		var ours strings.Builder
		for _, tr := range gitobj.Trailers(want) {
			fmt.Fprintf(&ours, "%s: %s\n", tr.Key, tr.Value)
		}
		if ours.String() != parsed {
			t.Fatalf("random case %d trailers of %q:\n got %q\nwant %q", i, want, ours.String(), parsed)
		}
		compared++
	}
	if compared < 140 {
		t.Fatalf("only %d of 150 random messages were compared", compared)
	}
}

// ValidName refuses exactly the names git fsck --strict refuses as
// spellings of .git (hasDotgit), which it checks as NTFS and HFS+ read them
// whatever the platform: Git itself is the oracle.
func TestValidNameRefusesWhatGitFsckTakesForDotGit(t *testing.T) {
	refused := []string{".git", ".GIT", ".Git", "git~1", "GIT~1", "Git~1", "git~1 ", "git~1.", "git~1. .", "git~1:x", "git~1:$DATA",
		".git ", ".git.", ".git . ", ".git:x", ".GIT:$DATA", "\u200c.git", ".\u200cgit", ".g\u200cit", ".gI\u200cT", ".git\u200d", "\ufeff.git\u206a", "\u202a.G\u202eIT\u200f",
		".git\\foobar", "x\\git~1", "x\\.GIT", ".\\.GIT\\foobar", "a\\b\\git~1 .", ".git\xff", ".git\ufffe", ".git\uffff"}
	kept := []string{"git~2", "git~1x", "git~10", " git~1", "xgit~1", "git~1\u200c", "\u200cgit~1", "git\u200c~1", ".gitx", ".git x", "git", ".github",
		".gitignore", ".gitmodules", "x.git", ".g\u0131t", "\u00a0.git", ".git\u200b", ".git\ufffd", ".git\U0001fffe", "a\\b", "\\", ".gitnotes-trash.json"}
	sig := gitobj.Signature{Name: "ForgetMeNote", Email: "forgetmenote@localhost", When: 1700000000, TZ: "+0000"}
	for _, names := range [][]string{refused, kept} {
		for _, name := range names {
			r := newRepo(t, true)
			blob, err := gitobj.WriteLoose(r.dir, "blob", []byte("x\n"))
			if err != nil {
				t.Fatal(err)
			}
			tree, err := gitobj.WriteLoose(r.dir, "tree", gitobj.EncodeTree([]gitobj.TreeEntry{{Name: name, Mode: gitobj.ModeFile, ID: blob}}))
			if err != nil {
				t.Fatal(err)
			}
			commit, err := gitobj.WriteLoose(r.dir, "commit", gitobj.EncodeCommit(gitobj.Commit{Tree: tree, Author: sig, Committer: sig, Message: "x\n"}))
			if err != nil {
				t.Fatal(err)
			}
			r.run(nil, nil, "update-ref", "refs/heads/main", commit.String())
			out, err := r.try(nil, nil, "fsck", "--strict", "--no-dangling", "--no-progress")
			if fsckOK := err == nil; gitobj.ValidName(name) != fsckOK {
				t.Errorf("ValidName(%q) = %v, but git fsck --strict says %v: %s", name, gitobj.ValidName(name), fsckOK, out)
			}
		}
	}
}
