package notestore

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

// A write whose read model is behind the head guesses the blocks to put
// from the model; its transaction puts the ones the head's file lacks.
func TestAWriteOnAStaleModelPutsTheBlocksTheHeadLacks(t *testing.T) {
	ctx := context.Background()
	dir, err := blob.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	s, err := Open(ctx, pgtest.Org(t), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	author := gitobj.Signature{Name: "Ada", Email: "ada@example.com", When: 1700000000, TZ: "+0000"}
	if _, err := s.Create(ctx, "notes", author); err != nil {
		t.Fatal(err)
	}
	n := s.Notebook("notes")
	var b strings.Builder
	for i := range 300 {
		b.WriteString("Paragraph " + strconv.Itoa(i) + "." + strings.Repeat(" Call me Ishmael.", 70) + "\n\n")
	}
	v1 := b.String()
	v2 := strings.Replace(v1, "Paragraph 5.", "Paragraph five, edited.", 1)
	v3 := strings.Replace(v1, "Paragraph 1.", "Paragraph one, edited.", 1)
	write := func(content string) {
		t.Helper()
		if _, err := n.Apply(ctx, ChangeSet{Subject: "Write", Author: author, Files: []FileChange{{Path: "book.md", Content: []byte(content)}}}); err != nil {
			t.Fatal(err)
		}
	}
	write(v1)
	stale := n.model.Load()
	write(v2)
	s.Close() // no refresh may replace the stale model; writes still work
	n.model.Store(stale)
	// The block v3 shares with v1 but not v2 is gone from the store, so
	// only the transaction's own put can bring it back.
	inV2 := map[[32]byte]bool{}
	for _, r := range prepare("book.md", []byte(v2)).refs {
		inV2[r.sha] = true
	}
	inV1 := map[[32]byte]bool{}
	for _, r := range prepare("book.md", []byte(v1)).refs {
		inV1[r.sha] = true
	}
	removed := 0
	for _, r := range prepare("book.md", []byte(v3)).refs {
		if inV1[r.sha] && !inV2[r.sha] {
			if err := dir.Delete(ctx, blockKey(r.sha)); err != nil {
				t.Fatal(err)
			}
			s.blocks.remove(r.sha)
			removed++
		}
	}
	if removed == 0 {
		t.Fatal("v3 shares no block with v1 alone")
	}
	write(v3)
	cold, err := Open(ctx, s.db, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	got, _, err := cold.Notebook("notes").Read(ctx, gitobj.ID{}, "book.md")
	if err != nil || string(got) != v3 {
		t.Fatal("v3 reads back:", len(got), err)
	}
}

// A write whose head moves on between every try of its transaction, each
// time to a file that lacks another of the write's blocks, still commits in
// a few tries, having put every block it needs.
func TestAWriteWhoseHeadMovesOnEveryTryCommits(t *testing.T) {
	ctx := context.Background()
	dir, err := blob.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	s, err := Open(ctx, pgtest.Org(t), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	author := gitobj.Signature{Name: "Ada", Email: "ada@example.com", When: 1700000000, TZ: "+0000"}
	if _, err := s.Create(ctx, "theirs", author); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := range 300 {
		b.WriteString("Paragraph " + strconv.Itoa(i) + "." + strings.Repeat(" Call me Ishmael.", 70) + "\n\n")
	}
	book := b.String()
	// heads[k] is the file at the other gateway's head after its k-th
	// edit, which changed paragraph k-1.
	var heads []gitobj.ID
	content := book
	for k := range 10 {
		if k > 0 {
			content = strings.Replace(content, "Paragraph "+strconv.Itoa(k-1)+".", "Paragraph "+strconv.Itoa(k-1)+", theirs.", 1)
		}
		if _, err := s.Notebook("theirs").Apply(ctx, ChangeSet{Subject: "Edit", Author: author, Files: []FileChange{{Path: "book.md", Content: []byte(content)}}}); err != nil {
			t.Fatal(err)
		}
		heads = append(heads, gitobj.BlobID([]byte(content)))
	}
	ours := prepare("book.md", []byte(strings.Replace(book, "Paragraph 299.", "Paragraph 299, ours.", 1)))
	inBook := map[[32]byte]bool{}
	for _, r := range prepare("book.md", []byte(book)).refs {
		inBook[r.sha] = true
	}
	// What prestage puts from a model at heads[0]: the one new block.
	staged := map[[32]byte]bool{}
	var pre []pendingBlock
	for i, r := range ours.refs {
		if !inBook[r.sha] {
			pre = append(pre, pendingBlock{r, ours.blocks[i]})
		}
	}
	if len(pre) != 1 {
		t.Fatal("new blocks:", len(pre))
	}
	if err := s.stage(ctx, pre, staged); err != nil {
		t.Fatal(err)
	}
	// The blocks of ours the later heads lack are gone from the store, so
	// only the write's own puts can bring them back.
	for i := range len(heads) - 1 {
		r := ours.refs[i]
		if err := dir.Delete(ctx, blockKey(r.sha)); err != nil {
			t.Fatal(err)
		}
		s.blocks.remove(r.sha)
	}
	tries := 0
	err = s.writeTx(ctx, func(tx pgx.Tx) error {
		tries++
		prev := map[gitobj.ID]gitobj.ID{ours.blob: heads[min(tries, len(heads)-1)]}
		_, err := s.storeVersions(ctx, tx, []gitobj.ID{ours.blob}, map[gitobj.ID]*version{ours.blob: ours}, prev, staged)
		return err
	}, staged)
	if err != nil {
		t.Fatalf("after %d tries: %v", tries, err)
	}
	if tries > 3 {
		t.Errorf("%d tries", tries)
	}
	cold, err := Open(ctx, s.db, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	got, err := cold.readContent(ctx, cold.db.Pool, ours.blob)
	if err != nil || string(got) != string(ours.data) {
		t.Fatal("ours reads back:", len(got), err)
	}
}
