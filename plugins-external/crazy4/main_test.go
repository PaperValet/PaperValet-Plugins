package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTexts(t *testing.T) {
	var texts []string
	if err := json.Unmarshal(textsJSON, &texts); err != nil {
		t.Fatal(err)
	}
	if len(texts) < 100 {
		t.Fatalf("only %d texts", len(texts))
	}
	seen := map[string]bool{}
	for _, s := range texts {
		if strings.TrimSpace(s) == "" || len([]rune(s)) > 4000 || seen[s] {
			t.Fatalf("bad text %q", s)
		}
		seen[s] = true
	}
}

func TestDeckNoRepeat(t *testing.T) {
	d := newDeck([]string{"a", "b", "c"})
	prev := ""
	for round := 0; round < 50; round++ {
		got := map[string]bool{}
		for i := 0; i < 3; i++ {
			s := d.next()
			if s == prev {
				t.Fatalf("repeat %q", s)
			}
			prev = s
			got[s] = true
		}
		if len(got) != 3 {
			t.Fatalf("round %d not a full cycle: %v", round, got)
		}
	}
}
