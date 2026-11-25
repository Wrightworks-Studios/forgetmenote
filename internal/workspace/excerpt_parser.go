package workspace

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	tableast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

const displayTextBudget = 64 << 10
const displayTableCells = 4096
const UnavailableExcerpt = "Snippet unavailable; open the note to inspect this match."
const unavailableExcerpt = UnavailableExcerpt

// Selection lives for one display parse. The original source and absolute
// coordinates stay intact; only irrelevant presentation nodes are retired.
type excerptSelection struct {
	ranges          [][2]int
	unavailable     []bool
	referenceText   int
	contextExceeded bool
}

func (s *excerptSelection) overlaps(start, end int) bool {
	for _, r := range s.ranges {
		if start < r[1] && r[0] < end {
			return true
		}
	}
	return false
}
func (s *excerptSelection) reject(lines *text.Segments) {
	for i, r := range s.ranges {
		for j := 0; j < lines.Len(); j++ {
			line := lines.At(j)
			if line.Start < r[1] && r[0] < line.Stop {
				s.unavailable[i] = true
				break
			}
		}
	}
}
func (s *excerptSelection) relevant(node ast.Node) bool {
	if node.Type() == ast.TypeBlock {
		for i := 0; i < node.Lines().Len(); i++ {
			line := node.Lines().At(i)
			if s.overlaps(line.Start, line.Stop) {
				return true
			}
		}
	}
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if s.relevant(child) {
			return true
		}
	}
	return false
}
func (s *excerptSelection) retire(parent ast.Node) {
	if parent == nil || parent.LastChild() == nil {
		return
	}
	// Goldmark uses the last child and its offset for continuing containers.
	// Older completed siblings are no longer grammar state.
	for node := parent.LastChild().PreviousSibling(); node != nil; {
		previous := node.PreviousSibling()
		if s.relevant(node) {
			break
		}
		parent.RemoveChild(parent, node)
		node = previous
	}
}
func (s *excerptSelection) neutral(lines *text.Segments) {
	if lines.Len() == 0 {
		return
	}
	first := lines.At(0)
	first.Stop = min(first.Stop, first.Start+1)
	first.Padding = 0
	first.ForceNewline = false
	lines.Clear()
	lines.Append(first)
}

// The actual table transformer decides syntax, including malformed-column
// behavior. Probes have isolated contexts so escaped-pipe fixups cannot leak.
func tableColumns(source []byte, segments ...text.Segment) int {
	parent := ast.NewDocument()
	paragraph := ast.NewParagraph()
	parent.AppendChild(parent, paragraph)
	for _, segment := range segments {
		paragraph.Lines().Append(segment.TrimLeftSpace(source))
	}
	extension.NewTableParagraphTransformer().Transform(paragraph, text.NewReader(source), parser.NewContext())
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		if child.Kind() == tableast.KindTable {
			return child.FirstChild().ChildCount()
		}
	}
	return 0
}
func delimiterProbe(line []byte) bool {
	if bytes.IndexByte(line, '-') < 0 {
		return false
	}
	for _, c := range line {
		if !strings.ContainsRune("|:- \t\r\n", rune(c)) {
			return false
		}
	}
	header := bytes.Map(func(c rune) rune {
		if c == '-' || c == ':' {
			return 'x'
		}
		return c
	}, line)
	source := append(append(append([]byte{}, header...), '\n'), line...)
	return tableColumns(source, text.NewSegment(0, len(header)+1), text.NewSegment(len(header)+1, len(source))) > 0
}
func (s *excerptSelection) table(lines *text.Segments, source []byte) bool {
	for i := 1; i < lines.Len(); i++ {
		delimiter := lines.At(i)
		if delimiter.Len() > displayTextBudget || !delimiterProbe(delimiter.Value(source)) {
			continue
		}
		header := lines.At(i - 1)
		// A valid delimiter with a mismatched header stops Goldmark's transformer.
		if header.Len() > displayTextBudget {
			return false
		}
		columns := tableColumns(source, header, delimiter)
		if columns == 0 {
			return false
		}
		selected := make([]text.Segment, 0, 8)
		rows := 1
		for j := 0; j < lines.Len(); j++ {
			line := lines.At(j)
			if j == i-1 || j == i || s.overlaps(line.Start, line.Stop) {
				selected = append(selected, line)
				if j > i {
					rows++
				}
			}
		}
		total := 0
		for _, line := range selected {
			total += line.Len()
		}
		// Goldmark pads sparse rows to the header width. Bound presentation
		// nodes as well as text so short wide tables cannot amplify memory.
		if total > displayTextBudget || columns > displayTableCells/rows {
			s.reject(lines)
			s.neutral(lines)
			return true
		}
		lines.Clear()
		lines.AppendAll(selected)
		return true
	}
	return false
}
func (s *excerptSelection) retainLines(lines *text.Segments, source []byte, literal bool) {
	if lines.Len() == 0 {
		return
	}
	if literal {
		selected := make([]text.Segment, 0, 8)
		for i := 0; i < lines.Len(); i++ {
			line := lines.At(i)
			if s.overlaps(line.Start, line.Stop) {
				selected = append(selected, line)
			}
		}
		lines.Clear()
		lines.AppendAll(selected)
		return
	}
	total := 0
	relevant := false
	for i := 0; i < lines.Len(); i++ {
		line := lines.At(i)
		total += line.Len()
		relevant = relevant || s.overlaps(line.Start, line.Stop)
	}
	if !relevant {
		s.neutral(lines)
		return
	}
	if total <= displayTextBudget {
		return
	}
	selected := make([]text.Segment, 0, 8)
	retained := 0
	for i := 0; i < lines.Len(); i++ {
		line := lines.At(i)
		left, right := line.Stop, line.Start
		for _, r := range s.ranges {
			if line.Start < r[1] && r[0] < line.Stop {
				left = min(left, max(line.Start, r[0]))
				right = max(right, min(line.Stop, r[1]))
			}
		}
		// Removing plain text cannot open or close an inline construct. Rich
		// context that cannot fit is an explicit unavailable preview, never a guess.
		if left >= right {
			if bytes.ContainsAny(line.Value(source), "<>![]_*~`\\&:@()") {
				s.reject(lines)
				s.neutral(lines)
				return
			}
			continue
		}
		if bytes.ContainsAny(source[line.Start:left], "<>![]_*~`\\&:@()") || bytes.ContainsAny(source[right:line.Stop], "<>![]_*~`\\&:@()") {
			s.reject(lines)
			s.neutral(lines)
			return
		}
		line.Start = left
		line.Stop = right
		for line.Start < line.Stop && !utf8.RuneStart(source[line.Start]) {
			line.Start++
		}
		line.Padding = 0
		selected = append(selected, line)
		retained += line.Len()
	}
	if retained > displayTextBudget {
		s.reject(lines)
		s.neutral(lines)
		return
	}
	lines.Clear()
	lines.AppendAll(selected)
}

