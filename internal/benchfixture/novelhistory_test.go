package benchfixture

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/workspace"
)

// testNovel is a small F1-shaped note, so history tests run without the cache.
func testNovel(chapters int) []byte {
	r := newRNG(7, "test-novel")
	var b strings.Builder
	b.WriteString("---\nfmn-id: " + NovelID + "\ntitle: Test Novel\nkind: manuscript\n---\n\n# Test Novel\n")
	for c := 1; c <= chapters; c++ {
		fmt.Fprintf(&b, "\n## Chapter %d. The Test.\n", c)
		for range 4 {
			b.WriteString("\n")
			for s := range 5 {
				if s > 0 {
					b.WriteString(" ")
				}
				b.WriteString(sentence(r))
			}
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

func collect(seq func(func(Revision) bool)) []Revision {
	var out []Revision
	for r := range seq {
		out = append(out, r)
	}
	return out
}

func headings(content []byte) []string {
	var out []string
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "## ") {
			out = append(out, line)
		}
	}
	slices.Sort(out)
	return out
}

func trailer(r Revision, key string) string {
	for _, t := range r.Trailers {
		if t.Key == key {
			return t.Value
		}
	}
	return ""
}

func TestNovelHistoryReplaysExactMixFromActors(t *testing.T) {
	novel := testNovel(12)
	seq, err := NovelHistory(novel, NovelHistoryOptions{Seed: 1, Edits: 400})
	if err != nil {
		t.Fatal(err)
	}
	revs := collect(seq)
	if len(revs) != 402 || revs[0].Kind != KindInit || len(revs[0].Changes) != 0 || revs[1].Kind != KindCreate || !bytes.Equal(revs[1].Changes[0].Content, novel) {
		t.Fatalf("stream starts %v %v, has %d revisions", revs[0].Kind, revs[1].Kind, len(revs))
	}
	kinds, actors := map[string]int{}, map[string]int{}
	path, content := revs[1].Changes[0].Path, novel
	for i, r := range revs[2:] {
		kinds[r.Kind]++
		actors[r.Actor]++
		prev := revs[i+1]
		if r.Seq != i+3 || !r.Time.After(prev.Time) || len(r.Changes) != 1 || r.Commit == "" || r.Commit == prev.Commit {
			t.Fatalf("revision %d: seq %d time %v changes %d commit %q", i+3, r.Seq, r.Time, len(r.Changes), r.Commit)
		}
		c := r.Changes[0]
		if id, err := workspace.NoteID(c.Content); err != nil || id != NovelID {
			t.Fatalf("revision %d lost its fmn-id: %q %v", r.Seq, id, err)
		}
		if _, err := workspace.ParseMetadata(c.Content); err != nil {
			t.Fatalf("revision %d front matter: %v", r.Seq, err)
		}
		if _, err := parseNovel(c.Content); err != nil {
			t.Fatalf("revision %d is no longer F1-shaped: %v", r.Seq, err)
		}
		switch r.Kind {
		case KindRename:
			if c.OldPath != path || c.Path == path || !bytes.Equal(c.Content, content) || r.Subject != fmt.Sprintf("Move %q to %q", path, c.Path) {
				t.Fatalf("rename %d: %q -> %q, subject %q", r.Seq, c.OldPath, c.Path, r.Subject)
			}
		case KindChapterMove:
			if !slices.Equal(headings(c.Content), headings(content)) || bytes.Equal(c.Content, content) {
				t.Fatalf("chapter move %d changed the chapter set or nothing", r.Seq)
			}
		default:
			if c.OldPath != "" || c.Path != path || bytes.Equal(c.Content, content) {
				t.Fatalf("%s %d: path %q old %q unchanged %v", r.Kind, r.Seq, c.Path, c.OldPath, bytes.Equal(c.Content, content))
			}
		}
		switch a, _ := ActorByKey(r.Actor); {
		case r.Subject == "Checkpoint live edits to "+path:
			if a.Agent || r.AuthorName != "ForgetMeNote collaboration" || trailer(r, "Gitnotes-Source") != "collaboration" || !strings.Contains(trailer(r, "Collaboration-Receipt"), a.Account) {
				t.Fatalf("live checkpoint %d: %+v", r.Seq, r)
			}
		case a.Key == "organizer":
			if trailer(r, "Gitnotes-Source") != "jev" || trailer(r, "Gitnotes-Client") != "organizer" || len(trailer(r, "Gitnotes-Operation")) != 64 || r.AuthorEmail != Actors[actorAda].Email {
				t.Fatalf("organizer %d: %+v", r.Seq, r.Trailers)
			}
			before, _ := workspace.ParseMetadata(content)
			after, _ := workspace.ParseMetadata(c.Content)
			if r.Kind == KindFrontMatter && (before.Status != after.Status || before.Title != after.Title || slices.Equal(before.Tags, after.Tags)) {
				t.Fatalf("organizer %d changed more than tags: %+v -> %+v", r.Seq, before, after)
			}
		default:
			if trailer(r, "Gitnotes-Source") != a.Source || trailer(r, "Gitnotes-User") != a.Account || r.AuthorName != a.Name {
				t.Fatalf("%s revision %d: %+v", a.Key, r.Seq, r.Trailers)
			}
		}
		path, content = c.Path, c.Content
	}
	if text := kinds[KindWordEdit] + kinds[KindSentenceEdit]; text != 280 || kinds[KindSentenceEdit] != 70 {
		t.Fatalf("word/sentence edits %d (%d sentence), want 280 (70)", text, kinds[KindSentenceEdit])
	}
	if p := kinds[KindParagraphInsert] + kinds[KindParagraphDelete]; p != 60 || kinds[KindParagraphInsert] == 0 || kinds[KindParagraphDelete] == 0 {
		t.Fatalf("paragraph edits %v", kinds)
	}
	if kinds[KindChapterMove] != 20 || kinds[KindFrontMatter] != 20 || kinds[KindRename] != 20 {
		t.Fatalf("mix %v", kinds)
	}
	for _, a := range Actors {
		if actors[a.Key] == 0 {
			t.Fatalf("actor %s never edits: %v", a.Key, actors)
		}
	}

	again := collect(seq)
	other, _ := NovelHistory(novel, NovelHistoryOptions{Seed: 2, Edits: 400})
	if again[len(again)-1].Commit != revs[len(revs)-1].Commit || collect(other)[401].Commit == revs[401].Commit {
		t.Fatal("history is not a deterministic function of the seed")
	}
}

func TestNovelHistoryMaterializesAsGitAndFiles(t *testing.T) {
	seq, err := NovelHistory(testNovel(6), NovelHistoryOptions{Seed: 3, Edits: 80})
	if err != nil {
		t.Fatal(err)
	}
	revs := collect(seq)
	dir := filepath.Join(t.TempDir(), "repo")
	result, err := WriteGit(t.Context(), dir, seq, false)
	if err != nil {
		t.Fatal(err)
	}
	last := revs[len(revs)-1]
	if result.Head != last.Commit || result.Revisions != len(revs) {
		t.Fatalf("result %+v, want head %s", result, last.Commit)
	}
	if out, err := runGit(t.Context(), dir, "fsck", "--strict", "--no-dangling"); err != nil {
		t.Fatal(out, err)
	}
	logged, err := runGit(t.Context(), dir, "log", "--reverse", "--format=%H%x00%an%x00%cn <%ce>%x00%aI%x00%(trailers:key=Gitnotes-Source,valueonly,separator=)")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(logged, "\n")
	if len(lines) != len(revs) {
		t.Fatalf("git has %d commits, want %d", len(lines), len(revs))
	}
	for i, line := range lines {
		f, r := strings.Split(line, "\x00"), revs[i]
		if f[0] != r.Commit || f[1] != r.AuthorName || f[2] != "ForgetMeNote <forgetmenote@localhost>" || !sameGitTime(f[3], r.Time) || f[4] != trailer(r, "Gitnotes-Source") {
			t.Fatalf("commit %d: git %q, revision %+v", i+1, f, r)
		}
	}
	worktree, err := os.ReadFile(filepath.Join(dir, last.Changes[0].Path))
	if err != nil || !bytes.Equal(worktree, last.Changes[0].Content) {
		t.Fatalf("checked out note differs: %v", err)
	}

	files := filepath.Join(t.TempDir(), "files")
	if _, err = WriteFiles(files, seq); err != nil {
		t.Fatal(err)
	}
	var paths []string
	filepath.WalkDir(files, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(files, p)
			paths = append(paths, filepath.ToSlash(rel))
		}
		return err
	})
	plain, _ := os.ReadFile(filepath.Join(files, last.Changes[0].Path))
	if !slices.Equal(paths, []string{last.Changes[0].Path}) || !bytes.Equal(plain, worktree) {
		t.Fatalf("plain files %v", paths)
	}
	if _, err = WriteGit(t.Context(), dir, seq, false); err == nil {
		t.Fatal("materialized over an existing repository")
	}
}

