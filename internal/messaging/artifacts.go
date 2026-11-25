package messaging

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"forgetmenote/internal/capacity"
	"html"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

type ArtifactRequest struct {
	Receipt string `json:"receipt"`
	Format  string `json:"format"`
	X       string `json:"x,omitempty"`
	Y       string `json:"y,omitempty"`
	Title   string `json:"title,omitempty"`
}

// RenderArtifact accepts source data selected by a receipt, never rows supplied by
// the model. Chart tooltips retain the source labels and values.
func RenderArtifact(data any, req ArtifactRequest) (Artifact, error) {
	a := Artifact{Receipt: req.Receipt}
	if len(req.Title) > 160 || len(req.X) > 200 || len(req.Y) > 200 {
		return a, errors.New("artifact label is too long")
	}
	if req.Format == "json" {
		a.Name = "result.json"
		a.MIME = "application/json"
		var err error
		a.Data, err = json.MarshalIndent(map[string]any{"receipt": req.Receipt, "data": data}, "", "  ")
		if len(a.Data) > capacity.ArtifactBytes.Limit() {
			return Artifact{}, capacity.ArtifactBytes.Error()
		}
		return a, err
	}
	columns, rows, err := resultTable(data)
	if err != nil {
		return a, err
	}
	switch req.Format {
	case "csv":
		var b bytes.Buffer
		w := csv.NewWriter(&b)
		headers := make([]string, len(columns))
		for i, v := range columns {
			headers[i] = csvCell(v)
		}
		_ = w.Write(headers)
		for _, row := range rows {
			record := make([]string, len(row))
			for i, v := range row {
				if _, numeric := v.(json.Number); numeric {
					record[i] = valueText(v)
				} else {
					record[i] = csvCell(valueText(v))
				}
			}
			_ = w.Write(record)
		}
		w.Flush()
		if err = w.Error(); err != nil {
			return a, err
		}
		a.Name = "result.csv"
		a.MIME = "text/csv"
		a.Data = b.Bytes()
	case "xlsx":
		if len(rows) >= 1048576 || len(columns) > 16384 {
			return a, errors.New("table exceeds the XLSX file format; use CSV or JSON")
		}
		a.Name = "result.xlsx"
		a.MIME = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
		a.Data, err = tableWorkbook(columns, rows, req.Receipt)
	case "bar", "line":
		a.Name = "chart.svg"
		a.MIME = "image/svg+xml"
		a.Data, err = tableChart(columns, rows, req)
		a.Caption = req.Title + " · " + req.X + " / " + req.Y + " · receipt " + req.Receipt
	default:
		return a, errors.New("choose csv, xlsx, json, bar or line")
	}
	if len(a.Data) > capacity.ArtifactBytes.Limit() {
		return Artifact{}, capacity.ArtifactBytes.Error()
	}
	return a, err
}
func valueText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}
func csvCell(s string) string {
	trim := strings.TrimLeft(s, " \t\r\n")
	if strings.ContainsAny(s, "\t\r\n") || trim != "" && strings.ContainsRune("=+-@", rune(trim[0])) {
		return "'" + s
	}
	return s
}
func resultTable(data any) ([]string, [][]any, error) {
	var value any
	raw, err := json.Marshal(data)
	if err != nil || len(raw) > capacity.ArtifactBytes.Limit() {
		return nil, nil, capacity.ArtifactBytes.Error()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err = d.Decode(&value); err != nil {
		return nil, nil, err
	}
	// Code operations retain their execution envelope in the source receipt.
	// Export only a complete successful stdout JSON value, never log fragments.
	if execution, ok := value.(map[string]any); ok {
		if stdout, present := execution["stdout"].(string); present && execution["exitCode"] != nil {
			if execution["exitCode"] != json.Number("0") || execution["timedOut"] != false || execution["stdoutTruncated"] != false || execution["stderrTruncated"] != false {
				return nil, nil, errors.New("table export requires a complete successful execution")
			}
			d = json.NewDecoder(strings.NewReader(stdout))
			d.UseNumber()
			if d.Decode(&value) != nil || d.Decode(new(any)) != io.EOF {
				return nil, nil, errors.New("print one JSON table or array of objects without additional stdout")
			}
		}
	}
	// Object-row APIs commonly return a rows array with auxiliary metadata.
	// Only the explicitly named table becomes the exported file.
	if table, ok := value.(map[string]any); ok && table["columns"] == nil {
		if items, ok := table["rows"].([]any); ok {
			value = items
		}
	}
	var columns []string
	var rows [][]any
	if table, ok := value.(map[string]any); ok {
		cols, ok := table["columns"].([]any)
		if !ok {
			return nil, nil, errors.New("expected rows and named columns, or an array of objects")
		}
		for _, v := range cols {
			name, ok := v.(string)
			if !ok {
				if col, yes := v.(map[string]any); yes {
					name, _ = col["name"].(string)
				}
			}
			if name == "" {
				return nil, nil, errors.New("table column has no name")
			}
			columns = append(columns, name)
		}
		items, ok := table["rows"].([]any)
		if !ok {
			return nil, nil, errors.New("table has no rows")
		}
		for _, v := range items {
			row, ok := v.([]any)
			if !ok || len(row) != len(columns) {
				return nil, nil, errors.New("table row does not match its columns")
			}
			rows = append(rows, row)
		}
	} else if items, ok := value.([]any); ok && len(items) > 0 {
		keys := map[string]bool{}
		for _, v := range items {
			row, ok := v.(map[string]any)
			if !ok {
				return nil, nil, errors.New("expected an array of objects")
			}
			for k := range row {
				keys[k] = true
			}
		}
		for k := range keys {
			columns = append(columns, k)
		}
		sort.Strings(columns)
		for _, v := range items {
			if len(items) > capacity.ArtifactBytes.Limit()/16/max(1, len(columns)) {
				return nil, nil, capacity.ArtifactBytes.Error()
			}
			row := make([]any, len(columns))
			for i, k := range columns {
				row[i] = v.(map[string]any)[k]
			}
			rows = append(rows, row)
		}
	} else {
		return nil, nil, errors.New("result has no tabular data")
	}
	if len(columns) == 0 {
		return nil, nil, errors.New("table exceeds attachment capacity")
	}
	seen := map[string]bool{}
	for _, c := range columns {
		if seen[c] {
			return nil, nil, errors.New("table column names must be unique")
		}
		seen[c] = true
	}
	return columns, rows, nil
}
func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func columnName(n int) string {
	s := ""
	for n++; n > 0; n = (n - 1) / 26 {
		s = string(rune('A'+(n-1)%26)) + s
	}
	return s
}
func tableWorkbook(columns []string, rows [][]any, receipt string) ([]byte, error) {
	var sheet strings.Builder
	sheet.WriteString(`<?xml version="1.0" encoding="UTF-8"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	all := make([][]any, 0, len(rows)+1)
	header := make([]any, len(columns))
	for i, c := range columns {
		header[i] = c
	}
	all = append(all, header)
	all = append(all, rows...)
	for ri, row := range all {
		fmt.Fprintf(&sheet, `<row r="%d">`, ri+1)
		for ci, v := range row {
			cell := columnName(ci) + strconv.Itoa(ri+1)
			switch v := v.(type) {
			case json.Number:
				fmt.Fprintf(&sheet, `<c r="%s"><v>%s</v></c>`, cell, xmlText(string(v)))
			default:
				fmt.Fprintf(&sheet, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, cell, xmlText(valueText(v)))
			}
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData></worksheet>`)
	files := map[string]string{
		"[Content_Types].xml":        `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/></Types>`,
		"_rels/.rels":                `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/></Relationships>`,
		"xl/workbook.xml":            `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Results" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   sheet.String(),
		"docProps/core.xml":          `<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:description>Source receipt: ` + xmlText(receipt) + `</dc:description></cp:coreProperties>`,
	}
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	names := []string{}
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := z.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err = f.Write([]byte(files[name])); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
func tableChart(columns []string, rows [][]any, req ArtifactRequest) ([]byte, error) {
	x, y := -1, -1
	for i, c := range columns {
		if c == req.X {
			x = i
		}
		if c == req.Y {
			y = i
		}
	}
	if x < 0 || y < 0 || x == y || len(rows) == 0 {
		return nil, errors.New("charts require distinct named x/y columns and at least one row")
	}
	values := make([]float64, len(rows))
	low, high := 0.0, 0.0
	for i, row := range rows {
		n, ok := row[y].(json.Number)
		if !ok {
			return nil, errors.New("chart y values must be numeric; nulls are not zero")
		}
		v, err := n.Float64()
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, errors.New("invalid chart value")
		}
		values[i] = v
		low = math.Min(low, v)
		high = math.Max(high, v)
	}
	if low == high {
		high = low + 1
	}
	py := func(v float64) float64 { return 350 - 270*(v-low)/(high-low) }
	var b strings.Builder
	title := req.Title
	if title == "" {
		title = req.Y + " by " + req.X
	}
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="960" height="540" viewBox="0 0 960 540" role="img"><title>%s</title><desc>Receipt %s. Each point is an unmodified source result.</desc><rect width="960" height="540" fill="#fff"/><g font-family="system-ui,sans-serif" fill="#243746"><text x="70" y="35" font-size="22">%s</text>`, html.EscapeString(title), html.EscapeString(req.Receipt), html.EscapeString(title))
	for i := 0; i <= 4; i++ {
		v := low + float64(i)*(high-low)/4
		fmt.Fprintf(&b, `<path d="M70 %.2fH920" stroke="#dce4e9"/><text x="62" y="%.2f" text-anchor="end" font-size="11">%s</text>`, py(v), py(v)+4, html.EscapeString(strconv.FormatFloat(v, 'g', 5, 64)))
	}
	points := []string{}
	step := 850 / float64(len(rows))
	for i, row := range rows {
		xx := 70 + step*(float64(i)+0.5)
		yy := py(values[i])
		label := valueText(row[x])
		fmt.Fprintf(&b, `<text transform="translate(%.2f 375) rotate(35)" font-size="10">%s</text>`, xx, html.EscapeString(label))
		tip := html.EscapeString(label + ": " + valueText(row[y]))
		if req.Format == "bar" {
			fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="#328ec2"><title>%s</title></rect>`, xx-step*.35, math.Min(yy, py(0)), step*.7, math.Abs(yy-py(0)), tip)
		} else {
			points = append(points, fmt.Sprintf("%.2f,%.2f", xx, yy))
			fmt.Fprintf(&b, `<circle cx="%.2f" cy="%.2f" r="3" fill="#328ec2"><title>%s</title></circle>`, xx, yy, tip)
		}
	}
	if req.Format == "line" {
		fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="#328ec2" stroke-width="2"/>`, strings.Join(points, " "))
	}
	fmt.Fprintf(&b, `<text x="70" y="518" font-size="11">Source receipt: %s</text></g></svg>`, html.EscapeString(req.Receipt))
	return []byte(b.String()), nil
}