type excerptParagraph struct{ selection *excerptSelection }

func (p excerptParagraph) Transform(node *ast.Paragraph, reader text.Reader, _ parser.Context) {
	lines := node.Lines()
	relevant := false
	for i := 0; i < lines.Len(); i++ {
		line := lines.At(i)
		relevant = relevant || p.selection.overlaps(line.Start, line.Stop)
	}
	if !relevant {
		p.selection.neutral(lines)
		p.selection.retire(node.Parent())
		return
	}
	if !p.selection.table(lines, reader.Source()) {
		p.selection.retainLines(lines, reader.Source(), false)
	}
	p.selection.retire(node.Parent())
}

// Reference definitions are processed eagerly before tables and inlines.
// Bound their input before Goldmark's removal loop and reference-context map;
// incomplete reference context makes every display range explicitly unavailable.
type excerptReferences struct{ selection *excerptSelection }

func (p excerptReferences) Transform(node *ast.Paragraph, reader text.Reader, _ parser.Context) {
	s := p.selection
	lines := node.Lines()
	if !s.contextExceeded {
		for i := 0; i < lines.Len(); i++ {
			line := lines.At(i)
			if bytes.HasPrefix(bytes.TrimLeft(line.Value(reader.Source()), " \t"), []byte("[")) {
				s.referenceText += line.Len()
				if s.referenceText > displayTextBudget {
					s.contextExceeded = true
					break
				}
			}
		}
	}
	if s.contextExceeded {
		for i := range s.unavailable {
			s.unavailable[i] = true
		}
		s.neutral(lines)
	}
}

type excerptBlock struct {
	parser.BlockParser
	selection *excerptSelection
}

func (p excerptBlock) Close(node ast.Node, reader text.Reader, pc parser.Context) {
	p.BlockParser.Close(node, reader, pc)
	if node.Parent() == nil {
		return
	}
	switch node.(type) {
	case *ast.CodeBlock, *ast.FencedCodeBlock:
		p.selection.retainLines(node.Lines(), reader.Source(), true)
	case *ast.Heading:
		p.selection.retainLines(node.Lines(), reader.Source(), false)
	}
	p.selection.retire(node.Parent())
}
func selectedMarkdownParser(selection *excerptSelection) parser.Parser {
	blocks := parser.DefaultBlockParsers()
	paragraphs := parser.DefaultParagraphTransformers()
	if selection != nil {
		for i := range blocks {
			blocks[i].Value = excerptBlock{blocks[i].Value.(parser.BlockParser), selection}
		}
		paragraphs = append(paragraphs, util.Prioritized(excerptReferences{selection}, 99), util.Prioritized(excerptParagraph{selection}, 199))
	}
	inlines := append(parser.DefaultInlineParsers(), util.Prioritized(extension.NewLinkifyParser(), 999))
	for i := range inlines {
		if inlines[i].Value == parser.NewAutoLinkParser() || i == len(inlines)-1 {
			inlines[i].Value = displayLabelInline{inlines[i].Value.(parser.InlineParser)}
		}
	}
	base := parser.NewParser(parser.WithBlockParsers(blocks...), parser.WithInlineParsers(inlines...), parser.WithParagraphTransformers(paragraphs...))
	return goldmark.New(goldmark.WithParser(base), goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList)).Parser()
}

// AutoLink hides its source segment. Capture the actual parser's current line
// before it consumes the label and record its exact source position without
// replacing the grammar node. Generated destinations never enter an excerpt.
type displayLabelInline struct{ parser.InlineParser }

func (p displayLabelInline) Parse(parent ast.Node, reader text.Reader, pc parser.Context) ast.Node {
	line, segment := reader.PeekLine()
	node := p.InlineParser.Parse(parent, reader, pc)
	if link, ok := node.(*ast.AutoLink); ok {
		label := link.Label(reader.Source())
		if at := bytes.Index(line, label); at >= 0 {
			link.SetPos(segment.Start + at)
		}
	}
	return node
}
