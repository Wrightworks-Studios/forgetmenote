package workspace

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

type BrowseItem struct {
	Path     string    `json:"path"`
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	Excerpt  string    `json:"excerpt,omitempty"`
	Metadata *Metadata `json:"metadata,omitempty"`
	Count    int       `json:"count,omitempty"`
}
type FolderBrowse struct {
	ScopeID    string       `json:"scopeId,omitempty"`
	Collection string       `json:"collection,omitempty"`
	Visibility string       `json:"visibility,omitempty"`
	Path       string       `json:"path"`
	Revision   string       `json:"revision"`
	Items      []BrowseItem `json:"items"`
}

func browseEntries(result FolderBrowse, entries []Entry) FolderBrowse {
	folder := result.Path
	prefix := ""
	if folder != "" {
		prefix = folder + "/"
	}
	folders := map[string]int{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Path, ".md") || !strings.HasPrefix(entry.Path, prefix) {
			continue
		}
		relative := strings.TrimPrefix(entry.Path, prefix)
		if child, _, nested := strings.Cut(relative, "/"); nested {
			folders[child]++
			continue
		}
		title := entry.Title
		if entry.Metadata != nil && entry.Metadata.Title != "" {
			title = entry.Metadata.Title
		}
		if title == "" {
			title = strings.TrimSuffix(relative, ".md")
		}
		result.Items = append(result.Items, BrowseItem{Path: entry.Path, Kind: "note", Title: title, Excerpt: entry.Excerpt, Metadata: entry.Metadata})
	}
	for folder, count := range folders {
		result.Items = append(result.Items, BrowseItem{Path: prefix + folder, Kind: "folder", Title: folder, Count: count})
	}
	// Folders, then notes, by title regardless of case, then by path, which
	// no two items share: one revision always lists in one order, although
	// folders are counted in a map.
	slices.SortFunc(result.Items, func(a, b BrowseItem) int {
		if a.Kind != b.Kind {
			if a.Kind == "folder" {
				return -1
			}
			return 1
		}
		return cmp.Or(strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)), strings.Compare(a.Path, b.Path))
	})
	return result
}

// NoteSummary derives a plain heading and preview without changing the Markdown.
func NoteSummary(content []byte) (string, string) {
	title, excerpt := noteText(content)
	if title == "" && excerpt != UnavailableExcerpt {
		title = shortenPreview(excerpt, 80)
	}
	return shortenPreview(title, 160), shortenPreview(excerpt, 240)
}

