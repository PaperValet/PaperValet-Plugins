package main

import (
	"strings"

	"github.com/gotd/td/tg"
)

// progressPrefixes are the running-state texts @ParseHubot posts while it
// parses, downloads and uploads; these messages are placeholders that later
// get edited into final results.
var progressPrefixes = []string{
	"解 析 中",
	"已有相同任务正在解析",
	"下 载 中",
	"上 传 中",
}

// decoRunes reports whether r is a decorative prefix character.
func decoRunes(r rune) bool {
	switch {
	case r == ' ' || r == '\t' || r == '\n' || r == '\r':
		return true
	case r >= 0x2580 && r <= 0x259F: // block elements
		return true
	case r >= 0x25A0 && r <= 0x25FF: // geometric shapes
		return true
	case r >= 0x2000 && r <= 0x200F: // en quad..joining modifiers/zero-width
		return true
	case r == 0xFEFF || r == 0x3000: // BOM / ideographic space
		return true
	}
	return false
}

// stripProgressDeco removes leading decorative characters from s.
func stripProgressDeco(s string) string {
	return strings.TrimLeftFunc(s, decoRunes)
}

// isProgressText reports whether text is a placeholder progress line.
func isProgressText(text string) bool {
	if text == "" {
		return false
	}
	stripped := strings.TrimSpace(stripProgressDeco(text))
	for _, prefix := range progressPrefixes {
		if strings.HasPrefix(stripped, prefix) {
			return true
		}
	}
	return false
}

// hasMedia reports whether the message carries real media: media always wins
// because the bot edits its progress text in place while attaching the file.
func hasMedia(m *tg.Message) bool {
	if m == nil || m.Media == nil {
		return false
	}
	if _, ok := m.Media.(*tg.MessageMediaEmpty); ok {
		return false
	}
	return true
}

// isFinalMessage reports whether m is a deliverable result: media, or any
// non-progress text.
func isFinalMessage(m *tg.Message) bool {
	if hasMedia(m) {
		return true
	}
	text := strings.TrimSpace(m.Message)
	if isProgressText(text) {
		return false
	}
	return text != ""
}

// linkTail trims closing punctuation that often follows pasted links.
const linkTail = ")]}。：！？、，>»"

// extractLinks returns deduplicated absolute links (http(s):// or www.)
// in order of appearance.
func extractLinks(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, raw := range scanLinks(text) {
		cleaned := strings.TrimRight(raw, linkTail)
		cleaned = strings.TrimSpace(cleaned)
		if cleaned == "" {
			continue
		}
		if !strings.HasPrefix(cleaned, "http") {
			cleaned = "https://" + cleaned
		}
		if !seen[cleaned] {
			seen[cleaned] = true
			out = append(out, cleaned)
		}
	}
	return out
}

// scanLinks finds candidate links: (https?://|www.) followed by non-boundary.
func scanLinks(text string) []string {
	var out []string
	s := text
	for {
		i := indexOfLinkStart(s)
		if i < 0 {
			return out
		}
		s = s[i:]
		j := strings.IndexFunc(s, isBoundaryRune)
		if j < 0 {
			out = append(out, s)
			return out
		}
		out = append(out, s[:j])
		s = s[j+1:]
	}
}

// indexOfLinkStart finds the first https://, http:// or www. in s.
func indexOfLinkStart(s string) int {
	best := -1
	for _, p := range []string{"https://", "http://", "www."} {
		if i := strings.Index(s, p); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// linkBoundaries are runes that end a pasted link (besides whitespace):
// CJK punctuation and angle-bracket closers.
const linkBoundaries = "，。：；！？、）」』】》〉〕)>]}>»\u00a0\u3000"

func isSpaceRune(r rune) bool {
	return r == ' ' || r == '	' || r == '\n' || r == '\r' || r == 0x3000 || r == 0xA0
}

// isBoundaryRune reports whether r ends a link candidate.
func isBoundaryRune(r rune) bool {
	if isSpaceRune(r) {
		return true
	}
	return strings.ContainsRune(linkBoundaries, r)
}

// mergeLinks unions two link lists, keeping order and dropping duplicates.
func mergeLinks(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	seen := map[string]bool{}
	for _, list := range [][]string{a, b} {
		for _, l := range list {
			if l != "" && !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	return out
}

// msgAction is what the relay does with one bot message.
type msgAction int

const (
	actionSkip     msgAction = iota // outgoing or before the baseline
	actionProgress                  // placeholder progress message
	actionNoise                     // empty/service noise: advance high-water only
	actionFinal                     // deliverable result
)

// classifyMessage decides how the relay treats one incoming bot message:
// progress placeholders, finals (media or text) and noise are separated so
// stale progress never blocks a completed result, and edits of a message id
// are re-classified on every poll.
func classifyMessage(m *tg.Message, baseline, maxFinalID int) msgAction {
	if m.Out || m.ID <= baseline {
		return actionSkip
	}
	if isProgressText(strings.TrimSpace(m.Message)) && !hasMedia(m) {
		// Stale progress older than a newer final result is noise.
		if maxFinalID > m.ID {
			return actionNoise
		}
		return actionProgress
	}
	if isFinalMessage(m) {
		return actionFinal
	}
	return actionNoise
}
