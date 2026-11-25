package knowledge

import (
	"errors"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"forgetmenote/internal/workspace"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Writing analysis is derived from original Markdown, never stored as content.
// All server positions are UTF-8 byte offsets; lines are one-based.
const WritingVersion = 1

// Cache only small projections, by content rather than path or revision. Scope
// and permission checks remain with each caller. No derived files are written.
func (e *Engine) Writing(content []byte) WritingAnalysis {
	key := strconv.Itoa(WritingVersion) + ":" + workspace.ContentHash(content)
	e.mu.Lock()
	cached, ok := e.writingCache[key]
	e.mu.Unlock()
	if ok {
		return cached
	}
	result := AnalyzeWriting(content)
	if len(content) <= 128<<10 {
		e.mu.Lock()
		if _, exists := e.writingCache[key]; !exists {
			if len(e.writingCache) >= 8 || e.writingCacheBytes+len(content) > 256<<10 {
				e.writingCache = nil
				e.writingCacheBytes = 0
			}
			if e.writingCache == nil {
				e.writingCache = map[string]WritingAnalysis{}
			}
			e.writingCache[key] = result
			e.writingCacheBytes += len(content)
		}
		e.mu.Unlock()
	}
	return result
}

type WritingStats struct {
	Words          int     `json:"words"`
	Characters     int     `json:"characters"`
	Paragraphs     int     `json:"paragraphs"`
	Sentences      int     `json:"sentences"`
	ReadingMinutes float64 `json:"readingMinutes"`
}
type WritingSection struct {
	Title        string `json:"title"`
	Kind         string `json:"kind"`
	Level        int    `json:"level"`
	StartByte    int    `json:"startByte"`
	EndByte      int    `json:"endByte"`
	StartLine    int    `json:"startLine"`
	Words        int    `json:"words"`
	SubtreeWords int    `json:"subtreeWords"`
}
type WritingAnalysis struct {
	Version  int              `json:"version"`
	Stats    WritingStats     `json:"stats"`
	Sections []WritingSection `json:"sections"`
	blocks   []writingBlock
}
type writingBlock struct {
	text         string
	starts, ends []int
	start        int
	heading      bool
}
type WritingTerm struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}
type TermMention struct {
	Term      string `json:"term"`
	Text      string `json:"text"`
	Excerpt   string `json:"excerpt"`
	StartByte int    `json:"startByte"`
	EndByte   int    `json:"endByte"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
}

var writingWords = regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}\p{M}]*(?:['’][\p{L}\p{N}\p{M}]+)*`)
var writingURLs = regexp.MustCompile(`(?:https?://|www\.)[^\s<>]+|^\[[ xX]\]\s`)
var writingSentence = regexp.MustCompile(`[.!?]+["'”’)]*(?:\s|$)`)
var writingEntity = regexp.MustCompile(`^&(?:#[xX][\da-fA-F]+|#\d+|[a-zA-Z][a-zA-Z\d]+);`)

func (b *writingBlock) append(value *strings.Builder, raw []byte, start int) {
	for i := 0; i < len(raw); {
		n := 1
		piece := string(raw[i : i+1])
		if raw[i] == '\\' && i+1 < len(raw) && strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", rune(raw[i+1])) {
			n = 2
			piece = string(raw[i+1 : i+2])
		} else if raw[i] == '&' {
			if entity := writingEntity.Find(raw[i:]); entity != nil {
				n = len(entity)
				piece = html.UnescapeString(string(entity))
			}
		}
		value.WriteString(piece)
		for range len(piece) {
			b.starts = append(b.starts, start+i)
			b.ends = append(b.ends, start+i+n)
		}
		i += n
	}
}
func writingBlockFor(n ast.Node, body []byte, offset int) writingBlock {
	b := writingBlock{start: offset + n.Lines().At(0).Start, heading: n.Kind() == ast.KindHeading}
	var value strings.Builder
	var walk func(ast.Node)
	walk = func(node ast.Node) {
		switch v := node.(type) {
		case *ast.Text:
			b.append(&value, v.Value(body), offset+v.Segment.Start)
			if v.SoftLineBreak() || v.HardLineBreak() {
				value.WriteByte('\n')
				b.starts = append(b.starts, offset+v.Segment.Stop)
				b.ends = append(b.ends, offset+v.Segment.Stop)
			}
		case *ast.String:
			b.append(&value, v.Value, b.start)
		case *ast.RawHTML:
			return
		case *ast.CodeSpan, *ast.Image, *ast.AutoLink:
			value.WriteByte(' ')
			b.starts = append(b.starts, b.start)
			b.ends = append(b.ends, b.start)
		default:
			for child := node.FirstChild(); child != nil; child = child.NextSibling() {
				walk(child)
			}
		}
	}
	walk(n)
	b.text = writingURLs.ReplaceAllStringFunc(value.String(), func(s string) string { return strings.Repeat(" ", len(s)) })
	return b
}

