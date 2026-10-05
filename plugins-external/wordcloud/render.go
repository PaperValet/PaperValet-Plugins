package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Pure-Go word cloud renderer (no node-canvas): white 900x640 PNG, words on
// an Archimedean spiral, fake bold by double-drawing with a 0.5px offset,
// bilingual footer caption.

const (
	imgWidth   = 900
	imgHeight  = 640
	imgMargin  = 32
	titleSize  = 34
	titleColor = "#111827"
	footerY    = imgHeight - 34

	fontPath = "/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc"
	fontDPI  = 72

	// spiral parameters (t is the step index)
	spiralAngleStep  = 0.38
	spiralRadiusCoef = 5.2
	spiralMaxSteps   = 3600
	layoutAttempts   = 4 // font-size shrinks by 4px per attempt
	sizeShrink       = 4
	overlapPadding   = 4
	minPlaceSize     = 10

	maxWordWidth = imgWidth - imgMargin*2
)

// palette in #RRGGBB order, indexed by wordItem.color.
var palette = []string{
	"#0f766e", "#166534", "#1d4ed8", "#0891b2", "#2563eb", "#ca8a04", "#dc2626", "#7c3aed",
}

// fontCache loads the CJK font once (sync.Once), repacked for sfnt (see
// ttcfix.go), and caches one face per integer pixel size. A Face is not
// safe for concurrent use, so every use goes through the cache mutex.
type fontCache struct {
	once  sync.Once
	f     *opentype.Font
	err   error
	mu    sync.Mutex
	faces map[int]*opentype.Face
}

var fonts fontCache

func (f *fontCache) load() error {
	f.once.Do(func() {
		b, err := loadFontFile(fontPath)
		if err != nil {
			f.err = fmt.Errorf("load font %s: %w", fontPath, err)
			return
		}
		font, err := opentype.Parse(b)
		if err != nil {
			f.err = fmt.Errorf("parse font %s: %w", fontPath, err)
			return
		}
		f.f = font
		f.faces = map[int]*opentype.Face{}
	})
	return f.err
}

// faceLocked returns (creating if needed) the face for sizePx; f.mu must be held.
func (f *fontCache) faceLocked(sizePx int) (*opentype.Face, error) {
	if fc, ok := f.faces[sizePx]; ok {
		return fc, nil
	}
	face, err := opentype.NewFace(f.f, &opentype.FaceOptions{Size: float64(sizePx), DPI: fontDPI})
	if err != nil {
		return nil, err
	}
	of := face.(*opentype.Face)
	f.faces[sizePx] = of
	return of, nil
}

// withFace runs fn with the face for sizePx under the cache lock. A Face is
// not safe for concurrent use, so all measure/draw work is serialized here.
func (f *fontCache) withFace(sizePx int, fn func(face *opentype.Face) error) error {
	if err := f.load(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	face, err := f.faceLocked(sizePx)
	if err != nil {
		return err
	}
	return fn(face)
}

func (f *fontCache) measure(word string, sizePx int) (w float64, h float64, err error) {
	err = f.withFace(sizePx, func(face *opentype.Face) error {
		m := face.Metrics()
		d := &font.Drawer{Face: face}
		adv := d.MeasureString(word)
		w = float64(adv) / 64
		h = float64(m.Ascent+m.Descent) / 64
		return nil
	})
	return w, h, err
}

// placedBox is the overlap-test rectangle of a placed word (baseline origin).
type placedBox struct {
	x1, y1, x2, y2 float64
}

func boxOf(w wordItem) placedBox {
	p := float64(overlapPadding)
	return placedBox{
		x1: w.x - p,
		y1: w.y - w.height - p,
		x2: w.x + w.width + p,
		y2: w.y + p,
	}
}

func (a placedBox) overlaps(b placedBox) bool {
	return a.x1 < b.x2 && a.x2 > b.x1 && a.y1 < b.y2 && a.y2 > b.y1
}

// layoutWords positions every word on the spiral, shrinking the font up to
// 4 times. Returns the placed subset and a callback-free pure result.
func layoutWords(words []wordItem) ([]wordItem, error) {
	var placed []wordItem
	var boxes []placedBox
	centerX := float64(imgWidth) / 2
	centerY := float64(imgHeight)/2 - 28

	for _, original := range words {
		originalSize := original.size
		w0, _, err := fonts.measure(original.word, originalSize)
		if err != nil {
			return nil, err
		}
		if w0 > maxWordWidth {
			continue
		}
		done := false
		for attempt := 0; attempt < layoutAttempts && !done; attempt++ {
			size := originalSize - attempt*sizeShrink
			if size < minPlaceSize {
				size = minPlaceSize
			}
			w, h, err := fonts.measure(original.word, size)
			if err != nil {
				return nil, err
			}
			item := original
			item.size = size
			item.width, item.height = w, h
			for t := 0; t < spiralMaxSteps; t++ {
				angle := float64(t) * spiralAngleStep
				radius := spiralRadiusCoef * math.Sqrt(float64(t))
				item.x = centerX + math.Cos(angle)*radius - item.width/2
				item.y = centerY + math.Sin(angle)*radius + item.height/2
				if item.x < imgMargin || item.y < imgMargin+item.height ||
					item.x+item.width > imgWidth-imgMargin || item.y > imgHeight-78 {
					continue
				}
				nb := boxOf(item)
				hit := false
				for _, b := range boxes {
					if nb.overlaps(b) {
						hit = true
						break
					}
				}
				if hit {
					continue
				}
				placed = append(placed, item)
				boxes = append(boxes, nb)
				done = true
				break
			}
		}
	}
	return placed, nil
}

// renderCloud renders words into a PNG buffer. limit is the message count
// label, valid the number of messages that contributed words.
func renderCloud(words []wordItem, limit, valid int) ([]byte, error) {
	if err := fonts.load(); err != nil {
		return nil, err
	}
	placed, err := layoutWords(words)
	if err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, imgWidth, imgHeight))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)

	for _, item := range placed {
		col, err := parseHex(palette[item.color])
		if err != nil {
			return nil, err
		}
		if err := fonts.withFace(item.size, func(face *opentype.Face) error {
			drawWordAt(img, face, item.word, item.x, item.y, col)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	// footer caption: "最近 N 条热词云 | M 条有效消息"
	title := fmt.Sprintf("最近 %d 条热词云 | %d 条有效消息", limit, valid)
	tc, err := parseHex(titleColor)
	if err != nil {
		return nil, err
	}
	if err := fonts.withFace(titleSize, func(face *opentype.Face) error {
		drawWordAt(img, face, title, 42, footerY, tc)
		return nil
	}); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawWordAt draws s with baseline at (x,y), double-drawn for fake bold.
func drawWordAt(img *image.RGBA, face *opentype.Face, s string, x, y float64, col color.RGBA) {
	d := &font.Drawer{Dst: img, Src: image.NewUniform(col), Face: face}
	for pass := 0; pass < 2; pass++ {
		d.Dot = fixed.Point26_6{
			X: fixed.Int26_6(int(math.Round(x*64))) + fixed.Int26_6(pass)*32, // +0.5px pass 2
			Y: fixed.Int26_6(int(math.Round(y * 64))),
		}
		d.DrawString(s)
	}
}

func parseHex(s string) (color.RGBA, error) {
	var r, g, b uint8
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b); err != nil {
		return color.RGBA{}, fmt.Errorf("bad color %q", s)
	}
	return color.RGBA{R: r, G: g, B: b, A: 255}, nil
}
