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

// A purge removes at once the blocks no stored version uses, even one a
// write put a moment before its transaction; the write finds the block gone
// and puts it again rather than storing a version whose block is missing.
func TestAWriteWhoseStagedBlockAPurgeRemovedPutsItAgain(t *testing.T) {
	ctx := context.Background()
	dir, err := blob.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	db := pgtest.Org(t)
	s, err := Open(ctx, db, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var b strings.Builder
	for i := range 200 {
		b.WriteString("Paragraph " + strconv.Itoa(i) + "." + strings.Repeat(" Call me Ishmael.", 70) + "\n\n")
	}
	v := prepare("book.md", []byte(b.String()))
	staged := map[[32]byte]bool{}
	var blocks []pendingBlock
	var keys [][]byte
	for i, r := range v.refs {
		blocks = append(blocks, pendingBlock{r, v.blocks[i]})
		keys = append(keys, r.sha[:])
	}
	if err := s.stage(ctx, blocks, staged); err != nil {
		t.Fatal(err)
	}
	// What a purge of the same content elsewhere does meanwhile.
	if _, err := db.Pool.Exec(ctx, `DELETE FROM content_blocks WHERE org = $1 AND block_sha = ANY($2)`, db.Org, keys); err != nil {
		t.Fatal(err)
	}
	for _, r := range v.refs {
		if err := dir.Delete(ctx, blockKey(r.sha)); err != nil {
			t.Fatal(err)
		}
		s.blocks.remove(r.sha)
	}
	err = s.writeTx(ctx, func(tx pgx.Tx) error {
		_, err := s.storeVersions(ctx, tx, []gitobj.ID{v.blob}, map[gitobj.ID]*version{v.blob: v}, nil, staged)
		return err
	}, staged)
	if err != nil {
		t.Fatal(err)
	}
	cold, err := Open(ctx, db, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	content, err := cold.readContent(ctx, db.Pool, v.blob)
	if err != nil || string(content) != b.String() {
		t.Fatal("the stored version lost its blocks:", err)
	}
}

// Removing a purged version's segments keeps every segment, and every
// block, that another stored version still uses, even when all of them are.
func TestPurgingSegmentsKeepsThoseAnotherVersionUses(t *testing.T) {
	ctx := context.Background()
	dir, err := blob.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	db := pgtest.Org(t)
	s, err := Open(ctx, db, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	author := gitobj.Signature{Name: "Ada", Email: "ada@example.com", When: 1700000000, TZ: "+0000"}
	if _, err := s.Create(ctx, "notes", author); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := range 200 {
		b.WriteString("Paragraph " + strconv.Itoa(i) + "." + strings.Repeat(" Call me Ishmael.", 70) + "\n\n")
	}
	if _, err := s.Notebook("notes").Apply(ctx, ChangeSet{Subject: "Write", Author: author, Files: []FileChange{{Path: "book.md", Content: []byte(b.String())}}}); err != nil {
		t.Fatal(err)
	}
	v := prepare("book.md", []byte(b.String()))
	var segments [][32]byte
	for _, seg := range v.segments {
		segments = append(segments, seg.sha)
	}
	removedSegments, removedBlocks, err := s.purgeSegments(ctx, segments)
	if err != nil || removedSegments != 0 || removedBlocks != 0 {
		t.Fatal("removed segments and blocks a stored version uses:", removedSegments, removedBlocks, err)
	}
	cold, err := Open(ctx, db, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	if content, _, err := cold.Notebook("notes").Read(ctx, gitobj.ID{}, "book.md"); err != nil || string(content) != b.String() {
		t.Fatal("the stored version lost its blocks:", err)
	}
}
