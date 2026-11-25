package webcore

import (
	"bytes"
	"forgetmenote/internal/workspace"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"regexp"
)

// Renderer owns the instance Markdown parser and sanitizer together. HTML
// from notes, source previews and AI output must cross this same boundary.
type Renderer struct {
	markdown  goldmark.Markdown
	sanitizer *bluemonday.Policy
}

func NewRenderer() *Renderer {
	sanitizer := bluemonday.UGCPolicy()
	// The browser runs a view block only through the view endpoint; the class
	// just tells it which code blocks are views. Goldmark omits raw HTML, so
	// only a fenced view block can carry it.
	sanitizer.AllowAttrs("class").Matching(regexp.MustCompile(`^language-view$`)).OnElements("code")
	return &Renderer{markdown: goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithParserOptions(parser.WithAutoHeadingID())), sanitizer: sanitizer}
}

type NoteOutput struct {
	Title         string             `json:"title"`
	Path          string             `json:"path"`
	Content       string             `json:"content"`
	Revision      string             `json:"revision"`
	HTML          string             `json:"html"`
	Version       string             `json:"version"`
	Metadata      workspace.Metadata `json:"metadata"`
	MetadataError string             `json:"metadataError,omitempty"`
}

// HeadingIDPrefix namespaces generated heading IDs. Rendered content shares
// the browser document with application elements, so a heading such as
// "Notice" must not become id="notice". Link fragments keep the unprefixed
// slug; the browser's rendered-content installer resolves them.
const HeadingIDPrefix = "h-"

// headingIDs keeps goldmark's slugs and per-document uniqueness, then adds the
// prefix. Every generated ID is prefixed, so uniqueness is preserved.
type headingIDs struct{ parser.IDs }

func (ids headingIDs) Generate(value []byte, kind ast.NodeKind) []byte {
	return append([]byte(HeadingIDPrefix), ids.IDs.Generate(value, kind)...)
}

func (r *Renderer) Render(content string) string {
	content = string(workspace.MarkdownBody([]byte(content)))
	var b bytes.Buffer
	ids := headingIDs{parser.NewContext().IDs()}
	r.markdown.Convert([]byte(content), &b, parser.WithContext(parser.NewContext(parser.WithIDs(ids))))
	return r.sanitizer.Sanitize(b.String())
}
func (r *Renderer) Output(d workspace.Document) NoteOutput {
	m, err := workspace.ParseMetadata(d.Content)
	message := ""
	if err != nil {
		message = err.Error()
	}
	title, _ := workspace.NoteSummary(d.Content)
	if m.Title != "" {
		title = m.Title
	}
	return NoteOutput{Title: title, Path: d.Path, Content: string(d.Content), Revision: d.Revision, HTML: r.Render(string(d.Content)), Version: d.Version, Metadata: m, MetadataError: message}
}