func noteText(content []byte) (string, string) {
	body := MarkdownBody(content)
	if len(body) > displayTextBudget {
		end := displayTextBudget
		for end > 0 && !utf8.RuneStart(body[end]) {
			end--
		}
		body = body[:end]
	}
	selection := &excerptSelection{ranges: [][2]int{{0, len(body)}}, unavailable: make([]bool, 1)}
	tree := selectedMarkdownParser(selection).Parse(text.NewReader(body))
	var title, excerpt strings.Builder
	ast.Walk(tree, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			if node.Type() == ast.TypeBlock {
				excerpt.WriteByte(' ')
			}
			return ast.WalkContinue, nil
		}
		if h, ok := node.(*ast.Heading); ok && h.Level == 1 && title.Len() == 0 {
			title.Write(h.Text(body))
			return ast.WalkSkipChildren, nil
		}
		switch n := node.(type) {
		case *ast.Text:
			excerpt.Write(n.Segment.Value(body))
			if n.SoftLineBreak() || n.HardLineBreak() {
				excerpt.WriteByte(' ')
			}
		case *ast.String:
			excerpt.Write(n.Value)
		case *ast.AutoLink:
			excerpt.Write(n.Label(body))
		case *ast.HTMLBlock, *ast.RawHTML:
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	plainTitle := strings.Join(strings.Fields(title.String()), " ")
	if selection.unavailable[0] {
		return plainTitle, UnavailableExcerpt
	}
	return plainTitle, strings.Join(strings.Fields(excerpt.String()), " ")
}

func shortenPreview(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

// SearchExcerpt is display text only. Citations and evidence retain their original Markdown.
func SearchExcerpt(content []byte, query string) string {
	query = strings.TrimSpace(query)
	body := MarkdownBody(content)
	offset := len(content) - len(body)
	index := 0
	if query != "" {
		index = max(0, previewMatch(string(body), query))
	}
	start := max(0, index-2048)
	end := min(len(body), index+len(query)+4096)
	for start > 0 && !utf8.RuneStart(body[start]) {
		start--
	}
	for end < len(body) && !utf8.RuneStart(body[end]) {
		end++
	}
	result := markdownRangeExcerpts(content, [][2]int{{offset + start, offset + end}}, query, true)[0]
	if result == "" {
		title, _ := noteText(content)
		return shortenPreview(title, 240)
	}
	return result
}

func plainSearchExcerpt(plain, query string) string {
	runes := []rune(plain)
	if len(runes) <= 240 {
		return plain
	}
	index := previewMatch(plain, query)
	start := 0
	if index >= 0 {
		start = max(0, utf8.RuneCountInString(plain[:index])-60)
	}
	prefix := ""
	if start > 0 {
		prefix = "…"
	}
	return prefix + shortenPreview(string(runes[start:]), 240)
}

// MarkdownRangeExcerpts parses the bounded original file once so continuation
// chunks retain their table, fence and HTML context. Only source segments inside
// each selected evidence range become display text; citations keep original bytes.
func MarkdownRangeExcerpts(content []byte, ranges [][2]int, query string) []string {
	return markdownRangeExcerpts(content, ranges, query, false)
}
func markdownRangeExcerpts(content []byte, ranges [][2]int, query string, omitTitle bool) []string {
	body := MarkdownBody(content)
	offset := len(content) - len(body)
	selection := &excerptSelection{ranges: make([][2]int, len(ranges)), unavailable: make([]bool, len(ranges))}
	for i, r := range ranges {
		selection.ranges[i] = [2]int{max(0, r[0]-offset), min(len(body), r[1]-offset)}
	}
	tree := selectedMarkdownParser(selection).Parse(text.NewReader(body))
	builders := make([]strings.Builder, len(ranges))
	write := func(segment text.Segment) {
		for i, bounds := range ranges {
			start := max(segment.Start, bounds[0]-offset)
			end := min(segment.Stop, bounds[1]-offset)
			if start < end {
				builders[i].Write(body[start:end])
			}
		}
	}
	space := func() {
		for i := range builders {
			builders[i].WriteByte(' ')
		}
	}
	ast.Walk(tree, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			if node.Type() == ast.TypeBlock {
				space()
			}
			return ast.WalkContinue, nil
		}
		if heading, ok := node.(*ast.Heading); ok && omitTitle && heading.Level == 1 {
			return ast.WalkSkipChildren, nil
		}
		switch n := node.(type) {
		case *ast.HTMLBlock, *ast.RawHTML:
			return ast.WalkSkipChildren, nil
		case *ast.CodeBlock, *ast.FencedCodeBlock:
			for i := 0; i < node.Lines().Len(); i++ {
				write(node.Lines().At(i))
				space()
			}
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			write(n.Segment)
			if n.SoftLineBreak() || n.HardLineBreak() {
				space()
			}
		case *ast.AutoLink:
			write(text.NewSegment(n.Pos(), n.Pos()+len(n.Label(body))))
		}
		return ast.WalkContinue, nil
	})
	out := make([]string, len(ranges))
	for i := range out {
		if selection.unavailable[i] {
			out[i] = unavailableExcerpt
		} else {
			out[i] = plainSearchExcerpt(strings.Join(strings.Fields(builders[i].String()), " "), query)
		}
	}
	return out
}

// Lowercasing preserves rune count, but byte widths can grow or shrink.
// Translate the matching rune position back to the original string before slicing.
func previewMatch(value, query string) int {
	lower := strings.ToLower(value)
	index := strings.Index(lower, strings.ToLower(query))
	if index < 0 {
		return -1
	}
	remaining := utf8.RuneCountInString(lower[:index])
	for offset := range value {
		if remaining == 0 {
			return offset
		}
		remaining--
	}
	return len(value)
}
