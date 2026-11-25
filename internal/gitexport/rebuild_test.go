package gitexport

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

const secret = "sk-live-0123456789-LEAKED"

// leakyHistory is history with a credential that later commits keep; with
// purged set, the same history as a purge rewrites it: the credential
// replaced and each rewritten commit marked with its purge.
func leakyHistory(t *testing.T, e *env, purge string) (notestore.Head, []gitobj.ID) {
	t.Helper()
	n := e.create("notes")
	history(t, n)
	text, mark := secret, []gitobj.Trailer(nil)
	if purge != "" {
		text, mark = "[purged]", []gitobj.Trailer{{Key: "Gitnotes-Purge", Value: purge}}
	}
	var commits []gitobj.ID
	for i, files := range [][]notestore.FileChange{
		{write("Inbox/keys.md", note("33333333333333333333333333333333", "Keys", "The key is "+text+".\n"))},
		{write("Inbox/after.md", "Written after the leak.\n")},
	} {
		sig := bob
		sig.When += int64(i) * 60
		commits = append(commits, apply(t, n, notestore.ChangeSet{Author: sig, Subject: "Update notes", Files: files, Trailers: mark}).Head.Commit)
	}
	head, err := n.Head(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return head, commits
}

// Only a purge's rebuild replaces exported history. It drops every object
// the rewritten branch does not reach and writes the flag the backup worker
// needs to replace the remote branch once.
func TestRebuildReplacesRewrittenHistoryAndFlagsThePurge(t *testing.T) {
	e := newEnv(t)
	oldHead, leaked := leakyHistory(t, e, "")
	if _, err := e.ex.Sync(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	repo := e.ex.Path("notes")
	if err := e.ex.Maintain(e.ctx, "notes"); err != nil {
		t.Fatal(err) // the leaked objects are packed as well as loose
	}
	if _, err := e.ex.Sync(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}

	// The purge rewrites the notebook's history.
	if err := e.store.Delete(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	newHead, rewritten := leakyHistory(t, e, "purge-1")
	if _, err := e.ex.Sync(e.ctx, "notes"); !errors.Is(err, ErrDiverged) {
		t.Fatal("an export of history the notebook no longer has:", err)
	}
	if refOf(t, repo) != oldHead.Commit {
		t.Fatal("a refused export moved the branch")
	}
	if err := e.ex.Rebuild(e.ctx, "notes", "purge-2"); err == nil {
		t.Fatal("a rebuild for a purge no revision records")
	}
	if _, ok, _ := ReadPurge(repo); ok || refOf(t, repo) != oldHead.Commit {
		t.Fatal("a refused rebuild changed the export")
	}

	if err := e.ex.Rebuild(e.ctx, "notes", "purge-1"); err != nil {
		t.Fatal(err)
	}
	if refOf(t, repo) != newHead.Commit {
		t.Fatal("the branch is not the rewritten head")
	}
	git(t, repo, "fsck", "--strict", "--no-dangling")
	flag, ok, err := ReadPurge(repo)
	if err != nil || !ok {
		t.Fatal("purge flag:", ok, err)
	}
	slices.SortFunc(leaked, func(a, b gitobj.ID) int { return bytes.Compare(a[:], b[:]) })
	if flag.Purge != "purge-1" || flag.Head != rewritten[1] || !slices.Equal(flag.Replaces, leaked) {
		t.Fatalf("flag %+v, want head %s replacing %v", flag, rewritten[1], leaked)
	}
	all := git(t, repo, "cat-file", "--batch-all-objects", "--unordered", "--batch")
	if strings.Contains(all, secret) || strings.Contains(all, oldHead.Commit.String()) {
		t.Fatal("the rebuilt export still holds purged content")
	}
	if count := git(t, repo, "count-objects", "-v"); !strings.HasPrefix(count, "count: 0\n") {
		t.Fatal("loose objects remain:", count)
	}

	// A rebuild that runs again keeps what the first one replaced.
	if err := e.ex.Rebuild(e.ctx, "notes", "purge-1"); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := ReadPurge(repo); !slices.Equal(again.Replaces, leaked) {
		t.Fatal("a repeated rebuild lost the replaced commits:", again.Replaces)
	}
	// The export carries on from the rewritten history.
	next := apply(t, e.store.Notebook("notes"), notestore.ChangeSet{Files: []notestore.FileChange{write("Inbox/later.md", "Later.\n")}}).Head
	if got, err := e.ex.Sync(e.ctx, "notes"); err != nil || got.Commit != next.Commit {
		t.Fatal(got, err)
	}
}

// A bundle larger than its limit stops Git and fails, as notebook exports
// always have; Remove deletes the export, which the next export recreates.
func TestBundleLimitAndRemove(t *testing.T) {
	e := newEnv(t)
	head := history(t, e.create("notes"))
	var out bytes.Buffer
	if err := e.ex.Bundle(e.ctx, "notes", &out, 64); !errors.Is(err, ErrBundleLimit) || out.Len() > 64 {
		t.Fatal("an oversized bundle:", out.Len(), err)
	}
	if err := e.ex.Bundle(e.ctx, "missing", &out, 64); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a bundle of a missing notebook:", err)
	}
	if err := e.ex.Remove(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.ex.Path("notes")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the removed export remains:", err)
	}
	if got, err := e.ex.Sync(e.ctx, "notes"); err != nil || got != head {
		t.Fatal(got, err)
	}
	git(t, e.ex.Path("notes"), "fsck", "--strict", "--no-dangling")
	if _, err := e.ex.Sync(e.ctx, "../escape"); err == nil {
		t.Fatal("a notebook ID outside the export directory")
	}
}

// Rebuild replaces the purge flag only under the purge lock, which the
// backup worker holds to remove a flag it spent, so the worker never removes
// a newer purge's flag.
func TestRebuildWritesThePurgeFlagUnderItsLock(t *testing.T) {
	e := newEnv(t)
	oldHead, _ := leakyHistory(t, e, "")
	if _, err := e.ex.Sync(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	leakyHistory(t, e, "purge-1")
	repo := e.ex.Path("notes")
	lock, err := os.OpenFile(filepath.Join(repo, PurgeLock), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.ex.Rebuild(e.ctx, "notes", "purge-1") }()
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatal("the rebuild did not wait for the purge lock:", err)
	default:
	}
	if _, ok, _ := ReadPurge(repo); ok || refOf(t, repo) != oldHead.Commit {
		t.Fatal("the flag or the branch changed under the worker's lock")
	}
	lock.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if flag, ok, err := ReadPurge(repo); err != nil || !ok || flag.Purge != "purge-1" {
		t.Fatal("purge flag:", flag, ok, err)
	}
	// A canceled rebuild stops waiting.
	lock, err = os.OpenFile(filepath.Join(repo, PurgeLock), os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(e.ctx, 300*time.Millisecond)
	defer cancel()
	if err := e.ex.Rebuild(ctx, "notes", "purge-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("a rebuild waiting past its context:", err)
	}
}

// A rebuild writes the whole history again as one pack and computes its
// deltas: history whose later revisions bring objects back (an undone edit,
// a file deleted and written again) rebuilds and checks clean, the versions
// of a note are stored as deltas, and a pack a stopped rebuild left behind
// is cleared.
func TestRebuildOfHistoryThatBringsObjectsBack(t *testing.T) {
	e := newEnv(t)
	n := e.create("notes")
	page := strings.Repeat("A line every version of the draft keeps.\n", 60)
	draft := write("Inbox/draft.md", page+"The key is "+secret+".\n")
	for _, files := range [][]notestore.FileChange{
		{draft, write("Inbox/other.md", "Other.\n")},
		{write("Inbox/draft.md", page+"Rewritten.\n")},
		{draft}, // undone: the first version's blob and trees again
		{{Path: "Inbox/other.md"}},
		{write("Inbox/other.md", "Other.\n")},
	} {
		apply(t, n, notestore.ChangeSet{Files: files})
	}
	if _, err := e.ex.Sync(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	repo := e.ex.Path("notes")
	stale := filepath.Join(repo, "objects", "pack", "tmp_rebuild_stale")
	if err := os.WriteFile(stale, []byte("PACK half written"), 0o644); err != nil {
		t.Fatal(err)
	}
	redact := func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
		data, err := content()
		return bytes.ReplaceAll(data, []byte(secret), []byte("[purged]")), true, err
	}
	rewritten, err := n.Rewrite(e.ctx, 2, "purge-1", redact, notestore.RewriteOptions{Actor: "owner", Reason: "a leaked key"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ex.Rebuild(e.ctx, "notes", "purge-1"); err != nil {
		t.Fatal(err)
	}
	if refOf(t, repo) != rewritten.Head.Commit {
		t.Fatal("the branch is not the rewritten head")
	}
	git(t, repo, "fsck", "--strict", "--no-dangling")
	if all := git(t, repo, "cat-file", "--batch-all-objects", "--unordered", "--batch"); strings.Contains(all, secret) {
		t.Fatal("the rebuilt export still holds purged content")
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the stopped rebuild's pack remains:", err)
	}
	if deltified(t, repo) == 0 {
		t.Fatal("the rebuilt export has no deltas")
	}
}