// Goldmark discards thematic-break positions. Retain the accepted source line
// at the parser boundary so adjacent breaks, quotes and fenced code stay exact.
type writingSceneParser struct{ parser.BlockParser }

func (p writingSceneParser) Open(parent ast.Node, reader text.Reader, ctx parser.Context) (ast.Node, parser.State) {
	_, line := reader.PeekLine()
	node, state := p.BlockParser.Open(parent, reader, ctx)
	if node != nil {
		node.Lines().Append(line)
	}
	return node, state
}

func AnalyzeWriting(content []byte) WritingAnalysis {
	body := workspace.MarkdownBody(content)
	offset := len(content) - len(body)
	md := goldmark.New(goldmark.WithParserOptions(parser.WithBlockParsers(util.Prioritized(writingSceneParser{parser.NewThematicBreakParser()}, 199))))
	tree := md.Parser().Parse(text.NewReader(body))
	out := WritingAnalysis{Version: WritingVersion, Sections: []WritingSection{}}
	ast.Walk(tree, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if !enter {
			return ast.WalkContinue, nil
		}
		kind := n.Kind()
		if kind == ast.KindHeading || kind == ast.KindParagraph || kind == ast.KindTextBlock {
			if n.Lines().Len() == 0 {
				return ast.WalkSkipChildren, nil
			}
			b := writingBlockFor(n, body, offset)
			out.blocks = append(out.blocks, b)
			count := len(writingWords.FindAllStringIndex(b.text, -1))
			out.Stats.Words += count
			out.Stats.Characters += utf8.RuneCountInString(strings.TrimSpace(b.text))
			if b.heading {
				start := b.start
				for start > 0 && content[start-1] != '\n' {
					start--
				}
				out.Sections = append(out.Sections, WritingSection{Title: strings.TrimSpace(b.text), Kind: "heading", Level: n.(*ast.Heading).Level, StartByte: start})
			} else if count > 0 {
				out.Stats.Paragraphs++
				out.Stats.Sentences += max(1, len(writingSentence.FindAllStringIndex(b.text, -1)))
			}
			return ast.WalkSkipChildren, nil
		}
		if kind == ast.KindThematicBreak {
			out.Sections = append(out.Sections, WritingSection{Title: "Scene break", Kind: "scene", Level: 7, StartByte: offset + n.Lines().At(0).Start})
		}
		return ast.WalkContinue, nil
	})
	if len(out.blocks) > 0 && (len(out.Sections) == 0 || out.blocks[0].start < out.Sections[0].StartByte) {
		out.Sections = append([]WritingSection{{Title: "Opening", Kind: "opening", StartByte: offset}}, out.Sections...)
	}
	index := 0
	for _, block := range out.blocks {
		for index+1 < len(out.Sections) && out.Sections[index+1].StartByte <= block.start {
			index++
		}
		if index < len(out.Sections) {
			out.Sections[index].Words += len(writingWords.FindAllStringIndex(block.text, -1))
		}
	}
	stack, parents := []int{}, make([]int, len(out.Sections))
	line, at := 1, 0
	for i := range out.Sections {
		section := &out.Sections[i]
		section.EndByte = len(content)
		if i+1 < len(out.Sections) {
			section.EndByte = out.Sections[i+1].StartByte
		}
		for at < section.StartByte {
			if content[at] == '\n' {
				line++
			}
			at++
		}
		section.StartLine = line
		section.SubtreeWords = section.Words
		for len(stack) > 0 && out.Sections[stack[len(stack)-1]].Level >= section.Level {
			stack = stack[:len(stack)-1]
		}
		parents[i] = -1
		if len(stack) > 0 {
			parents[i] = stack[len(stack)-1]
		}
		stack = append(stack, i)
	}
	for i := len(out.Sections) - 1; i >= 0; i-- {
		if parents[i] >= 0 {
			out.Sections[parents[i]].SubtreeWords += out.Sections[i].SubtreeWords
		}
	}
	out.Stats.ReadingMinutes = float64(out.Stats.Words) / 225
	return out
}
func ValidateWritingTerms(terms []WritingTerm) error {
	if len(terms) == 0 || len(terms) > 32 {
		return errors.New("use 1–32 terms")
	}
	for _, term := range terms {
		if len(term.Aliases) > 8 {
			return errors.New("use at most 8 aliases per term")
		}
		for _, name := range append([]string{term.Name}, term.Aliases...) {
			if strings.TrimSpace(name) != name || name == "" || utf8.RuneCountInString(name) > 100 || strings.ContainsAny(name, "\r\n|") {
				return errors.New("term names and aliases must be 1–100 characters without line breaks or pipes")
			}
		}
	}
	return nil
}

