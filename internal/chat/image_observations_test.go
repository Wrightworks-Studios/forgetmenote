package chat

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestImageObservationsDoNotLabelWhiteObjectsOrMargins(t *testing.T) {
	im := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			im.Set(x, y, color.Black)
		}
	}
	im.Set(1, 1, color.White)
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	out, err := ObserveImage(t.Context(), b.Bytes())
	if err != nil || out.NearWhitePixels != 1 || out.Pixels != 12 || out.LeftUniformColumns != 1 || out.RightUniformColumns != 2 {
		t.Fatal(out, err)
	}
}

func TestImageObservationsSkipOverCapacityAndHonorCancellation(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(nil), b.Bytes()...)
	binary.BigEndian.PutUint32(raw[16:20], 4001)
	binary.BigEndian.PutUint32(raw[20:24], 4000)
	binary.BigEndian.PutUint32(raw[29:33], crc32.ChecksumIEEE(raw[12:29]))
	out, err := ObserveImage(t.Context(), raw)
	if err != nil || out.Version != 0 {
		t.Fatal(out, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ObserveImage(ctx, b.Bytes()); err == nil {
		t.Fatal("cancelled observation succeeded")
	}
}

func TestImageObservationsMeasureEdgeInkWithoutDeclaringClipping(t *testing.T) {
	im := image.NewRGBA(image.Rect(0, 0, 20, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 20; x++ {
			im.Set(x, y, color.Black)
		}
	}
	for x := 2; x < 7; x++ {
		im.Set(x, 0, color.White)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	out, err := ObserveImage(t.Context(), b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	edges, ok := value["edgeInk"].(map[string]any)
	if !ok || edges["top"] != float64(5) || edges["bottom"] != float64(0) {
		t.Fatal("missing measured edge pixels", string(encoded))
	}
}

// A solid foreground perimeter (for example a cropped large glyph) can have
// the same zero edge count as an empty background. This is not a clearance.
func TestUniformEdgeObservationDoesNotEstablishSemanticAbsence(t *testing.T) {
	for _, foreground := range []bool{false, true} {
		im := image.NewRGBA(image.Rect(0, 0, 20, 20))
		for y := 0; y < 20; y++ {
			for x := 0; x < 20; x++ {
				c := color.Black
				if foreground && (x < 3 || y < 3 || x >= 17 || y >= 17) {
					c = color.White
				}
				im.Set(x, y, c)
			}
		}
		var b bytes.Buffer
		if err := png.Encode(&b, im); err != nil {
			t.Fatal(err)
		}
		out, err := ObserveImage(t.Context(), b.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		for edge, count := range out.EdgeInk {
			if count != 0 {
				t.Fatal(edge, count)
			}
		}
	}
}
