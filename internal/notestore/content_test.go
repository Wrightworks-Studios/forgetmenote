package notestore

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestMarkdownBlocksCutAfterParagraphsAndJoinToTheContent(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	var doc strings.Builder
	for doc.Len() < 400<<10 {
		doc.WriteString(strings.Repeat("word ", 20+rng.IntN(200)))
		doc.WriteString("\n\n")
	}
	doc.WriteString(strings.Repeat("x", 150<<10)) // one paragraph over 64 KiB, unterminated
	content := []byte(doc.String())
	blocks := splitBlocks("notes/book.md", content)
	if !bytes.Equal(bytes.Join(blocks, nil), content) {
		t.Fatal("blocks do not join to the content")
	}
	for i, b := range blocks {
		if len(b) > mdMaxBlock {
			t.Fatalf("block %d holds %d bytes", i, len(b))
		}
		last := i == len(blocks)-1
		if !last && len(b) < mdMinBlock {
			t.Fatalf("block %d holds %d bytes", i, len(b))
		}
		if !last && len(b) < mdMaxBlock && !bytes.HasSuffix(b, []byte("\n\n")) {
			t.Fatalf("block %d does not end after a blank line", i)
		}
	}
	// A paragraph edit leaves most blocks as they were.
	edited := bytes.Replace(content, []byte("word word word"), []byte("word, an inserted phrase, word word"), 1)
	old := map[string]bool{}
	for _, b := range blocks {
		old[string(b)] = true
	}
	fresh := 0
	for _, b := range splitBlocks("notes/book.md", edited) {
		if !old[string(b)] {
			fresh++
		}
	}
	if fresh > 3 {
		t.Fatalf("one edit made %d new blocks of %d", fresh, len(blocks))
	}
	binary := bytes.Repeat([]byte{1}, binaryBlock*2+5)
	parts := splitBlocks("assets/big.bin", binary)
	if len(parts) != 3 || len(parts[0]) != binaryBlock || len(parts[2]) != 5 {
		t.Fatalf("binary blocks: %d", len(parts))
	}
	if got := splitBlocks("crlf.md", []byte(strings.Repeat("line\r\n\r\n", 20000))); len(got) != 3 {
		t.Fatalf("a note without blank LF lines is cut every 64 KiB: %d blocks", len(got))
	}
}

func TestSegmentsCutTheBlockListByContent(t *testing.T) {
	var doc strings.Builder
	for i := 0; doc.Len() < 1200<<10; i++ {
		doc.WriteString(strings.Repeat("paragraph ", 50+i%90))
		doc.WriteString("\n\n")
	}
	v := prepare("book.md", []byte(doc.String()))
	if len(v.segments) < 8 || len(v.refs) < 200 {
		t.Fatalf("%d blocks in %d segments", len(v.refs), len(v.segments))
	}
	var total int64
	blocks := 0
	for i, seg := range v.segments {
		refs, err := decodeBlocks(v.bodies[i])
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) > segmentMax || i < len(v.segments)-1 && len(refs) < segmentMin {
			t.Fatalf("segment %d has %d blocks", i, len(refs))
		}
		blocks += len(refs)
		total += seg.length
	}
	if blocks != len(v.refs) || total != int64(doc.Len()) {
		t.Fatalf("segments cover %d blocks and %d bytes", blocks, total)
	}
	decoded, err := decodeSegments(encodeSegments(v.segments))
	if err != nil || len(decoded) != len(v.segments) || decoded[3] != v.segments[3] {
		t.Fatal("segment list round trip:", err)
	}
	if small := prepare("small.md", []byte("tiny")); !small.inline() || small.segments != nil {
		t.Fatal("a small file is inline")
	}
}