// Merge one next match per term so a page never materializes all occurrences
// in a long paragraph. The right boundary is consumed by RE2 but the next
// search starts at the term's end, preserving adjacent occurrences.
func (a WritingAnalysis) Mentions(content []byte, terms []WritingTerm, after, limit int) ([]TermMention, bool) {
	type pattern struct {
		name string
		re   *regexp.Regexp
	}
	patterns := []pattern{}
	for _, term := range terms {
		names := append([]string{term.Name}, term.Aliases...)
		sort.SliceStable(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
		for i := range names {
			names[i] = regexp.QuoteMeta(names[i])
		}
		re := regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}\p{M}])(` + strings.Join(names, "|") + `)(?:$|[^\p{L}\p{N}\p{M}])`)
		patterns = append(patterns, pattern{term.Name, re})
	}
	newlines := []int{}
	for i, b := range content {
		if b == '\n' {
			newlines = append(newlines, i)
		}
	}
	matches := []TermMention{}
	seen := 0
	for _, block := range a.blocks {
		positions := make([][2]int, len(patterns))
		advance := func(i, offset int) {
			m := patterns[i].re.FindStringSubmatchIndex(block.text[offset:])
			positions[i] = [2]int{-1, -1}
			if m != nil {
				positions[i] = [2]int{offset + m[2], offset + m[3]}
			}
		}
		for i := range patterns {
			advance(i, 0)
		}
		for {
			selected := -1
			for i, p := range positions {
				if p[0] >= 0 && (selected < 0 || p[0] < positions[selected][0]) {
					selected = i
				}
			}
			if selected < 0 {
				break
			}
			m := positions[selected]
			advance(selected, m[1])
			if seen < after {
				seen++
				continue
			}
			if len(matches) == limit {
				return matches, true
			}
			start, end := block.starts[m[0]], block.ends[m[1]-1]
			left, right := max(0, m[0]-80), min(len(block.text), m[1]+120)
			for left > 0 && !utf8.RuneStart(block.text[left]) {
				left--
			}
			for right < len(block.text) && !utf8.RuneStart(block.text[right]) {
				right++
			}
			matches = append(matches, TermMention{Term: patterns[selected].name, Text: block.text[m[0]:m[1]], Excerpt: block.text[left:right], StartByte: start, EndByte: end, StartLine: 1 + sort.SearchInts(newlines, start), EndLine: 1 + sort.SearchInts(newlines, end)})
		}
	}
	return matches, false
}
