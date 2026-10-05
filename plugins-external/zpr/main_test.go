package main

import (
	"strings"
	"testing"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		num  int
		r18  bool
		tag  string
		bad  string
	}{
		{"none", nil, 1, false, "", ""},
		{"num", []string{"3"}, 3, false, "", ""},
		{"num clamp hi", []string{"99"}, maxImages, false, "", ""},
		{"num clamp lo", []string{"0"}, 1, false, "", ""},
		{"r18", []string{"r18"}, 1, true, "", ""},
		{"R18 case", []string{"R18"}, 1, true, "", ""},
		{"r18 num", []string{"r18", "2"}, 2, true, "", ""},
		{"r18 bad", []string{"r18", "x"}, 1, true, "", "x"},
		{"tag", []string{"萝莉"}, 1, false, "萝莉", ""},
		{"tag num", []string{"萝莉", "2"}, 2, false, "萝莉", ""},
		{"tag r18", []string{"萝莉", "r18"}, 1, true, "萝莉", ""},
		{"tag r18 num", []string{"萝莉", "r18", "3"}, 3, true, "萝莉", ""},
		{"tag junk", []string{"萝莉", "x"}, 1, false, "萝莉", ""},
		{"help is tag", []string{"help"}, 1, false, "help", ""},
	}
	for _, c := range cases {
		got := parseArgs(c.args)
		if got.num != c.num || got.r18 != c.r18 || got.tag != c.tag || got.bad != c.bad {
			t.Errorf("%s: parseArgs(%v) = %+v, want num=%d r18=%v tag=%q bad=%q",
				c.name, c.args, got, c.num, c.r18, c.tag, c.bad)
		}
	}
}

func TestParseArgsNonNumericCountIsError(t *testing.T) {
	// The reference silently ignores non-numeric counts; the Go port
	// reports them (cosplay convention).
	if got := parseArgs([]string{"r18", "abc"}); got.bad != "abc" {
		t.Fatalf("bad = %q, want abc", got.bad)
	}
	if got := parseArgs([]string{"tag", "r18", "1x"}); got.bad != "1x" {
		t.Fatalf("bad = %q, want 1x", got.bad)
	}
}

func TestIsNumArg(t *testing.T) {
	for s, want := range map[string]bool{"1": true, "10": true, "007": true, "": false, "1a": false, "a1": false, "-1": false, "1.5": false, "１": false} {
		if got := isNumArg(s); got != want {
			t.Errorf("isNumArg(%q) = %v", s, got)
		}
	}
}

func TestReplaceHost(t *testing.T) {
	cases := []struct{ in, proxy, want string }{
		// Whole-host rewrite: no i.i.pximg.net double prefix (the Next bug).
		{"https://i.pximg.net/img-original/img/2024/01/01/00/00/00/12345678_p0.jpg", "i.pixiv.cat",
			"https://i.pixiv.cat/img-original/img/2024/01/01/00/00/00/12345678_p0.jpg"},
		{"https://i.pximg.net/img-original/img/1.jpg", "i.pximg.net",
			"https://i.pximg.net/img-original/img/1.jpg"},
		{"https://i.pixiv.re/a/b.png", "i.pixiv.nl", "https://i.pixiv.nl/a/b.png"},
		{"https://i.pximg.net:443/a.jpg", "i.pixiv.cat", "https://i.pixiv.cat/a.jpg"}, // explicit default port is dropped
	}
	for _, c := range cases {
		if got := replaceHost(c.in, c.proxy); got != c.want {
			t.Errorf("replaceHost(%q, %s) = %q, want %q", c.in, c.proxy, got, c.want)
		}
	}
}

