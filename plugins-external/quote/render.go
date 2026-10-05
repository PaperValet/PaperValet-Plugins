package main

// render.go — quote image drawing (bubble, tail, gradient, avatar, text).

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// render scaling (all metrics in these comments are 1x reference values).
const (
	baseFontSize    = 22 // body text px at scale 1
	baseNameSize    = 18 // sender name px at scale 1
	baseAvatar      = 50 // avatar diameter px at scale 1
	baseEmoji       = 26 // emoji box px at scale 1
	basePad         = 14 // bubble inner padding px at scale 1
	baseBubbleGap   = 10 // gap between message bubbles at scale 1
	baseMaxTextW    = 380
	baseMaxContentW = baseMaxTextW + 2*basePad
	mediaMaxSide    = 240 // media preview max side at scale 1
	replyBarW       = 3   // reply accent bar px at scale 1
	quotePad        = 16  // outer padding at scale 1 (png mode)
	storiesW        = 720
	storiesH        = 1280
)

// renderMsg is one message prepared for rendering.
type renderMsg struct {
	senderID   int64
	name       string
	nameHidden bool
	avatar     image.Image // square, nil = draw initial
	text       string
	entities   []entityRef
	reply      *renderReply
	media      image.Image // decoded preview, nil = none
	mediaCrop  bool
	voice      *voiceInfo
	fileRow    *fileRow
	bgColor    color.RGBA
}

type renderReply struct {
	name  string
	color color.RGBA
	text  string
}

type voiceInfo struct {
	waveform []byte
	duration int
}

type fileRow struct {
	name string
	size int64
}

// renderOptions carries command flags into the renderer.
type renderOptions struct {
	scale     int
	stories   bool
	pngMode   bool
	media     bool
	crop      bool
	emojiFont *emojiFont
}

// loadedFonts bundles parsed font resources.
type loadedFonts struct {
	sans *opentype.Face
	// faces by size
	sansFaces map[int]font.Face
	monoFaces map[int]font.Face
}

var fonts = &loadedFonts{sansFaces: map[int]font.Face{}, monoFaces: map[int]font.Face{}}

func (l *loadedFonts) sansFace(size int) font.Face {
	if f, ok := l.sansFaces[size]; ok {
		return f
	}
	f := faceCache.face(size)
	l.sansFaces[size] = f
	return f
}

func (l *loadedFonts) monoFace(size int) font.Face {
	if f, ok := l.monoFaces[size]; ok {
		return f
	}
	f := faceCache.face(size) // wqy zenhei mono slice, same file
	l.monoFaces[size] = f
	return f
}

// renderer is one layout pass; sizes are already multiplied by scale.
type renderer struct {
	opt    renderOptions
	scaleF float64
	fonts  *loadedFonts
}

func newRenderer(opt renderOptions) *renderer {
	return &renderer{opt: opt, scaleF: float64(opt.scale), fonts: fonts}
}

func (r *renderer) px(v int) int { return int(float64(v) * r.scaleF) }

// fontMetrics returns line height and ascent in px for a face size.
func fontLineMetrics(f font.Face) (ascent, height int) {
	m := f.Metrics()
	ascent = m.Ascent.Ceil()
	height = m.Ascent.Ceil() + m.Descent.Ceil()
	return
}

