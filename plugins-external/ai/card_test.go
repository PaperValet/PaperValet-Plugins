package main

import (
	"strings"
	"testing"
)

func tl(zh, en string) string { return zh }

func TestChunkText(t *testing.T) {
	// Short text stays whole.
	if got := chunkText("hello", 100); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("got %v", got)
	}
	if got := chunkText("", 100); got != nil {
		t.Fatalf("empty → nil, got %v", got)
	}

	// Lines are packed to the limit.
	lines := []string{}
	for i := 0; i < 30; i++ {
		lines = append(lines, strings.Repeat("x", 40))
	}
	parts := chunkText(strings.Join(lines, "\n"), 100)
	for i, p := range parts {
		if n := len([]rune(p)); n > 100 {
			t.Fatalf("part %d too long: %d", i, n)
		}
	}
	joined := strings.Join(parts, "\n")
	if joined != strings.Join(lines, "\n") {
		t.Fatal("round-trip mismatch")
	}

	// A single oversized line is split by runes.
	one := strings.Repeat("中", 250)
	parts = chunkText(one, 100)
	if len(parts) != 3 {
		t.Fatalf("got %d parts", len(parts))
	}
	if parts[0] != strings.Repeat("中", 100) {
		t.Fatal("rune split wrong")
	}

	// max<=0 returns the whole text.
	if got := chunkText("abc", 0); len(got) != 1 {
		t.Fatalf("max=0 → %v", got)
	}
}

func TestShortQuestion(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"what is 2+2", "what is 2+2"},
		{"上下文:\nsome text\n\n问题:\nreal question", "real question"},
		{"plain\nmulti\nline", "plain…"},
	}
	for _, c := range cases {
		if got := shortQuestion(c.in); got != c.want {
			t.Errorf("shortQuestion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := shortQuestion(""); got != "" {
		t.Errorf("empty → %q", got)
	}
	long := strings.Repeat("q", 300)
	got := shortQuestion(long)
	if n := len([]rune(got)); n != 121 || !strings.HasSuffix(got, "…") {
		t.Errorf("long cap = %d %q", n, got[:20])
	}
}

func TestAnswerCard(t *testing.T) {
	card := answerCard(tl, "gpt-x", "hi", "hello **bold**")
	if !strings.Contains(card, "🤖 **AI**") || !strings.Contains(card, "`gpt-x`") {
		t.Fatalf("card = %q", card)
	}
	if !strings.Contains(card, "> hi") {
		t.Fatalf("question quote missing: %q", card)
	}
	// Markdown from the model passes through untouched.
	if !strings.Contains(card, "hello **bold**") {
		t.Fatal("model markdown should pass through")
	}
}

func TestContCard(t *testing.T) {
	c := contCard(tl, 2, 3, "body")
	if !strings.HasPrefix(c, "📋 **续 2/3:**") {
		t.Fatalf("card = %q", c)
	}
}

func TestErrCard(t *testing.T) {
	if c := errCard(tl, &apiError{status: 429, msg: "slow"}); !strings.Contains(c, "429") && !strings.Contains(c, "频繁") {
		t.Fatalf("429 card = %q", c)
	}
	if c := errCard(tl, &apiError{status: 401, msg: "bad key"}); !strings.Contains(c, "密钥") {
		t.Fatalf("401 card = %q", c)
	}
	if c := errCard(tl, &apiError{msg: "boom [x]"}); !strings.Contains(c, `boom \[x\]`) {
		t.Fatalf("escaped card = %q", c)
	}
}
