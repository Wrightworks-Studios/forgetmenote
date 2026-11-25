package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type sheetRange struct {
	Sheet          string
	C1, C2, R1, R2 int
}

var a1Range = regexp.MustCompile(`^'((?:[^']|'')+)'!([A-Z]+)([1-9][0-9]*)?(?::([A-Z]+)([1-9][0-9]*)?)?$`)

func column(s string) (int, error) {
	n := 0
	for _, c := range s {
		n = n*26 + int(c-'A'+1)
		if n > 18278 {
			return 0, Rejected("column is outside Sheets bounds")
		}
	}
	return n, nil
}
func colName(n int) string {
	out := ""
	for n > 0 {
		n--
		out = string(rune('A'+n%26)) + out
		n /= 26
	}
	return out
}
func parseSheetRange(v string) (sheetRange, error) {
	m := a1Range.FindStringSubmatch(v)
	if m == nil {
		return sheetRange{}, Rejected("use one quoted-tab A1 range; named, relative and multi-area ranges are not allowed")
	}
	if strings.ContainsAny(m[1], "\r\n\x00") {
		return sheetRange{}, Rejected("invalid sheet name")
	}
	a := sheetRange{Sheet: strings.ReplaceAll(m[1], "''", "'")}
	var err error
	a.C1, err = column(m[2])
	if err != nil {
		return a, err
	}
	a.C2 = a.C1
	if m[4] != "" {
		a.C2, err = column(m[4])
		if err != nil {
			return a, err
		}
	}
	if m[3] != "" {
		a.R1, err = strconv.Atoi(m[3])
		if err != nil || a.R1 > 10000000 {
			return a, Rejected("row outside Sheets bounds")
		}
		a.R2 = a.R1
	}
	if m[5] != "" {
		a.R2, err = strconv.Atoi(m[5])
		if err != nil || a.R2 > 10000000 {
			return a, Rejected("row outside Sheets bounds")
		}
	}
	if a.C2 < a.C1 || a.R2 < a.R1 || m[3] == "" && m[5] != "" || m[3] != "" && m[4] != "" && m[5] == "" {
		return a, Rejected("ambiguous or reversed range")
	}
	return a, nil
}
func (a sheetRange) String() string {
	prefix := "'" + strings.ReplaceAll(a.Sheet, "'", "''") + "'!"
	if a.R1 == 0 {
		return prefix + colName(a.C1) + ":" + colName(a.C2)
	}
	return fmt.Sprintf("%s%s%d:%s%d", prefix, colName(a.C1), a.R1, colName(a.C2), a.R2)
}
func (a sheetRange) contains(b sheetRange) bool {
	return a.Sheet == b.Sheet && a.C1 <= b.C1 && a.C2 >= b.C2 && (a.R1 == 0 || b.R1 >= a.R1 && b.R2 <= a.R2 && b.R1 > 0)
}
func (r *runtime) sheetArea(target sheetRange, write, appendRows bool) (SheetArea, error) {
	for _, area := range r.config.Sheets.Areas {
		a, _ := parseSheetRange(area.Range)
		if a.contains(target) && (!write || area.Write) && (!appendRows || area.Append && area.AutomationOwned && a.R1 == 0 && a.C1 == 1) {
			return area, nil
		}
	}
	return SheetArea{}, Rejected("range is outside the connection's permitted spreadsheet, tab or area")
}
func integer(v any) (int, error) {
	switch v := v.(type) {
	case json.Number:
		n, err := strconv.Atoi(string(v))
		return n, err
	case float64:
		if v == float64(int(v)) {
			return int(v), nil
		}
	}
	return 0, errors.New("missing integer in Sheets response")
}
func (r *runtime) sheetMetadata(ctx context.Context, token string) (map[string]any, error) {
	path := "/v4/spreadsheets/" + r.config.Sheets.Spreadsheet
	result, err := r.googleJSON(ctx, token, "GET", path, url.Values{"fields": {"spreadsheetId,properties(locale,timeZone),sheets(properties(sheetId,title,gridProperties))"}}, nil)
	if err != nil {
		return nil, err
	}
	if str(result["spreadsheetId"]) != r.config.Sheets.Spreadsheet {
		return nil, errors.New("spreadsheet identity mismatch")
	}
	sheets := []any{}
	for _, raw := range array(result["sheets"]) {
		p := object(object(raw)["properties"])
		id, err := integer(p["sheetId"])
		if err != nil {
			return nil, err
		}
		for _, area := range r.config.Sheets.Areas {
			if int64(id) == area.SheetID {
				a, _ := parseSheetRange(area.Range)
				if str(p["title"]) != a.Sheet {
					return nil, errors.New("configured tab identity changed; update operator policy")
				}
				sheets = append(sheets, raw)
				break
			}
		}
	}
	result["sheets"] = sheets
	return result, nil
}
func sheetGrid(metadata map[string]any, area SheetArea) (int, int, error) {
	for _, raw := range array(metadata["sheets"]) {
		p := object(object(raw)["properties"])
		id, _ := integer(p["sheetId"])
		if int64(id) != area.SheetID {
			continue
		}
		grid := object(p["gridProperties"])
		rows, e1 := integer(grid["rowCount"])
		cols, e2 := integer(grid["columnCount"])
		if e1 != nil || e2 != nil {
			return 0, 0, errors.New("tab grid dimensions unavailable")
		}
		return rows, cols, nil
	}
	return 0, 0, errors.New("configured tab is missing")
}
func (r *runtime) sheetRead(ctx context.Context, token string, target sheetRange, area SheetArea, metadata map[string]any) (map[string]any, error) {
	rows, cols, err := sheetGrid(metadata, area)
	if err != nil {
		return nil, err
	}
	if target.R1 == 0 {
		target.R1 = 1
		target.R2 = rows
	}
	if target.R2 > rows || target.C2 > cols {
		return nil, Rejected("requested cells exceed the existing tab grid")
	}
	if target.R2-target.R1+1 > r.config.MaxRows || (target.R2-target.R1+1)*(target.C2-target.C1+1) > r.config.MaxBytes/18 {
		return nil, Rejected("range exceeds maxRows; read explicit smaller ranges")
	}
	result, err := r.googleJSON(ctx, token, "GET", "/v4/spreadsheets/"+r.config.Sheets.Spreadsheet, url.Values{"ranges": {target.String()}, "includeGridData": {"true"}, "fields": {"spreadsheetId,sheets(properties(sheetId,title),data(startRow,startColumn,rowData(values(userEnteredValue,effectiveValue,formattedValue))))"}}, nil)
	if err != nil {
		return nil, err
	}
	if str(result["spreadsheetId"]) != r.config.Sheets.Spreadsheet {
		return nil, errors.New("spreadsheet identity mismatch")
	}
	if len(array(result["sheets"])) != 1 {
		return nil, errors.New("Sheets omitted the requested tab")
	}
	cells := make([][]any, target.R2-target.R1+1)
	for i := range cells {
		cells[i] = make([]any, target.C2-target.C1+1)
		for j := range cells[i] {
			cells[i][j] = map[string]any{"present": false}
		}
	}
	for _, raw := range array(result["sheets"]) {
		sheet := object(raw)
		p := object(sheet["properties"])
		id, e := integer(p["sheetId"])
		if e != nil || int64(id) != area.SheetID || str(p["title"]) != target.Sheet {
			return nil, errors.New("unexpected tab in range response")
		}
		for _, rawGrid := range array(sheet["data"]) {
			grid := object(rawGrid)
			sr, sc := 0, 0
			if grid["startRow"] != nil {
				sr, e = integer(grid["startRow"])
				if e != nil {
					return nil, e
				}
			}
			if grid["startColumn"] != nil {
				sc, e = integer(grid["startColumn"])
				if e != nil {
					return nil, e
				}
			}
			for i, row := range array(grid["rowData"]) {
				for j, cell := range array(object(row)["values"]) {
					ri, cj := sr+i-target.R1+1, sc+j-target.C1+1
					if ri < 0 || ri >= len(cells) || cj < 0 || cj >= len(cells[ri]) {
						return nil, errors.New("Sheets returned cells outside requested range")
					}
					v := object(cell)
					if v == nil {
						return nil, errors.New("malformed cell")
					}
					v["present"] = true
					cells[ri][cj] = v
				}
			}
		}
	}
	return map[string]any{"range": target.String(), "rows": cells, "rowCount": len(cells), "columnCount": target.C2 - target.C1 + 1, "sheetID": area.SheetID}, nil
}
func (r *runtime) sheetsCall(ctx context.Context, token, name string, in map[string]any, account string) (any, error) {
	targets := []sheetRange{}
	areas := []SheetArea{}
	read := name == "read" || name == "metadata"
	if name != "metadata" {
		ranges := []any{in["range"]}
		if name == "read" {
			ranges = array(in["ranges"])
		}
		if len(ranges) == 0 {
			return nil, Rejected("at least one range required")
		}
		for _, raw := range ranges {
			target, err := parseSheetRange(str(raw))
			if err != nil {
				return nil, err
			}
			area, err := r.sheetArea(target, !read, name == "append")
			if err != nil {
				return nil, err
			}
			targets = append(targets, target)
			areas = append(areas, area)
		}
	}
	metadata, err := r.sheetMetadata(ctx, token)
	if err != nil {
		return nil, err
	}
	if name == "metadata" {
		metadata["account"] = account
		metadata["complete"] = true
		metadata["retrievedAt"] = timeNow()
		return metadata, nil
	}
	if name == "read" {
		ranges := []any{}
		for i, target := range targets {
			v, err := r.sheetRead(ctx, token, target, areas[i], metadata)
			if err != nil {
				return nil, err
			}
			ranges = append(ranges, v)
		}
		return map[string]any{"account": account, "spreadsheetId": r.config.Sheets.Spreadsheet, "properties": metadata["properties"], "ranges": ranges, "complete": true, "retrievedAt": timeNow()}, nil
	}
	return r.sheetWrite(ctx, token, name, in, account, targets[0], areas[0], metadata)
}
func (r *runtime) sheetWrite(ctx context.Context, token, name string, in map[string]any, account string, target sheetRange, area SheetArea, metadata map[string]any) (any, error) {
	if !identifier.MatchString(str(in["actionID"])) {
		return nil, Rejected("stable actionID required")
	}
	values := array(in["values"])
	expected := array(in["expected"])
	if len(values) != 1 || len(array(values[0])) != target.C2-target.C1+1 {
		return nil, Rejected("write one complete row within the allowed columns")
	}
	newRow := array(values[0])
	for _, v := range newRow {
		if _, ok := v.(string); !ok {
			return nil, Rejected("RAW row values must be strings; preserve identifiers and exact decimal text")
		}
	}
	key, err := integer(in["identityColumn"])
	if err != nil || key < 1 || key > len(newRow) || str(in["identity"]) == "" || newRow[key-1] != in["identity"] {
		return nil, Rejected("new row must retain its configured stable identity")
	}
	_, cols, err := sheetGrid(metadata, area)
	if err != nil {
		return nil, Rejected(err.Error())
	}
	if name == "append" && (target.R1 != 0 || target.C1 != 1 || target.C2 != cols) {
		return nil, Rejected("append requires an automation-owned entire-tab column area")
	}
	view, err := r.sheetRead(ctx, token, target, area, metadata)
	if err != nil {
		return nil, Rejected(err.Error())
	}
	rows := view["rows"].([][]any)
	match := -1
	for i, row := range rows {
		value := object(object(row[key-1])["userEnteredValue"])
		if str(value["stringValue"]) == str(in["identity"]) {
			if match >= 0 {
				return nil, Rejected("duplicate business identity")
			}
			match = i
		}
	}
	if name == "append" {
		if match >= 0 {
			return nil, Rejected("identity already exists; reconcile instead of appending")
		}
		if len(expected) != 0 {
			return nil, Rejected("append expected values must be empty")
		}
	} else {
		if match < 0 || len(expected) != 1 || hash(expected[0]) != hash(rows[match]) {
			return nil, Rejected("row identity or expected cells changed; reread before writing")
		}
	}
	method, path := "PUT", "/v4/spreadsheets/"+r.config.Sheets.Spreadsheet+"/values/"
	actual := target
	if name == "update" {
		if actual.R1 == 0 {
			actual.R1 = 1
		}
		actual.R1 += match
		actual.R2 = actual.R1
	}
	path += url.PathEscape(actual.String())
	query := url.Values{"valueInputOption": {"RAW"}, "includeValuesInResponse": {"true"}, "responseValueRenderOption": {"UNFORMATTED_VALUE"}}
	if name == "append" {
		method = "POST"
		path += ":append"
		query.Set("insertDataOption", "INSERT_ROWS")
	}
	response, err := r.googleJSON(ctx, token, method, path, query, map[string]any{"range": actual.String(), "majorDimension": "ROWS", "values": values})
	if err != nil {
		return nil, err
	}
	update := response
	if name == "append" {
		update = object(response["updates"])
	}
	affected := str(update["updatedRange"])
	if strings.HasPrefix(affected, target.Sheet+"!") {
		affected = "'" + strings.ReplaceAll(target.Sheet, "'", "''") + "'!" + strings.TrimPrefix(affected, target.Sheet+"!")
	}
	changed, err := parseSheetRange(affected)
	if err != nil || !target.contains(changed) || changed.C1 != target.C1 || changed.C2 != target.C2 || changed.R1 != changed.R2 || name == "update" && changed != actual {
		return nil, Unknown("Sheets affected range is unexpected")
	}
	count, e := integer(update["updatedCells"])
	if e != nil || count != len(newRow) || str(response["spreadsheetId"]) != r.config.Sheets.Spreadsheet {
		return nil, Unknown("Sheets acknowledgment cell count or identity mismatch")
	}
	if hash(object(update["updatedData"])["values"]) != hash(values) {
		return nil, Unknown("Sheets returned values differ from submitted RAW values")
	}
	return map[string]any{"account": account, "spreadsheetId": r.config.Sheets.Spreadsheet, "updatedRange": changed.String(), "identity": in["identity"], "actionID": in["actionID"], "accepted": true, "atomic": false, "retrievedAt": timeNow()}, nil
}
