package main

import (
	"path/filepath"
	"testing"
)

func TestDetectProvider(t *testing.T) {
	cases := map[string]providerKind{
		"https://api.openai.com/v1":                          providerOpenAI,
		"https://generativelanguage.googleapis.com/v1beta":   providerGemini,
		"https://ark.cn-beijing.volces.com/api/v3":           providerDoubao,
		"https://ark.ap-southeast.volces.com":                providerDoubao,
		"https://gateway.ai.cloudflare.com/v1/acc/gw/openai": providerOpenAI,
		"not a url": providerOpenAI,
	}
	for in, want := range cases {
		if got := detectProvider(in); got != want {
			t.Errorf("detectProvider(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestChatEndpoint(t *testing.T) {
	cases := []struct {
		base string
		kind providerKind
		want string
	}{
		{"https://api.openai.com/v1", providerOpenAI, "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/", providerOpenAI, "https://api.openai.com/v1/chat/completions"},
		{"https://x.example/v1/chat/completions", providerOpenAI, "https://x.example/v1/chat/completions"},
		{"https://ark.cn-beijing.volces.com", providerDoubao, "https://ark.cn-beijing.volces.com/api/v3/chat/completions"},
		{"https://ark.cn-beijing.volces.com/api/v3", providerDoubao, "https://ark.cn-beijing.volces.com/api/v3/chat/completions"},
	}
	for _, c := range cases {
		if got := chatEndpoint(c.base, c.kind); got != c.want {
			t.Errorf("chatEndpoint(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

func TestTrimTurns(t *testing.T) {
	h := []turn{
		{roleUser, "q1"}, {roleAssistant, "a1"},
		{roleUser, "q2"}, {roleAssistant, "a2"},
		{roleUser, "q3"},
	}
	if got := trimTurns(h, 0); got != nil {
		t.Fatalf("turns=0 should return nil, got %v", got)
	}
	if got := trimTurns(h, 10); len(got) != 5 {
		t.Fatalf("turns=10 should keep all 5, got %d", len(got))
	}
	// 1 turn = 2 messages → [a2, q3] would start mid-pair; must extend to q2.
	got := trimTurns(h, 1)
	if len(got) != 3 || got[0].Text != "q2" || got[2].Text != "q3" {
		t.Fatalf("turns=1 = %v, want [q2 a2 q3]", got)
	}
	got = trimTurns(h, 2)
	if len(got) != 5 || got[0].Text != "q1" {
		t.Fatalf("turns=2 = %v, want all five starting at q1", got)
	}
	if got := trimTurns(nil, 3); got != nil {
		t.Fatal("empty history should be nil")
	}
}

func TestHistoryStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newHistoryStore(dir)
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	s.appendUser(1, "hi")
	s.appendAssistant(1, "hello")
	s.appendUser(2, "other")
	s.rollbackUser(2)
	if n := len(s.history(2, 5)); n != 0 {
		t.Fatalf("chat 2 should be empty after rollback, got %d", n)
	}

	again := newHistoryStore(dir)
	if err := again.load(); err != nil {
		t.Fatal(err)
	}
	h := again.history(1, 5)
	if len(h) != 2 || h[0].Role != roleUser || h[1].Text != "hello" {
		t.Fatalf("reloaded history = %v", h)
	}
	if n := again.reset(1); n != 2 {
		t.Fatalf("reset returned %d, want 2", n)
	}
	if len(again.history(1, 5)) != 0 {
		t.Fatal("history should be empty after reset")
	}
	if _, err := filepath.Abs(filepath.Join(dir, historyFile)); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryStoreCap(t *testing.T) {
	s := newHistoryStore(t.TempDir())
	for i := 0; i < storeCap+20; i++ {
		s.appendUser(7, "x")
	}
	if n := len(s.appendUser(7, "y")); n != storeCap {
		t.Fatalf("store should cap at %d, got %d", storeCap, n)
	}
}
