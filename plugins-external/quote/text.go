package main

// text.go — text layout: entity styling, emoji cell building, line breaking
// (CJK anywhere, Latin by word, emoji atomic).

import (
	"strings"
	"unicode"
	"unicode/utf16"

	"golang.org/x/image/font"
)

const wqyFontPath = "/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc"

// style describes entity formatting applied to a span of runes.
type style struct {
	bold, italic, underline, strike, code, spoiler bool
}

// entityRef is a normalized message entity (UTF-16 offsets).
type entityRef struct {
	kind   string
	offset int
	length int
}

// styledRune is one rune with its resolved style.
type styledRune struct {
	r rune
	s style
}

// splitStyles resolves text+entities (UTF-16 offsets, Telegram convention)
// into per-rune styles.
func splitStyles(text string, entities []entityRef) []styledRune {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	u16 := make([]int, len(runes)+1)
	for i, r := range runes {
		u16[i+1] = u16[i] + utf16.RuneLen(r)
	}
	st := make([]style, len(runes))
	for _, e := range entities {
		start, end := len(runes), 0
		for i := range runes {
			if u16[i] >= e.offset && u16[i] < e.offset+e.length {
				if i < start {
					start = i
				}
				if i+1 > end {
					end = i + 1
				}
			}
		}
		for i := start; i < end && i < len(runes); i++ {
			switch e.kind {
			case "bold":
				st[i].bold = true
			case "italic":
				st[i].italic = true
			case "underline":
				st[i].underline = true
			case "strikethrough":
				st[i].strike = true
			case "code", "pre":
				st[i].code = true
			case "spoiler":
				st[i].spoiler = true
			}
		}
	}
	out := make([]styledRune, len(runes))
	for i, r := range runes {
		out[i] = styledRune{r: r, s: st[i]}
	}
	return out
}

// cell is one layout atom: a text rune or an emoji bitmap.
type cell struct {
	r     rune
	s     style
	emoji bool
	glyph uint16 // emoji glyph id when emoji
	w     int    // pixel width (advance or emoji box)
}

// isEmojiRune reports whether r commonly has an emoji presentation.
func isEmojiRune(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FBFF,
		r >= 0x2600 && r <= 0x27BF,
		r >= 0x2B00 && r <= 0x2BFF,
		r >= 0x2190 && r <= 0x21FF && (r == 0x21A9 || r == 0x21AA || (r >= 0x2194 && r <= 0x2199)),
		r == 0x203C, r == 0x2049, r == 0x2122, r == 0x2139,
		r == 0x24C2, r == 0x25AA, r == 0x25AB, r == 0x25B6, r == 0x25C0,
		r == 0x25FB, r == 0x25FC, r == 0x25FD, r == 0x25FE,
		r == 0x2934, r == 0x2935, r == 0x3030, r == 0x303D, r == 0x3297, r == 0x3299,
		r == 0x00A9, r == 0x00AE:
		return true
	}
	return false
}

// isIgnorable reports variation selectors / ZWJ that only matter inside an
// emoji cluster.
func isIgnorable(r rune) bool {
	return r == 0xFE0F || r == 0xFE0E || r == 0x200D || r == 0x20E3
}

// isSkinToneOrRegional reports emoji modifiers that attach to a previous
// base but never stand alone visually.
func isSkinTone(r rune) bool { return r >= 0x1F3FB && r <= 0x1F3FF }

// buildCells converts styled runes into layout cells, folding emoji
// sequences (ligatures, skin tones, flags, keycaps) into single emoji cells.
// face/monoFace provide advances; size is the emoji box size in px.
func buildCells(rs []styledRune, ef *emojiFont, size int, face, monoFace font.Face) []cell {
	var out []cell
	i := 0
	for i < len(rs) {
		r := rs[i].r
		startEmoji := false
		if ef != nil {
			if isEmojiRune(r) && ef.glyph(r) != 0 {
				startEmoji = true
			}
			// keycap: digit/#/* + FE0F + 20E3
			if (r >= '0' && r <= '9') || r == '#' || r == '*' {
				if i+2 < len(rs) && rs[i+1].r == 0xFE0F && rs[i+2].r == 0x20E3 && ef.glyph(r) != 0 {
					startEmoji = true
				}
			}
		}
		if startEmoji {
			// collect the candidate glyph run
			var glyphs []uint16
			var j = i
			for j < len(rs) {
				g := ef.glyph(rs[j].r)
				if g == 0 {
					// unmapped modifiers still belong to the cluster if ignorable
					if isIgnorable(rs[j].r) || isSkinTone(rs[j].r) {
						glyphs = append(glyphs, 0)
						j++
						continue
					}
					break
				}
				glyphs = append(glyphs, g)
				j++
			}
			// try ligatures from the start
			if g, n, ok := ef.ligature(glyphs, 0); ok && n > 0 {
				if ef.emojiImage(g) != nil {
					out = append(out, cell{r: r, s: rs[i].s, emoji: true, glyph: g, w: size})
					i += n
					continue
				}
			}
			// single emoji glyph
			if len(glyphs) > 0 && glyphs[0] != 0 && ef.emojiImage(glyphs[0]) != nil {
				out = append(out, cell{r: r, s: rs[i].s, emoji: true, glyph: glyphs[0], w: size})
				// swallow trailing modifiers/selectors
				i++
				for i < len(rs) && (isIgnorable(rs[i].r) || isSkinTone(rs[i].r)) {
					// keep skin tones that fail to blend as their own emoji
					if isSkinTone(rs[i].r) && ef.glyph(rs[i].r) != 0 && ef.emojiImage(ef.glyph(rs[i].r)) != nil {
						g := ef.glyph(rs[i].r)
						if _, _, ok := ef.ligature([]uint16{glyphs[0], g}, 0); !ok {
							out = append(out, cell{r: rs[i].r, s: rs[i].s, emoji: true, glyph: g, w: size})
						}
					}
					i++
				}
				continue
			}
		}
		// plain text cell (drop stray selectors)
		if isIgnorable(r) && !isSkinTone(r) && r != 0x20E3 {
			i++
			continue
		}
		f := face
		if rs[i].s.code && monoFace != nil {
			f = monoFace
		}
		out = append(out, cell{r: r, s: rs[i].s, w: advanceOf(f, r)})
		i++
	}
	return out
}

