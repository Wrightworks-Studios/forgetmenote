//go:build fmn_omit_ocr

package sources

import (
	"context"
	"errors"

	"forgetmenote/internal/integrations"
)

const (
	pdfCanceledTools = "Poppler"
	pdfExtractor     = "ForgetMeNote PDF v2; Poppler text and geometry; no local OCR (text-first, physical pages)"
	pdfWarning       = "This build leaves out local OCR, so scanned regions are not read. Tables, equations and reading order may contain errors. Verify against the retained original. No external upload was performed."
)

// A page with images is marked incomplete with this error.
func ocrPDFPage(context.Context, string, string, int, pdfGeometry) (string, error) {
	return "", errors.New("this build leaves out local OCR")
}

// Image sources are refused before extraction in this build.
func tesseractFrame(context.Context, string, []byte, []byte) ([]byte, error) {
	return nil, refused(integrations.Check("ocr"))
}