func TestNovelHistoryOnF1(t *testing.T) {
	novel := NovelOrSkip(t)
	seq, err := NovelHistory(novel, NovelHistoryOptions{Edits: 200})
	if err != nil {
		t.Fatal(err)
	}
	revs := collect(seq)
	final := revs[len(revs)-1].Changes[0].Content
	if len(headings(final)) != 136 || len(final) < 1100000 || len(final) > 1300000 {
		t.Fatalf("F1 after 200 edits: %d chapters, %d bytes", len(headings(final)), len(final))
	}
}

func TestWriteGitAcceptsRelativeDirectory(t *testing.T) {
	seq, err := NovelHistory(testNovel(3), NovelHistoryOptions{Seed: 4, Edits: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	result, err := WriteGit(t.Context(), "relative/repo", seq, false)
	if err != nil {
		t.Fatal(err)
	}
	if head, err := runGit(t.Context(), "relative/repo", "rev-parse", "HEAD"); err != nil || head != result.Head {
		t.Fatalf("HEAD %q %v, want %s", head, err, result.Head)
	}
}

// sameGitTime compares git's strict ISO date with a revision time. Newer Git
// (2.54) spells UTC "Z", older Git (2.43) "+00:00", so compare instant and offset.
func sameGitTime(got string, want time.Time) bool {
	t, err := time.Parse(time.RFC3339, got)
	if err != nil {
		return false
	}
	_, gotOffset := t.Zone()
	_, wantOffset := want.Zone()
	return t.Equal(want) && gotOffset == wantOffset
}
