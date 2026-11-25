package sources

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"forgetmenote/internal/workspace"
)

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
func headingLevel(style string) int {
	style = strings.ToLower(strings.ReplaceAll(style, " ", ""))
	if strings.HasPrefix(style, "heading") {
		if level, err := strconv.Atoi(strings.TrimPrefix(style, "heading")); err == nil && level >= 1 && level <= 9 {
			return min(6, level+1)
		}
	}
	return 0
}
func docxText(p *documentPackage) (string, error) {
	part, err := p.officePart("word/document.xml")
	if err != nil {
		return "", err
	}
	data, err := p.read(part)
	if err != nil {
		return "", err
	}
	decoder := xml.NewDecoder(contextReader{p.ctx, bytes.NewReader(data)})
	var out, paragraph strings.Builder
	level, inParagraph, table, tableNumber := 0, false, false, 0
	rows := []tableRow{}
	var cells []string
	var cell []string
	inCell := false
	root := ""
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errors.New("invalid DOCX document XML")
		}
		switch n := token.(type) {
		case xml.Directive:
			return "", errors.New("document XML directives are not supported")
		case xml.StartElement:
			if root == "" {
				root = n.Name.Local
				if root != "document" {
					return "", errors.New("DOCX main part is not a Word document")
				}
			}
			switch n.Name.Local {
			case "del", "moveFrom", "drawing", "pict":
				if err := decoder.Skip(); err != nil {
					return "", err
				}
			case "tbl":
				if table {
					if err := decoder.Skip(); err != nil {
						return "", err
					}
					continue
				}
				table = true
				rows = nil
				tableNumber++
			case "tr":
				if table {
					cells = nil
				}
			case "tc":
				if table {
					cell = nil
					inCell = true
				}
			case "p":
				inParagraph = true
				paragraph.Reset()
				level = 0
			case "pStyle":
				if inParagraph {
					level = headingLevel(attr(n, "val"))
				}
			case "outlineLvl":
				if inParagraph {
					if value, err := strconv.Atoi(attr(n, "val")); err == nil && value >= 0 && value < 9 {
						level = min(6, value+2)
					}
				}
			case "t":
				if inParagraph {
					var text string
					if err := decoder.DecodeElement(&text, &n); err != nil {
						return "", err
					}
					paragraph.WriteString(text)
				}
			case "tab":
				if inParagraph {
					paragraph.WriteByte('\t')
				}
			case "br", "cr":
				if inParagraph {
					paragraph.WriteByte('\n')
				}
			}
		case xml.EndElement:
			switch n.Name.Local {
			case "p":
				if inParagraph {
					value := strings.TrimSpace(paragraph.String())
					if inCell {
						cell = append(cell, value)
					} else if value != "" {
						if level > 0 {
							out.WriteString(strings.Repeat("#", level) + " ")
						}
						out.WriteString(workspace.EscapeSourceText(value) + "\n\n")
					}
					inParagraph = false
				}
			case "tc":
				if inCell {
					cells = append(cells, strings.Join(cell, "\n"))
					inCell = false
					if len(cells) > maxTableColumns() {
						return "", errors.New("DOCX table exceeds 128 columns")
					}
				}
			case "tr":
				if table {
					rows = append(rows, tableRow{len(rows) + 1, cells})
					if len(rows) > maxTableRows() {
						return "", errors.New("DOCX table exceeds the row budget derived from FMN_LIMIT_SOURCE_TEXT_BYTES")
					}
				}
			case "tbl":
				if table {
					fmt.Fprintf(&out, "## Table %d\n\n", tableNumber)
					if err := writeTable(&out, rows); err != nil {
						return "", err
					}
					table = false
				}
			}
		}
		if out.Len()+paragraph.Len() > workspace.MaxSourceText() {
			return "", errors.New("extracted DOCX text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
		}
	}
	if root == "" {
		return "", errors.New("empty DOCX document")
	}
	return out.String(), nil
}

type stringRuns struct {
	Text string `xml:"t"`
	Runs []struct {
		Text string `xml:"t"`
	} `xml:"r"`
}

func (s stringRuns) value() string {
	var b strings.Builder
	b.WriteString(s.Text)
	for _, run := range s.Runs {
		b.WriteString(run.Text)
	}
	return b.String()
}

type workbookSheet struct {
	Name  string `xml:"name,attr"`
	ID    string `xml:"id,attr"`
	State string `xml:"state,attr"`
}
type worksheetCell struct {
	Ref     string     `xml:"r,attr"`
	Type    string     `xml:"t,attr"`
	Value   string     `xml:"v"`
	Formula *string    `xml:"f"`
	Inline  stringRuns `xml:"is"`
}

