package main

// pangu.go — the spacing algorithm, ported one-to-one from TeleBox
// pangu.ts (PanguSpacer). The regexes and their order are kept identical so
// the output matches the source byte for byte, including its quirks:
//
//   - the JS "i" flag on [a-z0-9…] classes also matches A-Z → Go classes
//     spell out a-zA-Z0-9…
//   - JS `\s` is [ \t\n\r\f\v]
//   - JS "$1$3$5" on missing groups renders them as empty strings, which
//     Go does as well
//   - "$1 $2" replacements never overlap (both engines resume scanning
//     after the end of the previous match), reproducing the source's
//     "中 a文 b" intermediate results.

import (
	"regexp"
)

const (
	cjkRanges = `\x{2E80}-\x{2EFF}\x{2F00}-\x{2FDF}\x{3040}-\x{309F}\x{30A0}-\x{30FA}\x{30FC}-\x{30FF}\x{3100}-\x{312F}\x{3200}-\x{32FF}\x{3400}-\x{4DBF}\x{4E00}-\x{9FFF}\x{F900}-\x{FAFF}`
	// JS: [a-z0-9`~\!$\^&\*\-\=\+\\|\;\,\.\?\/] with the "i" flag → A-Z too.
	// The source escapes -, =, ? and \\ so they are literals, not a range.
	ansChars = "a-zA-Z0-9`~!$^&*\\-=+\\\\|;,.?/"
)

var (
	reAnyCJK = regexp.MustCompile(`[` + cjkRanges + `]`)

	reCJKQuote = regexp.MustCompile(`([` + cjkRanges + `])(["'])`)
	reQuoteCJK = regexp.MustCompile(`(["'])([` + cjkRanges + `])`)
	// JS: /(["'])\s*(.+?)\s*(["'])/g — JS dot excludes line terminators.
	reFixQuoteAnyQuote = regexp.MustCompile(`(["'])[ 	\n\r\f\v]*([^\n\r\x{2028}\x{2029}]+?)[ 	\n\r\f\v]*(["'])`)

	// JS: ([CJK])(#(\S+)) and ((\S+)#)([CJK])
	reCJKHash = regexp.MustCompile(`([` + cjkRanges + `])(#(\S+))`)
	reHashCJK = regexp.MustCompile(`((\S+)#)([` + cjkRanges + `])`)

	reCJKANS = regexp.MustCompile(`([` + cjkRanges + `])([` + ansChars + `])`)
	reANSCJK = regexp.MustCompile(`([` + ansChars + `])([` + cjkRanges + `])`)

	reCJKBracket = regexp.MustCompile(`([` + cjkRanges + `])([\(\[\{<>\x{201C}])`)
	reBracketCJK = regexp.MustCompile(`([\)\]\}>\x{201D}])([` + cjkRanges + `])`)
	// JS: /([\(\[{<\u201c]+)(\s*)(.+?)(\s*)([\)\]}>\u201d]+)/g — JS dot excludes
	// line terminators.
	reFixBracketAnyBracket = regexp.MustCompile(`([\(\[{<\x{201C}]+)[ 	\n\r\f\v]*([^\n\r\x{2028}\x{2029}]+?)[ 	\n\r\f\v]*([\)\]}>\x{201D}]+)`)

	reCJKANSCJK = regexp.MustCompile(`([` + cjkRanges + `])([` + ansChars + `]+)([` + cjkRanges + `])`)
	reANSCJKANS = regexp.MustCompile(`([` + ansChars + `]+)([` + cjkRanges + `])([` + ansChars + `]+)`)

	reURL = regexp.MustCompile(`https?://(www\.)?[-a-zA-Z0-9@:%._\+~#=]{1,256}\.[a-zA-Z0-9()]{1,6}\b([-a-zA-Z0-9()@:%_\+~#?&//=]*)`)

	rePlaceholder = regexp.MustCompile("\uFFFF(\\d+)\uFFFF")
)

// spacing inserts pangu spaces between CJK and Latin/digit characters,
// keeping URLs intact. It is the direct port of PanguSpacer.spacing.
func spacing(text string) string {
	if text == "" || len([]rune(text)) <= 1 {
		return text
	}
	if !reAnyCJK.MatchString(text) {
		return text
	}

	// Protect URLs: replace with \uFFFF<index>\uFFFF placeholders, restore later.
	urls := []string{}
	t := reURL.ReplaceAllStringFunc(text, func(m string) string {
		urls = append(urls, m)
		return "\uFFFF" + itoa(len(urls)-1) + "\uFFFF"
	})

	t = reCJKQuote.ReplaceAllString(t, "$1 $2")
	t = reQuoteCJK.ReplaceAllString(t, "$1 $2")
	t = reFixQuoteAnyQuote.ReplaceAllString(t, "$1$2$3")

	t = reCJKHash.ReplaceAllString(t, "$1 $2")
	t = reHashCJK.ReplaceAllString(t, "$1 $3")

	t = reCJKANS.ReplaceAllString(t, "$1 $2")
	t = reANSCJK.ReplaceAllString(t, "$1 $2")

	t = reCJKBracket.ReplaceAllString(t, "$1 $2")
	t = reBracketCJK.ReplaceAllString(t, "$1 $2")
	t = reFixBracketAnyBracket.ReplaceAllString(t, "$1$2$3")

	t = reCJKANSCJK.ReplaceAllString(t, "$1 $2 $3")
	t = reANSCJKANS.ReplaceAllString(t, "$1 $2 $3")

	return rePlaceholder.ReplaceAllStringFunc(t, func(m string) string {
		idx, err := atoi(rePlaceholder.FindStringSubmatch(m)[1])
		if err != nil || idx >= len(urls) {
			return m
		}
		return urls[idx]
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func atoi(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errNaN
		}
		n = n*10 + int(s[i]-'0')
		if n > 1<<30 {
			return 0, errNaN
		}
	}
	return n, nil
}

type parseErr struct{}

func (parseErr) Error() string { return "pangu: invalid placeholder index" }

var errNaN = parseErr{}
