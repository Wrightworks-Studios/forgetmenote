//go:build !fmn_omit_ocr

package sources

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestNativeImageFormatsAndAllTIFFFrames(t *testing.T) {
	tool, err := exec.LookPath("tesseract")
	if err != nil {
		t.Skip("requires Tesseract")
	}
	dir := t.TempDir()
	if err := os.Symlink(tool, dir+"/tesseract"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, ext := range []string{"png", "jpg", "tiff", "webp", "bmp"} {
		t.Run(ext, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/reference." + ext)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Prepare(t.Context(), "image."+ext, "", raw)
			if err != nil || !strings.Contains(got.Text, "Amber") || !strings.Contains(got.Record.Extractor, "Tesseract") || string(got.Original) != string(raw) {
				t.Fatalf("native %s extraction: %q %v", ext, got.Text, err)
			}
			if ext == "tiff" && (!strings.Contains(got.Text, "## Page 2") || !strings.Contains(got.Text, "## Page 3") || strings.Count(got.Text, "Amber") != 2) {
				t.Fatalf("TIFF omitted or duplicated frames: %q", got.Text)
			}
		})
	}
}
