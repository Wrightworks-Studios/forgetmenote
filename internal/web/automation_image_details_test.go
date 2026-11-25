package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"forgetmenote/internal/chat"
)

func TestScriptImageDetailsPreserveOriginalPixels(t *testing.T) {
	app, execution := scriptFixture(t, "def main(host): return {}", []string{"web.fetch", "ai.images"}, nil, nil)
	original := image.NewRGBA(image.Rect(0, 0, 600, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 600; x++ {
			original.Set(x, y, color.RGBA{uint8(x), uint8(y), 20, 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, original); err != nil {
		t.Fatal(err)
	}
	app.scriptImageFetch = func(context.Context, string) ([]byte, string, string, error) {
		return encoded.Bytes(), "image/png", "https://images.example.org/original.png", nil
	}
	value, err := hostStep(t, app, execution, "original", "web.fetch", map[string]any{"url": "https://images.example.org/original.png", "image": true, "detail": true})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(map[string]any)
	inline, _ := chat.InlineImage(encoded.Bytes())
	if result["image"] != inline {
		t.Fatal("original bytes changed")
	}
	regions, ok := result["regions"].([]chat.WebsiteSection)
	if !ok || len(regions) != 3 {
		t.Fatalf("missing native detail regions: %T", result["regions"])
	}
	end := 0
	for _, region := range regions {
		if region.X > end || region.Y != 0 || region.Height != 200 {
			t.Fatal("region gap")
		}
		raw, err := base64.StdEncoding.DecodeString(strings.SplitN(region.Image, ",", 2)[1])
		if err != nil {
			t.Fatal(err)
		}
		crop, err := png.Decode(bytes.NewReader(raw))
		if err != nil || crop.Bounds().Dx() != region.Width || crop.At(0, 0) != original.At(region.X, 0) {
			t.Fatal("detail pixels changed", err)
		}
		end = region.X + region.Width
	}
	if end != 600 {
		t.Fatal("detail omits original edge")
	}
}

func TestScriptImageDetailsExposeNativeOriginalEdges(t *testing.T) {
	app, execution := scriptFixture(t, "def main(host): return {}", []string{"web.fetch", "ai.images"}, nil, nil)
	original := image.NewRGBA(image.Rect(0, 0, 160, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 160; x++ {
			original.Set(x, y, color.Black)
		}
	}
	for x := 0; x < 16; x++ {
		original.Set(x, 0, color.White)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, original); err != nil {
		t.Fatal(err)
	}
	app.scriptImageFetch = func(context.Context, string) ([]byte, string, string, error) {
		return encoded.Bytes(), "image/png", "https://images.example.org/original.png", nil
	}
	value, err := hostStep(t, app, execution, "original", "web.fetch", map[string]any{"url": "https://images.example.org/original.png", "image": true, "detail": true})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(map[string]any)
	regions, ok := result["edgeRegions"].([]chat.WebsiteSection)
	if !ok || len(regions) != 2 {
		t.Fatalf("missing edge regions: %T", result["edgeRegions"])
	}
	if regions[0].Y != 0 || regions[1].Y+regions[1].Height != 80 {
		t.Fatal("original top/bottom omitted", regions)
	}
	for _, region := range regions {
		if region.X != 0 || region.Width != 160 {
			t.Fatal("edge strip does not retain original sides", region)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.SplitN(region.Image, ",", 2)[1])
		if err != nil {
			t.Fatal(err)
		}
		crop, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < region.Height; y++ {
			for x := 0; x < region.Width; x++ {
				if crop.At(x, y) != original.At(x, region.Y+y) {
					t.Fatal("edge pixels changed")
				}
			}
		}
	}
}
