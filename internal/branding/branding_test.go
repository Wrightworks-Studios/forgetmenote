package branding

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestLogoConversionStripsTrailingContentAndBoundsDecoding(t *testing.T) {
	im := image.NewRGBA(image.Rect(0, 0, 12, 8))
	im.Set(0, 0, color.White)
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, im, nil); err != nil {
		t.Fatal(err)
	}
	raw.WriteString("private EXIF-like content")
	result, err := NormalizeLogo(raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(result, []byte("private")) {
		t.Fatal("original metadata retained")
	}
	decoded, err := png.Decode(bytes.NewReader(result))
	if err != nil || decoded.Bounds() != im.Bounds() {
		t.Fatal("incorrect normalized image", err)
	}
	var tall bytes.Buffer
	if err := png.Encode(&tall, image.NewGray(image.Rect(0, 0, 1, 2049))); err != nil {
		t.Fatal(err)
	}
	for _, input := range [][]byte{[]byte("<svg/>"), make([]byte, MaxLogoBytes+1), tall.Bytes(), result[:20]} {
		if _, err := NormalizeLogo(input); err == nil {
			t.Fatal("unsafe or malformed image accepted")
		}
	}
}

func decodeIcon(t *testing.T, raw []byte, err error, size int) *image.NRGBA {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if im.Bounds() != image.Rect(0, 0, size, size) {
		t.Fatalf("icon is %v, want %d × %d", im.Bounds(), size, size)
	}
	out := image.NewNRGBA(im.Bounds())
	for y := range size {
		for x := range size {
			out.Set(x, y, im.At(x, y))
		}
	}
	return out
}

func encodeLogo(t *testing.T, im image.Image) []byte {
	t.Helper()
	var raw bytes.Buffer
	if err := png.Encode(&raw, im); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func TestAppIconPadsLogosToSquares(t *testing.T) {
	red, blue := color.NRGBA{200, 0, 0, 255}, color.NRGBA{0, 0, 200, 255}
	// A wide logo with an opaque edge: the padding continues its edge color.
	wide := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			wide.Set(x, y, red)
		}
	}
	for y := 8; y < 12; y++ {
		for x := 18; x < 22; x++ {
			wide.Set(x, y, blue)
		}
	}
	raw := encodeLogo(t, wide)
	icon, err := AppIcon(raw, 192, false)
	got := decodeIcon(t, icon, err, 192)
	if got.NRGBAAt(0, 0) != red || got.NRGBAAt(191, 191) != red || got.NRGBAAt(96, 96) != blue {
		t.Fatalf("wide logo: corner %v, center %v", got.NRGBAAt(0, 0), got.NRGBAAt(96, 96))
	}

	// A logo with transparent edges stays transparent around an ordinary icon
	// and gains an opaque background in the maskable one.
	tall := image.NewNRGBA(image.Rect(0, 0, 20, 40))
	for y := 10; y < 30; y++ {
		for x := 5; x < 15; x++ {
			tall.Set(x, y, blue)
		}
	}
	raw = encodeLogo(t, tall)
	icon, err = AppIcon(raw, 512, false)
	got = decodeIcon(t, icon, err, 512)
	if got.NRGBAAt(0, 0).A != 0 || got.NRGBAAt(256, 256) != blue {
		t.Fatalf("transparent logo: corner %v, center %v", got.NRGBAAt(0, 0), got.NRGBAAt(256, 256))
	}
	icon, err = AppIcon(raw, 512, true)
	got = decodeIcon(t, icon, err, 512)
	if got.NRGBAAt(0, 0) != (color.NRGBA{255, 255, 255, 255}) || got.NRGBAAt(256, 256) != blue {
		t.Fatalf("maskable logo: corner %v, center %v", got.NRGBAAt(0, 0), got.NRGBAAt(256, 256))
	}
	// The maskable logo keeps to the central 60%: its 40 px height becomes 307,
	// so the blue middle half spans about 154 px rather than 256.
	if got.NRGBAAt(256, 256-70) != blue || got.NRGBAAt(256, 256-90) == blue {
		t.Fatal("maskable logo is not scaled into the safe zone")
	}

	// Shrinking averages every source pixel rather than sampling some.
	stripes := image.NewGray(image.Rect(0, 0, 2048, 1024))
	for y := range 1024 {
		for x := 0; x < 2048; x += 2 {
			stripes.SetGray(x, y, color.Gray{255})
		}
	}
	icon, err = AppIcon(encodeLogo(t, stripes), 512, false)
	got = decodeIcon(t, icon, err, 512)
	for x := 8; x < 504; x++ {
		if c := got.NRGBAAt(x, 256); c.R < 125 || c.R > 130 {
			t.Fatalf("pixel %d is %v, want an even grey", x, c)
		}
	}

	// A one-pixel logo enlarges; oversized and malformed input is refused.
	icon, err = AppIcon(encodeLogo(t, image.NewNRGBA(image.Rect(0, 0, 1, 1))), 512, true)
	decodeIcon(t, icon, err, 512)
	if _, err := AppIcon([]byte("<svg/>"), 192, false); err == nil {
		t.Fatal("malformed logo accepted")
	}
	if _, err := AppIcon(raw, 4096, false); err == nil {
		t.Fatal("oversized icon accepted")
	}
}
