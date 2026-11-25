package reposource

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	mdast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

const chunkerID = "source-v2"
const payloadVersion = "source-context-v1"

type SourceChunk struct {
	Container     string `json:"container,omitempty"`
	NoteID        string `json:"noteId,omitempty"`
	Title         string `json:"title,omitempty"`
	ID            string `json:"id"`
	Path          string `json:"path"`
	BlobID        string `json:"blobId"`
	Language      string `json:"language"`
	Role          string `json:"role"`
	Symbol        string `json:"symbol,omitempty"`
	StartLine     int    `json:"startLine"`
	EndLine       int    `json:"endLine"`
	StartByte     int    `json:"startByte"`
	EndByte       int    `json:"endByte"`
	ContentHash   string `json:"contentHash"`
	EmbeddingHash string `json:"embeddingHash"`
	Text          string `json:"text"`
}

func (ch SourceChunk) payload() string {
	return ch.Path + "\n" + ch.Language + "\n" + ch.Symbol + "\n" + ch.Text
}
func (ch SourceChunk) embeddingKey(config IndexConfig) string {
	// Keep the persisted key namespace: identical provider input can reuse its
	// vector even when a new chunker changes other excerpts in the same file.
	return hash([]byte(config.embeddingID() + "\nsource-v1\n" + payloadVersion + "\n" + ch.payload()))
}

var pythonDecl = regexp.MustCompile(`^(?:async\s+)?(?:def|class)\s+([\pL_][\pL\pN_]*)`)
var pythonField = regexp.MustCompile(`^([\pL_][\pL\pN_]*)(?:\s*:\s*[^=]+)?\s*=\s*[^=]`)
var pythonInterpreter = regexp.MustCompile(`^(?:python|pypy)(?:[23](?:\.\d+)*)?$`)

// Track just strings, comments and continuation brackets. Declaration hints
// must not turn a constructor keyword or a docstring example into a definition.
type pythonLex struct {
	quote string
	depth int
}

func (s *pythonLex) code(line string) string {
	var out strings.Builder
	for i := 0; i < len(line); {
		if s.quote != "" {
			if line[i] == '\\' {
				i += min(2, len(line)-i)
				continue
			}
			if strings.HasPrefix(line[i:], s.quote) {
				i += len(s.quote)
				s.quote = ""
			} else {
				i++
			}
			continue
		}
		switch line[i] {
		case '#':
			return out.String()
		case '\'', '"':
			s.quote = line[i : i+1]
			if strings.HasPrefix(line[i:], strings.Repeat(s.quote, 3)) {
				s.quote = strings.Repeat(s.quote, 3)
			}
			i += len(s.quote)
			out.WriteByte('_')
			continue
		case '(', '[', '{':
			s.depth++
		case ')', ']', '}':
			s.depth = max(0, s.depth-1)
		}
		out.WriteByte(line[i])
		i++
	}
	return out.String()
}

func sourceLanguage(p string, b []byte) string {
	lang := strings.TrimPrefix(strings.ToLower(path.Ext(p)), ".")
	if lang != "" || !bytes.HasPrefix(b, []byte("#!")) {
		return lang
	}
	line, _, _ := strings.Cut(string(b[:min(len(b), 256)]), "\n")
	for _, word := range strings.Fields(strings.TrimPrefix(line, "#!")) {
		name := path.Base(word)
		if pythonInterpreter.MatchString(name) {
			return "py"
		}
		switch name {
		case "sh", "bash", "dash", "ksh", "zsh":
			return "sh"
		case "node", "nodejs":
			return "js"
		}
	}
	return ""
}