// drawTextLine draws styled cells at (x, baselineY) and returns the width.
func (r *renderer) drawTextLine(dst *image.NRGBA, line []cell, x, baseline int, textColor color.Color) int {
	face := r.fonts.sansFace(r.px(baseFontSize))
	mono := r.fonts.monoFace(r.px(baseFontSize))
	if face == nil {
		return x
	}
	if mono == nil {
		mono = face
	}
	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(textColor),
		Face: face,
	}
	emojiSize := r.px(baseEmoji)
	for _, c := range line {
		if c.emoji {
			img := r.opt.emojiFont.emojiImage(c.glyph)
			if img == nil {
				continue
			}
			// vertically center the emoji on the text line
			m := face.Metrics()
			box := emojiSize
			y0 := baseline - m.Ascent.Ceil() - (box-m.Ascent.Ceil()-m.Descent.Ceil())/2
			drawScaled(dst, img, image.Rect(x, y0, x+box, y0+box))
			x += box
			continue
		}
		cf := face
		if c.s.code {
			cf = mono
		}
		d.Face = cf
		d.Dot = fixed.P(x, baseline)
		startX := x
		d.DrawString(string(c.r))
		adv := advanceOf(cf, c.r)
		// fake bold: double draw with 1px offset (wqy has no bold face)
		if c.s.bold {
			boldOff := 1
			if r.opt.scale > 2 {
				boldOff = r.opt.scale / 3
			}
			d.Dot = fixed.P(x+boldOff, baseline)
			d.DrawString(string(c.r))
		}
		if c.s.italic {
			// shear via secondary draw offset by 1px right at baseline-1/3
			d.Dot = fixed.P(x+1, baseline-r.px(baseFontSize)/6)
			d.DrawString(string(c.r))
		}
		decor := func(dy int, col color.Color) {
			thick := max(1, r.opt.scale/2)
			fillRect(dst, image.Rect(startX, baseline+dy, startX+adv, baseline+dy+thick), col)
		}
		if c.s.underline {
			decor(2, textColor)
		}
		if c.s.strike {
			decor(-r.px(baseFontSize)/3, textColor)
		}
		if c.s.spoiler {
			fillRect(dst, image.Rect(startX, baseline-r.px(baseFontSize)+2, startX+adv, baseline+2), color.RGBA{20, 20, 20, 255})
		}
		if c.s.code {
			// subtle rounded box behind the rune: simple fill
			fillRect(dst, image.Rect(startX, baseline-r.px(baseFontSize), startX+adv, baseline+2), color.RGBA{255, 255, 255, 26})
		}
		x += adv
	}
	return x
}

// drawScaled scales src into dstRect of dst.
func drawScaled(dst *image.NRGBA, src image.Image, rect image.Rectangle) {
	xdraw.ApproxBiLinear.Scale(dst, rect, src, src.Bounds(), xdraw.Over, nil)
}

// fillRect fills r in c (clipped to dst bounds).
func fillRect(dst *image.NRGBA, r image.Rectangle, c color.Color) {
	draw.Draw(dst, r.Intersect(dst.Bounds()), image.NewUniform(c), image.Point{}, draw.Over)
}

// roundRectPath clips to a rounded rectangle.
func roundRect(r image.Rectangle, radius int) *roundedPath {
	return &roundedPath{r: r, radius: radius}
}

type roundedPath struct {
	r      image.Rectangle
	radius int
}

// contains reports whether the point is inside the rounded rect.
func (p *roundedPath) contains(x, y int) bool {
	r, rad := p.r, p.radius
	if x < r.Min.X || x > r.Max.X || y < r.Min.Y || y > r.Max.Y {
		return false
	}
	// corner check
	cx0, cy0 := r.Min.X+rad, r.Min.Y+rad
	cx1, cy1 := r.Max.X-rad, r.Max.Y-rad
	switch {
	case x < cx0 && y < cy0:
		return dist2(x-cx0, y-cy0) <= rad*rad
	case x > cx1 && y < cy0:
		return dist2(x-cx1, y-cy0) <= rad*rad
	case x < cx0 && y > cy1:
		return dist2(x-cx0, y-cy1) <= rad*rad
	case x > cx1 && y > cy1:
		return dist2(x-cx1, y-cy1) <= rad*rad
	}
	return true
}

func dist2(dx, dy int) int { return dx*dx + dy*dy }

// drawBubble paints a rounded-rect bubble with hairline and soft shadow.
func drawBubble(img *image.NRGBA, r image.Rectangle, radius int, col color.RGBA) {
	// soft shadow (offset down-right, low alpha, drawn first)
	shadow := color.RGBA{0, 0, 0, 60}
	sr := r.Add(image.Pt(0, max(2, r.Dy()/100)))
	fillRounded(img, sr, radius, shadow)
	// bubble body
	fillRounded(img, r, radius, col)
	// hairline border
	strokeRounded(img, r, radius, color.RGBA{255, 255, 255, 28}, 1)
}

