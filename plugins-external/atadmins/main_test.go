package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestChunkMentionsCount(t *testing.T) {
	var admins []admin
	for i := 0; i < 60; i++ {
		admins = append(admins, admin{ID: int64(i + 1), Name: fmt.Sprintf("u%d", i)})
	}
	chunks := chunkMentions(admins, "hi: ", 3500, 25)
	if len(chunks) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(chunks))
	}
	// Only the first chunk carries the full header; continuations start
	// with the short marker so the message is not repeated verbatim.
	if !strings.HasPrefix(chunks[0], "hi: ") {
		t.Fatalf("first chunk missing header: %q", chunks[0][:20])
	}
	for _, c := range chunks[1:] {
		if strings.HasPrefix(c, "hi: ") {
			t.Fatalf("continuation repeats the header: %q", c[:20])
		}
		if !strings.HasPrefix(c, "…") {
			t.Fatalf("continuation missing marker: %q", c[:10])
		}
	}
	if n := strings.Count(chunks[2], "tg://user?id="); n != 10 {
		t.Fatalf("last chunk mentions = %d", n)
	}
}

func TestChunkMentionsLength(t *testing.T) {
	long := strings.Repeat("x", 40)
	var admins []admin
	for i := 0; i < 10; i++ {
		admins = append(admins, admin{ID: int64(i + 1), Name: long})
	}
	// Budget is measured on the rendered mention (link markup included),
	// which is longer than the bare name — so more chunks than the old
	// rune-count math suggested.
	chunks := chunkMentions(admins, "h", 100, 25)
	if len(chunks) < 5 {
		t.Fatalf("want at least 5 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 100+50 { // header/marker + one mention fits ~150
			t.Errorf("chunk %d suspiciously long: %d bytes", i, len(c))
		}
	}
}

func TestChunkMentionsEscape(t *testing.T) {
	chunks := chunkMentions([]admin{{ID: 1, Name: "a_b"}}, "*x*", 3500, 25)
	if len(chunks) != 1 || !strings.HasPrefix(chunks[0], `\*x\*`) || !strings.Contains(chunks[0], `a\_b`) {
		t.Fatalf("bad escaping: %q", chunks)
	}
}

func TestChunkMentionsEmpty(t *testing.T) {
	if c := chunkMentions(nil, "h", 10, 2); len(c) != 0 {
		t.Fatalf("expected none, got %v", c)
	}
}

func TestValidMessage(t *testing.T) {
	if got, err := validMessage("  请 查看 置顶 "); err != nil || got != "请 查看 置顶" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := validMessage(""); err != nil || got != "" {
		t.Fatal("empty must clear")
	}
	if _, err := validMessage(strings.Repeat("字", 201)); err == nil {
		t.Fatal("too long accepted")
	}
}

// Length is measured on the rendered Mention markdown, so a name full of
// markdown metacharacters (whose escaping inflates the rendered text far
// beyond its rune count) must still produce chunks within the cap.
func TestChunkMentionsRenderedLengthBudget(t *testing.T) {
	name := strings.Repeat("*_`", 20) // 60 runes, but ~180 rendered with escapes
	var admins []admin
	for i := 0; i < 6; i++ {
		admins = append(admins, admin{ID: int64(i + 1), Name: name})
	}
	const cap = 500
	chunks := chunkMentions(admins, "call:", cap, 25)
	if len(chunks) < 2 {
		t.Fatalf("escaped-heavy names must split into several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		// Every chunk is itself a rendered markdown string; its length is
		// what the API ultimately carries.
		if len(c) > cap+int(float64(cap)*0.1) {
			t.Errorf("chunk %d length = %d, want <= ~%d", i, len(c), cap)
		}
	}
	// Continuation chunks use the short marker instead of the full header.
	for _, c := range chunks[1:] {
		if strings.HasPrefix(c, "call:") {
			t.Errorf("continuation chunk repeats the full header: %q", c[:20])
		}
		if !strings.HasPrefix(c, "…") {
			t.Errorf("continuation chunk missing short marker: %q", c[:10])
		}
	}
	// All six mentions survived the chunking.
	total := 0
	for _, c := range chunks {
		total += strings.Count(c, "tg://user?id=")
	}
	if total != 6 {
		t.Fatalf("mentions after chunking = %d, want 6", total)
	}
}
