package main

import (
	"reflect"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
)

// utf16Len returns the length of s in UTF-16 code units (Telegram offsets).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if l := utf16.RuneLen(r); l > 0 {
			n += l
		} else {
			n++ // invalid sequences decode to U+FFFD (1 unit)
		}
	}
	return n
}

func isRegionalIndicator(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// extends reports whether r attaches to the previous cluster.
func extends(r rune) bool {
	switch {
	case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Mc, r):
		return true
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF: // variation selectors
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // skin tones
		return true
	case r >= 0xE0020 && r <= 0xE007F: // tag characters (flag subdivisions)
		return true
	case r == 0x200D || r == 0x20E3: // ZWJ, keycap
		return true
	}
	return false
}

// clusters splits a line into approximate grapheme clusters so emoji
// sequences (ZWJ families, flags, skin tones, keycaps) and combining marks
// survive reversal intact.
func clusters(s string) []string {
	var out []string
	var cur []rune
	prevZWJ := false
	riCount := 0
	for _, r := range s {
		join := len(cur) > 0 && (extends(r) || prevZWJ ||
			(isRegionalIndicator(r) && riCount%2 == 1))
		if !join && len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
			riCount = 0
		}
		cur = append(cur, r)
		if isRegionalIndicator(r) {
			riCount++
		}
		prevZWJ = r == 0x200D
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

// reverseLine reverses one line by cluster.
func reverseLine(line string) string {
	cs := clusters(line)
	var b strings.Builder
	b.Grow(len(line))
	for i := len(cs) - 1; i >= 0; i-- {
		b.WriteString(cs[i])
	}
	return b.String()
}

// reverseText reverses the characters of every line while keeping line
// order (reference behaviour).
func reverseText(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = reverseLine(l)
	}
	return strings.Join(lines, "\n")
}

// entityLike is the subset of tg.MessageEntityClass we need; relocate uses
// reflection so every tg.MessageEntity* type is supported.
type entityLike interface {
	GetOffset() int
	GetLength() int
}

// relocate returns a copy of e (a pointer to a tg entity struct) with new
// offset/length, or nil if it cannot be copied.
func relocate[T any](e T, offset, length int) (T, bool) {
	var zero T
	v := reflect.ValueOf(e)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return zero, false
	}
	elem := v.Elem()
	cp := reflect.New(elem.Type())
	cp.Elem().Set(elem)
	of := cp.Elem().FieldByName("Offset")
	lf := cp.Elem().FieldByName("Length")
	if !of.IsValid() || !lf.IsValid() || !of.CanSet() || !lf.CanSet() {
		return zero, false
	}
	of.SetInt(int64(offset))
	lf.SetInt(int64(length))
	out, ok := cp.Interface().(T)
	return out, ok
}

// reverseEntities maps entities of text onto reverseText(text). Because
// each line is reversed independently, an entity spanning lines is split
// into one entity per line segment (the reference used a whole-text
// mirror, which misplaces entities on multi-line text).
func reverseEntities[T entityLike](text string, ents []T) []T {
	if len(ents) == 0 {
		return nil
	}
	type span struct{ start, end int }
	var lines []span
	pos := 0
	for _, l := range strings.Split(text, "\n") {
		n := utf16Len(l)
		lines = append(lines, span{pos, pos + n})
		pos += n + 1
	}
	total := pos - 1
	var out []T
	for _, e := range ents {
		a, b := e.GetOffset(), e.GetOffset()+e.GetLength()
		if a < 0 {
			a = 0
		}
		if b > total {
			b = total
		}
		if a >= b {
			continue
		}
		for _, ln := range lines {
			s, t := max(a, ln.start), min(b, ln.end)
			if s >= t {
				continue
			}
			ns := ln.start + (ln.end - t)
			if cp, ok := relocate(e, ns, t-s); ok {
				out = append(out, cp)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].GetOffset() < out[j].GetOffset() })
	return out
}

// shiftEntities moves entities from a full text into a substring that
// starts at UTF-16 offset base and has UTF-16 length n, clipping or
// dropping entities that fall outside.
func shiftEntities[T entityLike](ents []T, base, n int) []T {
	var out []T
	for _, e := range ents {
		a, b := e.GetOffset()-base, e.GetOffset()+e.GetLength()-base
		a, b = max(a, 0), min(b, n)
		if a >= b {
			continue
		}
		if cp, ok := relocate(e, a, b-a); ok {
			out = append(out, cp)
		}
	}
	return out
}

// mediaOptions parses leading h / v / c tokens (reference semantics):
// h = horizontal flip, v = vertical flip, c = invert colours. With no flip
// given the default is horizontal, unless the only tokens are "c".
type mediaOptions struct {
	flip      string // "h", "v" or ""
	invert    bool
	remaining []string
}

func extractMediaOptions(args []string) mediaOptions {
	var o mediaOptions
	flipSet := false
	i := 0
	for ; i < len(args); i++ {
		switch strings.ToLower(args[i]) {
		case "h":
			o.flip, flipSet = "h", true
			continue
		case "v":
			o.flip, flipSet = "v", true
			continue
		case "c":
			o.invert = true
			continue
		}
		break
	}
	if !flipSet && !(o.invert && i == len(args) && len(args) > 0) {
		o.flip = "h"
	}
	o.remaining = args[i:]
	return o
}

// rawRemainder locates the remaining argument tokens inside the original
// message text and returns that verbatim tail (keeps newlines and runs of
// spaces) with its UTF-16 start offset. ok=false when the tokens cannot be
// matched (e.g. they came from an alias expansion).
func rawRemainder(full string, remaining []string) (string, int, bool) {
	if len(remaining) == 0 {
		return "", 0, false
	}
	type tok struct {
		s     string
		start int
	}
	var toks []tok
	inTok := false
	start := 0
	for i, r := range full {
		if unicode.IsSpace(r) {
			if inTok {
				toks = append(toks, tok{full[start:i], start})
				inTok = false
			}
			continue
		}
		if !inTok {
			start, inTok = i, true
		}
	}
	if inTok {
		toks = append(toks, tok{full[start:], start})
	}
	if len(toks) < len(remaining) {
		return "", 0, false
	}
	tail := toks[len(toks)-len(remaining):]
	for i := range tail {
		if tail[i].s != remaining[i] {
			return "", 0, false
		}
	}
	b := tail[0].start
	sub := strings.TrimRightFunc(full[b:], unicode.IsSpace)
	return sub, utf16Len(full[:b]), true
}

// trimWithEntities trims surrounding whitespace and re-bases entities.
func trimWithEntities[T entityLike](text string, ents []T) (string, []T) {
	lead := text[:len(text)-len(strings.TrimLeftFunc(text, unicode.IsSpace))]
	out := strings.TrimSpace(text)
	return out, shiftEntities(ents, utf16Len(lead), utf16Len(out))
}
