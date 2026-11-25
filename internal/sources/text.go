package sources

import (
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"forgetmenote/internal/capacity"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"forgetmenote/internal/workspace"
)

func plainText(raw []byte) (string, error) {
	data := bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	if len(data) >= 2 && (bytes.Equal(data[:2], []byte{0xff, 0xfe}) || bytes.Equal(data[:2], []byte{0xfe, 0xff})) {
		if len(data)%2 != 0 {
			return "", errors.New("invalid UTF-16 text")
		}
		var order binary.ByteOrder = binary.LittleEndian
		if data[0] == 0xfe {
			order = binary.BigEndian
		}
		units := make([]uint16, 0, (len(data)-2)/2)
		for i := 2; i < len(data); i += 2 {
			units = append(units, order.Uint16(data[i:i+2]))
		}
		for i := 0; i < len(units); i++ {
			if units[i] >= 0xd800 && units[i] <= 0xdbff {
				if i+1 >= len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
					return "", errors.New("invalid UTF-16 surrogate")
				}
				i++
			} else if units[i] >= 0xdc00 && units[i] <= 0xdfff {
				return "", errors.New("invalid UTF-16 surrogate")
			}
		}
		data = []byte(string(utf16.Decode(units)))
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", errors.New("text must be UTF-8 or UTF-16 with a byte-order mark")
	}
	if len(data) > workspace.MaxSourceText() {
		return "", errors.New("text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
	}
	return strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n"), nil
}

func maxTableRows() int    { return maxTableCells() }
func maxTableColumns() int { return maxTableCells() }

// Each logical cell retains strings and row/grid metadata, even when empty.
func maxTableCells() int { return capacity.SourceTextBytes.Limit() / 40 }

type tableRow struct {
	number int
	cells  []string
}

func columnName(index int) string {
	out := ""
	for index >= 0 {
		out = string(rune('A'+index%26)) + out
		index = index/26 - 1
	}
	return out
}
func tableCell(value string) string {
	value = workspace.EscapeSourceText(strings.ReplaceAll(value, "\r", ""))
	return strings.ReplaceAll(value, "\n", "<br>")
}
func writeTable(out *strings.Builder, rows []tableRow) error {
	if len(rows) > maxTableRows() {
		return errors.New("table exceeds the row budget derived from FMN_LIMIT_SOURCE_TEXT_BYTES")
	}
	width, total, textBytes := 0, 0, 0
	for _, row := range rows {
		width = max(width, len(row.cells))
		total += len(row.cells)
		for _, cell := range row.cells {
			textBytes += len(cell)
		}
	}
	if textBytes > workspace.MaxSourceText() {
		return errors.New("table cell text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
	}
	if width > maxTableColumns() || total > maxTableCells() || width*len(rows) > maxTableCells() {
		return errors.New("table exceeds the logical cell budget derived from FMN_LIMIT_SOURCE_TEXT_BYTES")
	}
	for start := 0; start < len(rows); start += 100 {
		end := min(start+100, len(rows))
		fmt.Fprintf(out, "### Rows %d–%d\n\n| Row |", rows[start].number, rows[end-1].number)
		for col := 0; col < width; col++ {
			fmt.Fprintf(out, " %s |", columnName(col))
		}
		out.WriteString("\n| --- |" + strings.Repeat(" --- |", width) + "\n")
		for _, row := range rows[start:end] {
			fmt.Fprintf(out, "| %d |", row.number)
			for col := 0; col < width; col++ {
				cell := ""
				if col < len(row.cells) {
					cell = tableCell(row.cells[col])
				}
				out.WriteString(" " + cell + " |")
			}
			out.WriteByte('\n')
			if out.Len() > workspace.MaxSourceText() {
				return errors.New("extracted text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
			}
		}
		out.WriteByte('\n')
	}
	return nil
}
func delimitedText(raw []byte, delimiter rune) (string, error) {
	text, err := plainText(raw)
	if err != nil {
		return "", err
	}
	reader := csv.NewReader(strings.NewReader(text))
	reader.Comma = delimiter
	reader.FieldsPerRecord = -1
	rows := []tableRow{}
	cells := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("invalid delimited text at record %d: %w", len(rows)+1, err)
		}
		cells += len(record)
		if len(rows) >= maxTableRows() || len(record) > maxTableColumns() || cells > maxTableCells() {
			return "", errors.New("table exceeds the logical cell budget derived from FMN_LIMIT_SOURCE_TEXT_BYTES")
		}
		rows = append(rows, tableRow{len(rows) + 1, record})
	}
	var out strings.Builder
	err = writeTable(&out, rows)
	return out.String(), err
}
func cellColumn(ref string) (int, int, error) {
	col, i := 0, 0
	for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
		col = col*26 + int(ref[i]-'A'+1)
		i++
		if col > 16384 {
			return 0, 0, errors.New("worksheet exceeds the XLSX maximum of 16384 columns")
		}
	}
	row, err := strconv.Atoi(ref[i:])
	if i == 0 || err != nil || row < 1 || row > 1048576 {
		return 0, 0, errors.New("invalid spreadsheet cell reference")
	}
	return col - 1, row, nil
}
