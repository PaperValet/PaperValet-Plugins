package main

// args.go — bg argument token parsing (kept from the TeleBox port). The
// resolved token is passed straight to the official quote-api renderer,
// whose parseBackgroundColor understands "#hex", "#hex/#hex", "//#hex"
// (luminance pair) and CSS color names (via node-canvas).

import (
	"fmt"
	"math/rand"
	"strings"
)

const defaultBackground = "#231d2b/#372e44"

// isColorToken mirrors the TeleBox arg recognition: "random", #hex forms and
// bare hex forms (optionally two, a/b). Leading "//" is the quote-api
// luminance-pair form ("//abc" → light/dark pair around the base color).
func isColorToken(arg string) bool {
	if arg == "random" {
		return true
	}
	if strings.HasPrefix(arg, "//") {
		return isHexColor(arg[2:])
	}
	parts := strings.Split(arg, "/")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !isHexColor(p) {
			return false
		}
	}
	return true
}

// isHexColor accepts #rgb, #rrggbb, rgb, rrggbb (empty string allowed only
// as the gradient-from side "//abc").
func isHexColor(s string) bool {
	if s == "" {
		return false
	}
	s = strings.TrimPrefix(s, "#")
	if len(s) != 3 && len(s) != 6 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// normalizeColorToken turns a recognized color token into a canonical form
// understood by the renderer's parseBackgroundColor.
func normalizeColorToken(arg string) string {
	if strings.EqualFold(arg, "random") {
		return fmt.Sprintf("#%02x%02x%02x", rand.Intn(256), rand.Intn(256), rand.Intn(256))
	}
	if strings.HasPrefix(arg, "//") {
		// keep the quote-api luminance-pair form
		return "//" + strings.TrimPrefix(normalizeColorToken(arg[2:]), "//")
	}
	parts := strings.Split(arg, "/")
	for i, p := range parts {
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "#") {
			parts[i] = "#" + p
		}
	}
	return strings.Join(parts, "/")
}