// fillRounded fills a rounded rect with a solid color (antialiased edge).
func fillRounded(img *image.NRGBA, r image.Rectangle, rad int, col color.RGBA) {
	if rad > r.Dx()/2 {
		rad = r.Dx() / 2
	}
	if rad > r.Dy()/2 {
		rad = r.Dy() / 2
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			a := roundedAlpha(x, y, r, rad)
			if a == 0 {
				continue
			}
			blendPixel(img, x, y, col, uint8(a))
		}
	}
}

// strokeRounded strokes the rounded rect border.
func strokeRounded(img *image.NRGBA, r image.Rectangle, rad int, col color.RGBA, w int) {
	// outer minus inner fill
	for y := r.Min.Y - w; y < r.Max.Y+w; y++ {
		for x := r.Min.X - w; x < r.Max.X+w; x++ {
			aOut := roundedAlpha(x, y, r, rad)
			if aOut > 0 {
				continue // inside
			}
			aIn := roundedAlpha(x, y, r.Inset(-1), rad) // approximation of inner
			_ = aIn
			// pixel is in the ring if it's within w px of the rounded edge
			if nearRoundedEdge(x, y, r, rad, w) {
				blendPixel(img, x, y, col, 255)
			}
		}
	}
}

// nearRoundedEdge reports whether (x,y) lies within w px outside the
// rounded rect.
func nearRoundedEdge(x, y int, r image.Rectangle, rad, w int) bool {
	// sample: point inside expanded rounded rect but outside r
	expanded := image.Rect(r.Min.X-w, r.Min.Y-w, r.Max.X+w, r.Max.Y+w)
	p := roundRect(expanded, rad)
	if !p.contains(x, y) {
		return false
	}
	q := roundRect(r, rad)
	return !q.contains(x, y)
}

// roundedAlpha returns the coverage (0-255) of a pixel by a rounded rect.
func roundedAlpha(x, y int, r image.Rectangle, rad int) int {
	if !roundRect(r, rad).contains(x, y) {
		return 0
	}
	return 255
}

func blendPixel(img *image.NRGBA, x, y int, c color.RGBA, alpha uint8) {
	if x < 0 || y < 0 || x >= img.Bounds().Dx() || y >= img.Bounds().Dy() {
		return
	}
	i := img.PixOffset(x, y)
	srcA := uint32(alpha) * uint32(c.A) / 255
	if srcA == 0 {
		return
	}
	da := uint32(img.Pix[i+3])
	if da == 0 || srcA == 255 {
		img.Pix[i] = c.R
		img.Pix[i+1] = c.G
		img.Pix[i+2] = c.B
		img.Pix[i+3] = uint8(srcA)
		return
	}
	outA := srcA + da*(255-srcA)/255
	if outA == 0 {
		return
	}
	img.Pix[i] = uint8((uint32(c.R)*srcA + uint32(img.Pix[i])*da*(255-srcA)/255) / outA)
	img.Pix[i+1] = uint8((uint32(c.G)*srcA + uint32(img.Pix[i+1])*da*(255-srcA)/255) / outA)
	img.Pix[i+2] = uint8((uint32(c.B)*srcA + uint32(img.Pix[i+2])*da*(255-srcA)/255) / outA)
	img.Pix[i+3] = uint8(outA)
}

// drawTail paints the bubble tail under the avatar.
func drawTail(img *image.NRGBA, x, y int, size int, col color.RGBA) {
	// simple triangle pointing down-left to the avatar
	for i := 0; i < size; i++ {
		for j := 0; j <= i; j++ {
			blendPixel(img, x+j, y+i, col, 255)
		}
	}
}

