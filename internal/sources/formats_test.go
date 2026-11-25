package sources

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestDocumentAndSpreadsheetSources(t *testing.T) {
	for _, ext := range []string{"txt", "csv", "tsv", "docx", "epub", "xlsx"} {
		t.Run(ext, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/reference." + ext)
			if err != nil {
				t.Fatal(err)
			}
			capture, err := Prepare(context.Background(), "reference."+ext, "", raw)
			if err != nil {
				t.Fatal("source format rejected", err)
			}
			if !strings.Contains(capture.Text, "Amber") || capture.Record.Extraction != "extracted" || string(capture.Original) != string(raw) {
				t.Fatal("source content/provenance missing", capture)
			}
			if strings.Contains(capture.Text, "discarded secret") || strings.Contains(capture.Text, "DO NOT EXECUTE") {
				t.Fatal("inactive content extracted", capture.Text)
			}
			if ext == "epub" && strings.Index(capture.Text, "First chapter") > strings.Index(capture.Text, "Second chapter") {
				t.Fatal("spine order lost", capture.Text)
			}
			if ext == "xlsx" && (!strings.Contains(capture.Text, "Supplementary evidence") || !strings.Contains(capture.Text, "no saved result") || !strings.Contains(capture.Text, "Rich text")) {
				t.Fatal("worksheet data missing", capture.Text)
			}
		})
	}
}
