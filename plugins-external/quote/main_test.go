package main

import (
	"image/color"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		in   string
		want quoteArgs
	}{
		{"", quoteArgs{count: 1, scale: 2, bg: "#231d2b/#372e44"}},
		{"5", quoteArgs{count: 5, scale: 2, bg: "#231d2b/#372e44"}},
		{"-10", quoteArgs{count: -10, scale: 2, bg: "#231d2b/#372e44"}},
		{"100", quoteArgs{count: 50, scale: 2, bg: "#231d2b/#372e44"}},
		{"r 3", quoteArgs{count: 3, reply: true, scale: 2, bg: "#231d2b/#372e44"}},
		{"png", quoteArgs{count: 1, png: true, scale: 2, bg: "#231d2b/#372e44"}},
		{"stories", quoteArgs{count: 1, png: true, stories: true, scale: 2, bg: "#231d2b/#372e44"}},
		{"hidden media crop", quoteArgs{count: 1, hidden: true, media: true, crop: true, scale: 2, bg: "#231d2b/#372e44"}},
		{"scale 4", quoteArgs{count: 1, scale: 4, bg: "#231d2b/#372e44"}},
		{"s=8", quoteArgs{count: 1, scale: 8, bg: "#231d2b/#372e44"}},
		{"scale 30", quoteArgs{count: 1, scale: 20, bg: "#231d2b/#372e44"}},
		{"#1b1429", quoteArgs{count: 1, scale: 2, bg: "#1b1429"}},
		{"#111/#222", quoteArgs{count: 1, scale: 2, bg: "#111/#222"}},
		{"bg #0af", quoteArgs{count: 1, scale: 2, bg: "#0af"}},
		{"bg=1a2b3c", quoteArgs{count: 1, scale: 2, bg: "#1a2b3c"}},
		{"3 r png", quoteArgs{count: 3, reply: true, png: true, scale: 2, bg: "#231d2b/#372e44"}},
	}
	for _, c := range cases {
		got := parseArgs(c.in)
		if got != c.want {
			t.Errorf("parseArgs(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	// random bg resolves to some #hex
	got := parseArgs("bg random")
	if len(got.bg) != 7 || got.bg[0] != '#' {
		t.Errorf("bg random → %q, want #rrggbb", got.bg)
	}
}

func TestColorTokens(t *testing.T) {
	for _, ok := range []string{"random", "#fff", "#1b1429", "1b1429", "#111/#222", "abc/def", "//abc"} {
		if !isColorToken(ok) {
			t.Errorf("isColorToken(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "red", "#12345", "12", "#ggg"} {
		if isColorToken(bad) {
			t.Errorf("isColorToken(%q) = true, want false", bad)
		}
	}
	if n := normalizeColorToken("1b1429"); n != "#1b1429" {
		t.Errorf("normalizeColorToken → %q", n)
	}
	if n := normalizeColorToken("111/222"); n != "#111/#222" {
		t.Errorf("normalizeColorToken → %q", n)
	}
}

func TestParseHexColor(t *testing.T) {
	c, ok := parseHexColor("#fff")
	if !ok || c != (color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Errorf("parseHexColor(#fff) = %v %v", c, ok)
	}
	c, ok = parseHexColor("#1b1429")
	if !ok || c != (color.RGBA{0x1B, 0x14, 0x29, 0xFF}) {
		t.Errorf("parseHexColor(#1b1429) = %v %v", c, ok)
	}
	if _, ok := parseHexColor("#1b142"); ok {
		t.Error("parseHexColor(#1b142) should fail")
	}
	if _, ok := parseHexColor("zzz"); ok {
		t.Error("parseHexColor(zzz) should fail")
	}
}

func TestBackgroundColor(t *testing.T) {
	c1, c2, grad := backgroundColor("#111/#222")
	if !grad || c1 != (color.RGBA{0x11, 0x11, 0x11, 0xFF}) || c2 != (color.RGBA{0x22, 0x22, 0x22, 0xFF}) {
		t.Errorf("backgroundColor gradient = %v %v %v", c1, c2, grad)
	}
	c1, c2, grad = backgroundColor("#abc")
	if grad || c1 != c2 {
		t.Errorf("backgroundColor single = %v %v %v", c1, c2, grad)
	}
	// invalid falls back to default
	c1, _, _ = backgroundColor("nope")
	if c1 != (color.RGBA{0x23, 0x1D, 0x2B, 0xFF}) {
		t.Errorf("backgroundColor fallback = %v", c1)
	}
}

func TestSplitStyles(t *testing.T) {
	// bold over "bc" (UTF-16 offsets)
	rs := splitStyles("abcd", []entityRef{{kind: "bold", offset: 1, length: 2}})
	if len(rs) != 4 {
		t.Fatalf("len=%d", len(rs))
	}
	if rs[1].s.bold != true || rs[2].s.bold != true || rs[0].s.bold || rs[3].s.bold {
		t.Errorf("bold span wrong: %+v", rs)
	}
	// CJK with astral emoji ( surrogate pair counts 2 units )
	rs = splitStyles("a😀b", []entityRef{{kind: "italic", offset: 1, length: 2}})
	if !rs[1].s.italic {
		t.Errorf("emoji should be italic: %+v", rs[1])
	}
	if rs[2].s.italic {
		t.Errorf("b should not be italic")
	}
}

func TestWrapCells(t *testing.T) {
	// 5 cells per word, 10px each; CJK cells separate
	mk := func(r rune) cell { return cell{r: r, w: 10} }
	latin := []cell{}
	for _, r := range "aa bb cc" {
		latin = append(latin, mk(r))
	}
	chunks := buildChunks(latin)
	lines := wrapCells(chunks, 50) // fits "aa bb" exactly
	var got []string
	for _, ln := range lines {
		s := ""
		for _, c := range ln {
			s += string(c.r)
		}
		got = append(got, s)
	}
	if len(got) != 2 || got[0] != "aa bb" || got[1] != "cc" {
		t.Errorf("wrap = %q", got)
	}

	// CJK breaks anywhere
	cjk := []cell{}
	for _, r := range "你好世界" {
		cjk = append(cjk, mk(r))
	}
	lines = wrapCells(buildChunks(cjk), 30) // 3 per line
	if len(lines) != 2 || len(lines[0]) != 3 || len(lines[1]) != 1 {
		t.Errorf("cjk wrap = %d lines %v", len(lines), lines)
	}

	// unbreakable long word hard-splits
	long := []cell{}
	for i := 0; i < 9; i++ {
		long = append(long, mk('x'))
	}
	lines = wrapCells(buildChunks(long), 40) // 4 per line
	if len(lines) != 3 || len(lines[0]) != 4 {
		t.Errorf("hard split = %d lines", len(lines))
	}
}

func TestEmojiFontCanned(t *testing.T) {
	if _, err := loadEmojiFont(); err != nil {
		t.Skipf("emoji font unavailable: %v", err)
	}
	e, _ := loadEmojiFont()
	// known glyphs
	if g := e.glyph(0x1F600); g == 0 {
		t.Error("1F600 unmapped")
	}
	// PNG decodes for a handful of emoji
	for _, cp := range []rune{0x1F600, 0x2764, 0x1F44D, 0x263A} {
		g := e.glyph(cp)
		if g == 0 {
			continue
		}
		if img := e.emojiImage(g); img == nil {
			t.Errorf("no bitmap for %U", cp)
		}
	}
	// ligature: CN flag 1F1E8 1F1F3
	cn := e.glyph(0x1F1E8)
	flag2 := e.glyph(0x1F1F3)
	if cn != 0 && flag2 != 0 {
		if g, n, ok := e.ligature([]uint16{cn, flag2}, 0); !ok || n != 2 || g == 0 {
			t.Errorf("CN flag ligature failed: %d %d %v", g, n, ok)
		}
	}
	// skin tone ligature: 1F44D 1F3FB
	thumb, skin := e.glyph(0x1F44D), e.glyph(0x1F3FB)
	if thumb != 0 && skin != 0 {
		if _, _, ok := e.ligature([]uint16{thumb, skin}, 0); !ok {
			t.Error("thumbs-up + skin tone ligature failed")
		}
	}
}

func TestBuildCellsEmoji(t *testing.T) {
	e, err := loadEmojiFont()
	if err != nil {
		t.Skipf("emoji font unavailable: %v", err)
	}
	// 1F600 + VS16 + skin (ignored) renders as one emoji cell
	in := []styledRune{{r: 0x1F600}, {r: 0xFE0F}, {r: 'a'}}
	cells := buildCells(in, e, 26, nil, nil)
	if len(cells) != 2 || !cells[0].emoji || cells[1].r != 'a' {
		t.Errorf("cells = %+v", cells)
	}
	// CN flag folds into one cell
	in = []styledRune{{r: 0x1F1E8}, {r: 0x1F1F3}, {r: 'x'}}
	cells = buildCells(in, e, 26, nil, nil)
	if len(cells) != 2 || !cells[0].emoji || cells[0].glyph == 0 {
		t.Errorf("flag cells = %+v", cells)
	}
}

func TestRenderSmoke(t *testing.T) {
	// the wqy font must load for a real render
	f, err := openWQY()
	if err != nil {
		t.Skipf("wqy font unavailable: %v", err)
	}
	faceCache.ttf = f
	e, _ := loadEmojiFont()
	opt := renderOptions{scale: 2, emojiFont: e}
	r := newRenderer(opt)
	msgs := []*renderMsg{
		{senderID: 42, name: "张三", text: "你好，世界！Hello world 😀", avatar: nil, bgColor: paletteColor(42)},
		{senderID: 7, name: "Alice", text: "multi\nline 中文 and English text", reply: &renderReply{name: "Bob", text: "replied text", color: paletteColor(3)}},
	}
	img := r.renderQuoteImage(msgs, color.RGBA{0x23, 0x1D, 0x2B, 0xFF}, color.RGBA{0x37, 0x2E, 0x44, 0xFF})
	if img.Bounds().Dx() < 100 || img.Bounds().Dy() < 50 {
		t.Errorf("image too small: %v", img.Bounds())
	}
}

func TestTruncAndFormat(t *testing.T) {
	if s := truncVisually("hello", 3); s != "he…" {
		t.Errorf("trunc = %q", s)
	}
	if s := truncVisually("hi", 10); s != "hi" {
		t.Errorf("trunc = %q", s)
	}
	if s := formatDuration(75); s != "1:15" {
		t.Errorf("dur = %q", s)
	}
	if s := humanSize(1536); s != "1.5 KB" {
		t.Errorf("humanSize = %q", s)
	}
}