// drawAvatar paints a circular avatar (or colored initial) at r.
func drawAvatar(img *image.NRGBA, r image.Rectangle, av image.Image, initial rune, col color.RGBA) {
	cx := r.Min.X + r.Dx()/2
	cy := r.Min.Y + r.Dy()/2
	rad := r.Dx() / 2
	if av != nil {
		// draw scaled then mask circle
		tmp := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
		drawScaled(tmp, av, tmp.Bounds())
		for y := 0; y < r.Dy(); y++ {
			for x := 0; x < r.Dx(); x++ {
				dx, dy := x-r.Dx()/2, y-r.Dy()/2
				d := math.Sqrt(float64(dx*dx + dy*dy))
				nc := tmp.NRGBAAt(x, y)
				rc := color.RGBA{R: nc.R, G: nc.G, B: nc.B, A: nc.A}
				if d <= float64(rad) {
					blendPixel(img, r.Min.X+x, r.Min.Y+y, rc, 255)
				} else if d <= float64(rad)+1 {
					blendPixel(img, r.Min.X+x, r.Min.Y+y, rc, uint8(255*(float64(rad)+1-d)))
				}
			}
		}
		return
	}
	// colored disc + initial
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			dx, dy := x-r.Dx()/2, y-r.Dy()/2
			d := math.Sqrt(float64(dx*dx + dy*dy))
			if d <= float64(rad) {
				blendPixel(img, r.Min.X+x, r.Min.Y+y, col, 255)
			} else if d <= float64(rad)+1 {
				blendPixel(img, r.Min.X+x, r.Min.Y+y, col, uint8(255*(float64(rad)+1-d)))
			}
		}
	}
	if initial != 0 && initial != '?' || true {
		face := faceCache.face(r.Dx() * 2 / 3)
		if face == nil {
			return
		}
		m := face.Metrics()
		d := &font.Drawer{Dst: img, Src: image.NewUniform(color.RGBA{255, 255, 255, 255}), Face: face}
		adv := advanceOf(face, initial)
		d.Dot = fixed.P(cx-adv/2, cy-m.Ascent.Ceil()/2+(m.Ascent.Ceil()+m.Descent.Ceil())/2-m.Descent.Ceil()/2)
		d.DrawString(string(initial))
	}
}

// paintGradient fills img with a vertical gradient between c1 and c2.
func paintGradient(img *image.NRGBA, c1, c2 color.RGBA) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		t := float64(y-b.Min.Y) / float64(max(1, b.Dy()-1))
		c := lerpColor(c1, c2, t)
		for x := b.Min.X; x < b.Max.X; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i] = c.R
			img.Pix[i+1] = c.G
			img.Pix[i+2] = c.B
			img.Pix[i+3] = 255
		}
	}
}

func lerpColor(a, b color.RGBA, t float64) color.RGBA {
	f := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.RGBA{R: f(a.R, b.R), G: f(a.G, b.G), B: f(a.B, b.B), A: 255}
}

// ---------------------------------------------------------------- layout

// measureMsg computes the bubble size for one message.
func (r *renderer) measureMsg(m *renderMsg) (bubble image.Rectangle, textLines [][]cell, meta msgMeta) {
	maxW := r.px(baseMaxTextW)

	// media preview size
	if m.media != nil {
		b := m.media.Bounds()
		side := r.px(mediaMaxSide)
		w, h := b.Dx(), b.Dy()
		if w > side || h > side || (!m.mediaCrop && (w > r.px(160) || h > r.px(160))) {
			s := float64(side) / float64(max(w, h))
			if s > 1 && !m.mediaCrop {
				s = 1
			}
			w, h = int(float64(w)*s), int(float64(h)*s)
		}
		meta.mediaW, meta.mediaH = w, h
	}

	// text lines
	if m.text != "" {
		rs := splitStyles(m.text, m.entities)
		cells := buildCells(rs, r.opt.emojiFont, r.px(baseEmoji), r.fonts.sansFace(r.px(baseFontSize)), r.fonts.monoFace(r.px(baseFontSize)))
		tw := maxW
		if meta.mediaW > 0 {
			tw = maxW // text stays full width; media sits above
		}
		textLines = wrapCells(buildChunks(cells), tw)
	}

	ascent, lh := r.lineMetrics()
	meta.ascent, meta.lineH = ascent, lh

	innerW := maxW
	pad := r.px(basePad)
	if meta.mediaW > innerW {
		innerW = meta.mediaW
	}
	for _, ln := range textLines {
		if w := linesWidth(ln); w > meta.textW {
			meta.textW = w
		}
	}

	// reply block height: name line + preview line
	if m.reply != nil {
		meta.replyH = 2*r.px(baseNameSize) + r.px(6)
	}

	// file/voice row
	if m.voice != nil || m.fileRow != nil {
		meta.rowH = r.px(baseFontSize) + r.px(8)
	}

	bodyH := 0
	if meta.mediaH > 0 {
		bodyH += meta.mediaH + r.px(8)
	}
	if meta.replyH > 0 {
		bodyH += meta.replyH + r.px(6)
	}
	if len(textLines) > 0 {
		bodyH += len(textLines) * meta.lineH
	}
	if meta.rowH > 0 {
		bodyH += meta.rowH
	}
	if bodyH == 0 {
		bodyH = meta.lineH
	}

	// name header row
	if !m.nameHidden {
		meta.nameH = r.px(baseNameSize) + r.px(8)
		bodyH += meta.nameH
	}

	innerH := bodyH
	w := innerW + 2*pad
	h := innerH + 2*pad
	bubble = image.Rect(0, 0, w, h)
	return bubble, textLines, meta
}

