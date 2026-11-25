package workspace

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var markdownDestination = regexp.MustCompile(`!?\[[^\]\n]*\]\(\s*(<[^>\n]*>|[^\s()]+)`)
var referenceDestination = regexp.MustCompile(`(?m)^ {0,3}\[[^\]\n]+\]:[ \t]*(<[^>\n]*>|[^\s]+)`)

// Rewrite Markdown destinations, preserving labels, titles, fragments, and code.
// Only local links that change location are touched; remote URLs are opaque.
func rewriteLinks(source []byte, oldNote, newNote, from, to string) []byte {
	source = rewriteCover(source, from, to)
	source = rewriteSourceOriginal(source, from, to)
	masked := append([]byte{}, source...)
	// Use the same Markdown parser as preview to distinguish code and HTML from
	// links, including nested lists and fences longer than three backticks.
	hide := func(start, end int) {
		for i := start; i < end && i < len(masked); i++ {
			if masked[i] != '\n' {
				masked[i] = ' '
			}
		}
	}
	body := MarkdownBody(source)
	hide(0, len(source)-len(body))
	tree := goldmark.DefaultParser().Parse(text.NewReader(masked))
	ast.Walk(tree, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.CodeSpan:
			for child := n.FirstChild(); child != nil; child = child.NextSibling() {
				segment := child.(*ast.Text).Segment
				hide(segment.Start, segment.Stop)
			}
		case *ast.RawHTML:
			for i := 0; i < n.Segments.Len(); i++ {
				segment := n.Segments.At(i)
				hide(segment.Start, segment.Stop)
			}
		case *ast.HTMLBlock:
			for i := 0; i < n.Lines().Len(); i++ {
				segment := n.Lines().At(i)
				hide(segment.Start, segment.Stop)
			}
			if n.HasClosure() {
				hide(n.ClosureLine.Start, n.ClosureLine.Stop)
			}
		case *ast.FencedCodeBlock:
			if n.Info != nil {
				hide(n.Info.Segment.Start, n.Info.Segment.Stop)
			}
			for i := 0; i < n.Lines().Len(); i++ {
				segment := n.Lines().At(i)
				hide(segment.Start, segment.Stop)
			}
		case *ast.CodeBlock:
			for i := 0; i < n.Lines().Len(); i++ {
				segment := n.Lines().At(i)
				hide(segment.Start, segment.Stop)
			}
		}
		return ast.WalkContinue, nil
	})

	type replacement struct {
		start, end int
		value      string
	}
	replacements := []replacement{}
	for _, pattern := range []*regexp.Regexp{markdownDestination, referenceDestination} {
		for _, match := range pattern.FindAllSubmatchIndex(masked, -1) {
			start, end := match[2], match[3]
			if pattern == markdownDestination && masked[start] != '<' {
				depth := 0
				end = start
				for end < len(masked) {
					c := masked[end]
					if c == '(' {
						depth++
					}
					if c == ')' {
						if depth == 0 {
							break
						}
						depth--
					}
					if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
						break
					}
					end++
				}
			}
			raw := string(source[start:end])
			angle := strings.HasPrefix(raw, "<") && strings.HasSuffix(raw, ">")
			href := raw
			if angle {
				href = href[1 : len(href)-1]
			}
			u, err := url.Parse(href)
			if err != nil || u.IsAbs() || u.Host != "" || u.Opaque != "" || u.Path == "" || strings.Contains(u.Path, "\\") {
				continue
			}
			target := path.Clean(path.Join(path.Dir(oldNote), u.Path))
			root := strings.HasPrefix(u.Path, "/")
			if root {
				target = strings.TrimPrefix(path.Clean(u.Path), "/")
			}
			if target == ".." || strings.HasPrefix(target, "../") {
				continue
			}
			dest := movedPath(target, from, to)
			if revisionPattern.MatchString(u.Query().Get("fmn-rev")) {
				dest = target
			}
			if dest == target && oldNote == newNote {
				continue
			}
			next := "/" + dest
			if !root {
				next, err = filepath.Rel(path.Dir(newNote), dest)
				if err != nil {
					continue
				}
				next = filepath.ToSlash(next)
			}
			if strings.HasSuffix(u.Path, "/") && !strings.HasSuffix(next, "/") {
				next += "/"
			}
			oldPath := u.Path
			u.Path = next
			u.RawPath = ""
			value := u.String()
			if angle {
				value = "<" + value + ">"
			}
			// Avoid cosmetic changes to links whose relative destination is unchanged.
			if next == oldPath {
				continue
			}
			replacements = append(replacements, replacement{start, end, value})
		}
	}
	// Reverse source order keeps offsets valid even when replacement lengths differ.
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start > replacements[j].start })
	result := append([]byte{}, source...)
	for _, r := range replacements {
		result = append(append(append([]byte{}, result[:r.start]...), []byte(r.value)...), result[r.end:]...)
	}
	return result
}