func chunkFile(p, blob string, b []byte, config IndexConfig) []SourceChunk {
	lang := sourceLanguage(p, b)
	role := "source"
	low := strings.ToLower(p)
	switch {
	case strings.Contains(low, "test") || strings.Contains(low, "fixture"):
		role = "test"
	case lang == "md" || lang == "rst" || strings.HasPrefix(low, "docs/"):
		role = "documentation"
	case lang == "yaml" || lang == "yml" || lang == "json" || lang == "toml" || lang == "ini" || strings.HasSuffix(low, "lock"):
		role = "configuration"
	}
	starts := []int{0}
	for i, c := range b {
		if c == '\n' && i+1 < len(b) {
			starts = append(starts, i+1)
		}
	}
	line := func(offset int) int { return sort.Search(len(starts), func(i int) bool { return starts[i] > offset }) }
	symbols := map[int]string{}
	if lang == "go" {
		fs := token.NewFileSet()
		if f, _ := parser.ParseFile(fs, p, b, parser.ParseComments); f != nil {
			for _, d := range f.Decls {
				name := ""
				pos := d.Pos()
				switch v := d.(type) {
				case *ast.FuncDecl:
					name = v.Name.Name
					if v.Doc != nil {
						pos = v.Doc.Pos()
					}
				case *ast.GenDecl:
					if v.Doc != nil {
						pos = v.Doc.Pos()
					}
					if len(v.Specs) > 0 {
						switch n := v.Specs[0].(type) {
						case *ast.TypeSpec:
							name = n.Name.Name
						case *ast.ValueSpec:
							if len(n.Names) > 0 {
								name = n.Names[0].Name
							}
						}
					}
				}
				if name != "" {
					symbols[fs.Position(pos).Offset] = name
				}
			}
		}
	}
	if lang == "md" {
		doc := goldmark.DefaultParser().Parse(text.NewReader(b))
		_ = mdast.Walk(doc, func(n mdast.Node, enter bool) (mdast.WalkStatus, error) {
			if h, ok := n.(*mdast.Heading); ok && enter && h.Lines().Len() > 0 {
				seg := h.Lines().At(0)
				idx := line(seg.Start) - 1
				symbols[starts[max(0, idx)]] = string(h.Text(b))
			}
			return mdast.WalkContinue, nil
		})
	}
	type pythonScope struct {
		indent int
		class  bool
	}
	scopes := []pythonScope{}
	var py pythonLex
	for i, start := range starts {
		end := len(b)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		v := strings.TrimSpace(string(b[start:end]))
		continued := false
		if lang == "py" {
			continued = py.depth > 0 || py.quote != ""
			v = strings.TrimSpace(py.code(string(b[start:end])))
		}
		if lang == "py" && !continued && v != "" {
			indent := 0
			for _, c := range b[start:end] {
				if c == ' ' {
					indent++
				} else if c == '\t' {
					indent += 8 - indent%8
				} else {
					break
				}
			}
			for len(scopes) > 0 && indent <= scopes[len(scopes)-1].indent {
				scopes = scopes[:len(scopes)-1]
			}
			if m := pythonDecl.FindStringSubmatch(v); m != nil {
				symbols[start] = m[1]
				scopes = append(scopes, pythonScope{indent, strings.HasPrefix(v, "class ")})
			} else if indent == 0 || len(scopes) > 0 && scopes[len(scopes)-1].class {
				if m := pythonField.FindStringSubmatch(v); m != nil {
					symbols[start] = m[1]
				}
			}
		}
		if lang == "rst" && i > 0 && len(v) >= 3 && strings.Trim(v, "=-~^`:#\"+") == "" {
			symbols[starts[i-1]] = strings.TrimSpace(string(b[starts[i-1]:start]))
		}
	}
	boundaries := []int{0}
	for start := range symbols {
		if start > 0 {
			boundaries = append(boundaries, start)
		}
	}
	sort.Ints(boundaries)
	boundaries = append(boundaries, len(b))
	out := []SourceChunk{}
	symbol := ""
	for k := 0; k+1 < len(boundaries); k++ {
		begin, end := boundaries[k], boundaries[k+1]
		if v := symbols[begin]; v != "" {
			symbol = v
		}
		// Pack adjacent small declarations without splitting any of them. This avoids
		// one embedding per trivial method while keeping bounded original excerpts.
		if lang == "py" || lang == "go" {
			for k+2 < len(boundaries) {
				next := boundaries[k+2]
				nextSymbol := symbols[end]
				combined := symbol
				if nextSymbol != "" {
					if combined != "" {
						combined += ", "
					}
					combined += nextSymbol
				}
				if next-begin+len(p)+len(lang)+min(len(combined), 240)+3 > 4096 || utf8.RuneCount(b[begin:next]) > 1600 || line(next-1)-line(begin) >= 80 {
					break
				}
				k++
				end = next
				symbol = combined
			}
		}
		symbol = clipUTF8(symbol, 240)
		prefix := len(p) + len(lang) + len(symbol) + 3
		limit := 4096 - prefix
		for pos := begin; pos < end; {
			stop := pos
			runes := 0
			first := line(pos)
			for stop < end {
				_, n := utf8.DecodeRune(b[stop:end])
				if stop+n-pos > limit || runes >= 1600 || line(stop) > first+79 {
					break
				}
				stop += n
				runes++
			}
			if stop == pos {
				break
			}
			if stop < end {
				if nl := bytes.LastIndexByte(b[pos:stop], '\n'); nl >= 0 && nl > (stop-pos)/2 {
					stop = pos + nl + 1
				}
			}
			ch := SourceChunk{Path: p, BlobID: blob, Language: lang, Role: role, Symbol: symbol, StartLine: line(pos), EndLine: line(stop - 1), StartByte: pos, EndByte: stop, Text: string(b[pos:stop])}
			ch.ContentHash = hash([]byte(ch.Text))
			ch.EmbeddingHash = ch.embeddingKey(config)
			ch.ID = hash([]byte(fmt.Sprintf("%s\n%s\n%d\n%d", p, blob, pos, stop)))
			out = append(out, ch)
			pos = stop
		}
	}
	return out
}
func clipUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