type msgMeta struct {
	mediaW, mediaH      int
	textW               int
	ascent, lineH       int
	nameH, replyH, rowH int
}

// laidOut is a measured message ready to paint.
type laidOut struct {
	m      *renderMsg
	bubble image.Rectangle
	lines  [][]cell
	meta   msgMeta
}

func (r *renderer) lineMetrics() (int, int) {
	f := r.fonts.sansFace(r.px(baseFontSize))
	if f == nil {
		return r.px(baseFontSize), int(float64(r.px(baseFontSize)) * 1.45)
	}
	m := f.Metrics()
	return m.Ascent.Ceil(), m.Ascent.Ceil() + m.Descent.Ceil() + r.px(2)
}

// renderQuoteImage composes the full quote image.
func (r *renderer) renderQuoteImage(msgs []*renderMsg, bg1, bg2 color.RGBA) *image.NRGBA {
	pad := r.px(basePad)
	gap := r.px(baseBubbleGap)
	avatarD := r.px(baseAvatar)

	var items []laidOut
	maxW := 0
	for _, m := range msgs {
		b, lines, meta := r.measureMsg(m)
		b = b.Add(image.Pt(avatarD+pad, 0)) // shift right for avatar column
		items = append(items, laidOut{m: m, bubble: b, lines: lines, meta: meta})
		if b.Dx() > maxW {
			maxW = b.Dx()
		}
	}

	// overall canvas (sticker mode: transparent padding around bubbles)
	totalH := 2 * pad
	for i, it := range items {
		totalH += it.bubble.Dy()
		if i < len(items)-1 {
			totalH += gap
		}
	}
	totalW := maxW + 2*pad
	if totalW < avatarD+pad+r.px(40) {
		totalW = avatarD + pad + r.px(40)
	}

	var img *image.NRGBA
	if r.opt.stories {
		img = image.NewNRGBA(image.Rect(0, 0, storiesW, storiesH))
		paintGradient(img, bg1, bg2)
		// center content
		offX := (storiesW - totalW) / 2
		offY := (storiesH - totalH) / 2
		r.drawMessages(img, items, image.Pt(offX, offY), pad, gap, avatarD)
	} else if r.opt.pngMode {
		img = image.NewNRGBA(image.Rect(0, 0, totalW+2*r.px(quotePad), totalH+2*r.px(quotePad)))
		paintGradient(img, bg1, bg2)
		r.drawMessages(img, items, image.Pt(r.px(quotePad), r.px(quotePad)), pad, gap, avatarD)
	} else {
		img = image.NewNRGBA(image.Rect(0, 0, totalW, totalH))
		r.drawMessages(img, items, image.Pt(0, pad), pad, gap, avatarD)
	}
	return img
}

