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
	for _, c := range chunks {
		if !strings.HasPrefix(c, "hi: ") {
			t.Fatalf("chunk missing header: %q", c)
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
	chunks := chunkMentions(admins, "h", 100, 25)
	if len(chunks) != 5 {
		t.Fatalf("want 5 chunks, got %d", len(chunks))
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
