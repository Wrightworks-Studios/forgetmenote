//go:build !fmn_omit_ocr

package sources

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"forgetmenote/internal/workspace"
)

// Local OCR runs Tesseract on images and on the scanned regions of PDF pages.
// A build with the tag fmn_omit_ocr leaves it out (ocr_omitted.go): images are
// then not offered as sources, and PDFs keep their native text.

const (
	pdfCanceledTools = "Poppler and Tesseract"
	pdfExtractor     = "ForgetMeNote PDF v2; Poppler text and geometry; Tesseract eng PSM3 (text-first, physical pages)"
	pdfWarning       = "OCR, tables, equations and reading order may contain errors. Verify against the retained original. No external upload was performed."
)

func ocrPDFPage(ctx context.Context, dir, filename string, number int, geometry pdfGeometry) (string, error) {
	if geometry.Width <= 0 || geometry.Height <= 0 || math.IsNaN(geometry.Width) || math.IsNaN(geometry.Height) || math.IsInf(geometry.Width, 0) || math.IsInf(geometry.Height, 0) {
		return "", errors.New("invalid page dimensions")
	}
	rendered := filepath.Join(dir, "page")
	defer os.Remove(rendered + ".png")
	defer os.Remove(filepath.Join(dir, "masked.png"))
	if _, err := sourceCommand(ctx, dir, 1024, "pdftoppm", "-f", strconv.Itoa(number), "-l", strconv.Itoa(number), "-r", "300", "-scale-to", "4096", "-singlefile", "-png", filename, rendered); err != nil {
		return "", err
	}
	f, err := os.Open(rendered + ".png")
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 32<<20 {
		return "", errors.New("rendered page exceeds 32 MiB scratch budget")
	}
	config, err := png.DecodeConfig(f)
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 {
		return "", errors.New("rendered page exceeds pixel budget")
	}
	if _, err = f.Seek(0, 0); err != nil {
		return "", err
	}
	decoded, err := png.Decode(f)
	if err != nil {
		return "", err
	}
	masked, ok := decoded.(draw.Image)
	if !ok {
		return "", errors.New("unsupported rendered pixel format")
	}
	sx, sy := float64(config.Width)/geometry.Width, float64(config.Height)/geometry.Height
	// Native words remain authoritative. Erase their rendered glyphs before OCR,
	// so an image behind a text layer cannot duplicate those same words. The rest
	// of the page remains available, including scanned regions beside native text.
	for _, word := range geometry.words() {
		if math.IsNaN(word.X1) || math.IsNaN(word.Y1) || math.IsNaN(word.X2) || math.IsNaN(word.Y2) || math.IsInf(word.X1, 0) || math.IsInf(word.Y1, 0) || math.IsInf(word.X2, 0) || math.IsInf(word.Y2, 0) {
			return "", errors.New("invalid word geometry")
		}
		rect := image.Rect(int(math.Floor(word.X1*sx))-2, int(math.Floor(word.Y1*sy))-2, int(math.Ceil(word.X2*sx))+2, int(math.Ceil(word.Y2*sy))+2).Intersect(masked.Bounds())
		draw.Draw(masked, rect, image.NewUniform(color.White), image.Point{}, draw.Src)
	}
	ink := 0
	bounds := masked.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if y%64 == 0 && ctx.Err() != nil {
			return "", ctx.Err()
		}
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := masked.At(x, y).RGBA()
			if r+g+b < 3*0xc000 {
				ink++
				if ink > 100 {
					break
				}
			}
		}
		if ink > 100 {
			break
		}
	}
	if ink <= 100 {
		return "", nil
	}
	target, err := os.OpenFile(filepath.Join(dir, "masked.png"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	err = png.Encode(&scratchWriter{Writer: target, remaining: 32 << 20}, masked)
	closeErr := target.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	info, err = os.Stat(target.Name())
	if err != nil || info.Size() > 32<<20 {
		return "", errors.New("masked page exceeds scratch budget")
	}
	raw, err := sourceCommand(ctx, dir, workspace.MaxSourceText(), "tesseract", target.Name(), "stdout", "-l", "eng", "--psm", "3")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "", errors.New("rendered content produced no readable OCR text")
	}
	return string(raw), nil
}

func tesseractFrame(ctx context.Context, dir string, raw, header []byte) ([]byte, error) {
	decoded, err := decodeImage(ctx, raw, header)
	if err != nil {
		return nil, refused(err)
	}
	target, err := os.OpenFile(filepath.Join(dir, "frame.png"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer os.Remove(target.Name())
	err = png.Encode(&scratchWriter{Writer: target, remaining: 32 << 20}, decoded)
	closeErr := target.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return sourceCommand(ctx, dir, workspace.MaxSourceText(), "tesseract", target.Name(), "stdout", "-l", "eng", "--psm", "3")
}
