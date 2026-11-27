package sources

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/workspace"
)

// Bound synchronous compatibility callers and leave time for a response within the server's
// existing two-minute write deadline. Chat's shorter parent deadline still wins.
const ImportTimeout = 110 * time.Second

// Raster and native-library conversion share one bounded work slot across
// browser, API/MCP, chat and automation callers.
var documentWork = make(chan struct{}, 1)

type documentBlock struct {
	Kind  string     `json:"kind"`
	Text  string     `json:"text,omitempty"`
	Level int        `json:"level,omitempty"`
	Rows  [][]string `json:"rows,omitempty"`
}

type documentResult struct {
	Extractor string            `json:"extractor"`
	Partial   bool              `json:"partial"`
	Unit      string            `json:"unit"`
	Sections  []documentSection `json:"sections"`
}

type documentSection struct {
	Number int             `json:"number"`
	Title  string          `json:"title,omitempty"`
	Blocks []documentBlock `json:"blocks"`
}

func extractDocument(ctx context.Context, extension string, data []byte, options Options) (string, string, string, error) {
	switch extension {
	case ".pptx", ".odt", ".ods", ".odp", ".pages", ".key":
		p, err := openPackage(data)
		if err != nil {
			return "", "", "", refused(err)
		}
		p.ctx = ctx
		// Bound expansion and reject XML entities before any third-party parser
		// reads the package. Parts are never extracted into the filesystem.
		for name := range p.files {
			if strings.HasSuffix(name, ".xml") || strings.HasSuffix(name, ".rels") || strings.HasSuffix(name, ".apxl") {
				raw, err := p.read(name)
				if err != nil {
					return "", "", "", refused(err)
				}
				if err := validateDocumentXML(ctx, raw); err != nil {
					return "", "", "", refused(err)
				}
			}
		}
	}
	if extension == ".pptx" || extension == ".odt" || extension == ".ods" || extension == ".odp" {
		p, err := openPackage(data)
		if err != nil {
			return "", "", "", err
		}
		p.ctx = ctx
		return nativeStructured(options, extension, p)
	}
	if extension == ".eml" {
		text, err := emailText(ctx, data)
		return text, "ForgetMeNote native MIME v1", "extracted", err
	}
	if extension == ".png" || extension == ".jpg" || extension == ".tiff" || extension == ".webp" || extension == ".bmp" {
		return extractImage(ctx, extension, data, options)
	}
	if extension == ".pages" || extension == ".key" {
		return extractIWork(ctx, extension, data)
	}
	if extension == ".msg" {
		text, err := outlookText(ctx, data)
		return text, "ForgetMeNote native Outlook CFB v1", "extracted", err
	}
	return "", "", "", errors.New("unsupported native source format")
}

func validateDocumentXML(ctx context.Context, raw []byte) error {
	const tableNS = "urn:oasis:names:tc:opendocument:xmlns:table:1.0"
	type tableBudget struct{ rows, cells, columns, rowRepeat, declaredColumns int }
	var tables []tableBudget
	decoder := xml.NewDecoder(contextReader{ctx, bytes.NewReader(raw)})
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New("invalid document XML")
		}
		switch token := token.(type) {
		case xml.Directive:
			return errors.New("document XML directives are not supported")
		case xml.StartElement:
			if token.Name.Space != tableNS {
				continue
			}
			// ODF can encode billions of cells in a tiny ZIP entry. Bound the
			// logical expansion before native parsers expand rows or allocate cells.
			rowRepeat, colRepeat := 1, 1
			for _, attr := range token.Attr {
				if attr.Name.Space != tableNS {
					continue
				}
				limit := 0
				switch attr.Name.Local {
				case "number-rows-repeated", "number-rows-spanned":
					limit = maxTableRows()
				case "number-columns-repeated", "number-columns-spanned":
					limit = maxTableColumns()
				}
				if limit == 0 {
					continue
				}
				n, err := strconv.Atoi(attr.Value)
				if err != nil || n < 1 || n > limit {
					return errors.New("OpenDocument table repetition or span exceeds extraction budget")
				}
				if attr.Name.Local == "number-rows-repeated" {
					rowRepeat = n
				} else if attr.Name.Local == "number-columns-repeated" {
					colRepeat = n
				}
			}
			if token.Name.Local == "table" {
				tables = append(tables, tableBudget{})
			} else if len(tables) > 0 {
				table := &tables[len(tables)-1]
				switch token.Name.Local {
				case "table-row":
					table.rows += rowRepeat
					table.rowRepeat, table.columns = rowRepeat, 0
				case "table-cell", "covered-table-cell":
					table.columns += colRepeat
					table.cells += colRepeat * table.rowRepeat
				case "table-column":
					table.declaredColumns += colRepeat
				}
				if table.rows > maxTableRows() || table.columns > maxTableColumns() || table.declaredColumns > maxTableColumns() || table.cells > maxTableCells() {
					return errors.New("OpenDocument table exceeds extraction budget")
				}
			}
		case xml.EndElement:
			if token.Name.Space == tableNS && token.Name.Local == "table" && len(tables) > 0 {
				tables = tables[:len(tables)-1]
			}
		}
	}
}

