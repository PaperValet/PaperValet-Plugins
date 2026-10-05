package main

// emoji.go — color emoji rendering from NotoColorEmoji.ttf.
//
// The font stores every emoji as a 136x128 PNG inside the CBDT bitmap table,
// indexed through CBLC (indexFormat 1 subtables, imageFormat 17 bitmaps). We
// parse those tables directly (x/image/sfnt refuses CBDT fonts), plus the
// GSUB ligature table so multi-codepoint sequences (flags, skin tones, ZWJ
// families, keycaps) resolve to their single composed glyph.

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"image"
	"image/png"
	"os"
	"sync"
)

const emojiFontPath = "/usr/share/fonts/truetype/noto/NotoColorEmoji.ttf"

type ligature struct {
	comps []uint16 // glyphs after the first one
	glyph uint16   // composed result glyph
}

// emojiFont is a lazily parsed CBDT color-emoji font.
type emojiFont struct {
	data []byte

	// cmap format 12 groups (sorted)
	cmapStart, cmapEnd, cmapStartGID []uint32

	// CBLC index subtables
	subs []emojiSub
	cbdt int // absolute offset of the CBDT table

	// GSUB ligatures: first glyph → candidates
	ligands map[uint16][]ligature

	mu    sync.Mutex
	lru   *list.List               // glyph ids, front = oldest
	elem  map[uint16]*list.Element // glyph → lru element
	cache map[uint16]image.Image
}

type emojiSub struct {
	first, last uint16
	base        int // absolute offset of the index subtable header
}

var (
	emojiOnce sync.Once
	emojiLib  *emojiFont
	emojiErr  error
)

// loadEmojiFont parses the system Noto Color Emoji font. A missing font or a
// parse failure is not fatal: callers fall back to monochrome glyphs.
func loadEmojiFont() (*emojiFont, error) {
	emojiOnce.Do(func() {
		data, err := os.ReadFile(emojiFontPath)
		if err != nil {
			emojiErr = err
			return
		}
		e := &emojiFont{data: data, lru: list.New(), elem: map[uint16]*list.Element{}, cache: map[uint16]image.Image{}}
		tables, err := e.tables()
		if err != nil {
			emojiErr = err
			return
		}
		if err := e.parseCmap(tables["cmap"]); err != nil {
			emojiErr = err
			return
		}
		if err := e.parseCBLC(tables["CBLC"], tables["CBDT"]); err != nil {
			emojiErr = err
			return
		}
		e.parseGSUB(tables["GSUB"]) // ligatures are optional
		emojiLib = e
	})
	return emojiLib, emojiErr
}

type errBadFont string

func (e errBadFont) Error() string { return "bad emoji font: " + string(e) }

func (e *emojiFont) u16(o int) int    { return int(binary.BigEndian.Uint16(e.data[o:])) }
func (e *emojiFont) u32(o int) uint32 { return binary.BigEndian.Uint32(e.data[o:]) }

// tables returns the SFNT table directory (tag → absolute offset).
func (e *emojiFont) tables() (map[string]int, error) {
	if len(e.data) < 12 {
		return nil, errBadFont("truncated")
	}
	num := e.u16(4)
	out := map[string]int{}
	for i := 0; i < num && 12+16*(i+1) <= len(e.data); i++ {
		o := 12 + 16*i
		out[string(e.data[o:o+4])] = int(e.u32(o + 8))
	}
	return out, nil
}

func (e *emojiFont) parseCmap(off int) error {
	if off <= 0 {
		return errBadFont("no cmap")
	}
	n := e.u16(off + 2)
	var fmt12 int
	for i := 0; i < n; i++ {
		sub := off + int(e.u32(off+8+8*i))
		if sub+2 <= len(e.data) && e.u16(sub) == 12 {
			fmt12 = sub
		}
	}
	if fmt12 == 0 {
		return errBadFont("no cmap format 12")
	}
	groups := int(e.u32(fmt12 + 12))
	e.cmapStart = make([]uint32, groups)
	e.cmapEnd = make([]uint32, groups)
	e.cmapStartGID = make([]uint32, groups)
	for i := 0; i < groups && fmt12+16+12*(i+1) <= len(e.data); i++ {
		g := fmt12 + 16 + 12*i
		e.cmapStart[i] = e.u32(g)
		e.cmapEnd[i] = e.u32(g + 4)
		e.cmapStartGID[i] = e.u32(g + 8)
	}
	return nil
}

// glyph maps one codepoint to its glyph id (0 when unmapped).
func (e *emojiFont) glyph(r rune) uint16 {
	cp := uint32(r)
	lo, hi := 0, len(e.cmapStart)-1
	for lo <= hi {
		m := (lo + hi) / 2
		switch {
		case cp < e.cmapStart[m]:
			hi = m - 1
		case cp > e.cmapEnd[m]:
			lo = m + 1
		default:
			return uint16(e.cmapStartGID[m] + cp - e.cmapStart[m])
		}
	}
	return 0
}

func (e *emojiFont) parseCBLC(cblc, cbdt int) error {
	if cblc <= 0 || cbdt <= 0 {
		return errBadFont("no CBLC/CBDT")
	}
	e.cbdt = cbdt
	// CBLC header: version u32, numSizes u32; then BitmapSizeTables. The
	// first (only) size table carries the subtable array offset @+8 and the
	// subtable count @+16 (relative to CBLC).
	arrayOff := cblc + int(e.u32(cblc+8))
	nSub := e.u32(cblc + 16)
	for i := 0; i < int(nSub); i++ {
		a := arrayOff + 8*i
		if a+8 > len(e.data) {
			break
		}
		first, last := uint16(e.u16(a)), uint16(e.u16(a+2))
		e.subs = append(e.subs, emojiSub{first: first, last: last, base: arrayOff + int(e.u32(a+4))})
	}
	return nil
}

