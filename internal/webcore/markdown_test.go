package webcore

import (
	"forgetmenote/internal/workspace"
	"strings"
	"testing"
)

func TestSharedRendererPreservesNoteWireDataAndSanitizesContent(t *testing.T) {
	renderer := NewRenderer()
	document := workspace.Document{Path: "note.md", Revision: strings.Repeat("a", 40), Version: "version", Content: []byte("---\ntitle: Saved title\ntags: [shared]\n---\n# Safe\n[bad](javascript:alert(1))\n<script>alert(1)</script>\n")}
	for i := 0; i < 8; i++ {
		t.Run("concurrent", func(t *testing.T) {
			t.Parallel()
			output := renderer.Output(document)
			if output.Path != document.Path || output.Revision != document.Revision || output.Version != document.Version || output.Content != string(document.Content) || output.Title != "Saved title" || len(output.Metadata.Tags) != 1 {
				t.Fatal("note response lost content, properties or revision", output)
			}
			if strings.Contains(output.HTML, "javascript:") || strings.Contains(output.HTML, "<script") || strings.Contains(output.HTML, "title: Saved") || !strings.Contains(output.HTML, `id="h-safe"`) {
				t.Fatal("sanitizer or Markdown contract changed", output.HTML)
			}
		})
	}
}

// Content anchors share the document with application elements. Generated
// heading IDs carry a prefix so `## Notice` cannot replace the app's #notice.
func TestRendererPrefixesUniqueHeadingIDs(t *testing.T) {
	renderer := NewRenderer()
	for i := 0; i < 2; i++ {
		html := renderer.Render("## Notice\n\n## Notice\n\n## Comment text\n\n## h notice\n\n## ☃\n")
		for _, id := range []string{`id="h-notice"`, `id="h-notice-1"`, `id="h-comment-text"`, `id="h-h-notice"`, `id="h-heading"`} {
			if strings.Count(html, id) != 1 {
				t.Fatalf("render %d: want exactly one %s in %s", i, id, html)
			}
		}
		if strings.Contains(html, `id="notice"`) || strings.Contains(html, `id="comment-text"`) {
			t.Fatal("unprefixed heading ID", html)
		}
	}
}