// sectionLabels name the units a converter or an agent numbers its sections
// in; the label heads each section and makes its citation anchor.
var sectionLabels = map[string]string{"page": "Page", "slide": "Slide", "section": "Section", "segment": "Segment", "sheet": "Sheet"}

func renderDocumentResult(result documentResult) (string, string, string, error) {
	if len(result.Sections) == 0 || len(result.Sections) > 500 || result.Extractor == "" || len(result.Extractor) > 1024 || strings.ContainsAny(result.Extractor, "\x00\r\n") {
		return "", "", "", errors.New("invalid document conversion result")
	}
	if _, ok := sectionLabels[result.Unit]; !ok {
		return "", "", "", errors.New("invalid document section unit")
	}
	for i, page := range result.Sections {
		// Preserve physical numbering, including empty pages and slides.
		if page.Number != i+1 {
			return "", "", "", errors.New("invalid document section sequence")
		}
		if !validSectionTitle(page.Title) {
			return "", "", "", errors.New("invalid document section title")
		}
	}
	markdown, readable, partial, err := renderSections(result)
	if err != nil {
		return "", "", "", err
	}
	state := "extracted"
	if partial || result.Partial {
		state = "partial"
	} else if !readable {
		state = "no-text"
	}
	return markdown, result.Extractor, state, nil
}

func validSectionTitle(title string) bool {
	return utf8.ValidString(title) && !strings.ContainsAny(title, "\x00\r\n") && utf8.RuneCountInString(title) <= 200
}

// renderSections writes validated, physically numbered sections as Markdown:
// a heading per section that its citation anchor follows, then its blocks.
// It reports whether any block had text and whether a table lost its text.
func renderSections(result documentResult) (markdown string, readable, partial bool, err error) {
	label := sectionLabels[result.Unit]
	var out boundedText
	for _, page := range result.Sections {
		title := ""
		if page.Title != "" {
			title = " — " + workspace.EscapeSourceText(page.Title)
		}
		if _, err := fmt.Fprintf(&out, "## %s %d%s\n\n", label, page.Number, title); err != nil {
			return "", false, false, err
		}
		for _, block := range page.Blocks {
			if block.Kind == "table" && !tableHasText(block.Rows) {
				partial = true
				block = documentBlock{Kind: "text", Text: "Table text could not be extracted. See the original file."}
			}
			value, err := documentMarkdown(block)
			if err != nil {
				return "", false, false, err
			}
			if strings.TrimSpace(value) != "" {
				readable = true
				if _, err := out.Write([]byte(value + "\n\n")); err != nil {
					return "", false, false, err
				}
			}
		}
	}
	return out.String(), readable, partial, nil
}

func tableHasText(rows [][]string) bool {
	for _, row := range rows {
		for _, cell := range row {
			if strings.TrimSpace(cell) != "" {
				return true
			}
		}
	}
	return false
}

// Only the converter's structural labels become markup. Source words and table
// cells stay literal, including apparent links, HTML, and Markdown instructions.
func documentMarkdown(block documentBlock) (string, error) {
	if !utf8.ValidString(block.Text) || strings.ContainsRune(block.Text, 0) {
		return "", errors.New("invalid document text")
	}
	block.Text = strings.ReplaceAll(strings.ReplaceAll(block.Text, "\r\n", "\n"), "\r", "\n")
	text := workspace.EscapeSourceText(strings.TrimSpace(block.Text))
	if block.Kind != "table" && text == "" {
		return "", nil
	}
	switch block.Kind {
	case "heading":
		return strings.Repeat("#", min(6, max(3, block.Level+2))) + " " + strings.Join(strings.Fields(text), " "), nil
	case "list":
		return "- " + strings.ReplaceAll(text, "\n", "\n  "), nil
	case "code":
		return "    " + strings.ReplaceAll(block.Text, "\n", "\n    "), nil
	case "text":
		return text, nil
	case "table":
		if len(block.Rows) == 0 || len(block.Rows[0]) == 0 {
			return "", nil
		}
		var table boundedText
		columns := len(block.Rows[0])
		if columns > maxTableColumns() || len(block.Rows) > maxTableRows() || columns*len(block.Rows) > maxTableCells() {
			return "", errors.New("document table exceeds extraction budget")
		}
		for i, row := range block.Rows {
			if len(row) != columns {
				return "", errors.New("invalid document table")
			}
			cells := make([]string, columns)
			for j, cell := range row {
				if !utf8.ValidString(cell) || strings.ContainsRune(cell, 0) {
					return "", errors.New("invalid document cell")
				}
				cells[j] = tableCell(cell)
			}
			if _, err := fmt.Fprintf(&table, "| %s |\n", strings.Join(cells, " | ")); err != nil {
				return "", err
			}
			if i == 0 {
				if _, err := fmt.Fprintf(&table, "|%s\n", strings.Repeat(" --- |", columns)); err != nil {
					return "", err
				}
			}
		}
		return table.String(), nil
	default:
		return "", errors.New("unknown document block type")
	}
}
