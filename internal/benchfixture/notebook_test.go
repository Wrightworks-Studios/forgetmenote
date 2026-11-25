package benchfixture

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"forgetmenote/internal/workspace/workspacetest"
)

var smallNotebook = NotebookOptions{Seed: 11, Notes: 140, Assets: 8, Revisions: 1800, MinAssetBytes: 4096, MaxAssetBytes: 16384}

func runNotebook(t *testing.T, o NotebookOptions) (*notebookSim, []Revision) {
	t.Helper()
	if err := o.defaults(); err != nil {
		t.Fatal(err)
	}
	sim := newNotebookSim(o)
	var revs []Revision
	sim.run(func(r Revision) bool { revs = append(revs, r); return true })
	return sim, revs
}

func replay(revs []Revision) map[string][]byte {
	files := map[string][]byte{}
	for _, r := range revs {
		for _, c := range r.Changes {
			if c.OldPath != "" {
				delete(files, c.OldPath)
			}
			if c.Content == nil {
				delete(files, c.Path)
			} else {
				files[c.Path] = c.Content
			}
		}
	}
	return files
}

// reencode decodes raw into v and checks encoding/json writes the same bytes.
func reencode(t *testing.T, name string, raw []byte, v any, newline bool) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	again, _ := json.MarshalIndent(v, "", "  ")
	if newline {
		again = append(again, '\n')
	}
	if !bytes.Equal(again, raw) {
		t.Fatalf("%s is not in the workspace format:\n%s\nworkspace writes\n%s", name, raw, again)
	}
}

var (
	wikiLink     = regexp.MustCompile(`\[\[([^\[\]|\n]+)(?:\|[^\[\]\n]+)?\]\]`)
	markdownLink = regexp.MustCompile(`(!?)\[[^\]\n]*\]\(([^\s()]+)\)`)
)

