package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func zh(zhText, _ string) string { return zhText }

// ---------------------------------------------------------------- matching

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"Hello-World":    "hello world",
		"  a_b.c|d/e#f ": "a b c d e f",
		"AB-1 2":         "ab 1 2",
		"xxx":            "xxx",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFuzzyMatch(t *testing.T) {
	cases := []struct {
		text, query string
		want        bool
	}{
		{"hello world", "hello", true},
		{"hello world", "hello world", true},
		{"hello world", "world hello", true},  // every part matches somewhere
		{"hello world", "hello there", false}, // "there" absent
		{"abc 123", "abc123", true},           // letters+digits squashed
		{"abcd1234", "abc 123", true},         // single query part squash
		{"", "x", false},
		{"anything", "", false},
	}
	for _, c := range cases {
		if got := fuzzyMatch(normalize(c.text), normalize(c.query)); got != c.want {
			t.Errorf("fuzzyMatch(%q, %q) = %v, want %v", c.text, c.query, got, c.want)
		}
	}
}

func TestLettersDigits(t *testing.T) {
	yes := []string{"abc1", "abc 1", "ab12"}
	no := []string{"abc", "1abc", "abc 1x", "", "a 1 2"}
	for _, s := range yes {
		if !lettersDigits(s) {
			t.Errorf("lettersDigits(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if lettersDigits(s) {
			t.Errorf("lettersDigits(%q) = true, want false", s)
		}
	}
}

// ---------------------------------------------------------------- messages

func msg(text string, attrs ...tg.DocumentAttributeClass) *tg.Message {
	m := &tg.Message{Message: text}
	if len(attrs) > 0 {
		m.Media = &tg.MessageMediaDocument{Document: &tg.Document{Attributes: attrs}}
	}
	return m
}

func TestMatchesAndAd(t *testing.T) {
	m := msg("Sakura Trick EP07", &tg.DocumentAttributeFilename{FileName: "sakura_trick_07.mp4"})
	if !matches(m, "sakura trick") {
		t.Error("text match failed")
	}
	if !matches(m, "sakura_trick_07") {
		t.Error("filename match failed")
	}
	if matches(m, "yuru yuri") {
		t.Error("false positive match")
	}
	ad := msg("联系客服办理业务 广告", &tg.DocumentAttributeFilename{FileName: "promo.mp4"})
	if !isAd([]string{"广告", "promo"}, ad) {
		t.Error("ad not detected")
	}
	if isAd([]string{"广告"}, m) {
		t.Error("clean message flagged as ad")
	}
}

func TestScoreOf(t *testing.T) {
	byName := msg("EP07", &tg.DocumentAttributeFilename{FileName: "hit EP07.mp4"})
	byText := msg("hit EP07", &tg.DocumentAttributeFilename{FileName: "x.mp4"})
	none := msg("EP07")
	for _, c := range []struct {
		m    *tg.Message
		want int
	}{{byName, 100}, {byText, 50}, {none, 0}} {
		if got := scoreOf(c.m, "hit ep07"); got != c.want {
			t.Errorf("scoreOf = %d, want %d", got, c.want)
		}
	}
}

func TestVideoOf(t *testing.T) {
	video := msg("v", &tg.DocumentAttributeVideo{Duration: 65, W: 1280, H: 720})
	if _, v, ok := videoOf(video); !ok || v.Duration != 65 {
		t.Error("video not detected")
	}
	round := msg("v", &tg.DocumentAttributeVideo{Duration: 60, RoundMessage: true})
	if _, _, ok := videoOf(round); ok {
		t.Error("round video message should not count as video post")
	}
	web := &tg.Message{Message: "v", Media: &tg.MessageMediaWebPage{}}
	if _, _, ok := videoOf(web); ok {
		t.Error("webpage preview must not count as video")
	}
	if _, _, ok := videoOf(msg("plain")); ok {
		t.Error("text message must not count as video")
	}
}

func TestDurationFilter(t *testing.T) {
	dur := func(d float64) *tg.Message {
		return msg("v", &tg.DocumentAttributeVideo{Duration: d})
	}
	for _, c := range []struct {
		m       *tg.Message
		inRange bool
	}{{dur(20), true}, {dur(19.5), false}, {dur(180), true}, {dur(181), false}} {
		v := durationOf(c.m)
		if got := v >= 20 && v <= 180; got != c.inRange {
			t.Errorf("duration %v in range = %v, want %v", v, got, c.inRange)
		}
	}
}

// ---------------------------------------------------------------- links

func TestChannelLink(t *testing.T) {
	c := channel{Username: "@durov", ChatID: -1001234}
	if got := c.link(42); got != "https://t.me/durov/42" {
		t.Errorf("username link = %q", got)
	}
	private := channel{ChatID: -1001234567890}
	if got := private.link(7); got != "https://t.me/c/1234567890/7" {
		t.Errorf("private link = %q", got)
	}
	none := channel{Title: "??"}
	if got := none.link(7); got != "" {
		t.Errorf("no-reference link should be empty, got %q", got)
	}
	if got := c.link(0); got != "" {
		t.Errorf("zero msg id link should be empty, got %q", got)
	}
}

func TestChatRefLink(t *testing.T) {
	r := chatRef{ChatID: -1001234567890}
	if got := r.link(3); got != "https://t.me/c/1234567890/3" {
		t.Errorf("chatRef link = %q", got)
	}
	if got := (chatRef{}).link(3); got != "" {
		t.Errorf("empty chatRef link should be empty, got %q", got)
	}
}

// parseChatID must reject tokens that merely start with a number:
// Sscanf("123abc") used to succeed with 123 and resolve a garbage peer.
func TestParseChatID(t *testing.T) {
	for _, s := range []string{"-1001234567890", "-1234", "1234"} {
		if _, err := parseChatID(s); err != nil {
			t.Errorf("parseChatID(%q) = %v, want ok", s, err)
		}
	}
	for _, s := range []string{"123abc", "abc", "", "12 34", "-100x", "1.5"} {
		if _, err := parseChatID(s); err == nil {
			t.Errorf("parseChatID(%q) accepted garbage", s)
		}
	}
}

func TestParseTME(t *testing.T) {
	cases := map[string]string{
		"https://t.me/durov":        "durov",
		"https://t.me/durov/12":     "durov",
		"t.me/some_chat?x":          "some_chat",
		"telegram.me/foo":           "foo",
		"https://t.me/joinchat/xyz": "joinchat",
	}
	for in, want := range cases {
		got, ok := parseTME(in)
		if !ok {
			t.Errorf("parseTME(%q) not ok", in)
			continue
		}
		if got != want {
			t.Errorf("parseTME(%q) = %q, want %q", in, got, want)
		}
	}
	// non-link inputs pass through unchanged and reported as not-a-link
	for _, in := range []string{"@bar", "+abc", "plainname"} {
		if got, ok := parseTME(in); ok || got != in {
			t.Errorf("parseTME(%q) = %q ok=%v, want passthrough false", in, got, ok)
		}
	}
	if got, ok := parseTME("https://example.com/x"); ok {
		t.Errorf("foreign url should not parse, got %q", got)
	}
}

// ---------------------------------------------------------------- config

func testConfig(t *testing.T) *config {
	t.Helper()
	return &config{
		DefaultChannel: "@two",
		Channels: []channel{
			{Title: "One", Handle: "@one"},
			{Title: "Two", Handle: "@two"},
			{Title: "Three", Handle: "@three"},
		},
	}
}

func TestSearchOrder(t *testing.T) {
	c := testConfig(t)
	got := strings.Join(c.searchOrder(), ",")
	want := "@two,@one,@three"
	if got != want {
		t.Errorf("searchOrder = %q, want %q", got, want)
	}
}

func TestRemoveChannels(t *testing.T) {
	c := testConfig(t)
	removed := c.removeChannels(map[string]bool{"@two": true})
	if len(removed) != 1 || removed[0].Handle != "@two" {
		t.Fatalf("removed = %+v", removed)
	}
	if c.DefaultChannel != "@one" {
		t.Errorf("default should fall back to @one, got %q", c.DefaultChannel)
	}
	if len(c.Channels) != 2 {
		t.Errorf("2 channels should remain, got %d", len(c.Channels))
	}
	// removing everything clears the default
	c.removeChannels(map[string]bool{"@one": true, "@three": true})
	if c.DefaultChannel != "" || len(c.Channels) != 0 {
		t.Errorf("wipe failed: %+v", c)
	}
}

func TestExpandIndexes(t *testing.T) {
	c := testConfig(t)
	h := expandIndexes(c, []string{"1", "@three", "99"})
	if !h["@one"] || !h["@three"] || !h["99"] {
		t.Errorf("expandIndexes = %v", h)
	}
}

func TestAtoiPositive(t *testing.T) {
	if n, ok := atoiPositive("42"); !ok || n != 42 {
		t.Errorf("atoiPositive(42) = %d %v", n, ok)
	}
	for _, s := range []string{"", "x", "4x", "-3"} {
		if _, ok := atoiPositive(s); ok {
			t.Errorf("atoiPositive(%q) should fail", s)
		}
	}
}

// ---------------------------------------------------------------- store

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &store{dir: filepath.Join(dir, "d"), cfg: config{AdFilters: defaultAdFilters}}
	if err := s.withLock(func(c *config) error {
		c.Channels = []channel{{Title: "X", Handle: "@x", Username: "x", ChatID: -1001234}}
		c.DefaultChannel = "@x"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d", "config.json")); err != nil {
		t.Fatal("config.json not written")
	}
	s2 := &store{dir: filepath.Join(dir, "d"), cfg: config{AdFilters: defaultAdFilters}}
	b, err := os.ReadFile(filepath.Join(dir, "d", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &s2.cfg); err != nil {
		t.Fatal(err)
	}
	if len(s2.cfg.Channels) != 1 || s2.cfg.Channels[0].Handle != "@x" || s2.cfg.DefaultChannel != "@x" {
		t.Errorf("round trip lost data: %+v", s2.cfg)
	}
	if len(s2.cfg.AdFilters) == 0 || s2.cfg.AdFilters[0] != "广告" {
		t.Errorf("default ad filters missing: %v", s2.cfg.AdFilters[:min(3, len(s2.cfg.AdFilters))])
	}
}

// ---------------------------------------------------------------- rendering

func TestChunkLines(t *testing.T) {
	head := "HEAD"
	lines := []string{"aaa", "bbb", "ccc"}
	out := chunkLines(head, lines, 100)
	if len(out) != 1 || !strings.Contains(out[0], "HEAD") || !strings.Contains(out[0], "ccc") {
		t.Errorf("single chunk expected, got %v", out)
	}
	// force a split: head + first line fits, second does not
	out = chunkLines(head, lines, len("HEAD\n\naaa")+2)
	if len(out) != 2 {
		t.Fatalf("two chunks expected, got %d: %v", len(out), out)
	}
	if !strings.Contains(out[0], "aaa") || !strings.Contains(out[1], "ccc") {
		t.Errorf("chunk contents wrong: %v", out)
	}
}

func TestPreviewAndMeta(t *testing.T) {
	m := msg(strings.Repeat("字", 100))
	if got := preview(m, zh); got != strings.Repeat("字", 80)+"…" {
		t.Errorf("preview clip failed: %d runes", len([]rune(got)))
	}
	if got := preview(msg(""), zh); got != "[帖子]" {
		t.Errorf("empty preview = %q", got)
	}
	v := msg("cap", &tg.DocumentAttributeVideo{Duration: 125}, &tg.DocumentAttributeFilename{FileName: "v.mp4"})
	if got := metaLine(hit{Msg: v, FileName: "v.mp4"}, zh); !strings.Contains(got, "2:05") || !strings.Contains(got, "v.mp4") {
		t.Errorf("metaLine = %q", got)
	}
}

func TestFlags(t *testing.T) {
	if !containsFlag("hello -r world", "-r") {
		t.Error("-r not detected")
	}
	if containsFlag("hello -run x", "-r") {
		t.Error("-run must not count as -r")
	}
	if got := removeFlag("hello -R world", "-r"); got != "hello world" {
		t.Errorf("removeFlag case-insensitive failed: %q", got)
	}
}

func TestFail(t *testing.T) {
	flood := fail(zh, tgerr.New(420, "FLOOD_WAIT_120"))
	if !strings.Contains(flood, "121") { // +1s rounded up, like his
		t.Errorf("flood wait message missing seconds: %q", flood)
	}
	priv := fail(zh, tgerr.New(400, "CHANNEL_PRIVATE"))
	if !strings.Contains(priv, "无法访问") {
		t.Errorf("channel private message: %q", priv)
	}
	other := fail(zh, tgerr.New(400, "WEIRD_ERROR"))
	if !strings.Contains(other, "WEIRD_ERROR") {
		t.Errorf("raw type should surface: %q", other)
	}
}

func TestDedupeAndSortHits(t *testing.T) {
	a := hit{Msg: &tg.Message{ID: 1}, Chat: chatRef{ChatID: -1001}, Score: 10}
	b := hit{Msg: &tg.Message{ID: 1}, Chat: chatRef{ChatID: -1001}, Score: 50}
	c := hit{Msg: &tg.Message{ID: 2}, Chat: chatRef{ChatID: -1001}, Score: 5}
	d := hit{Msg: &tg.Message{ID: 1}, Chat: chatRef{ChatID: -1002}, Score: 99}
	got := dedupeHits([]hit{a, b, c, d})
	if len(got) != 3 {
		t.Fatalf("dedupe = %d hits, want 3", len(got))
	}
	if got[0].Score != 10 {
		t.Errorf("first occurrence should win, got score %d", got[0].Score)
	}
	sortHits(got)
	if got[0].Score != 99 {
		t.Errorf("sort by score failed: %d", got[0].Score)
	}
}

func TestErrText(t *testing.T) {
	if errText(nil) != "" {
		t.Error("nil error should be empty")
	}
	long := strings.Repeat("x", 200)
	if got := errText(func() error { var e error = errString(long); return e }()); len([]rune(got)) != 161 {
		t.Errorf("long error not clipped: %d", len([]rune(got)))
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
