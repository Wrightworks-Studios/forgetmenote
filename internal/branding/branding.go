// Package branding owns the default brand assets and the uploaded-image boundary.
package branding

import (
	"bytes"
	"embed"
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"io/fs"
	"math"
)

const (
	MaxLogoBytes = 1 << 20
	maxLogoSide  = 2048
)

//go:embed assets/*
var assets embed.FS

func Files() fs.FS {
	files, _ := fs.Sub(assets, "assets")
	return files
}

func DefaultIcon() []byte {
	raw, _ := assets.ReadFile("assets/app-icon-512.png")
	return raw
}

var errLogo = errors.New("choose a PNG or JPEG logo up to 1 MiB and 2048 × 2048 pixels")

// decodeLogo checks the size limits before decoding any pixels.
func decodeLogo(raw []byte) (image.Image, error) {
	if len(raw) > MaxLogoBytes {
		return nil, errLogo
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || format != "png" && format != "jpeg" || config.Width < 1 || config.Height < 1 || config.Width > maxLogoSide || config.Height > maxLogoSide {
		return nil, errLogo
	}
	im, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, errLogo
	}
	return im, nil
}

// NormalizeLogo strips metadata and active content by decoding a bounded raster
// and encoding fresh PNG pixels. The same bytes serve the UI and plugin package.
func NormalizeLogo(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	im, err := decodeLogo(raw)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err = png.Encode(&out, im); err != nil {
		return nil, err
	}
	if out.Len() > MaxLogoBytes {
		return nil, errors.New("logo is too large after PNG conversion; use a smaller image")
	}
	return out.Bytes(), nil
}

// AppIcon pads a logo to a size × size PNG for installed-app launchers. An
// ordinary icon fills the square along the logo's longer side. A maskable icon
// keeps the logo within the central 60%, inside the circle platforms may crop
// to, on an opaque background. Padding continues an opaque logo's edge color;
// otherwise it is transparent, or white where the icon must be opaque.
func AppIcon(logo []byte, size int, maskable bool) ([]byte, error) {
	if size < 1 || size > maxLogoSide {
		return nil, errors.New("unsupported icon size")
	}
	im, err := decodeLogo(logo)
	if err != nil {
		return nil, err
	}
	b := im.Bounds()
	src := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(src, src.Bounds(), im, b.Min, draw.Src)

	icon := image.NewRGBA(image.Rect(0, 0, size, size))
	background, opaque := edgeColor(src)
	if !opaque && maskable {
		background, opaque = color.RGBA{255, 255, 255, 255}, true
	}
	if opaque {
		draw.Draw(icon, icon.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
	}
	box := size
	if maskable {
		box = max(1, size*3/5)
	}
	long := max(b.Dx(), b.Dy())
	w, h := max(1, (b.Dx()*box+long/2)/long), max(1, (b.Dy()*box+long/2)/long)
	at := image.Pt((size-w)/2, (size-h)/2)
	draw.Draw(icon, image.Rectangle{at, at.Add(image.Pt(w, h))}, resize(src, w, h), image.Point{}, draw.Over)
	var out bytes.Buffer
	if err := png.Encode(&out, icon); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// edgeColor averages the outermost pixels and reports whether all are opaque.
func edgeColor(im *image.RGBA) (color.RGBA, bool) {
	b := im.Bounds()
	var sum [3]int
	count := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if y != b.Min.Y && y != b.Max.Y-1 && x != b.Min.X && x != b.Max.X-1 {
				x = b.Max.X - 2 // skip the interior of this row
				continue
			}
			c := im.RGBAAt(x, y)
			if c.A != 255 {
				return color.RGBA{}, false
			}
			sum[0], sum[1], sum[2] = sum[0]+int(c.R), sum[1]+int(c.G), sum[2]+int(c.B)
			count++
		}
	}
	return color.RGBA{uint8(sum[0] / count), uint8(sum[1] / count), uint8(sum[2] / count), 255}, true
}

// resize scales premultiplied pixels with a triangle filter whose width
// follows the reduction, averaging every source pixel when shrinking and
// interpolating bilinearly when enlarging.
func resize(src *image.RGBA, w, h int) *image.RGBA {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	columns, rows := filterTaps(sw, w), filterTaps(sh, h)
	// Horizontal pass into a w × sh buffer, then vertical into the result.
	mid := make([]float32, w*sh*4)
	for y := range sh {
		line := src.Pix[y*src.Stride:]
		for x, tap := range columns {
			var c [4]float32
			for i, weight := range tap.weights {
				p := line[(tap.first+i)*4:]
				for k := range c {
					c[k] += weight * float32(p[k])
				}
			}
			copy(mid[(y*w+x)*4:], c[:])
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y, tap := range rows {
		for x := range w {
			var c [4]float32
			for i, weight := range tap.weights {
				p := mid[((tap.first+i)*w+x)*4:]
				for k := range c {
					c[k] += weight * p[k]
				}
			}
			p := dst.Pix[y*dst.Stride+x*4:]
			for k := range c {
				p[k] = uint8(min(255, max(0, math.Round(float64(c[k])))))
			}
		}
	}
	return dst
}

type filterTap struct {
	first   int
	weights []float32
}

func filterTaps(from, to int) []filterTap {
	scale := float64(from) / float64(to)
	radius := max(scale, 1)
	taps := make([]filterTap, to)
	for i := range taps {
		center := (float64(i) + 0.5) * scale
		first := max(0, int(math.Floor(center-radius)))
		last := min(from, int(math.Ceil(center+radius)))
		weights := make([]float32, 0, last-first)
		var total float64
		for j := first; j < last; j++ {
			weight := max(0, 1-math.Abs(float64(j)+0.5-center)/radius)
			weights = append(weights, float32(weight))
			total += weight
		}
		for k := range weights {
			weights[k] = float32(float64(weights[k]) / total)
		}
		taps[i] = filterTap{first, weights}
	}
	return taps
}
