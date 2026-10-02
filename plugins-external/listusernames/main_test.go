package main

import (
	"strings"
	"testing"

	"github.com/gotd/td/tg"
)

func en(_, e string) string { return e }

func TestRender(t *testing.T) {
	out := strings.Join(render(en, []entry{{Title: "A", Username: "a", ID: -1001, Broadcast: true}, {Title: "B", ID: -1002}}), "\n")
	for _, want := range []string{"· 2", "📢 Channel", "👥 Group", "@a", "1 channels · 1 groups"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
}

func TestChunk(t *testing.T) {
	lines := []string{strings.Repeat("a", 6), strings.Repeat("b", 6), "c"}
	got := chunk(lines, 10)
	if len(got) != 3 && len(got) != 2 {
		t.Fatalf("got %d parts: %q", len(got), got)
	}
	for _, s := range got {
		if len([]rune(s)) > 10 {
			t.Fatalf("part too long: %q", s)
		}
	}
	if strings.Join(got, "\n") != strings.Join(lines, "\n") {
		t.Fatalf("content changed: %q", got)
	}
}

func TestChannelUsername(t *testing.T) {
	ch := &tg.Channel{Usernames: []tg.Username{{Username: "x"}, {Username: "y", Active: true}}}
	if got := channelUsername(ch); got != "y" {
		t.Fatalf("got %q", got)
	}
}
