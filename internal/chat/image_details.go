package chat

import (
	"bytes"
	"context"
	"image"
	"image/png"
)

// ImageDetails supplies supplemental native-pixel regions for a second look.
// Original bytes remain authoritative. Optional regions are prepared only when
// the original and the three regions fit Clef's documented media capacity.
func ImageDetails(ctx context.Context, data []byte) ([]WebsiteSection, error) {
	return imageDetails(ctx, data, false)
}

// ImageEdgeDetails preserves original top/bottom edges at native pixel scale.
func ImageEdgeDetails(ctx context.Context, data []byte) ([]WebsiteSection, error) {
	return imageDetails(ctx, data, true)
}

func imageDetails(ctx context.Context, data []byte, edges bool) ([]WebsiteSection, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if int64(cfg.Width)*int64(cfg.Height) > 16_000_000 || len(data) > 4<<20 {
		return nil, nil
	}
	input := &websiteImageBuffer{Buffer: *bytes.NewBuffer(data), ctx: ctx}
	original, _, err := image.Decode(input)
	if err != nil {
		return nil, err
	}
	axis := cfg.Width
	vertical := cfg.Height > cfg.Width
	if vertical {
		axis = cfg.Height
	}
	if axis < 3 {
		return nil, nil
	}
	var rectangles []image.Rectangle
	for i := 0; i < 3; i++ {
		start, end := i*axis/3, (i+1)*axis/3
		overlap := min(32, axis/12)
		if i > 0 {
			start -= overlap
		}
		if i < 2 {
			end += overlap
		}
		r := image.Rect(start, 0, end, cfg.Height)
		if vertical {
			r = image.Rect(0, start, cfg.Width, end)
		}
		rectangles = append(rectangles, r)
	}
	if edges {
		height := max(1, cfg.Height/4)
		rectangles = []image.Rectangle{image.Rect(0, 0, cfg.Width, height), image.Rect(0, cfg.Height-height, cfg.Width, cfg.Height)}
	}
	regions := make([]WebsiteSection, 0, len(rectangles))
	total := len(data)
	for _, r := range rectangles {
		crop := original.(interface {
			SubImage(image.Rectangle) image.Image
		}).SubImage(r)
		encoded := websiteImageBuffer{ctx: ctx}
		if err := png.Encode(&encoded, crop); err != nil {
			return nil, err
		}
		total += encoded.Len()
		if encoded.Len() > 4<<20 || total > 8<<20 {
			return nil, nil
		}
		inline, err := InlineImage(encoded.Bytes())
		if err != nil {
			return nil, err
		}
		regions = append(regions, WebsiteSection{Image: inline, X: r.Min.X, Y: r.Min.Y, Width: r.Dx(), Height: r.Dy()})
	}
	return regions, nil
}