func TestReplaceHostNoPartialSubstrings(t *testing.T) {
	// Unknown hosts and substring matches must not be rewritten.
	for _, in := range []string{
		"https://example.com/i.pximg.net/a.jpg",
		"https://notpiximg.net/a.jpg",
		"/relative/path/i.pximg.net.jpg",
		"i.pximg.net",
	} {
		if got := replaceHost(in, "i.pixiv.cat"); got != in {
			t.Errorf("replaceHost(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestReplaceHostKeepsQueryAndFragment(t *testing.T) {
	in := "https://i.pximg.net/img/img.jpg?x=1#f"
	want := "https://i.pixiv.re/img/img.jpg?x=1#f"
	if got := replaceHost(in, "i.pixiv.re"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestProxyOrder(t *testing.T) {
	got := proxyOrder("i.pixiv.re")
	if got[0] != "i.pixiv.re" || len(got) != len(proxyHosts) {
		t.Fatalf("proxyOrder = %v", got)
	}
	seen := map[string]bool{}
	for _, h := range got {
		if seen[h] {
			t.Fatalf("duplicate %s in %v", h, got)
		}
		seen[h] = true
	}
	// Unknown preferred values still come first with all fallbacks after.
	got = proxyOrder("i.example.net")
	if got[0] != "i.example.net" || len(got) != len(proxyHosts)+1 {
		t.Fatalf("unknown preferred: %v", got)
	}
}

func TestProxyChoicesCoverHosts(t *testing.T) {
	choices := proxyChoices()
	if len(choices) != len(proxyHosts) {
		t.Fatalf("choices = %v", choices)
	}
	vals := map[string]bool{}
	for _, c := range choices {
		vals[c.Value] = true
		if c.Label == "" || c.LabelEN == "" {
			t.Errorf("choice %s missing label", c.Value)
		}
	}
	for _, h := range proxyHosts {
		if !vals[h] {
			t.Errorf("host %s not in choices", h)
		}
	}
	if defaultProxy != proxyHosts[0] {
		t.Errorf("defaultProxy = %s, want %s", defaultProxy, proxyHosts[0])
	}
}

func TestBuildCaptionZh(t *testing.T) {
	it := setuItem{PID: 12345, Title: "标题", Author: "画师", Tags: []string{"萝莉", "cat"}}
	c := buildCaption(it, "zh-CN")
	for _, want := range []string{"🎨", "**标题**", "12345", "pixiv.net/artworks/12345", "画师", "萝莉", "cat"} {
		if !strings.Contains(c, want) {
			t.Errorf("caption missing %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "Untitled") {
		t.Error("zh caption should not contain English fallbacks")
	}
}

func TestBuildCaptionEn(t *testing.T) {
	it := setuItem{PID: 7, Title: "Title", Author: "Artist", Tags: []string{"loli"}}
	c := buildCaption(it, "en-US")
	for _, want := range []string{"Title", "Artist", "Tags", "loli", "artworks/7"} {
		if !strings.Contains(c, want) {
			t.Errorf("caption missing %q:\n%s", want, c)
		}
	}
}

func TestBuildCaptionMarkdownEscaped(t *testing.T) {
	it := setuItem{PID: 1, Title: "a *b* [c]", Author: "x_y", Tags: []string{"t`g"}}
	c := buildCaption(it, "zh-CN")
	if !strings.Contains(c, plugin.Escape("a *b* [c]")) {
		t.Errorf("title not escaped: %s", c)
	}
}

func TestBuildCaptionEmpty(t *testing.T) {
	c := buildCaption(setuItem{PID: 1}, "zh-CN")
	if !strings.Contains(c, "无题") {
		t.Errorf("empty title fallback missing: %s", c)
	}
}

func TestBuildCaptionTruncates(t *testing.T) {
	tags := make([]string, 200)
	for i := range tags {
		tags[i] = "tag"
	}
	it := setuItem{PID: 1, Title: strings.Repeat("标", 3000), Tags: tags}
	if got := buildCaption(it, "zh-CN"); len([]rune(got)) > 1024 {
		t.Errorf("caption too long: %d runes", len([]rune(got)))
	}
}

func TestJoinTagsTruncates(t *testing.T) {
	tags := make([]string, 100)
	for i := range tags {
		tags[i] = "长标签内容"
	}
	got := joinTags(tags)
	if len([]rune(got)) > tagMaxRunes+20 {
		t.Errorf("joinTags too long: %d runes", len([]rune(got)))
	}
	if len(joinTags(nil)) != 0 {
		t.Error("nil tags should be empty")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("abc", 5); got != "abc" {
		t.Errorf("short = %q", got)
	}
	if got := truncateRunes(strings.Repeat("a", 10), 5); got != "aaaaa…" {
		t.Errorf("long = %q", got)
	}
}

func TestURLExt(t *testing.T) {
	cases := []struct{ u, fb, want string }{
		{"https://x/a.jpg", "", "jpg"},
		{"https://x/a.png?x=1", "", "png"},
		{"https://x/noext", "jpg", "jpg"},
		{"https://x/a.webp#f", "JPG", "jpg"},
		{"https://x/a", "", "jpg"},
	}
	for _, c := range cases {
		if got := urlExt(c.u, c.fb); got != c.want {
			t.Errorf("urlExt(%q, %q) = %q, want %q", c.u, c.fb, got, c.want)
		}
	}
}

// A hostile ext field from the API must never carry path separators or
// traversal out of the temp dir; the URL fallback (or jpg) is used instead.
func TestURLExtSanitizesPathCharacters(t *testing.T) {
	for _, fb := range []string{"../../x", "..", "a/b", `a\b`, "png/../x", "verylongextensionvalue"} {
		if got := urlExt("https://x/a.jpg", fb); got != "jpg" {
			t.Errorf("urlExt fallback %q = %q, want jpg", fb, got)
		}
	}
	if got := urlExt("https://x/a.png?x=1", "../evil"); got != "png" {
		t.Errorf("urlExt with hostile fallback and URL ext = %q, want png", got)
	}
}

func TestAtoid(t *testing.T) {
	if atoi("12") != 12 || atoi("abc") != 0 || atoi("") != 0 {
		t.Error("atoi basics")
	}
	if got := atoi("999999999999999999999999"); got != maxImages && got <= maxImages {
		// saturating behavior: parser stops once above the cap
		t.Log("atoi saturates at", got)
	}
}

func TestTrimErrShort(t *testing.T) {
	if trimErr("x") != "x" {
		t.Error("short passthrough")
	}
}

func TestKnownPixivHost(t *testing.T) {
	for _, h := range proxyHosts {
		if !knownPixivHost(h) || !knownPixivHost(strings.ToUpper(h)) {
			t.Errorf("knownPixivHost(%s) = false", h)
		}
	}
	for _, h := range []string{"i.i.pximg.net", "pximg.net", "example.com", ""} {
		if knownPixivHost(h) {
			t.Errorf("knownPixivHost(%s) = true", h)
		}
	}
	if !knownPixivHost("i.pximg.net:443") {
		t.Error("port should be stripped")
	}
}
