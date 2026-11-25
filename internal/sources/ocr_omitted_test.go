//go:build fmn_omit_ocr

package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Without local OCR a PDF keeps its native text, and a page with images says
// that its scanned regions were not read rather than rendering it for OCR.
func TestPDFWithoutLocalOCRKeepsNativeTextAndMarksScannedPages(t *testing.T) {
	dir := t.TempDir()
	bbox := `<html><body><doc><page width="612" height="792"><flow><block><line><word xMin="10" yMin="10" xMax="30" yMax="20">Amber native</word></line></block></flow></page><page width="612" height="792"/></doc></body></html>`
	for name, body := range map[string]string{"pdftotext": "cat <<'XML'\n" + bbox + "\nXML\n", "pdfimages": "printf '2 0 image\\n'\n", "pdftoppm": "touch rendered\nexit 1\n", "tesseract": "touch read\nexit 1\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := Prepare(t.Context(), "scan.pdf", "", []byte("%PDF-1.7\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Record.Extraction != "partial" || !strings.Contains(got.Text, "Amber native") || !strings.Contains(got.Text, "this build leaves out local OCR") || !strings.Contains(got.Record.Extractor, "no local OCR") {
		t.Fatalf("%+v", got)
	}
	for _, name := range []string{"rendered", "read"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatal("ran the OCR step", name)
		}
	}
}