func TestNotebookShapeHistoryAndFormats(t *testing.T) {
	o := smallNotebook
	if err := o.defaults(); err != nil {
		t.Fatal(err)
	}
	_, revs := runNotebook(t, o)
	if len(revs) != o.Revisions || revs[0].Kind != KindInit {
		t.Fatalf("%d revisions", len(revs))
	}
	kinds := map[string]int{}
	commits := map[string]int{}
	titles := map[string]bool{}
	start, end := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC)
	for i, r := range revs {
		kinds[r.Kind]++
		commits[r.Commit] = r.Seq
		if r.Time.Before(start) || !r.Time.Before(end) || i > 0 && !r.Time.After(revs[i-1].Time) || (i > 0) != (len(r.Changes) > 0) {
			t.Fatalf("revision %d at %v with %d changes", r.Seq, r.Time, len(r.Changes))
		}
		if r.Kind == KindNoteCreate {
			meta, err := workspace.ParseMetadata(r.Changes[0].Content)
			title, _ := workspace.NoteSummary(r.Changes[0].Content)
			if err != nil || meta.Title != "" && meta.Title != title {
				t.Fatalf("created note %s: %v %q %q", r.Changes[0].Path, err, meta.Title, title)
			}
			titles[strings.ToLower(title)] = true
		}
	}
	if span := revs[len(revs)-1].Time.Sub(revs[0].Time); span < 15*30*24*time.Hour {
		t.Fatalf("history spans only %v", span)
	}
	if kinds[KindNoteCreate] <= o.Notes || kinds[KindAssetAdd] != o.Assets {
		t.Fatalf("created %d notes and %d assets", kinds[KindNoteCreate], kinds[KindAssetAdd])
	}
	for _, k := range []string{KindNoteEdit, KindFrontMatter, KindComment, KindCommentReply, KindCommentResolve, KindMove, KindLock, KindUnlock, KindTrash, KindRestore, KindEmptyTrash, KindAssetReplace} {
		if kinds[k] == 0 {
			t.Fatalf("no %s revision: %v", k, kinds)
		}
	}

	files := replay(revs)
	var index workspace.TrashIndex
	reencode(t, trashIndexPath, files[trashIndexPath], &index, true)
	var policy struct {
		Version int      `json:"version"`
		Paths   []string `json:"paths"`
	}
	reencode(t, locksPath, files[locksPath], &policy, true)
	if policy.Version != 1 || !slices.IsSorted(policy.Paths) {
		t.Fatalf("lock policy %+v", policy)
	}
	inTrash := map[string]bool{}
	for _, rec := range index.Records {
		if !rec.Emptied && !rec.Restored {
			inTrash[rec.Path] = true
		}
		for p, history := range rec.History {
			for _, c := range history {
				if seq, ok := commits[c]; !ok || p == "" {
					t.Fatalf("Trash record %s cites unknown commit %s for %s (seq %d)", rec.ID, c, p, seq)
				}
			}
		}
		if len(rec.History[rec.OriginalPath]) == 0 {
			t.Fatalf("Trash record %s has no history at its original path", rec.ID)
		}
	}
	ids, notes, live, assets := map[string]bool{}, map[string]bool{}, 0, 0
	for p, content := range files {
		switch {
		case strings.HasSuffix(p, ".md"):
			id, err := workspace.NoteID(content)
			if err != nil || !workspace.ValidNoteID(id) || ids[id] {
				t.Fatalf("%s: fmn-id %q %v (duplicate %v)", p, id, err, ids[id])
			}
			ids[id], notes[p] = true, true
			if workspace.IsTrash(p) {
				if !inTrash[p] {
					t.Fatalf("%s is in Trash without a record", p)
				}
				continue
			}
			live++
			if len(content) < o.MinNoteBytes || len(content) > o.MaxNoteBytes {
				t.Fatalf("%s has %d bytes", p, len(content))
			}
		case strings.HasPrefix(p, "assets/"):
			assets++
			img, err := png.Decode(bytes.NewReader(content))
			if err != nil || len(content) < o.MinAssetBytes || len(content) > o.MaxAssetBytes || img.Bounds().Dx() != 32 {
				t.Fatalf("%s: %d bytes, %v", p, len(content), err)
			}
		}
	}
	for p := range inTrash {
		if !notes[p] {
			t.Fatalf("Trash record %s has no file", p)
		}
	}
	if live != o.Notes || len(inTrash) == 0 || assets != o.Assets {
		t.Fatalf("%d live notes, %d in Trash, %d assets", live, len(inTrash), assets)
	}
	discussions := 0
	for p := range notes {
		raw, ok := files[commentPath(p)]
		if !ok {
			continue
		}
		discussions++
		var comments []workspace.Comment
		reencode(t, p+" comments", raw, &comments, false)
		if len(comments) == 0 || comments[0].ID == "" || comments[0].Author == "" {
			t.Fatalf("%s: comments %+v", p, comments)
		}
	}
	for p := range files {
		if strings.HasPrefix(p, "comments/") && !slices.ContainsFunc(slices.Collect(func(yield func(string) bool) {
			for n := range notes {
				if !yield(commentPath(n)) {
					return
				}
			}
		}), func(c string) bool { return c == p }) {
			t.Fatalf("orphaned discussion %s", p)
		}
	}
	if discussions == 0 {
		t.Fatal("no note has comments")
	}

	wiki, wikiLive, noteLinks, noteLinksLive := 0, 0, 0, 0
	liveTitles := map[string]bool{}
	for p := range notes {
		if !workspace.IsTrash(p) {
			title, _ := workspace.NoteSummary(files[p])
			liveTitles[strings.ToLower(title)] = true
		}
	}
	for p := range notes {
		for _, m := range wikiLink.FindAllStringSubmatch(string(files[p]), -1) {
			wiki++
			if !titles[strings.ToLower(m[1])] {
				t.Fatalf("%s links unknown page %q", p, m[1])
			}
			if liveTitles[strings.ToLower(m[1])] {
				wikiLive++
			}
		}
		for _, m := range markdownLink.FindAllStringSubmatch(string(files[p]), -1) {
			target, err := url.PathUnescape(m[2])
			target = path.Join(path.Dir(p), target)
			if err != nil || strings.HasPrefix(target, "..") {
				t.Fatalf("%s: bad link %q", p, m[2])
			}
			if m[1] == "!" {
				if _, ok := files[target]; !ok {
					t.Fatalf("%s embeds missing image %s", p, target)
				}
				continue
			}
			noteLinks++
			if notes[target] {
				noteLinksLive++
			}
		}
	}
	if wiki == 0 || noteLinks == 0 || wikiLive*10 < wiki*8 || noteLinksLive*10 < noteLinks*7 {
		t.Fatalf("wiki links %d (%d live), note links %d (%d resolve)", wiki, wikiLive, noteLinks, noteLinksLive)
	}

	_, again := runNotebook(t, o)
	o.Seed++
	_, other := runNotebook(t, o)
	if again[len(again)-1].Commit != revs[len(revs)-1].Commit || other[len(other)-1].Commit == revs[len(revs)-1].Commit {
		t.Fatal("notebook is not a deterministic function of the seed")
	}
}

