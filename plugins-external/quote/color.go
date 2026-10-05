package main

// color.go — user colors and background parsing.

import (
	"crypto/rand"
	"fmt"
	"image/color"
	"strings"
)

const defaultBackground = "#231d2b/#372e44"

var userPalette = []color.RGBA{
	{0xE1, 0x70, 0x76, 0xFF}, // red
	{0x21, 0x96, 0xF3, 0xFF}, // blue
	{0x8B, 0xC3, 0x4A, 0xFF}, // green
	{0xFF, 0xB7, 0x4D, 0xFF}, // orange
	{0xBA, 0x68, 0xC8, 0xFF}, // purple
	{0xFF, 0xD5, 0x4E, 0xFF}, // yellow
	{0x4D, 0xB6, 0xAC, 0xFF}, // teal
	{0xF0, 0x62, 0x92, 0xFF}, // pink
	{0x7E, 0x57, 0xC2, 0xFF}, // deep purple
	{0x26, 0xC6, 0xDA, 0xFF}, // cyan
}

// paletteColor picks a stable palette color for a chat id.
func paletteColor(id int64) color.RGBA {
	if id < 0 {
		id = -id
	}
	return userPalette[id%int64(len(userPalette))]
}

// randomHex returns a random #rrggbb color.
func randomHex() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "#231d2b"
	}
	return fmt.Sprintf("#%02x%02x%02x", b[0], b[1], b[2])
}

// parseHexColor parses #rgb or #rrggbb.
func parseHexColor(s string) (color.RGBA, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	var r, g, bl, a uint8 = 0, 0, 0, 0xFF
	switch len(s) {
	case 3:
		if _, err := fmt.Sscanf(s, "%1x%1x%1x", &r, &g, &bl); err != nil {
			return color.RGBA{}, false
		}
		r, g, bl = r*17, g*17, bl*17
	case 6:
		var ri, gi, bi int
		if _, err := fmt.Sscanf(s, "%2x%2x%2x", &ri, &gi, &bi); err != nil {
			return color.RGBA{}, false
		}
		r, g, bl = uint8(ri), uint8(gi), uint8(bi)
	default:
		return color.RGBA{}, false
	}
	return color.RGBA{R: r, G: g, B: bl, A: a}, true
}

// isColorToken mirrors the TeleBox arg recognition: "random", #hex forms and
// bare hex forms (with optional #hex/#hex gradient).
func isColorToken(arg string) bool {
	if arg == "" {
		return false
	}
	if strings.EqualFold(arg, "random") {
		return true
	}
	parts := strings.SplitN(strings.TrimPrefix(arg, "//"), "/", 2)
	for _, p := range parts {
		if !isHexLen3or6(strings.TrimPrefix(p, "#")) {
			return false
		}
	}
	return true
}

func isHexLen3or6(s string) bool {
	if len(s) != 3 && len(s) != 6 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// normalizeColorToken turns a recognized color token into a canonical
// "#hex" or "#hex/#hex" value, resolving "random".
func normalizeColorToken(arg string) string {
	if strings.EqualFold(arg, "random") {
		return randomHex()
	}
	parts := strings.SplitN(strings.TrimPrefix(arg, "//"), "/", 2)
	for i, p := range parts {
		parts[i] = "#" + strings.TrimPrefix(p, "#")
	}
	return strings.Join(parts, "/")
}

// defaultBG1/defaultBG2 are the parsed halves of defaultBackground.
var defaultBG1, defaultBG2 = mustHex("#231d2b"), mustHex("#372e44")

func mustHex(s string) color.RGBA {
	c, ok := parseHexColor(s)
	if !ok {
		return color.RGBA{}
	}
	return c
}

// backgroundColor resolves the background spec to one or two colors,
// falling back to the default background on parse failures.
func backgroundColor(spec string) (color.RGBA, color.RGBA, bool) {
	parts := strings.SplitN(spec, "/", 2)
	c1, ok := parseHexColor(parts[0])
	if !ok {
		c1, c2 := defaultBG1, defaultBG2
		return c1, c2, true
	}
	if len(parts) < 2 {
		return c1, c1, false
	}
	c2, ok := parseHexColor(parts[1])
	if !ok {
		return c1, c1, false
	}
	return c1, c2, true
}
