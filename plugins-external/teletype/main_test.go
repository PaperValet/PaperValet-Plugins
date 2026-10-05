package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func TestSteps(t *testing.T) {
	cases := []struct {
		in  string
		raw []string // expected sequence "text|cursor"
	}{
		{"", []string{"|1"}},
		{"a", []string{"|1", "a|1", "a|0"}},
		{"ab", []string{"|1", "a|1", "a|0", "ab|1", "ab|0"}},
		// runes, not bytes: one frame per code point, like the source's for..of
		{"你", []string{"|1", "你|1", "你|0"}},
		{"👍🏽", []string{"|1", "👍|1", "👍|0", "👍🏽|1", "👍🏽|0"}},
	}
	for _, c := range cases {
		var got []string
		for _, f := range steps(c.in) {
			got = append(got, f.text+"|"+map[bool]string{true: "1", false: "0"}[f.cursor])
		}
		if !reflect.DeepEqual(got, c.raw) {
			t.Errorf("steps(%q) = %v, want %v", c.in, got, c.raw)
		}
	}
}

func TestStepsLastFrameIsFullText(t *testing.T) {
	for _, in := range []string{"hello", "你好世界", "a b", strings.Repeat("长", 50)} {
		fr := steps(in)
		last := fr[len(fr)-1]
		if last.text != in || last.cursor {
			t.Errorf("steps(%q) last frame = %+v, want clean full text", in, last)
		}
	}
}

func TestFrameMD(t *testing.T) {
	cases := []struct {
		f    frame
		want string
	}{
		{frame{cursor: true}, "█"},
		{frame{text: "hi"}, "hi"},
		{frame{text: "hi", cursor: true}, "hi█"},
		{frame{text: "a*b"}, `a\*b`},
		{frame{text: "a*b", cursor: true}, `a\*b█`},
		{frame{text: "你好_世界", cursor: true}, `你好\_世界█`},
		{frame{text: "`code`", cursor: true}, "\\`code\\`█"},
		{frame{text: "[a](b)", cursor: true}, `\[a\](b)█`},
	}
	for _, c := range cases {
		if got := frameMD(c.f); got != c.want {
			t.Errorf("frameMD(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
}

func TestTooLong(t *testing.T) {
	if tooLong(strings.Repeat("x", maxRunes)) {
		t.Error("exactly maxRunes runes must be allowed")
	}
	if !tooLong(strings.Repeat("x", maxRunes+1)) {
		t.Error("maxRunes+1 must be rejected")
	}
	if tooLong(strings.Repeat("你", maxRunes)) {
		t.Error("CJK runes count as one each")
	}
	if !tooLong(strings.Repeat("你", maxRunes+1)) {
		t.Error("CJK over the limit must be rejected")
	}
}

func TestIntervalMs(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"50", 50 * time.Millisecond},
		{"25", 25 * time.Millisecond},
		{"200", 200 * time.Millisecond},
		{"", 50 * time.Millisecond},       // unset → source default
		{"abc", 50 * time.Millisecond},    // garbage → default
		{"0", 50 * time.Millisecond},      // zero → default
		{"-5", 50 * time.Millisecond},     // negative → default
		{"1", minInterval},                // clamped to the floor
		{" 100 ", 100 * time.Millisecond}, // trimmed
		{"999999", 999999 * time.Millisecond},
	}
	for _, c := range cases {
		if got := intervalMs(c.in); got != c.want {
			t.Errorf("intervalMs(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestAutoPick(t *testing.T) {
	prefixes := []string{".", "。", "!"}
	cases := []struct {
		name string
		sig  autoSignal
		want bool
	}{
		{"plain own message", autoSignal{out: true, text: "hello"}, true},
		{"cjk two runes", autoSignal{out: true, text: "你好"}, true},
		{"single char skipped", autoSignal{out: true, text: "a"}, false},
		{"single cjk skipped", autoSignal{out: true, text: "你"}, false},
		{"space-padded single char", autoSignal{out: true, text: "  a  "}, false},
		{"edited skipped", autoSignal{edited: true, out: true, text: "hello"}, false},
		{"incoming skipped", autoSignal{out: false, text: "hello"}, false},
		{"media skipped", autoSignal{out: true, media: true, text: "hello"}, false},
		{"command skipped", autoSignal{out: true, text: ".status"}, false},
		{"cjk dot command", autoSignal{out: true, text: "。status"}, false},
		{"bang command", autoSignal{out: true, text: "!ping"}, false},
		{"empty skipped", autoSignal{out: true, text: ""}, false},
		{"spaces skipped", autoSignal{out: true, text: "   "}, false},
	}
	for _, c := range cases {
		if got := autoPick(c.sig, prefixes); got != c.want {
			t.Errorf("%s: autoPick(%+v) = %v, want %v", c.name, c.sig, got, c.want)
		}
	}
}

func TestMetadata(t *testing.T) {
	if Metadata.Name != "teletype" || New().Name() != "teletype" {
		t.Fatalf("name mismatch: %q vs %q", Metadata.Name, New().Name())
	}
}

func TestHelpBilingual(t *testing.T) {
	p := New()
	zh := p.help(&plugin.CommandContext{Lang: "zh-CN"})
	en := p.help(&plugin.CommandContext{Lang: "en-US"})
	if !strings.Contains(zh, "打字机") || !strings.Contains(zh, "自动模式") {
		t.Errorf("zh help missing: %q", zh)
	}
	if !strings.Contains(en, "Typewriter") || !strings.Contains(en, "Auto mode") {
		t.Errorf("en help missing: %q", en)
	}
}
