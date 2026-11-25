package web

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
)

// noisyLogo is a PNG of n×n pixels that compresses poorly, as photographs do.
func noisyLogo(t testing.TB, n int, seed uint64) []byte {
	t.Helper()
	random := rand.New(rand.NewPCG(seed, seed))
	im := image.NewNRGBA(image.Rect(0, 0, n, n))
	for i := range im.Pix {
		im.Pix[i] = byte(random.Uint32())
	}
	im.Set(0, 0, color.NRGBA{A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, im); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestBrandVersionFollowsTheCurrentLogo(t *testing.T) {
	app, _ := fixture(t)
	if brand := currentBrand(app.catalog); brand.Custom || brand.version != "" {
		t.Fatalf("default brand: %+v", brand)
	}
	for seed := range uint64(2) {
		logo := noisyLogo(t, 64, seed)
		if err := app.catalog.SetInstanceBranding(catalog.Bootstrap, "Team", &logo); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("%x", sha256.Sum256(app.catalog.InstanceLogo()))
		brand := currentBrand(app.catalog)
		if !brand.Custom || brand.version != want || brand.Icon != "/static/brand/instance.png?v="+want {
			t.Fatalf("logo %d brand: %+v, want version %s", seed, brand, want)
		}
		for _, icon := range []string{"/static/brand/instance.png", "/favicon.ico"} {
			if w := call(app, "GET", icon, "", ""); w.Header().Get("ETag") != `"`+want+`"` || !bytes.Equal(w.Body.Bytes(), app.catalog.InstanceLogo()) {
				t.Fatalf("logo %d %s ETag %s", seed, icon, w.Header().Get("ETag"))
			}
		}
		if w := call(app, "GET", "/", "", testToken); !strings.Contains(w.Body.String(), "instance.png?v="+want) {
			t.Fatalf("logo %d page lacks its version", seed)
		}
	}
	empty := []byte{}
	if err := app.catalog.SetInstanceBranding(catalog.Bootstrap, "Team", &empty); err != nil {
		t.Fatal(err)
	}
	if brand := currentBrand(app.catalog); brand.Custom || brand.Icon != "/static/brand/flower.svg" {
		t.Fatalf("restored default brand: %+v", brand)
	}
}

// BenchmarkCurrentBrand measures what every MCP result and page render pays
// for the instance brand with a large custom logo.
func BenchmarkCurrentBrand(b *testing.B) {
	app, _ := fixture(b)
	logo := noisyLogo(b, 500, 1)
	if err := app.catalog.SetInstanceBranding(catalog.Bootstrap, "Team", &logo); err != nil {
		b.Fatal(err)
	}
	size := len(app.catalog.InstanceLogo())
	b.ReportAllocs()
	for b.Loop() {
		currentBrand(app.catalog)
	}
	b.ReportMetric(float64(size), "logo-bytes")
}
