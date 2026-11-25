package workspace

import (
	"bytes"
	"regexp"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// ViewLanguage is the info string of a fenced code block that the browser runs
// as an interactive view, inside a sandbox of its own. Its code is never note
// text: search and the knowledge index leave it out.
const ViewLanguage = "view"

var viewFence = regexp.MustCompile("(```|~~~)[ \t]*" + ViewLanguage + `\b`)

type viewBlock struct {
	source     string
	start, end int // the whole block, fences included, as offsets into the note
}

func viewBlocks(content []byte) []viewBlock {
	// Most notes have no fence naming a view, and are not parsed.
	if !viewFence.Match(content) {
		return nil
	}
	body := MarkdownBody(content)
	offset := len(content) - len(body)
	blocks := []viewBlock{}
	ast.Walk(goldmark.New().Parser().Parse(text.NewReader(body)), func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		block, ok := n.(*ast.FencedCodeBlock)
		if !enter || !ok || block.Info == nil || string(block.Language(body)) != ViewLanguage {
			return ast.WalkContinue, nil
		}
		var source bytes.Buffer
		end := lineEnd(body, block.Info.Segment.Stop)
		for i := 0; i < block.Lines().Len(); i++ {
			line := block.Lines().At(i)
			source.Write(line.Value(body))
			end = lineEnd(body, line.Stop)
		}
		// The closing fence, when there is one, is the next line.
		if closing := bytes.TrimLeft(body[end:lineEnd(body, end)], " \t>"); bytes.HasPrefix(closing, []byte("```")) || bytes.HasPrefix(closing, []byte("~~~")) {
			end = lineEnd(body, end)
		}
		start := bytes.LastIndexByte(body[:block.Info.Segment.Start], '\n') + 1
		blocks = append(blocks, viewBlock{source: source.String(), start: offset + start, end: offset + end})
		return ast.WalkSkipChildren, nil
	})
	return blocks
}

// lineEnd returns the offset just past the line holding offset i, newline included.
func lineEnd(body []byte, i int) int {
	if i >= len(body) {
		return len(body)
	}
	if n := bytes.IndexByte(body[i:], '\n'); n >= 0 {
		return i + n + 1
	}
	return len(body)
}

// ViewBlocks returns the source of each view block in a note, in document
// order, which is how the rendered page numbers them.
func ViewBlocks(content []byte) []string {
	blocks := viewBlocks(content)
	sources := make([]string, len(blocks))
	for i, block := range blocks {
		sources[i] = block.source
	}
	return sources
}

// ViewBlockRanges returns the byte range of each view block in a note, fences
// included, as offsets into the whole note.
func ViewBlockRanges(content []byte) [][2]int {
	blocks := viewBlocks(content)
	ranges := make([][2]int, len(blocks))
	for i, block := range blocks {
		ranges[i] = [2]int{block.start, block.end}
	}
	return ranges
}

// SearchableText is a note's content with its view blocks left out.
func SearchableText(content string) string {
	ranges := ViewBlockRanges([]byte(content))
	if len(ranges) == 0 {
		return content
	}
	var b bytes.Buffer
	last := 0
	for _, r := range ranges {
		b.WriteString(content[last:r[0]])
		b.WriteByte('\n')
		last = r[1]
	}
	b.WriteString(content[last:])
	return b.String()
}