// TestNotebookMatchesWorkspaceBehaviour imports the materialized notebook into
// the notes store, as an import of Git history does,
// opens it with today's workspace and checks that the app reads every
// committed format and computes the same Trash histories, link rewrites and
// anchors the generator predicts.
func TestNotebookMatchesWorkspaceBehaviour(t *testing.T) {
	o := smallNotebook
	sim, revs := runNotebook(t, o)
	repo := filepath.Join(t.TempDir(), "notebook.git")
	result, err := WriteGit(t.Context(), repo, slices.Values(revs), true)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(t.Context(), repo, "fsck", "--strict", "--no-dangling"); err != nil {
		t.Fatal(out, err)
	}
	ws, nb := importNotebook(t, repo)
	ctx := t.Context()

	entries, err := ws.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, e := range entries {
		listed[e.Path] = true
	}
	for _, n := range sim.live {
		if !listed[n.path] {
			t.Fatalf("workspace does not list %s", n.path)
		}
	}
	trash, err := ws.ListTrash(ctx)
	if err != nil || len(trash.Entries) != len(sim.trashed) || trash.Revision != result.Head {
		t.Fatalf("ListTrash: %d entries, %v; generator has %d", len(trash.Entries), err, len(sim.trashed))
	}
	locks, err := ws.Locks(ctx, "")
	if err != nil || len(locks.Rules) != len(sim.locks) {
		t.Fatalf("Locks: %+v %v; generator has %v", locks, err, sim.locks)
	}
	for _, n := range sim.live {
		d, err := ws.Comments(ctx, n.path)
		if err != nil || len(d.Comments) != len(n.comments) {
			t.Fatalf("Comments(%s): %d, %v; generator has %d", n.path, len(d.Comments), err, len(n.comments))
		}
	}

	// Moves: the app rewrites the same links the generator rewrites.
	sim.head = result.Head
	moved := 0
	for _, n := range slices.Clone(sim.live) {
		if moved == 3 {
			break
		}
		var r Revision
		r.Time = time.Now()
		from := n.path
		if !sim.editable(from) || !sim.moveNote(&r, Actors[actorAda], n) {
			continue
		}
		head := sim.head
		to := r.Changes[slices.IndexFunc(r.Changes, func(c Change) bool { return c.OldPath == from })].Path
		res, err := ws.Move(ctx, from, to, head, "ada")
		if err != nil {
			t.Fatalf("Move %s -> %s: %v", from, to, err)
		}
		for _, c := range r.Changes {
			doc, err := ws.Read(ctx, c.Path, "")
			if strings.HasPrefix(c.Path, "comments/") {
				continue
			}
			if err != nil || !bytes.Equal(doc.Content, c.Content) {
				t.Fatalf("after moving %s, %s differs from the prediction: %v\n%s\nwant\n%s", from, c.Path, err, doc.Content, c.Content)
			}
		}
		changed := changedPaths(t, nb, head, res.Revision)
		var predicted []string
		for _, c := range r.Changes {
			predicted = append(predicted, c.Path)
			if c.OldPath != "" {
				predicted = append(predicted, c.OldPath)
			}
		}
		slices.Sort(predicted)
		if !slices.Equal(changed, predicted) {
			t.Fatalf("move of %s changed %v, generator predicted %v", from, changed, predicted)
		}
		sim.committed(res.Revision)
		moved++
	}
	if moved == 0 {
		t.Fatal("no note could be moved")
	}

	// Trash: the app records the histories the generator records, for a moved,
	// a restored and an ordinary note.
	var restored, ordinary *nbNote
	for _, n := range sim.live {
		if n.restored != nil && restored == nil && sim.editable(n.path) {
			restored = n
		} else if ordinary == nil && sim.editable(n.path) && len(n.events) > 3 {
			ordinary = n
		}
	}
	if restored == nil {
		t.Fatal("no restored note is live")
	}
	for _, n := range []*nbNote{restored, ordinary} {
		status, _ := ws.Status(ctx)
		want := n.history()
		if _, err = ws.TrashNotes(ctx, []string{n.path}, status.Revision, "ada"); err != nil {
			t.Fatalf("TrashNotes(%s): %v", n.path, err)
		}
		policy, err := ws.TrashPolicy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := policy.Records[len(policy.Records)-1].History
		if !mapsEqual(got, want) {
			t.Fatalf("Trash history of %s:\nworkspace %v\ngenerator %v", n.path, got, want)
		}
	}

	// Anchors: the app accepts the passages the generator quotes.
	anchored := 0
	for _, n := range sim.live {
		if anchored == 10 || n == restored || n == ordinary || !sim.editable(n.path) {
			continue
		}
		a := sim.anchor(n)
		if a == nil {
			continue
		}
		d, err := ws.Comments(ctx, n.path)
		if err != nil {
			t.Fatal(err)
		}
		anchor := workspace.CommentAnchor{Quote: a.Quote, Prefix: a.Prefix, Suffix: a.Suffix}
		if _, err = ws.AddComment(ctx, n.path, "Check this passage.", d.Revision, "ada", workspace.CommentOptions{Anchor: &anchor}); err != nil {
			t.Fatalf("anchor %+v in %s: %v", anchor, n.path, err)
		}
		anchored++
	}
	if anchored < 5 {
		t.Fatalf("only %d anchors", anchored)
	}
}