// drawMessages paints bubbles + avatars at offset.
func (r *renderer) drawMessages(img *image.NRGBA, items []laidOut, offset image.Point, pad, gap, avatarD int) {
	y := offset.Y
	for i, it := range items {
		bx := offset.X + it.bubble.Min.X
		bubble := image.Rect(bx, y, bx+it.bubble.Dx(), y+it.bubble.Dy())
		radius := r.px(14)
		drawBubble(img, bubble, radius, r.bubbleColor(it.m))
		// avatar + tail for the first message of a sender group
		if i == 0 || items[i-1].m.senderID != it.m.senderID || it.m.senderID == 0 {
			av := image.Rect(offset.X, y, offset.X+avatarD, y+avatarD)
			if !it.m.nameHidden {
				drawTail(img, offset.X+avatarD-1, y+avatarD-r.px(6), r.px(10), r.bubbleColor(it.m))
				drawAvatar(img, av, it.m.avatar, firstGraphemeRune(it.m.name), it.m.bgColor)
			}
		}
		r.drawBubbleContent(img, it, bubble, pad, avatarD)
		y += bubble.Dy() + gap
	}
}

// bubbleColor picks a slightly translucent dark bubble fill.
func (r *renderer) bubbleColor(m *renderMsg) color.RGBA {
	return color.RGBA{R: 26, G: 23, B: 34, A: 0xF2}
}

// drawBubbleContent paints name/reply/media/text inside the bubble.
func (r *renderer) drawBubbleContent(img *image.NRGBA, it laidOut, bubble image.Rectangle, pad, avatarD int) {
	m, meta := it.m, it.meta
	inner := bubble.Inset(r.px(basePad))
	x := inner.Min.X
	y := inner.Min.Y
	nameCol := paletteColor(m.senderID)
	if m.nameHidden {
		nameCol = color.RGBA{0x88, 0x88, 0x88, 0xFF}
	}

	if !m.nameHidden && m.name != "" {
		face := r.fonts.sansFace(r.px(baseNameSize))
		baseline := y + r.px(baseNameSize)
		dx := x
		// double-draw for bold
		if face != nil {
			d := &font.Drawer{Dst: img, Src: image.NewUniform(nameCol), Face: face}
			for _, rn := range m.name {
				d.Dot = fixed.P(dx, baseline)
				d.DrawString(string(rn))
				d.Dot = fixed.P(dx+max(1, r.opt.scale/3), baseline)
				d.DrawString(string(rn))
				dx += advanceOf(face, rn)
				if dx > inner.Max.X-r.px(30) {
					// clip long names with ellipsis
					d.Dot = fixed.P(dx, baseline)
					d.DrawString("…")
					break
				}
			}
		}
		y += meta.nameH
	}

	if m.reply != nil {
		barX := x
		tx := barX + r.px(replyBarW) + r.px(8)
		rcol := paletteColor(0)
		_ = rcol
		// accent bar
		fillRect(img, image.Rect(barX, y+2, barX+r.px(replyBarW), y+meta.replyH-4), color.RGBA{0x8E, 0x8E, 0x93, 0xFF})
		// name
		nf := r.fonts.sansFace(r.px(baseNameSize) - r.px(4))
		if nf != nil {
			d := &font.Drawer{Dst: img, Src: image.NewUniform(m.reply.color), Face: nf}
			dx := tx
			for _, rn := range truncVisually(m.reply.name, 24) {
				d.Dot = fixed.P(dx, y+r.px(baseNameSize)-r.px(2))
				d.DrawString(string(rn))
				dx += advanceOf(nf, rn)
			}
		}
		// preview text
		tf := r.fonts.sansFace(r.px(baseFontSize) - r.px(4))
		if tf != nil {
			d := &font.Drawer{Dst: img, Src: image.NewUniform(color.RGBA{0xAA, 0xAA, 0xAA, 0xFF}), Face: tf}
			dx := tx
			for _, rn := range truncVisually(m.reply.text, 36) {
				d.Dot = fixed.P(dx, y+meta.replyH-r.px(6))
				d.DrawString(string(rn))
				dx += advanceOf(tf, rn)
				if dx > inner.Max.X {
					break
				}
			}
		}
		y += meta.replyH + r.px(6)
	}

	if m.media != nil && meta.mediaW > 0 {
		dst := image.Rect(x, y, x+meta.mediaW, y+meta.mediaH)
		drawScaled(img, m.media, dst)
		y += meta.mediaH + r.px(8)
	}

	if m.voice != nil {
		r.drawVoice(img, x, y, inner.Dx(), m.voice)
		y += meta.rowH
	} else if m.fileRow != nil {
		r.drawFileRow(img, x, y, inner.Dx(), m.fileRow)
		y += meta.rowH
	}

	textCol := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	for _, ln := range it.lines {
		baseline := y + meta.ascent
		r.drawTextLine(img, ln, x, baseline, textCol)
		y += meta.lineH
	}
}

