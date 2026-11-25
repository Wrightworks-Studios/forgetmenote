package chat

import (
	"bytes"
	"context"
	"image"
)

// ImageObservations reports measured pixels, not semantic design violations.
// White text/products and intentional margins still require context.
type ImageObservations struct {
	Version             int   `json:"version"`
	Width               int   `json:"width"`
	Height              int   `json:"height"`
	Pixels              int64 `json:"pixels"`
	NearWhitePixels     int64 `json:"nearWhitePixels"`
	NonOpaquePixels     int64 `json:"nonOpaquePixels"`
	LeftUniformColumns  int   `json:"leftUniformColumns"`
	RightUniformColumns int   `json:"rightUniformColumns"`
	// Counts differ from the most frequent corner color, not semantic ink.
	EdgeInk map[string]int `json:"edgeInk,omitempty"`
}

func ObserveImage(ctx context.Context, raw []byte) (ImageObservations, error) {
	out := ImageObservations{Version: 2}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return ImageObservations{}, err
	}
	if len(raw) > 4<<20 || int64(cfg.Width)*int64(cfg.Height) > 16_000_000 {
		return ImageObservations{}, nil
	}
	original, _, err := image.Decode(&websiteImageBuffer{Buffer: *bytes.NewBuffer(raw), ctx: ctx})
	if err != nil {
		return out, err
	}
	bounds := original.Bounds()
	out.Width = bounds.Dx()
	out.Height = bounds.Dy()
	out.Pixels = int64(out.Width) * int64(out.Height)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := original.At(x, y).RGBA()
			if a != 65535 {
				out.NonOpaquePixels++
			}
			if r > 235*257 && g > 235*257 && b > 235*257 {
				out.NearWhitePixels++
			}
		}
	}
	uniform := func(x int) bool {
		r, g, b, a := original.At(x, bounds.Min.Y).RGBA()
		for y := bounds.Min.Y + 1; y < bounds.Max.Y; y++ {
			if ctx.Err() != nil {
				return false
			}
			rr, gg, bb, aa := original.At(x, y).RGBA()
			if rr != r || gg != g || bb != b || aa != a {
				return false
			}
		}
		return true
	}
	for x := bounds.Min.X; x < bounds.Max.X && uniform(x); x++ {
		out.LeftUniformColumns++
	}
	for x := bounds.Max.X - 1; x >= bounds.Min.X && uniform(x); x-- {
		out.RightUniformColumns++
	}
	// Flat backgrounds make boundary detail measurable. Photographs, borders and
	// solid foregrounds may yield the same observation; do not infer clipping.
	pixel := func(x, y int) [4]uint32 { r, g, b, a := original.At(x, y).RGBA(); return [4]uint32{r, g, b, a} }
	colors := map[[4]uint32]int{}
	var reference [4]uint32
	most := 0
	visit := func(x, y int) {
		value := pixel(x, y)
		colors[value]++
		if colors[value] > most {
			most = colors[value]
			reference = value
		}
	}
	visit(bounds.Min.X, bounds.Min.Y)
	visit(bounds.Max.X-1, bounds.Min.Y)
	visit(bounds.Min.X, bounds.Max.Y-1)
	visit(bounds.Max.X-1, bounds.Max.Y-1)
	out.EdgeInk = map[string]int{"top": 0, "bottom": 0, "left": 0, "right": 0}
	for x := bounds.Min.X; x < bounds.Max.X; x++ {
		if pixel(x, bounds.Min.Y) != reference {
			out.EdgeInk["top"]++
		}
		if pixel(x, bounds.Max.Y-1) != reference {
			out.EdgeInk["bottom"]++
		}
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if pixel(bounds.Min.X, y) != reference {
			out.EdgeInk["left"]++
		}
		if pixel(bounds.Max.X-1, y) != reference {
			out.EdgeInk["right"]++
		}
	}
	if err := ctx.Err(); err != nil {
		return ImageObservations{}, err
	}
	return out, nil
}
