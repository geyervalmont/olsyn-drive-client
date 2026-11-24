package tray

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Colours from the web app's tokens: --navy, white, --accent (light and dark),
// --warn (dark theme value, legible on both taskbars) and a neutral grey.
var (
	navy  = color.NRGBA{0x21, 0x2e, 0x45, 0xff}
	light = color.NRGBA{0xff, 0xff, 0xff, 0xff}
	blue  = color.NRGBA{0x49, 0x64, 0x97, 0xff}
	sky   = color.NRGBA{0x95, 0xa6, 0xc6, 0xff}
	amber = color.NRGBA{0xe2, 0xb5, 0x66, 0xff}
	grey  = color.NRGBA{0x8a, 0x8f, 0x99, 0xff}
)

// Theme is the taskbar the icon sits on.
type Theme int

const (
	LightTaskbar Theme = iota
	DarkTaskbar
)

// Icon draws the tray icon for v as a PNG. size is in pixels (32 suits
// Windows at 200% scaling; Windows scales it for other DPIs).
//
// The design follows the app's LetterMark: a drive ring, drawn as a progress
// arc while syncing, with a small badge for paused, offline, attention and
// stopped. It is drawn in code so there are no image assets to license.
func Icon(v View, theme Theme, size int) ([]byte, error) {
	if size < 16 || size > 256 {
		return nil, fmt.Errorf("tray icon size %d out of range", size)
	}
	ink, track, accent := navy, withAlpha(navy, 0x40), blue
	if theme == DarkTaskbar {
		ink, track, accent = light, withAlpha(light, 0x48), sky
	}
	c := newCanvas(size)
	s := float64(size)
	cx, cy := s/2, s/2
	outer, inner := s*0.46, s*0.30

	switch v.Kind {
	case Syncing:
		c.ring(cx, cy, inner, outer, 0, 1, track)
		p := v.Progress
		if p < 0 {
			p = 0.25
		}
		c.ring(cx, cy, inner, outer, 0, math.Max(p, 0.04), accent)
	case Offline, Stopped:
		c.ring(cx, cy, inner, outer, 0, 1, withAlpha(ink, 0x80))
	default:
		c.ring(cx, cy, inner, outer, 0, 1, ink)
	}
	// Centre dot: the drive itself.
	c.disc(cx, cy, s*0.12, ink)

	badgeR := s * 0.2
	bx, by := s-badgeR-0.5, s-badgeR-0.5
	switch v.Kind {
	case Paused:
		c.disc(bx, by, badgeR, accent)
		w, h := badgeR*0.28, badgeR*1.0
		c.rect(bx-badgeR*0.42, by-h/2, w, h, light)
		c.rect(bx+badgeR*0.14, by-h/2, w, h, light)
	case Attention:
		c.disc(bx, by, badgeR, amber)
		c.rect(bx-badgeR*0.13, by-badgeR*0.62, badgeR*0.26, badgeR*0.78, navy)
		c.disc(bx, by+badgeR*0.5, badgeR*0.15, navy)
	case Offline:
		c.disc(bx, by, badgeR, grey)
		c.rect(bx-badgeR*0.55, by-badgeR*0.12, badgeR*1.1, badgeR*0.24, light)
	case Stopped:
		c.disc(bx, by, badgeR, grey)
		c.rect(bx-badgeR*0.45, by-badgeR*0.45, badgeR*0.9, badgeR*0.9, light)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, c.image()); err != nil {
		return nil, fmt.Errorf("encode tray icon: %w", err)
	}
	return buf.Bytes(), nil
}

