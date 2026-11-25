package gitexport

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"forgetmenote/internal/benchfixture"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// TestF2RoundTrip materializes the F2 benchmark notebook as a Git repository,
// imports it, verifies it, exports it and verifies the export against the
// store: the same commits, trees and blobs, and git fsck --strict passes.
// It runs a 2,000-revision F2 by default; FMN_GITEXPORT_F2=full runs the
// full shape (2,000 notes, 200 images, 20,000 revisions).
func TestF2RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("large round trip")
	}
	o := benchfixture.NotebookOptions{Notes: 200, Assets: 20, Revisions: 2000, MinAssetBytes: 4 << 10, MaxAssetBytes: 16 << 10}
	if os.Getenv("FMN_GITEXPORT_F2") == "full" {
		o = benchfixture.NotebookOptions{}
	}
	revs, err := benchfixture.Notebook(o)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	source := filepath.Join(t.TempDir(), "f2.git")
	started := time.Now()
	fixture, err := benchfixture.WriteGit(e.ctx, source, revs, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("materialized %d revisions in %v", fixture.Revisions, time.Since(started).Round(time.Millisecond))

	n := e.store.Notebook("f2")
	started = time.Now()
	head, err := ImportGitDir(e.ctx, n, source)
	if err != nil {
		t.Fatal(err)
	}
	if head.Commit.String() != fixture.Head || head.Seq != int64(fixture.Revisions) {
		t.Fatalf("imported head %+v, fixture %s with %d revisions", head, fixture.Head, fixture.Revisions)
	}
	t.Logf("imported in %v", time.Since(started).Round(time.Millisecond))

	started = time.Now()
	rep, err := Verify(e.ctx, n, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("verified %d commits, %d trees and %d blobs in %v", rep.Commits, rep.Trees, rep.Blobs, time.Since(started).Round(time.Millisecond))
	if rep.Commits != fixture.Revisions {
		t.Fatal("commits:", rep.Commits)
	}

	started = time.Now()
	exported, err := e.ex.Sync(e.ctx, "f2")
	if err != nil || exported != head {
		t.Fatal(exported, err)
	}
	t.Logf("exported in %v", time.Since(started).Round(time.Millisecond))
	repo := e.ex.Path("f2")
	started = time.Now()
	if err := e.ex.Maintain(e.ctx, "f2"); err != nil {
		t.Fatal(err)
	}
	t.Logf("repacked and checked the export in %v", time.Since(started).Round(time.Millisecond))
	if _, err := Verify(e.ctx, n, repo); err != nil {
		t.Fatal("the export:", err)
	}
	if git(t, repo, "rev-parse", "main") != fixture.Head {
		t.Fatal("the export's head differs")
	}
	// Notes keep the identities their fmn-id declares.
	files, err := n.Files(e.ctx, gitobj.ID{})
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if filepath.Ext(f.Path) != ".md" || checked == 20 {
			continue
		}
		content, _, err := n.Read(e.ctx, gitobj.ID{}, f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if id := declaredID(f.Path, content); id != "" {
			if f.EntryID != id {
				t.Fatalf("%s has entry %s, fmn-id %s", f.Path, f.EntryID, id)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no notes with fmn-ids")
	}
	// One new revision on the large history exports on its own.
	for _, f := range files {
		if filepath.Ext(f.Path) == ".md" {
			content, _, err := n.Read(e.ctx, gitobj.ID{}, f.Path)
			if err != nil {
				t.Fatal(err)
			}
			next := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{{Path: f.Path, Content: append(content, "One more line.\n"...)}}}).Head
			started = time.Now()
			if got, err := e.ex.Sync(e.ctx, "f2"); err != nil || got != next {
				t.Fatal(got, err)
			}
			t.Logf("exported one new revision in %v", time.Since(started).Round(time.Microsecond))
			break
		}
	}
}