// bitmapPNG returns the embedded PNG bytes plus (height, width) metrics for
// a glyph.
func (e *emojiFont) bitmapPNG(g uint16) ([]byte, int, int, bool) {
	for _, s := range e.subs {
		if g < s.first || g > s.last {
			continue
		}
		hdr := s.base
		if e.u16(hdr) != 1 { // only indexFormat 1 occurs in this font
			continue
		}
		dataOff := int(e.u32(hdr + 4))
		idx := int(g - s.first)
		o1 := e.cbdt + dataOff + int(e.u32(hdr+8+4*idx))
		o2 := e.cbdt + dataOff + int(e.u32(hdr+8+4*(idx+1)))
		if o1 >= o2 || o2 > len(e.data) {
			return nil, 0, 0, false
		}
		rec := e.data[o1:o2]
		if len(rec) < 13 {
			return nil, 0, 0, false
		}
		// Empirical format-17 record layout in this font build:
		// h u8, w u8, bearingX i8, bearingY i8, advance u8, len u32@5,
		// PNG bytes @9 (the "length" high byte is shared with 0x89).
		h, w := int(rec[0]), int(rec[1])
		n := int(binary.BigEndian.Uint32(rec[5:9]))
		if n <= 0 || 9+n > len(rec) {
			return nil, 0, 0, false
		}
		pngData := rec[9 : 9+n]
		if len(pngData) < 8 || pngData[0] != 0x89 || pngData[1] != 'P' {
			return nil, 0, 0, false
		}
		return pngData, h, w, true
	}
	return nil, 0, 0, false
}

// parseGSUB loads the type-4 ligature lookup: first glyph → candidates with
// their composed result glyph.
func (e *emojiFont) parseGSUB(off int) {
	if off <= 0 {
		return
	}
	defer func() { recover() }() // malformed GSUB must never kill the font
	lookupList := off + e.u16(off+8)
	nLookup := e.u16(lookupList)
	for i := 0; i < nLookup; i++ {
		lo := lookupList + e.u16(lookupList+2+2*i)
		if lo+8 > len(e.data) || e.u16(lo) != 4 { // ligature substitution only
			continue
		}
		sub := lo + e.u16(lo+6)
		if e.u16(sub) != 1 {
			continue
		}
		var covGlyphs []uint16
		cov := sub + e.u16(sub+2)
		switch e.u16(cov) {
		case 1:
			for k := 0; k < e.u16(cov+2); k++ {
				covGlyphs = append(covGlyphs, uint16(e.u16(cov+4+2*k)))
			}
		case 2:
			for k := 0; k < e.u16(cov+2); k++ {
				r := cov + 4 + 6*k
				start, end := e.u16(r), e.u16(r+2)
				for g := start; g <= end && g-start < 4096; g++ {
					covGlyphs = append(covGlyphs, uint16(g))
				}
			}
		}
		nSet := e.u16(sub + 4)
		for s := 0; s < nSet && s < len(covGlyphs); s++ {
			first := covGlyphs[s]
			set := sub + e.u16(sub+6+2*s)
			for j := 0; j < e.u16(set); j++ {
				lig := set + e.u16(set+2+2*j)
				if lig+4 > len(e.data) {
					continue
				}
				composed := uint16(e.u16(lig))
				nComp := e.u16(lig + 2) // includes the first (coverage) glyph
				if nComp < 2 || lig+4+2*(nComp-1) > len(e.data) {
					continue
				}
				comps := make([]uint16, nComp-1)
				for c := 0; c < nComp-1; c++ {
					comps[c] = uint16(e.u16(lig + 4 + 2*c))
				}
				if e.ligands == nil {
					e.ligands = map[uint16][]ligature{}
				}
				e.ligands[first] = append(e.ligands[first], ligature{comps: comps, glyph: composed})
			}
		}
	}
}

// ligature tries to extend the glyph run starting at i through GSUB
// ligatures, returning the composed glyph and the number of glyphs consumed
// (longest match wins).
func (e *emojiFont) ligature(glyphs []uint16, i int) (uint16, int, bool) {
	best := -1
	var bestGlyph uint16
	for _, cand := range e.ligands[glyphs[i]] {
		if i+len(cand.comps)+1 > len(glyphs) {
			continue
		}
		ok := true
		for k, c := range cand.comps {
			if glyphs[i+1+k] != c {
				ok = false
				break
			}
		}
		if ok && len(cand.comps) > best {
			best = len(cand.comps)
			bestGlyph = cand.glyph
		}
	}
	if best < 0 {
		return 0, 0, false
	}
	return bestGlyph, best + 1, true
}

// emojiImage decodes (with a small LRU cache) the color bitmap for a glyph.
func (e *emojiFont) emojiImage(g uint16) image.Image {
	e.mu.Lock()
	if img, ok := e.cache[g]; ok {
		if el, ok := e.elem[g]; ok {
			e.lru.MoveToBack(el)
		}
		e.mu.Unlock()
		return img
	}
	e.mu.Unlock()

	pngBytes, _, _, ok := e.bitmapPNG(g)
	if !ok {
		return nil
	}
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil
	}

	e.mu.Lock()
	if len(e.cache) >= 512 {
		if front := e.lru.Front(); front != nil {
			old := front.Value.(uint16)
			delete(e.cache, old)
			delete(e.elem, old)
			e.lru.Remove(front)
		}
	}
	e.cache[g] = img
	e.elem[g] = e.lru.PushBack(g)
	e.mu.Unlock()
	return img
}