// AppIcon draws the application icon (the .exe, the window and the taskbar)
// as a PNG: the drive ring and centre dot in white on a navy rounded square,
// the tray icon's mark on a tile so it reads on any background.
func AppIcon(size int) ([]byte, error) {
	if size < 16 || size > 256 {
		return nil, fmt.Errorf("app icon size %d out of range", size)
	}
	c := newCanvas(size)
	s := float64(size)
	inset := math.Max(1, s*0.04)
	radius := s * 0.22
	c.fill(func(x, y float64) bool { return inRoundedSquare(x, y, inset, s-inset, radius) }, navy)
	cx, cy := s/2, s/2
	c.ring(cx, cy, s*0.21, s*0.33, 0, 1, light)
	c.disc(cx, cy, s*0.085, light)
	var buf bytes.Buffer
	if err := png.Encode(&buf, c.image()); err != nil {
		return nil, fmt.Errorf("encode app icon: %w", err)
	}
	return buf.Bytes(), nil
}

// inRoundedSquare reports whether (x, y) is inside the square [lo, hi]^2
// with corners rounded to radius r.
func inRoundedSquare(x, y, lo, hi, r float64) bool {
	if x < lo || x > hi || y < lo || y > hi {
		return false
	}
	nx := math.Max(math.Max(lo+r-x, x-(hi-r)), 0)
	ny := math.Max(math.Max(lo+r-y, y-(hi-r)), 0)
	return nx*nx+ny*ny <= r*r
}

func withAlpha(c color.NRGBA, a uint8) color.NRGBA { c.A = a; return c }

// canvas is a tiny anti-aliased rasteriser: each pixel is sampled 4x4 times.
type canvas struct {
	size int
	px   []float64 // premultiplied RGBA, 4 per pixel
}

const samples = 4

func newCanvas(size int) *canvas { return &canvas{size: size, px: make([]float64, size*size*4)} }

func (c *canvas) fill(inside func(x, y float64) bool, col color.NRGBA) {
	for py := 0; py < c.size; py++ {
		for px := 0; px < c.size; px++ {
			hits := 0
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					if inside(float64(px)+(float64(sx)+0.5)/samples, float64(py)+(float64(sy)+0.5)/samples) {
						hits++
					}
				}
			}
			if hits == 0 {
				continue
			}
			a := float64(col.A) / 255 * float64(hits) / (samples * samples)
			i := (py*c.size + px) * 4
			// Source-over in premultiplied space.
			c.px[i] = float64(col.R)/255*a + c.px[i]*(1-a)
			c.px[i+1] = float64(col.G)/255*a + c.px[i+1]*(1-a)
			c.px[i+2] = float64(col.B)/255*a + c.px[i+2]*(1-a)
			c.px[i+3] = a + c.px[i+3]*(1-a)
		}
	}
}

func (c *canvas) disc(cx, cy, r float64, col color.NRGBA) {
	c.fill(func(x, y float64) bool { return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r }, col)
}

func (c *canvas) rect(x0, y0, w, h float64, col color.NRGBA) {
	c.fill(func(x, y float64) bool { return x >= x0 && x <= x0+w && y >= y0 && y <= y0+h }, col)
}

// ring fills the annulus between r0 and r1 from fraction a0 to a1 of a turn, clockwise from 12 o'clock.
func (c *canvas) ring(cx, cy, r0, r1, a0, a1 float64, col color.NRGBA) {
	c.fill(func(x, y float64) bool {
		d := (x-cx)*(x-cx) + (y-cy)*(y-cy)
		if d < r0*r0 || d > r1*r1 {
			return false
		}
		if a1-a0 >= 1 {
			return true
		}
		t := math.Atan2(x-cx, cy-y) / (2 * math.Pi)
		if t < 0 {
			t++
		}
		return t >= a0 && t <= a1
	}, col)
}

func (c *canvas) image() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, c.size, c.size))
	for i := 0; i < c.size*c.size; i++ {
		a := c.px[i*4+3]
		if a <= 0 {
			continue
		}
		img.Pix[i*4] = uint8(math.Round(c.px[i*4] / a * 255))
		img.Pix[i*4+1] = uint8(math.Round(c.px[i*4+1] / a * 255))
		img.Pix[i*4+2] = uint8(math.Round(c.px[i*4+2] / a * 255))
		img.Pix[i*4+3] = uint8(math.Round(a * 255))
	}
	return img
}
