package workspace

import (
	"reflect"
	"strings"
	"testing"
)

func TestViewBlocksAreNumberedAndLeftOutOfSearchText(t *testing.T) {
	content := "---\ntitle: Views\n---\n# Views\n\nBefore.\n\n```view\n<p>first</p>\n```\n\n```js\nnot a view\n```\n\n- item\n\n  ~~~~ view extra\n  <p>second</p>\n  ~~~~\n\nAfter.\n\n```view\n<p>unterminated</p>\n"
	if got, want := ViewBlocks([]byte(content)), []string{"<p>first</p>\n", "<p>second</p>\n", "<p>unterminated</p>\n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("view blocks: got %q, want %q", got, want)
	}
	searchable := SearchableText(content)
	for _, hidden := range []string{"first", "second", "unterminated", "```view", "~~~~"} {
		if strings.Contains(searchable, hidden) {
			t.Fatalf("view code %q left in search text %q", hidden, searchable)
		}
	}
	for _, kept := range []string{"title: Views", "Before.", "not a view", "- item", "After."} {
		if !strings.Contains(searchable, kept) {
			t.Fatalf("note text %q missing from search text %q", kept, searchable)
		}
	}
	for _, plain := range []string{"# Review\n\nAn overview of the viewer.\n", "```viewer\ncode\n```\n", "Inline ```view``` text.\n"} {
		if len(ViewBlocks([]byte(plain))) != 0 || SearchableText(plain) != plain {
			t.Fatalf("note without views changed: %q", plain)
		}
	}
}