func xlsxText(p *documentPackage) (string, error) {
	part, err := p.officePart("xl/workbook.xml")
	if err != nil {
		return "", err
	}
	var book struct {
		XMLName xml.Name        `xml:"workbook"`
		Sheets  []workbookSheet `xml:"sheets>sheet"`
	}
	if err = p.xml(part, &book); err != nil {
		return "", err
	}
	if len(book.Sheets) == 0 || len(book.Sheets) > 256 {
		return "", errors.New("workbook needs 1–256 sheets")
	}
	links, err := p.relationships(part)
	if err != nil {
		return "", err
	}
	shared := []string{}
	for _, link := range links {
		if strings.HasSuffix(link.Type, "/sharedStrings") {
			if link.Mode == "External" {
				return "", errors.New("external shared strings are not supported")
			}
			target, err := partTarget(part, link.Target)
			if err != nil {
				return "", err
			}
			var doc struct {
				Items []stringRuns `xml:"si"`
			}
			if err = p.xml(target, &doc); err != nil {
				return "", err
			}
			if len(doc.Items) > maxTableCells() {
				return "", errors.New("too many shared spreadsheet strings")
			}
			for _, item := range doc.Items {
				shared = append(shared, item.value())
			}
		}
	}
	var out strings.Builder
	total, textBytes := 0, 0
	for index, sheet := range book.Sheets {
		name := sheet.Name
		if sheet.State == "hidden" || sheet.State == "veryHidden" {
			name += " (hidden)"
		}
		fmt.Fprintf(&out, "## Sheet %d — %s\n\n", index+1, workspace.EscapeSourceText(name))
		link, ok := links[sheet.ID]
		if !ok {
			return "", errors.New("worksheet relationship is missing")
		}
		if link.Mode == "External" {
			return "", errors.New("external worksheets are not supported")
		}
		if !strings.HasSuffix(link.Type, "/worksheet") {
			out.WriteString("This sheet has no supported worksheet cells. See the original workbook.\n\n")
			continue
		}
		target, err := partTarget(part, link.Target)
		if err != nil {
			return "", err
		}
		var doc struct {
			XMLName xml.Name `xml:"worksheet"`
			Rows    []struct {
				Number int             `xml:"r,attr"`
				Cells  []worksheetCell `xml:"c"`
			} `xml:"sheetData>row"`
		}
		if err = p.xml(target, &doc); err != nil {
			return "", err
		}
		if len(doc.Rows) > maxTableRows() {
			return "", errors.New("worksheet exceeds the row budget derived from FMN_LIMIT_SOURCE_TEXT_BYTES")
		}
		rows := []tableRow{}
		lastRow := 0
		for _, row := range doc.Rows {
			number := row.Number
			if number == 0 {
				number = lastRow + 1
			}
			if number <= lastRow || number > 1048576 {
				return "", errors.New("invalid worksheet row order")
			}
			lastRow = number
			cells := []string{}
			lastCol := -1
			for _, cell := range row.Cells {
				col := lastCol + 1
				if cell.Ref != "" {
					var referencedRow int
					col, referencedRow, err = cellColumn(cell.Ref)
					if err != nil {
						return "", err
					}
					if referencedRow != number {
						return "", errors.New("cell row does not match worksheet row")
					}
				}
				if col <= lastCol || col >= maxTableColumns() {
					return "", errors.New("invalid or oversized worksheet columns")
				}
				lastCol = col
				for len(cells) <= col {
					cells = append(cells, "")
				}
				value := cell.Value
				switch cell.Type {
				case "s":
					i, e := strconv.Atoi(value)
					if e != nil || i < 0 || i >= len(shared) {
						return "", errors.New("invalid shared-string reference")
					}
					value = shared[i]
				case "inlineStr":
					value = cell.Inline.value()
				case "b":
					if value == "1" {
						value = "TRUE"
					} else if value == "0" {
						value = "FALSE"
					}
				}
				if cell.Formula != nil {
					if value == "" {
						value = "=" + *cell.Formula + " (no saved result)"
					} else {
						value += " (cached formula result)"
					}
				}
				textBytes += len(value)
				if textBytes > workspace.MaxSourceText() {
					return "", errors.New("worksheet cell text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
				}
				cells[col] = value
				total++
				if total > maxTableCells() {
					return "", errors.New("workbook exceeds the logical cell budget derived from FMN_LIMIT_SOURCE_TEXT_BYTES")
				}
			}
			rows = append(rows, tableRow{number, cells})
		}
		if len(rows) == 0 {
			out.WriteString("No stored cells.\n\n")
		}
		if err = writeTable(&out, rows); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}
