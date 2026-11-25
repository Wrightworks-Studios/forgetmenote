package messaging

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestArtifactsBeyondLegacyRowAndChartLimits(t *testing.T) {
	rows := make([][]any, 10001)
	for i := range rows {
		rows[i] = []any{fmt.Sprint(i), i}
	}
	a, err := RenderArtifact(map[string]any{"columns": []string{"label", "value"}, "rows": rows}, ArtifactRequest{Format: "csv"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := csv.NewReader(bytes.NewReader(a.Data)).ReadAll()
	if err != nil || len(parsed) != len(rows)+1 {
		t.Fatalf("incomplete export: %d %v", len(parsed), err)
	}
	a, err = RenderArtifact(map[string]any{"columns": []string{"label", "value"}, "rows": rows[:51]}, ArtifactRequest{Format: "line", X: "label", Y: "value"})
	if err != nil || !strings.Contains(string(a.Data), "50: 50") {
		t.Fatalf("chart lost last point: %v", err)
	}
}

func TestArtifactsPreserveRowsAndEscapeUntrustedCells(t *testing.T) {
	data := map[string]any{"columns": []string{"name", "count"}, "rows": [][]any{{"=HYPERLINK(\"evil\")", json.Number("7")}, {"<script>", json.Number("-2")}}}
	for _, format := range []string{"csv", "xlsx", "bar", "line", "json"} {
		t.Run(format, func(t *testing.T) {
			a, err := RenderArtifact(data, ArtifactRequest{Receipt: "source-receipt", Format: format, X: "name", Y: "count", Title: "<untrusted>"})
			if err != nil {
				t.Fatal(err)
			}
			switch format {
			case "csv":
				rows, err := csv.NewReader(bytes.NewReader(a.Data)).ReadAll()
				if err != nil || rows[1][0][0] != '\'' || rows[1][1] != "7" {
					t.Fatal(rows, err)
				}
			case "xlsx":
				z, err := zip.NewReader(bytes.NewReader(a.Data), int64(len(a.Data)))
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range z.File {
					r, _ := f.Open()
					raw, _ := io.ReadAll(r)
					r.Close()
					d := xml.NewDecoder(bytes.NewReader(raw))
					for {
						_, err := d.Token()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(f.Name, err)
						}
					}
					if f.Name == "xl/worksheets/sheet1.xml" && (strings.Contains(string(raw), "<f>") || !strings.Contains(string(raw), "&lt;script&gt;")) {
						t.Fatal(string(raw))
					}
				}
			case "bar", "line":
				if strings.Contains(string(a.Data), "<script>") || !strings.Contains(string(a.Data), "source-receipt") || !strings.Contains(string(a.Data), ": -2") {
					t.Fatal(string(a.Data))
				}
			}
		})
	}
}
func TestChartRejectsMissingNumericValuesAndAmbiguousColumns(t *testing.T) {
	for _, data := range []any{map[string]any{"columns": []string{"x", "y"}, "rows": [][]any{{"A", nil}}}, map[string]any{"columns": []string{"x", "x"}, "rows": [][]any{{"A", 7}}}} {
		if _, err := RenderArtifact(data, ArtifactRequest{Format: "bar", X: "x", Y: "y"}); err == nil {
			t.Fatal("invalid chart accepted")
		}
	}
}

func TestArtifactsExportCompleteExecutionTables(t *testing.T) {
	data := map[string]any{"exitCode": 0, "timedOut": false, "stdoutTruncated": false, "stderrTruncated": false, "stdout": `{"rows":[{"market":"LA","impressions":14619778}],"internal_rows":[{"private":"cost audit"}]}`}
	a, err := RenderArtifact(data, ArtifactRequest{Receipt: "execution-receipt", Format: "csv"})
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Data) != "impressions,market\n14619778,LA\n" {
		t.Fatalf("wrong table: %s", a.Data)
	}
	a, err = RenderArtifact(data, ArtifactRequest{Receipt: "execution-receipt", Format: "xlsx"})
	if err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(a.Data), int64(len(a.Data)))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range z.File {
		if f.Name != "xl/worksheets/sheet1.xml" {
			continue
		}
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		found = bytes.Contains(raw, []byte("14619778")) && bytes.Contains(raw, []byte("LA")) && !bytes.Contains(raw, []byte("cost audit"))
	}
	if !found {
		t.Fatal("workbook lost source table or included another table")
	}
	for _, key := range []string{"exitCode", "timedOut", "stdoutTruncated", "stderrTruncated"} {
		bad := make(map[string]any)
		for k, v := range data {
			bad[k] = v
		}
		if key == "exitCode" {
			bad[key] = 1
		} else {
			bad[key] = true
		}
		if _, err := RenderArtifact(bad, ArtifactRequest{Format: "csv"}); err == nil {
			t.Fatalf("accepted failed or partial execution: %s", key)
		}
	}
	for _, stdout := range []string{`startup noise\n[{"market":"LA"}]`, `[{"market":"LA"}] {"other":1}`} {
		data["stdout"] = stdout
		if _, err := RenderArtifact(data, ArtifactRequest{Format: "xlsx"}); err == nil {
			t.Fatal("accepted ambiguous stdout")
		}
	}
}
