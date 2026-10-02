package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func en(_, e string) string { return e }

const sample = `{"calendar":{"cYear":2026,"cMonth":10,"cDay":3,"ncWeek":"星期六","gzYear":"丙午","animal":"马","monthCn":"八月","dayCn":"廿三"},
"newsList":[{"title":"A [x]","url":"https://a.example/1"},{"title":"","url":"https://b"},{"title":"C","url":"javascript:1"}],
"historyList":[{"event":"1949 something"}],
"phrase":{"phrase":"轻虑浅谋","pinyin":"qīng lǜ","explain":"考虑不全面"},
"sentence":{"sentence":"s","author":"报摘"},
"poem":{"content":["l1","l2"],"title":"雨中花慢","author":"苏轼"}}`

func TestRender(t *testing.T) {
	var d newsData
	if err := json.Unmarshal([]byte(sample), &d); err != nil {
		t.Fatal(err)
	}
	all := render(en, &d, "国际", sections{true, true, true, true})
	if len(all) != 5 {
		t.Fatalf("want 5 blocks, got %d", len(all))
	}
	out := strings.Join(all, "\n")
	for _, want := range []string{"2026-10-03", "丙午马年 八月廿三", "1. [A", "https://a.example/1", "2. C", "· world", "轻虑浅谋", "《雨中花慢》苏轼"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "javascript") {
		t.Error("non-http URL must not become a link")
	}
	if n := len(render(en, &d, "", sections{})); n != 1 {
		t.Fatalf("sections off: %d blocks", n)
	}
}

func TestParseCategory(t *testing.T) {
	for in, want := range map[string]string{"国际": "国际", "World": "国际", "business": "商业"} {
		if got, ok := parseCategory(in); !ok || got != want {
			t.Errorf("%q → %q %v", in, got, ok)
		}
	}
	if _, ok := parseCategory("sports"); ok {
		t.Error("sports accepted")
	}
}

func TestPack(t *testing.T) {
	parts := pack([]string{"aaaa", "bbbb", "cc\ncc\ncc\ncc"}, 10)
	for _, p := range parts {
		if len([]rune(p)) > 10 {
			t.Fatalf("part too long %q", p)
		}
	}
	joined := strings.ReplaceAll(strings.Join(parts, "\n"), "\n\n", "\n")
	if joined != "aaaa\nbbbb\ncc\ncc\ncc\ncc" {
		t.Fatalf("content changed: %q", parts)
	}
}

func TestCategoryLabel(t *testing.T) {
	en := func(_, e string) string { return e }
	if got := categoryLabel(en, "国际"); got != "world" {
		t.Fatalf("got %q", got)
	}
}
