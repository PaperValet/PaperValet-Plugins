package main

import (
	"bytes"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- tokenizer

func TestIsUsefulWord(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"这个", false},
		{"哈哈哈", false},
		{"the", false},
		{"t.me", false},
		{"123", false}, // pure digits
		{"go", false},  // 1-2 letters
		{"ooo", false}, // o0 run
		{"-_-", false}, // pure symbols
		{"golang", true},
		{"词云", true},
		{"2023年", true},
	}
	for _, c := range cases {
		if got := isUsefulWord(c.in); got != c.want {
			t.Errorf("isUsefulWord(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCollectWordsEnglishNumbers(t *testing.T) {
	c := newWordCounts()
	collectWords("check https://example.com/x and @user #tag Golang golang 100% 99 3", c)
	m := map[string]int{}
	for _, e := range c.entries {
		m[e.word] = e.count
	}
	// URL/mention cleaned; "golang" twice at weight 2 = 4.
	if m["golang"] != 4 {
		t.Errorf("golang = %d, want 4", m["golang"])
	}
	if _, ok := m["check"]; !ok {
		t.Error("check missing")
	}
	// "100%" is dropped: % is stripped by cleaning, leaving pure digits "100"
	// which isUsefulWord rejects. Same for "99" and single "3".
	for _, gone := range []string{"100", "100%", "99", "3"} {
		if _, ok := m[gone]; ok {
			t.Errorf("%q should be dropped", gone)
		}
	}
	if _, ok := m["example"]; ok {
		t.Error("URL fragment example should be cleaned")
	}
	if _, ok := m["user"]; ok {
		t.Error("mention user should be cleaned")
	}
}

func TestCollectWordsHan(t *testing.T) {
	c := newWordCounts()
	// 2-char run: whole word weight 3.
	collectWords("词云", c)
	if len(c.entries) != 1 || c.entries[0].word != "词云" || c.entries[0].count != 3 {
		t.Fatalf("got %+v", c.entries)
	}
	// Long run (>4): n-grams 2..5 with edge bonus.
	c2 := newWordCounts()
	collectWords("今天天气真不错啊", c2)
	m := map[string]int{}
	for _, e := range c2.entries {
		m[e.word] = e.count
	}
	// "今天" is a stop word → dropped despite being an edge bigram.
	if _, ok := m["今天"]; ok {
		t.Error("stop word 今天 should be dropped")
	}
	// interior 2-gram weight 1.
	if m["天气"] != 1 {
		t.Errorf("天气 = %d, want 1", m["天气"])
	}
	// leading edge 5-gram exists (weight 2+1=3).
	if m["今天天气真"] != 3 {
		t.Errorf("edge 5-gram 今天天气真 = %d, want 3", m["今天天气真"])
	}
	// interior 4-gram weight 2.
	if m["天气真不错"] != 2 {
		t.Errorf("interior 4-gram 天气真不错 = %d, want 2", m["天气真不错"])
	}
}

func TestPruneOverlappingWords(t *testing.T) {
	// telegram itself is a stop word; use these.
	c := newWordCounts()
	c.add("telegrams", 10)
	c.add("tele", 9)    // fragment of telegrams, ≥0.9x → dropped
	c.add("telegr", 10) // fragment, ≥0.9x → dropped
	c.add("telegraphs", 10)
	got := pruneOverlappingWords(c.entries)
	words := map[string]bool{}
	for _, e := range got {
		words[e.word] = true
	}
	if words["tele"] || words["telegr"] {
		t.Errorf("fragments not pruned: %v", words)
	}
	if !words["telegrams"] || !words["telegraphs"] {
		t.Errorf("long words dropped: %v", words)
	}
	// Source semantics: the SHORT word is dropped when a longer word that
	// contains it is at least 0.9x as frequent. Here tele (2) is inside
	// telegrams (10) and 10 >= 0.9*2, so tele is still dropped — the check
	// is on the long word's count vs the short word's, not the reverse.
	c2 := newWordCounts()
	c2.add("telegrams", 10)
	c2.add("tele", 2)
	got2 := pruneOverlappingWords(c2.entries)
	if len(got2) != 1 || got2[0].word != "telegrams" {
		t.Errorf("expected telegrams only, got %+v", got2)
	}
	// A short word NOT contained in any long word always survives.
	c3 := newWordCounts()
	c3.add("telegrams", 10)
	c3.add("phone", 2)
	if got3 := pruneOverlappingWords(c3.entries); len(got3) != 2 {
		t.Errorf("expected both kept, got %+v", got3)
	}
}

func TestBuildWordItems(t *testing.T) {
	c := newWordCounts()
	c.add("词云", 30)
	c.add("golang", 20)
	c.add("测试", 10)
	c.add("低频", 1) // count < 2 → filtered
	items := buildWordItems(c)
	if len(items) != 3 {
		t.Fatalf("len = %d, want 3 (%+v)", len(items), items)
	}
	if items[0].word != "词云" {
		t.Errorf("top word = %q", items[0].word)
	}
	if items[0].size <= items[1].size || items[1].size <= items[2].size {
		t.Errorf("sizes not descending: %+v", items)
	}
	if items[0].size != 12+68 {
		t.Errorf("max size = %d, want 80", items[0].size)
	}
	if items[2].size != 12 {
		t.Errorf("min size = %d, want 12", items[2].size)
	}
	// colors cycle through the palette
	if items[0].color != 0 || items[1].color != 1 {
		t.Errorf("colors = %+v", items)
	}
	// empty counts → nil
	if got := buildWordItems(newWordCounts()); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// ---------------------------------------------------------------- schedule

func TestValidTimes(t *testing.T) {
	if v, err := validTimes("09:00, 21:30 , 09:00"); err != nil || v != "09:00,21:30" {
		t.Errorf("got %q, %v", v, err)
	}
	if _, err := validTimes("9:00"); err == nil {
		t.Error("9:00 should be invalid")
	}
	if _, err := validTimes("24:00"); err == nil {
		t.Error("24:00 should be invalid")
	}
	if _, err := validTimes("12:60"); err == nil {
		t.Error("12:60 should be invalid")
	}
	if v, err := validTimes("  "); err != nil || v != "" {
		t.Errorf("blank: got %q, %v", v, err)
	}
	if v, err := validTimes("09:00,"); err != nil || v != "09:00" {
		t.Errorf("trailing comma: got %q, %v", v, err)
	}
	if _, err := validTimes("09:00,09:01,09:02,09:03,09:04,09:05,09:06,09:07,09:08,09:09,09:10,09:11,09:12"); err == nil {
		t.Error("13 times should be invalid")
	}
}

func TestValidTarget(t *testing.T) {
	ok := []string{"@mygroup", "-1001234567890", "123456", "-1234"}
	for _, s := range ok {
		if v, err := validTarget(s); err != nil || v != s {
			t.Errorf("validTarget(%q) = %q, %v", s, v, err)
		}
	}
	bad := []string{"@", "@a b", "@x@y", "abc", "0", "12.5"}
	for _, s := range bad {
		if _, err := validTarget(s); err == nil {
			t.Errorf("validTarget(%q) should fail", s)
		}
	}
	if v, err := validTarget("  @mygroup  "); err != nil || v != "@mygroup" {
		t.Errorf("trim: got %q, %v", v, err)
	}
}

func TestNextFire(t *testing.T) {
	loc := time.FixedZone("t", 8*3600)
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, loc)
	at, ok := nextFire([]string{"09:00", "21:30"}, now)
	if !ok || at.Format("15:04") != "09:00" || at.Day() != now.Day() {
		t.Errorf("got %v, %v", at, ok)
	}
	now2 := time.Date(2026, 10, 5, 9, 30, 0, 0, loc)
	at2, _ := nextFire([]string{"09:00"}, now2)
	if at2.Format("15:04") != "09:00" || at2.Day() != now2.Day()+1 {
		t.Errorf("got %v", at2)
	}
	if _, ok := nextFire(nil, now); ok {
		t.Error("empty times should give ok=false")
	}
}

func TestParseLimitArg(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 500}, {"abc", 500}, {"0", 500}, {"-5", 500},
		{"10", 50}, {"500", 500}, {"5000", 2000}, {"2000", 2000},
	}
	for _, c := range cases {
		if got := parseLimitArg(c.in); got != c.want {
			t.Errorf("parseLimitArg(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- ttcfix

// TestRepackTTCBadInput guards the repacker against malformed data.
func TestRepackTTCBadInput(t *testing.T) {
	if _, err := repackTTC([]byte("nope"), 0); err == nil {
		t.Error("short input should fail")
	}
	if _, err := repackTTC(append([]byte("ttcf\x00\x01\x00\x00\x00\x00\x00\x00"), make([]byte, 8)...), 0); err == nil {
		t.Error("zero-font collection should fail")
	}
	ttcf := append([]byte("ttcf\x00\x01\x00\x00\x00\x00\x00\x01"), make([]byte, 8)...)
	ttcf = append(ttcf, make([]byte, 64)...)
	if _, err := repackTTC(ttcf, 5); err == nil {
		t.Error("out-of-range index should fail")
	}
}

// ---------------------------------------------------------------- render

// TestRenderSmoke renders a small cloud when the font exists (skipped
// otherwise) and verifies the PNG decodes at 900x640.
func TestRenderSmoke(t *testing.T) {
	if _, err := os.Stat(fontPath); err != nil {
		t.Skip("font not installed")
	}
	c := newWordCounts()
	for i := 0; i < 30; i++ {
		c.add("词云测试"+strings.Repeat("很", 1+i%5), 3)
		c.add(string(rune('A'+i%26))+string(rune('a'+i%26))+"word", 2)
	}
	items := buildWordItems(c)
	if len(items) == 0 {
		t.Fatal("no words built")
	}
	pngBytes, err := renderCloud(items, 500, 120)
	if err != nil {
		t.Fatalf("renderCloud: %v", err)
	}
	if len(pngBytes) < 1000 {
		t.Fatalf("png too small: %d bytes", len(pngBytes))
	}
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("png decode: %v", err)
	}
	if img.Bounds().Dx() != imgWidth || img.Bounds().Dy() != imgHeight {
		t.Errorf("bounds = %v", img.Bounds())
	}
	// Colored pixels must exist: the words actually rendered (guards the
	// fixed-point coordinate math — a bad conversion draws off-canvas).
	colored := 0
	for y := 0; y < imgHeight; y += 3 {
		for x := 0; x < imgWidth; x += 3 {
			r, g, b, _ := img.At(x, y).RGBA()
			if r>>8 != 0xFF || g>>8 != 0xFF || b>>8 != 0xFF {
				colored++
			}
		}
	}
	if colored < 200 {
		t.Errorf("only %d sampled colored pixels; words probably not drawn", colored)
	}
}

// TestLayoutPlacesWords checks the spiral layout stays inside the canvas
// and placed boxes do not overlap.
func TestLayoutPlacesWords(t *testing.T) {
	if _, err := os.Stat(fontPath); err != nil {
		t.Skip("font not installed")
	}
	c := newWordCounts()
	for i := 0; i < 40; i++ {
		c.add("词汇"+string(rune(0x4e00+i)), 5)
	}
	items := buildWordItems(c)
	placed, err := layoutWords(items)
	if err != nil {
		t.Fatalf("layoutWords: %v", err)
	}
	if len(placed) == 0 {
		t.Fatal("nothing placed")
	}
	boxes := make([]placedBox, len(placed))
	for i, w := range placed {
		boxes[i] = boxOf(w)
		if w.x < 0 || w.y-w.height < 0 || w.x+w.width > imgWidth || w.y > imgHeight {
			t.Errorf("word %q outside canvas: %+v", w.word, w)
		}
	}
	for i := 0; i < len(boxes); i++ {
		for j := i + 1; j < len(boxes); j++ {
			if boxes[i].overlaps(boxes[j]) {
				t.Errorf("placed words %q and %q overlap", placed[i].word, placed[j].word)
			}
		}
	}
}

// TestBoxOverlap covers the rectangle math directly.
func TestBoxOverlap(t *testing.T) {
	a := wordItem{x: 100, y: 200, width: 80, height: 40}
	b := wordItem{x: 150, y: 210, width: 80, height: 40}
	c := wordItem{x: 500, y: 400, width: 80, height: 40}
	if !boxOf(a).overlaps(boxOf(b)) {
		t.Error("a/b should overlap")
	}
	if boxOf(a).overlaps(boxOf(c)) {
		t.Error("a/c should not overlap")
	}
	// boxes are inflated by the padding (4px): a 5px gap still overlaps
	d := wordItem{x: 185, y: 200, width: 80, height: 40}
	if !boxOf(a).overlaps(boxOf(d)) {
		t.Error("padding overlap expected")
	}
	e := wordItem{x: 200, y: 200, width: 80, height: 40} // 20px gap
	if boxOf(a).overlaps(boxOf(e)) {
		t.Error("no overlap expected")
	}
}
