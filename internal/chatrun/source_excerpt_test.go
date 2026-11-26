package chatrun

import (
	"strings"
	"testing"
	"unicode/utf8"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

// A repository document read and a source file read cite the same bounded
// excerpt: only bytes actually cited, with their hash and exact ranges.
func TestChatSourceExcerptCitesExactBoundedBytes(t *testing.T) {
	short := chatSourceExcerpt(chat.Citation{ID: "short", Kind: "repository"}, "one\ntwo\n", 10, 3)
	if short.ID != "short" || short.Text != "one\ntwo\n" || short.Hash != workspace.ContentHash([]byte("one\ntwo\n")) || short.StartByte != 10 || short.EndByte != 18 || short.StartLine != 3 || short.EndLine != 4 {
		t.Fatalf("short excerpt %+v", short)
	}
	// Byte 12000 falls inside a two-byte character, so the excerpt ends before it.
	content := "xx" + strings.Repeat("é\n", 5000)
	long := chatSourceExcerpt(chat.Citation{}, content, 100, 1)
	if len(long.Text) != 11999 || !utf8.ValidString(long.Text) || long.Text != content[:11999] || long.Hash != workspace.ContentHash([]byte(long.Text)) {
		t.Fatalf("long excerpt of %d bytes", len(long.Text))
	}
	if long.StartByte != 100 || long.EndByte != 100+11999 || long.StartLine != 1 || long.EndLine != 1+3998 {
		t.Fatalf("long excerpt ranges bytes %d-%d lines %d-%d", long.StartByte, long.EndByte, long.StartLine, long.EndLine)
	}
}

// A legacy passage search without citations shows the model its passages,
// without the durable passage checksums.
func TestChatSearchResultWithoutCitationsShowsItsPassages(t *testing.T) {
	raw := []byte(`{"passages":[{"id":"p1","path":"notes/a.md","citation":"/notes/a.md?fmn-rev=r","heading":"A","text":"Passage text","revision":"r","noteHash":"note-hash","hash":"passage-hash"}]}`)
	compact, ok := CompactSourceResult("search_passages", raw, nil)
	if !ok || strings.Contains(compact, "passage-hash") || !strings.Contains(compact, `"id":"p1"`) || !strings.Contains(compact, "note-hash") || !strings.Contains(compact, "Passage text") {
		t.Fatalf("compact search result %s", compact)
	}
}
