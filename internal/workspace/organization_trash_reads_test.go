package workspace

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
)

// countingBlobs is a blob store that counts the reads of each object and
// can record the keys written.
type countingBlobs struct {
	blob.Store
	mu        sync.Mutex
	reads     map[string]int
	recording bool
	written   []string
}

func (c *countingBlobs) count(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads[key]++
}

func (c *countingBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.count(key)
	return c.Store.Get(ctx, key)
}

func (c *countingBlobs) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	c.count(key)
	return c.Store.GetRange(ctx, key, offset, length)
}

func (c *countingBlobs) Put(ctx context.Context, key string, body io.ReadSeeker) error {
	c.mu.Lock()
	if c.recording {
		c.written = append(c.written, key)
	}
	c.mu.Unlock()
	return c.Store.Put(ctx, key, body)
}

// record returns the keys written while fn runs.
func (c *countingBlobs) record(fn func()) []string {
	c.mu.Lock()
	c.recording, c.written = true, nil
	c.mu.Unlock()
	fn()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recording = false
	return slices.Clone(c.written)
}

// readsOf is how many times the objects at keys were read since the last
// reset.
func (c *countingBlobs) readsOf(keys []string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, key := range keys {
		total += c.reads[key]
	}
	return total
}

func (c *countingBlobs) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = map[string]int{}
}

// newCountingTestStore is newTestStore over a blob store that counts reads,
// with the notes store's smallest caches, 1 MiB each, so a read of content
// pushed out of them reaches the blob store.
func newCountingTestStore(t *testing.T) (*Store, *countingBlobs) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	inner, err := blob.OpenDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inner.Close() })
	blobs := &countingBlobs{Store: inner, reads: map[string]int{}}
	env := &testNotebook{db: pgtest.Org(t), blobs: inner}
	notes, err := notestore.Open(context.Background(), env.db, blobs, notestore.Options{ImportEntryID: NoteEntryID, BlockCacheBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(notes.Close)
	s, err := CreateDB(context.Background(), notes, "notes", filepath.Join(dir, "projects", "notes"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	testNotebooks.Store(s, env)
	return s, blobs
}

// prose is size bytes of Markdown paragraphs unique to name, which link to
// nothing and name no note.
func prose(name string, size int) string {
	var b strings.Builder
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "Paragraph %d of %s. %s\n\n", i, name, strings.Repeat("Words without any reference. ", 64))
	}
	return b.String()
}

// readTrashed reads the notes in the Trash trashed from origins as a move's
// plan does, straight from the notes store.
func readTrashed(t *testing.T, s *Store, entries map[string]TrashEntry, origins ...string) {
	t.Helper()
	n := backendOf(s)
	files, err := n.files(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var selected []notestore.File
	for _, origin := range origins {
		i := slices.IndexFunc(files, func(f notestore.File) bool { return f.Path == entries[origin].Path })
		if i < 0 {
			t.Fatal("not in the Trash:", origin)
		}
		selected = append(selected, files[i])
	}
	if err := n.readBlobs(context.Background(), selected, func(int, []byte) {}); err != nil {
		t.Fatal(err)
	}
}

// An organization move reads each note in the Trash once, to plan which of
// them link to the moved note, and once it has committed answers with that
// plan instead of reading the Trash again: under the bound its outcome has,
// reading a large Trash again would leave a committed move's outcome
// unknown.
func TestOrganizationMoveReadsTheTrashOnce(t *testing.T) {
	s, blobs := newCountingTestStore(t)
	ctx := organizationTestContext()
	saveNote(t, s, "notes/a.md", "# A\n")
	saveNote(t, s, "dest/index.md", "# Destination\n")
	saveNote(t, s, trashLinkerOrigin, trashLinker)
	written := blobs.record(func() { saveNote(t, s, "x.md", prose("x", 32<<10)) })
	if len(written) == 0 {
		t.Fatal("x.md was not written to the blob store")
	}
	fillers := []string{"f1.md", "f2.md", "f3.md"}
	for _, p := range fillers {
		saveNote(t, s, p, prose(p, 1<<20))
	}
	trashed, err := s.TrashNotes(context.Background(), append([]string{"x.md", trashLinkerOrigin}, fillers...), headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]TrashEntry{}
	for _, entry := range trashed.Entries {
		entries[entry.OriginalPath] = entry
	}
	// Every commit first reads the notes whose paths changed since the one
	// before, to keep the fmn-id index current; this one reads those
	// trashed, which the move then need not.
	saveNote(t, s, "after-trash.md", "# After the Trash\n")
	// The premise: reading the fillers between two reads of x.md pushes it
	// out of the notes store's caches, so a second read reaches the blobs.
	readTrashed(t, s, entries, "x.md")
	readTrashed(t, s, entries, fillers...)
	blobs.reset()
	readTrashed(t, s, entries, "x.md")
	once := blobs.readsOf(written)
	if once == 0 {
		t.Fatal("reading x.md after the fillers did not reach the blob store")
	}
	readTrashed(t, s, entries, fillers...)

	blobs.reset()
	move := OrganizationAction{ID: "run:1", Kind: "move", Path: "notes/a.md", Target: "dest/a.md", Allowed: []string{"notes/a.md"}, Folders: []string{"dest"}, Sources: []MutationSource{orgSource(t, s, "notes/a.md")}}
	applied, err := s.Organize(ctx, move, true)
	if err != nil {
		t.Fatal("the move:", err)
	}
	if want := []OrganizationTrashLink{{Path: entries[trashLinkerOrigin].Path, OriginalPath: trashLinkerOrigin, Updated: true}}; !slices.Equal(applied.TrashLinks, want) {
		t.Errorf("the move names %+v, want %+v", applied.TrashLinks, want)
	}
	if reads := blobs.readsOf(written); reads > once {
		t.Errorf("the move read x.md in the Trash %d times over, want at most once (%d reads)", reads/once, once)
	}
}
