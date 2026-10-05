package main

import (
	"errors"
	"net"
	"strings"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const maxMessageRunes = 3800

// answerCard renders the reply in the source's Q:/A: shape, adapted to the
// card style: bold title, quoted question, answer below.
func answerCard(tl func(string, string) string, model, question, answer string) string {
	var b strings.Builder
	b.WriteString("🤖 **" + tl("AI", "AI") + "** · " + plugin.Code(model) + "\n")
	if q := shortQuestion(question); q != "" {
		b.WriteString("> " + plugin.Escape(q) + "\n")
	}
	b.WriteString("\n" + answer)
	return b.String()
}

// shortQuestion renders the question header for the card: for context-wrapped
// questions only the 问题 part is shown, capped at one line.
func shortQuestion(question string) string {
	if i := strings.Index(question, "\n问题:\n"); i >= 0 {
		question = question[i+len("\n问题:\n"):]
	} else if strings.HasPrefix(question, "上下文:") {
		question = strings.TrimPrefix(question, "上下文:")
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return ""
	}
	if lines := strings.SplitN(question, "\n", 2); len(lines) > 1 {
		question = lines[0] + "…"
	}
	r := []rune(question)
	if len(r) > 120 {
		question = string(r[:120]) + "…"
	}
	return question
}

// contCard prefixes continuation chunks like the source's 📋 续 (n/m) header.
func contCard(tl func(string, string) string, n, total int, text string) string {
	return "📋 **" + tl("续 ", "Cont. ") + itoa(n) + "/" + itoa(total) + ":**\n\n" + text
}

// errCard renders a request failure, translating the common cases like the
// source's extractErrorMessage.
func errCard(tl func(string, string) string, err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "❌ " + tl("**错误** 请求超时", "**Error** request timed out")
	}
	var ae *apiError
	if errors.As(err, &ae) {
		switch {
		case ae.status == 429:
			return "❌ " + tl("**错误** 请求过于频繁，请稍后重试", "**Error** rate limited, try again soon")
		case ae.status == 401 || ae.status == 403:
			return "❌ " + tl("**错误** 密钥无效或没有权限（401/403），请检查面板里的 API 密钥", "**Error** invalid or unauthorized key (401/403); check the API key in the panel")
		}
	}
	msg := err.Error()
	if msg == "" {
		msg = "unknown error"
	}
	return "❌ " + tl("**错误** ", "**Error** ") + plugin.Escape(msg)
}

// chunkText splits a rendered message into parts of at most max runes,
// preferring line boundaries. Markdown structure of the card is simple
// enough that hard-wrapping at lines is safe.
func chunkText(s string, max int) []string {
	if max <= 0 {
		return []string{s}
	}
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	if len([]rune(s)) <= max {
		return []string{s}
	}
	var out []string
	lines := strings.Split(s, "\n")
	cur := ""
	flush := func() {
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
	}
	for _, line := range lines {
		// A single oversized line gets split by runes.
		for len([]rune(line)) > max {
			flush()
			r := []rune(line)
			out = append(out, string(r[:max]))
			line = string(r[max:])
		}
		add := len([]rune(line)) + 1
		if cur != "" && len([]rune(cur))+add > max {
			flush()
		}
		if cur == "" {
			cur = line
		} else {
			cur += "\n" + line
		}
	}
	flush()
	return out
}
