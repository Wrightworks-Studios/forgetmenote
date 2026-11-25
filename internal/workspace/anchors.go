package workspace

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"golang.org/x/net/html"
)

// Visible text is derived, never a second persisted representation of a note.
func commentText(content []byte) string {
	var rendered bytes.Buffer
	goldmark.New(goldmark.WithExtensions(extension.GFM)).Convert(MarkdownBody(content), &rendered)
	root, err := html.Parse(&rendered)
	if err != nil {
		return ""
	}
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
		}
		block := n.Type == html.ElementNode && strings.Contains(" p h1 h2 h3 h4 h5 h6 li td th pre blockquote div br ", " "+n.Data+" ")
		if block {
			text.WriteByte(' ')
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if block {
			text.WriteByte(' ')
		}
	}
	walk(root)
	return strings.Join(strings.Fields(text.String()), " ")
}
func validAnchor(text string, a *CommentAnchor) bool {
	if a.Quote == "" || len(a.Quote) > 4096 || len(a.Prefix) > 1024 || len(a.Suffix) > 1024 {
		return false
	}
	count := 0
	for offset := 0; offset <= len(text); {
		at := strings.Index(text[offset:], a.Quote)
		if at < 0 {
			break
		}
		at += offset
		end := at + len(a.Quote)
		if strings.HasSuffix(text[:at], a.Prefix) && strings.HasPrefix(text[end:], a.Suffix) {
			count++
		}
		offset = at + 1
	}
	return count == 1
}