// drawVoice paints a voice message row: play icon, waveform, duration.
func (r *renderer) drawVoice(img *image.NRGBA, x, y, w int, v *voiceInfo) {
	h := r.px(baseFontSize)
	mid := y + h/2
	// play triangle
	tri := r.px(baseFontSize) / 2
	for i := 0; i < tri; i++ {
		for j := 0; j < tri-i; j++ {
			blendPixel(img, x+i, mid-tri/2+j, color.RGBA{0x4D, 0xB6, 0xAC, 0xFF}, 255)
		}
	}
	// waveform bars
	bars := 24
	bx := x + tri + r.px(10)
	avail := w - tri - r.px(10) - r.px(baseFontSize)*2
	if avail < r.px(40) {
		avail = r.px(40)
	}
	step := avail / bars
	for i := 0; i < bars; i++ {
		var amp int
		if len(v.waveform) > 0 {
			amp = int(v.waveform[i*len(v.waveform)/bars]) * h * 2 / 5 / 31
		} else {
			amp = h / 3
		}
		if amp < 2 {
			amp = 2
		}
		barX := bx + i*step
		for dy := -amp; dy <= amp; dy++ {
			blendPixel(img, barX, mid+dy, color.RGBA{0x53, 0x6A, 0x87, 0xFF}, 255)
		}
	}
	// duration
	face := r.fonts.sansFace(r.px(baseFontSize) - r.px(4))
	if face != nil {
		d := &font.Drawer{Dst: img, Src: image.NewUniform(color.RGBA{0xAA, 0xAA, 0xAA, 0xFF}), Face: face}
		txt := formatDuration(v.duration)
		dx := x + w - r.px(baseFontSize)
		for _, rn := range txt {
			dx -= advanceOf(face, rn)
		}
		for _, rn := range txt {
			d.Dot = fixed.P(dx, y+h)
			d.DrawString(string(rn))
			dx += advanceOf(face, rn)
		}
	}
}

// drawFileRow paints a generic file row: icon, name, size.
func (r *renderer) drawFileRow(img *image.NRGBA, x, y, w int, f *fileRow) {
	h := r.px(baseFontSize)
	size := r.px(baseFontSize)
	// document icon (rounded sheet)
	for yy := 0; yy < size; yy++ {
		for xx := 0; xx < size; xx++ {
			c := color.RGBA{0x8E, 0x9A, 0xB0, 0xFF}
			if xx > size/4 && xx < 3*size/4 && yy > size/3 && yy < size/2 {
				c = color.RGBA{0x44, 0x4C, 0x5C, 0xFF}
			}
			blendPixel(img, x+xx, y+yy, c, 255)
		}
	}
	face := r.fonts.sansFace(r.px(baseFontSize))
	if face == nil {
		return
	}
	txt := truncVisually(f.name, 30)
	if sz := humanSize(f.size); sz != "" {
		txt += " · " + sz
	}
	d := &font.Drawer{Dst: img, Src: image.NewUniform(color.RGBA{0xDD, 0xDD, 0xDD, 0xFF}), Face: face}
	dx := x + size + r.px(10)
	for _, rn := range txt {
		if dx > x+w {
			break
		}
		d.Dot = fixed.P(dx, y+h)
		d.DrawString(string(rn))
		dx += advanceOf(face, rn)
	}
}

func formatDuration(sec int) string {
	if sec < 0 {
		sec = 0
	}
	m := sec / 60
	s := sec % 60
	return fmt.Sprintf("%d:%02d", m, s)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n > 0:
		return fmt.Sprintf("%d B", n)
	}
	return ""
}