// chunk is an unbreakable group of cells.
type chunk struct {
	cells []cell
	w     int
	space bool // breakable space run
	cjk   bool
}

// buildChunks groups cells: Latin words stay together, each CJK rune is its
// own chunk, emoji are atomic, spaces are breakable.
func buildChunks(cells []cell) []chunk {
	var out []chunk
	var cur []cell
	curW := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, chunk{cells: cur, w: curW})
			cur, curW = nil, 0
		}
	}
	for _, c := range cells {
		switch {
		case c.emoji:
			flush()
			out = append(out, chunk{cells: []cell{c}, w: c.w})
		case isCJK(c.r):
			flush()
			out = append(out, chunk{cells: []cell{c}, w: c.w, cjk: true})
		case c.r == ' ' || c.r == '\t' || c.r == 0x3000:
			flush()
			out = append(out, chunk{cells: []cell{c}, w: c.w, space: true})
		default:
			cur = append(cur, c)
			curW += c.w
		}
	}
	flush()
	return out
}

// wrapCells wraps chunks into lines of at most maxW px. Overlong single
// chunks are hard-split by cell.
func wrapCells(chunks []chunk, maxW int) [][]cell {
	if maxW <= 0 {
		return nil
	}
	var lines [][]cell
	var line []cell
	lineW := 0
	appendLine := func() {
		// trim leading spaces
		for len(line) > 0 && line[0].r == ' ' {
			line = line[1:]
		}
		if len(line) > 0 {
			lines = append(lines, line)
		}
		line, lineW = nil, 0
	}
	for _, ch := range chunks {
		if ch.space && len(line) == 0 {
			continue // skip leading spaces
		}
		if lineW+ch.w <= maxW {
			line = append(line, ch.cells...)
			lineW += ch.w
			continue
		}
		// chunk doesn't fit on the current line
		if len(ch.cells) == 1 || ch.w <= maxW {
			if len(line) > 0 {
				appendLine()
			}
			if ch.w <= maxW {
				line = append(line, ch.cells...)
				lineW += ch.w
				continue
			}
			// hard split single overlong chunk
			remaining := ch.cells
			for len(remaining) > 0 {
				w := 0
				k := 0
				for k < len(remaining) {
					if w+remaining[k].w > maxW && k > 0 {
						break
					}
					w += remaining[k].w
					k++
				}
				if k == 0 {
					k = 1
				}
				lines = append(lines, remaining[:k])
				remaining = remaining[k:]
			}
			line, lineW = nil, 0
			continue
		}
		// multi-cell word wider than the line: hard split
		if len(line) > 0 {
			appendLine()
		}
		remaining := ch.cells
		for len(remaining) > 0 {
			w, k := 0, 0
			for k < len(remaining) {
				if w+remaining[k].w > maxW && k > 0 {
					break
				}
				w += remaining[k].w
				k++
			}
			if k == 0 {
				k = 1
			}
			lines = append(lines, remaining[:k])
			remaining = remaining[k:]
		}
		line, lineW = nil, 0
	}
	appendLine()
	return lines
}

// linesWidth returns the pixel width of a line.
func linesWidth(line []cell) int {
	w := 0
	for _, c := range line {
		w += c.w
	}
	return w
}

// isCJK reports whether a rune may be wrapped anywhere (wide scripts).
func isCJK(r rune) bool {
	switch {
	case r >= 0x2E80 && r <= 0x9FFF,
		r >= 0xAC00 && r <= 0xD7AF,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFF01 && r <= 0xFF60,
		r >= 0x3040 && r <= 0x30FF,
		r >= 0x3400 && r <= 0x4DBF:
		return true
	}
	return false
}

// truncVisually truncates s to maxRunes with an ellipsis.
func truncVisually(s string, maxRunes int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxRunes {
		return string(r)
	}
	if maxRunes <= 1 {
		return "…"
	}
	return string(r[:maxRunes-1]) + "…"
}

// firstGraphemeRune returns the first printable rune of a name for the
// fallback avatar.
func firstGraphemeRune(s string) rune {
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsGraphic(r) {
			return r
		}
	}
	return '?'
}

// advanceOf returns the pixel advance of a rune in a face.
func advanceOf(f font.Face, r rune) int {
	if f == nil {
		return 0
	}
	adv, ok := f.GlyphAdvance(r)
	if !ok {
		return 0
	}
	return adv.Ceil()
}
