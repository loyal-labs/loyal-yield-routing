package og

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
	"unicode/utf16"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

const (
	Width  = 1200
	Height = 630
)

//go:embed assets
var assets embed.FS

type weight string

const (
	regular  weight = "Regular"
	medium   weight = "Medium"
	semiBold weight = "SemiBold"
)

var fonts = sync.OnceValues(func() (map[weight]*opentype.Font, error) {
	out := map[weight]*opentype.Font{}
	for _, w := range []weight{regular, medium, semiBold} {
		raw, err := assets.ReadFile("assets/fonts/Geist-" + string(w) + ".ttf")
		if err != nil {
			return nil, err
		}
		if out[w], err = opentype.Parse(raw); err != nil {
			return nil, fmt.Errorf("Geist %s: %w", w, err)
		}
	}
	return out, nil
})

type textStyle struct {
	weight        weight
	size          float64
	lineHeight    float64
	letterSpacing float64
	color         color.NRGBA
}

type face struct {
	font.Face
	style  textStyle
	ascent float64
	height float64
}

func newFace(style textStyle) (*face, error) {
	all, err := fonts()
	if err != nil {
		return nil, err
	}
	f, err := opentype.NewFace(all[style.weight], &opentype.FaceOptions{Size: style.size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil, err
	}
	m := f.Metrics()
	return &face{Face: f, style: style, ascent: toFloat(m.Ascent), height: toFloat(m.Ascent + m.Descent)}, nil
}

func toFloat(v fixed.Int26_6) float64 { return float64(v) / 64 }

func toFixed(v float64) fixed.Int26_6 { return fixed.Int26_6(math.Round(v * 64)) }

func (f *face) width(text string) float64 {
	total := 0.0
	prev := rune(-1)
	for _, r := range text {
		if prev >= 0 {
			total += toFloat(f.Kern(prev, r))
		}
		advance, _ := f.GlyphAdvance(r)
		total += toFloat(advance) + f.style.letterSpacing
		prev = r
	}
	return total
}

func (f *face) lineBox() float64 {
	if f.style.lineHeight == 0 {
		return f.style.size
	}
	return f.style.size * f.style.lineHeight
}

func (f *face) draw(dst *image.RGBA, x, top float64, text string) {
	baseline := top + (f.lineBox()-f.height)/2 + f.ascent
	d := font.Drawer{Dst: dst, Src: image.NewUniform(f.style.color), Face: f.Face, Dot: fixed.Point26_6{X: toFixed(x), Y: toFixed(baseline)}}
	prev := rune(-1)
	for _, r := range text {
		if prev >= 0 {
			d.Dot.X += f.Kern(prev, r)
		}
		d.DrawString(string(r))
		d.Dot.X += toFixed(f.style.letterSpacing)
		prev = r
	}
}

func decodePNG(name string) (image.Image, error) {
	raw, err := assets.ReadFile("assets/" + name)
	if err != nil {
		return nil, err
	}
	return png.Decode(bytes.NewReader(raw))
}

func drawScaled(dst *image.RGBA, name string, rect image.Rectangle) error {
	src, err := decodePNG(name)
	if err != nil {
		return err
	}
	xdraw.CatmullRom.Scale(dst, rect, src, src.Bounds(), xdraw.Over, nil)
	return nil
}

func fill(dst *image.RGBA, c color.NRGBA, path func(z *vector.Rasterizer)) {
	z := vector.NewRasterizer(dst.Bounds().Dx(), dst.Bounds().Dy())
	path(z)
	z.Draw(dst, dst.Bounds(), image.NewUniform(c), image.Point{})
}

func circle(z *vector.Rasterizer, cx, cy, r float64) {
	const k = 0.5522847498
	p := func(x, y float64) (float32, float32) { return float32(cx + x*r), float32(cy + y*r) }
	z.MoveTo(p(1, 0))
	cube := func(x1, y1, x2, y2, x3, y3 float64) {
		ax, ay := p(x1, y1)
		bx, by := p(x2, y2)
		cx3, cy3 := p(x3, y3)
		z.CubeTo(ax, ay, bx, by, cx3, cy3)
	}
	cube(1, k, k, 1, 0, 1)
	cube(-k, 1, -1, k, -1, 0)
	cube(-1, -k, -k, -1, 0, -1)
	cube(k, -1, 1, -k, 1, 0)
	z.ClosePath()
}

func roundStroke(dst *image.RGBA, c color.NRGBA, width float64, points ...[2]float64) {
	for i := 0; i+1 < len(points); i++ {
		a, b := points[i], points[i+1]
		dx, dy := b[0]-a[0], b[1]-a[1]
		length := math.Hypot(dx, dy)
		nx, ny := -dy/length*width/2, dx/length*width/2
		fill(dst, c, func(z *vector.Rasterizer) {
			z.MoveTo(float32(a[0]+nx), float32(a[1]+ny))
			z.LineTo(float32(b[0]+nx), float32(b[1]+ny))
			z.LineTo(float32(b[0]-nx), float32(b[1]-ny))
			z.LineTo(float32(a[0]-nx), float32(a[1]-ny))
			z.ClosePath()
		})
	}
	for _, p := range points {
		fill(dst, c, func(z *vector.Rasterizer) { circle(z, p[0], p[1], width/2) })
	}
}

func encode(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func jsLength(s string) int { return len(utf16.Encode([]rune(s))) }

func jsSlice(s string, start, end int) string {
	units := utf16.Encode([]rune(s))
	start, end = max(0, min(start, len(units))), max(0, min(end, len(units)))
	if start >= end {
		return ""
	}
	return string(utf16.Decode(units[start:end]))
}
