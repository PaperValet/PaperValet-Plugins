package main

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/tg"
)

// standardReactions are the emoticons Telegram accepts as free reactions,
// in the API form (no U+FE0F variation selectors).
var standardReactions = []string{
	"👍", "👎", "❤", "🔥", "🥰", "👏", "😁", "🤔", "🤯", "😱", "🤬", "😢", "🎉", "🤩", "🤮", "💩",
	"🙏", "👌", "🕊", "🤡", "🥱", "🥴", "😍", "🐳", "❤\u200d🔥", "🌚", "🌭", "💯", "🤣", "⚡", "🍌",
	"🏆", "💔", "🤨", "😐", "🍓", "🍾", "💋", "🖕", "😈", "😴", "😭", "🤓", "👻", "👨\u200d💻", "👀",
	"🎃", "🙈", "😇", "😨", "🤝", "✍", "🤗", "🫡", "🎅", "🎄", "☃", "💅", "🤪", "🗿", "🆒", "💘",
	"🙉", "🦄", "😘", "💊", "🙊", "😎", "👾", "🤷\u200d♂", "🤷", "🤷\u200d♀", "😡",
}

// byLength holds standardReactions longest first for greedy matching.
var byLength = func() []string {
	out := append([]string(nil), standardReactions...)
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}()

// cleanEmoji strips variation selectors and skin tones, which Telegram
// reactions do not carry.
func cleanEmoji(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == 0xFE0F || r == 0xFE0E || (r >= 0x1F3FB && r <= 0x1F3FF) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isCustomID(r string) bool {
	if r == "" {
		return false
	}
	_, err := strconv.ParseInt(r, 10, 64)
	return err == nil
}

// utf16Len counts UTF-16 code units (Telegram entity offsets).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// parseReactions extracts reactions from text[start:] in order: standard
// emoticons and, when allowCustom, custom emoji entities (stored as their
// decimal document id). ents use UTF-16 offsets into the whole text.
func parseReactions(text string, start int, ents []tg.MessageEntityClass, allowCustom bool) []string {
	if start < 0 || start > len(text) {
		return nil
	}
	type custom struct {
		end int
		id  int64
	}
	customAt := map[int]custom{}
	for _, e := range ents {
		if ce, ok := e.(*tg.MessageEntityCustomEmoji); ok {
			customAt[ce.Offset] = custom{end: ce.Offset + ce.Length, id: ce.DocumentID}
		}
	}
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	pos := utf16Len(text[:start])
	rest := text[start:]
	for len(rest) > 0 {
		if c, ok := customAt[pos]; ok {
			if allowCustom {
				add(strconv.FormatInt(c.id, 10))
			}
			// skip the placeholder covered by the entity
			for len(rest) > 0 && pos < c.end {
				r, n := utf8.DecodeRuneInString(rest)
				rest = rest[n:]
				pos += utf16Len(string(r))
			}
			continue
		}
		if m := matchStandard(rest); m > 0 {
			add(cleanEmoji(rest[:m]))
			pos += utf16Len(rest[:m])
			rest = rest[m:]
			continue
		}
		r, n := utf8.DecodeRuneInString(rest)
		rest = rest[n:]
		pos += utf16Len(string(r))
	}
	return out
}

// matchStandard returns the byte length of a standard reaction at the start
// of s (ignoring variation selectors and skin tones), or 0.
func matchStandard(s string) int {
	for _, want := range byLength {
		// walk s consuming runes until the cleaned prefix equals want
		var b strings.Builder
		i := 0
		for i < len(s) && b.Len() < len(want) {
			r, n := utf8.DecodeRuneInString(s[i:])
			i += n
			if r == 0xFE0F || r == 0xFE0E || (r >= 0x1F3FB && r <= 0x1F3FF) {
				continue
			}
			b.WriteRune(r)
		}
		if b.String() != want {
			continue
		}
		// swallow trailing selectors / skin tones
		for i < len(s) {
			r, n := utf8.DecodeRuneInString(s[i:])
			if r != 0xFE0F && r != 0xFE0E && !(r >= 0x1F3FB && r <= 0x1F3FF) {
				break
			}
			i += n
		}
		return i
	}
	return 0
}

// toTG converts stored reactions to API reactions.
func toTG(list []string) []tg.ReactionClass {
	out := make([]tg.ReactionClass, 0, len(list))
	for _, r := range list {
		if isCustomID(r) {
			id, _ := strconv.ParseInt(r, 10, 64)
			out = append(out, &tg.ReactionCustomEmoji{DocumentID: id})
		} else if r != "" {
			out = append(out, &tg.ReactionEmoji{Emoticon: r})
		}
	}
	return out
}

// displayReactions renders reactions as Markdown, custom emoji as entities.
func displayReactions(list []string) string {
	parts := make([]string, 0, len(list))
	for _, r := range list {
		if isCustomID(r) {
			parts = append(parts, "![😊](tg://emoji?id="+r+")")
		} else {
			parts = append(parts, r)
		}
	}
	return strings.Join(parts, " ")
}

// countCustom returns the number of custom emoji reactions.
func countCustom(list []string) int {
	n := 0
	for _, r := range list {
		if isCustomID(r) {
			n++
		}
	}
	return n
}

// matchKeyword returns the first keyword (sorted) contained in text.
func matchKeyword(text string, keywords map[string][]string) (string, bool) {
	if text == "" || len(keywords) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(keywords))
	for k := range keywords {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != "" && strings.Contains(text, k) {
			return k, true
		}
	}
	return "", false
}

// argsStart returns the byte offset in full just after skipping n
// whitespace-separated tokens plus the following whitespace, or -1.
func argsStart(full string, n int) int {
	i := 0
	for k := 0; k < n; k++ {
		for i < len(full) && isSpace(full[i]) {
			i++
		}
		if i >= len(full) {
			return -1
		}
		for i < len(full) && !isSpace(full[i]) {
			i++
		}
	}
	for i < len(full) && isSpace(full[i]) {
		i++
	}
	return i
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
