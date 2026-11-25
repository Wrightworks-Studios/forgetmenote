package sources

import (
	"context"
	"encoding/binary"
	"errors"
	"image"
	_ "image/jpeg"
	"io"
	"os"
	"strings"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

func imageFrames(raw []byte, extension string) ([][]byte, error) {
	if extension != ".tiff" {
		return [][]byte{nil}, nil
	}
	if len(raw) < 8 {
		return nil, errors.New("invalid TIFF header")
	}
	var order binary.ByteOrder
	switch string(raw[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return nil, errors.New("invalid TIFF byte order")
	}
	if order.Uint16(raw[2:4]) != 42 {
		return nil, errors.New("unsupported TIFF container; use a standard TIFF or PDF")
	}
	offset := order.Uint32(raw[4:8])
	seen := map[uint32]bool{}
	var frames [][]byte
	// Each frame shares its original body; only the eight-byte TIFF header varies.
	// Decoding is incremental below, so pixel buffers never accumulate.
	for offset != 0 {
		if seen[offset] || len(frames) >= 500 || uint64(offset)+2 > uint64(len(raw)) {
			return nil, errors.New("invalid, cyclic or oversized TIFF frame chain")
		}
		seen[offset] = true
		entries := uint64(order.Uint16(raw[offset : offset+2]))
		next := uint64(offset) + 2 + entries*12
		if entries > 4096 || next+4 > uint64(len(raw)) {
			return nil, errors.New("invalid TIFF directory")
		}
		header := append([]byte(nil), raw[:8]...)
		order.PutUint32(header[4:8], offset)
		frames = append(frames, header)
		offset = order.Uint32(raw[next : next+4])
	}
	if len(frames) == 0 {
		return nil, errors.New("TIFF has no frames")
	}
	return frames, nil
}

// headerReader substitutes the current TIFF directory without copying a large
// original for every frame. All offsets still address the unchanged source.
type headerReader struct {
	header, raw []byte
	offset      int
}

func (r *headerReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.raw) {
		return 0, io.EOF
	}
	n := copy(p, r.raw[r.offset:])
	if r.offset < len(r.header) {
		copy(p, r.header[r.offset:min(len(r.header), r.offset+n)])
	}
	r.offset += n
	return n, nil
}
func decodeImage(ctx context.Context, raw, header []byte) (image.Image, error) {
	reader := func() io.Reader { return contextReader{ctx, &headerReader{header: header, raw: raw}} }
	config, _, err := image.DecodeConfig(reader())
	if err != nil {
		return nil, errors.New("invalid or unsupported image encoding")
	}
	if config.Width < 1 || config.Height < 1 || config.Width > 16<<20 || config.Height > (16<<20)/config.Width {
		return nil, errors.New("image exceeds 16 million decoded pixels; resize it or use a PDF")
	}
	decoded, _, err := image.Decode(reader())
	if err != nil {
		return nil, errors.New("corrupt image frame")
	}
	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if max(width, height) > 4096 {
		if width >= height {
			height = max(1, height*4096/width)
			width = 4096
		} else {
			width = max(1, width*4096/height)
			height = 4096
		}
		resized := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.ApproxBiLinear.Scale(resized, resized.Bounds(), decoded, bounds, draw.Src, nil)
		decoded = resized
	}
	return decoded, nil
}
func extractImage(ctx context.Context, extension string, raw []byte, options Options) (string, string, string, error) {
	select {
	case documentWork <- struct{}{}:
		defer func() { <-documentWork }()
	case <-ctx.Done():
		return "", "", "", ctx.Err()
	}
	frames, err := imageFrames(raw, extension)
	if err != nil {
		return "", "", "", refused(err)
	}
	dir, err := scratchDirectory(ctx, "forgetmenote-image-")
	if err != nil {
		return "", "", "", err
	}
	defer os.RemoveAll(dir)
	result := documentResult{Extractor: sourceExtractor("ForgetMeNote direct Tesseract eng PSM 3 v1"), Unit: "page"}
	completed := map[int]PDFPage{}
	for _, page := range options.completedPages {
		completed[page.Number] = page
	}
	for i, header := range frames {
		n := i + 1
		if options.pageStart != nil {
			if err := options.pageStart(n); err != nil {
				return "", "", "", err
			}
		}
		page, done := completed[n]
		if !done {
			var text []byte
			// Hosted OCR decodes, bounds and re-encodes its selected frames itself,
			// so only frames read by local Tesseract are rendered to scratch.
			if !(options.ExternalOCR && options.selectedPage(n, len(frames))) {
				text, err = tesseractFrame(ctx, dir, raw, header)
				if err != nil {
					return "", "", "", err
				}
			}
			page = PDFPage{Number: n, Text: strings.TrimSpace(string(text))}
			// Image checkpoint text stays literal; the shared renderer escapes it once.
			if options.pageDone != nil {
				if err := options.pageDone(page); err != nil {
					return "", "", "", err
				}
			}
		}
		section := documentSection{Number: n}
		if page.Text != "" {
			section.Blocks = []documentBlock{{Kind: "text", Text: page.Text}}
		}
		result.Sections = append(result.Sections, section)
	}
	return renderDocumentResult(result)
}