// importNotebook imports the history of the Git repository gitDir into a
// new notebook kept in the notes store and opens it.
func importNotebook(t *testing.T, gitDir string) (*workspace.Store, *notestore.Notebook) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bench")
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.OpenDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	notes, err := notestore.Open(t.Context(), pgtest.Dir(t, dir), blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		t.Fatal(err)
	}
	defer notes.Close()
	if _, err = gitexport.ImportGitDir(t.Context(), notes.Notebook(filepath.Base(dir)), gitDir); err != nil {
		t.Fatal(err)
	}
	return workspacetest.New(t, dir, nil)
}

// changedPaths lists the paths revision to changed since from, sorted, a
// rename as its source and destination, as git diff --name-only
// --no-renames does.
func changedPaths(t *testing.T, nb *notestore.Notebook, from, to string) []string {
	t.Helper()
	var ids [2]gitobj.ID
	for i, revision := range []string{from, to} {
		id, err := gitobj.ParseID(revision)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	changes, err := nb.Changes(t.Context(), ids[0], ids[1])
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range changes {
		paths = append(paths, c.Path)
		if c.OldPath != "" {
			paths = append(paths, c.OldPath)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

func mapsEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

func TestNotebookSmallestOptions(t *testing.T) {
	if _, err := Notebook(NotebookOptions{Notes: 20, Assets: 1, Revisions: 40}); err == nil {
		t.Fatal("accepted fewer revisions than notes and assets need")
	}
	o := NotebookOptions{Notes: 20, Assets: 1, Revisions: 120, MinNoteBytes: 1024, MaxNoteBytes: 4096, MinAssetBytes: 256, MaxAssetBytes: 512}
	seq, err := Notebook(o)
	if err != nil {
		t.Fatal(err)
	}
	files := replay(collect(seq))
	for p, content := range files {
		if strings.HasSuffix(p, ".md") && !workspace.IsTrash(p) && (len(content) < 1024 || len(content) > 4096) || strings.HasPrefix(p, "assets/") && (len(content) < 256 || len(content) > 512) {
			t.Fatalf("%s has %d bytes", p, len(content))
		}
	}
}
