// Package knowledge owns rebuildable retrieval data and durable external-workflow state.
package knowledge

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/retrieval"
	"forgetmenote/internal/workspace"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Config struct {
	Enabled      bool   `json:"enabled"`
	Model        string `json:"model"`
	Dimensions   int    `json:"dimensions"`
	PassageRunes int    `json:"passageRunes"`
	Splitter     string `json:"splitter"`
	Endpoint     bool   `json:"endpoint"`
}

func (c Config) ID() string {
	model := c.Model
	if c.Endpoint {
		model = retrieval.EmbeddingModelKey(model)
	}
	raw, _ := json.Marshal(struct {
		Model                    string
		Dimensions, PassageRunes int
		Splitter                 string
	}{model, c.Dimensions, c.PassageRunes, c.Splitter})
	return workspace.ContentHash(raw)
}
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Model) == "" || len(c.Model) > 200 || c.Dimensions < 1 || c.Dimensions > 4096 || c.PassageRunes < 128 || c.PassageRunes > 4096 || c.Splitter != "markdown-heading-v1" {
		return errors.New("choose a model identity, 1–4096 dimensions, 128–4096 passage characters and markdown-heading-v1 splitter")
	}
	return nil
}

type Passage struct {
	NoteID      string `json:"noteId,omitempty"`
	Container   string `json:"container,omitempty"`
	Priority    int    `json:"priority,omitempty"`
	Citation    string `json:"citation"`
	ID          string `json:"id"`
	Path        string `json:"path"`
	Heading     string `json:"heading"`
	Anchor      string `json:"anchor,omitempty"`
	Hash        string `json:"hash"`
	NoteHash    string `json:"noteHash"`
	SectionHash string `json:"sectionHash"`
	Text        string `json:"text"`
	Revision    string `json:"revision"`
}

func Split(snapshot workspace.Snapshot, config Config) ([]Passage, error) {
	return splitPassages(snapshot, config, nil)
}
func splitPassages(snapshot workspace.Snapshot, config Config, selected func(Passage, [2]int)) ([]Passage, error) {
	if config.PassageRunes < 128 || config.PassageRunes > 4096 {
		return nil, errors.New("invalid passage size")
	}
	result := []Passage{}
	bytes := 0
	for _, file := range snapshot.Files {
		if !utf8.ValidString(file.Content) {
			return nil, errors.New("committed note is not UTF-8: " + file.Path)
		}
		body := workspace.MarkdownBody([]byte(file.Content))
		offset := len(file.Content) - len(body)
		views := workspace.ViewBlockRanges([]byte(file.Content))
		for _, section := range sections([]byte(file.Content)) {
			sectionText := strings.TrimSpace(string(body[section.start:section.end]))
			if sectionText == "" {
				continue
			}
			sectionHash := workspace.ContentHash([]byte(sectionText))
			part := 0
			// View code runs in its own sandbox and is not note text; the text
			// around it is split on its own, so each passage stays a contiguous
			// part of the note that citations can find.
			for _, segment := range withoutRanges(offset+section.start, offset+section.end, views) {
				rawSegment := file.Content[segment[0]:segment[1]]
				position := segment[0] + len(rawSegment) - len(strings.TrimLeftFunc(rawSegment, unicode.IsSpace))
				runes := []rune(strings.TrimSpace(rawSegment))
				for len(runes) > 0 {
					n := min(len(runes), config.PassageRunes)
					if n < len(runes) {
						for i := n; i > n/2; i-- {
							if runes[i-1] == '\n' || runes[i-1] == ' ' {
								n = i
								break
							}
						}
					}
					rawChunk := string(runes[:n])
					content := strings.TrimSpace(rawChunk)
					start := position + len(rawChunk) - len(strings.TrimLeftFunc(rawChunk, unicode.IsSpace))
					position += len(rawChunk)
					runes = runes[n:]
					if content == "" {
						continue
					}
					hash := workspace.ContentHash([]byte(content))
					id := workspace.ContentHash([]byte(file.Path + "\x00" + section.anchor + "\x00" + strconv.Itoa(part) + "\x00" + hash))
					part++
					citation := url.URL{Path: "/" + file.Path, Fragment: section.anchor, RawQuery: url.Values{"fmn-rev": {snapshot.Revision}, "fmn-passage": {hash}, "fmn-chars": {strconv.Itoa(config.PassageRunes)}}.Encode()}
					passage := Passage{NoteID: file.Metadata.ID, Container: file.Metadata.Container, Priority: file.Priority, Citation: citation.String(), ID: id, Path: file.Path, Heading: section.heading, Anchor: section.anchor, Hash: hash, NoteHash: file.Hash, SectionHash: sectionHash, Text: content, Revision: snapshot.Revision}
					if selected != nil {
						selected(passage, [2]int{start, start + len(content)})
					} else {
						result = append(result, passage)
					}
					// Account for retained text and passage metadata, including tiny sections.
					bytes += len(content) + len(file.Path) + len(section.heading) + len(citation.String()) + 512
					if bytes > capacity.IndexBytes.Limit() {
						return nil, capacity.IndexBytes.Error()
					}
				}
			}
		}
	}
	return result, nil
}

// withoutRanges returns the parts of [start, end) outside the sorted ranges.
func withoutRanges(start, end int, ranges [][2]int) [][2]int {
	parts := [][2]int{}
	for _, r := range ranges {
		if r[1] <= start || r[0] >= end {
			continue
		}
		if r[0] > start {
			parts = append(parts, [2]int{start, r[0]})
		}
		start = max(start, r[1])
	}
	if start < end {
		parts = append(parts, [2]int{start, end})
	}
	return parts
}

// sections uses the preview parser's headings, including setext headings, and
// ignores heading-like text in fenced code. Anchors follow Goldmark's IDs.
type section struct {
	start, end      int
	level           int
	heading, anchor string
}

func sections(content []byte) []section {
	body := workspace.MarkdownBody(content)
	tree := goldmark.New(goldmark.WithParserOptions(parser.WithAutoHeadingID())).Parser().Parse(text.NewReader(body))
	result := []section{{start: 0, end: len(body)}}
	ast.Walk(tree, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !enter || !ok {
			return ast.WalkContinue, nil
		}
		if h.Lines().Len() == 0 {
			return ast.WalkContinue, nil
		}
		start := h.Lines().At(0).Start
		// ATX heading text starts after '# '; include the whole heading line.
		for start > 0 && body[start-1] != '\n' {
			start--
		}
		label := string(h.Text(body))
		anchor := ""
		if value, ok := h.AttributeString("id"); ok {
			if b, ok := value.([]byte); ok {
				anchor = string(b)
			}
		}
		result[len(result)-1].end = start
		result = append(result, section{start: start, end: len(body), heading: label, anchor: anchor, level: h.Level})
		return ast.WalkContinue, nil
	})
	return result
}
