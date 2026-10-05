package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// ---------------------------------------------------------------- magic bytes

func TestDetectImageMime(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0}, "image/jpeg"},
		{"jpeg short", []byte{0xFF, 0xD8, 0xFF}, ""},
		{"png", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, "image/png"},
		{"gif", []byte("GIF89a0000"), "image/gif"},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00"), []byte("WEBPVP8 ")...), "image/webp"},
		{"webp short", []byte("RIFF\x00\x00\x00\x00WEB"), ""},
		{"not image", []byte("hello world!"), ""},
		{"tiny", []byte{0x00}, ""},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		if got := detectImageMime(c.data); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- think tags

func TestRemoveThinkTags(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"no tags", "羡慕富哥", "羡慕富哥"},
		{"simple", "<think>reasoning</think>羡慕富哥", "羡慕富哥"},
		{"leading whitespace", "<think>\nlong reasoning\n</think>\n\n羡慕打工魂", "羡慕打工魂"},
		{"only think", "<think>all</think>", ""},
		{"unclosed", "<think>reasoning only", "<think>reasoning only"},
		{"close without open", "answer with tail", "answer with tail"},
		{"no think at all", "nothing here", "nothing here"},
	}
	for _, c := range cases {
		if got := removeThinkTags(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- truncation

func TestClampAnswer(t *testing.T) {
	// Under the limit: unchanged.
	s := strings.Repeat("羡", 100)
	got, tokens, trunc := clampAnswer(s, maxResponseTokens, truncKeepRunes)
	if trunc || got != s || tokens != 25 {
		t.Fatalf("short: got %q trunc=%v tokens=%d", got, trunc, tokens)
	}

	// Over the limit: truncated to keepRunes runes plus ellipsis.
	long := strings.Repeat("a", maxResponseTokens*4*2) // ~2x limit
	got, tokens, trunc = clampAnswer(long, maxResponseTokens, truncKeepRunes)
	if !trunc {
		t.Fatal("long reply not flagged")
	}
	if tokens <= maxResponseTokens {
		t.Fatalf("token estimate %d should exceed %d", tokens, maxResponseTokens)
	}
	if want := string([]rune(long)[:truncKeepRunes]) + "..."; got != want {
		t.Fatalf("truncated text mismatch: len got %d want %d", len(got), len(want))
	}

	// Astral plane runes count 2 UTF-16 units each: 4001 units > 4000 tokens/4.
	surrogate := strings.Repeat("😀", 4001) // 8002 UTF-16 units → 2001 tokens
	_, tokens, _ = clampAnswer(surrogate, 2000, 100)
	if tokens != 2001 {
		t.Fatalf("astral token estimate: got %d want 2001", tokens)
	}
}

func TestUtf16Len(t *testing.T) {
	if got := utf16Len("abc"); got != 3 {
		t.Errorf("ascii: %d", got)
	}
	if got := utf16Len("羡慕"); got != 2 {
		t.Errorf("cjk: %d", got)
	}
	if got := utf16Len("😀"); got != 2 {
		t.Errorf("astral: %d", got)
	}
}

// ---------------------------------------------------------------- URL handling

func TestNormalizeBaseURL(t *testing.T) {
	ok := []struct{ in, want string }{
		{"https://api.openai.com/v1", "https://api.openai.com/v1"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1"},
		{"  https://x.example/base/// ", "https://x.example/base"},
		{"http://localhost:8080", "http://localhost:8080"},
		{"", ""},
	}
	for _, c := range ok {
		got, err := normalizeBaseURL(c.in)
		if err != nil || got != c.want {
			t.Errorf("normalize(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	bad := []string{"api.openai.com/v1", "ftp://x", "not a url", "https://", "//x"}
	for _, in := range bad {
		if _, err := normalizeBaseURL(in); err == nil {
			t.Errorf("normalize(%q) should fail", in)
		} else if _, ok := err.(*plugin.InvalidError); !ok {
			t.Errorf("normalize(%q) error type %T, want InvalidError", in, err)
		}
	}
}

func TestConfigDefaultsAndEnv(t *testing.T) {
	old := getenv
	defer func() { getenv = old }()

	// No settings, no env: defaults.
	getenv = func(string) string { return "" }
	p := &XmslPlugin{set: emptySettings{}}
	cfg := p.config()
	if cfg.mode != "openai" || cfg.model != "gpt-4o" || cfg.baseURL != defaultBaseURL || cfg.key != "" {
		t.Fatalf("defaults: %+v", cfg)
	}

	// Env fallbacks.
	getenv = func(k string) string {
		switch k {
		case "XMSL_API_KEY":
			return "sk-test"
		case "XMSL_BASE_URL":
			return "https://relay.example/v1/"
		case "XMSL_MODEL":
			return "gpt-4o-mini"
		case "XMSL_API_MODE":
			return "GEMINI"
		}
		return ""
	}
	cfg = p.config()
	if cfg.key != "sk-test" || cfg.baseURL != "https://relay.example/v1" ||
		cfg.model != "gpt-4o-mini" || cfg.mode != "gemini" {
		t.Fatalf("env fallback: %+v", cfg)
	}

	// Settings win over env.
	p.set = mapSettings{"mode": "openai", "key": "panel", "baseurl": "https://panel/v1", "model": "panel-model"}
	cfg = p.config()
	if cfg.mode != "openai" || cfg.key != "panel" || cfg.baseURL != "https://panel/v1" || cfg.model != "panel-model" {
		t.Fatalf("settings should win: %+v", cfg)
	}

	// A stray mode value is coerced to openai.
	p.set = mapSettings{"mode": "weird"}
	if cfg := p.config(); cfg.mode != "openai" {
		t.Fatalf("weird mode: %+v", cfg)
	}
}

// ---------------------------------------------------------------- media classify

func TestClassifyReplyMedia(t *testing.T) {
	// Photo.
	photo := &tg.MessageMediaPhoto{Photo: &tg.Photo{
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "m", W: 320, H: 320, Size: 12345},
			&tg.PhotoSize{Type: "x", W: 1280, H: 1280, Size: 99999},
		},
	}}
	info := classifyReplyMedia(photo)
	if info.kind != kindPhoto {
		t.Fatalf("photo kind: %v", info.kind)
	}
	loc, ok := info.location.(*tg.InputPhotoFileLocation)
	if !ok || loc.ThumbSize != "x" || info.size != 99999 {
		t.Fatalf("photo should pick the largest size: %+v", loc)
	}

	// Progressive photo sizes: last entry is the biggest.
	prog := &tg.MessageMediaPhoto{Photo: &tg.Photo{
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "m", W: 320, H: 320, Size: 1000},
			&tg.PhotoSizeProgressive{Type: "w", W: 2560, H: 2560, Sizes: []int{64, 4096}},
		},
	}}
	if _, w, _, size := largestPhotoSize(prog.Photo.(*tg.Photo).Sizes); w != 2560 || size != 4096 {
		t.Fatalf("progressive size: w=%d size=%d", w, size)
	}

	// Sticker/document kinds.
	doc := func(mime string) *tg.MessageMediaDocument {
		return &tg.MessageMediaDocument{Document: &tg.Document{
			MimeType: mime, Size: 54321,
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeSticker{Alt: "🙂"}},
		}}
	}
	if info := classifyReplyMedia(doc(tgsMime)); info.kind != kindTgs {
		t.Errorf("tgs kind: %v", info.kind)
	}
	if info := classifyReplyMedia(doc("video/webm")); info.kind != kindWebm {
		t.Errorf("webm kind: %v", info.kind)
	}
	if info := classifyReplyMedia(doc("image/webp")); info.kind != kindImageDoc {
		t.Errorf("webp doc kind: %v", info.kind)
	}
	if info := classifyReplyMedia(doc("video/mp4")); info.kind != kindNone {
		t.Errorf("mp4 should be unusable: %v", info.kind)
	}

	// No media at all.
	if info := classifyReplyMedia(nil); info.kind != kindNone {
		t.Errorf("nil media: %v", info.kind)
	}

	// Empty photo sizes.
	empty := &tg.MessageMediaPhoto{Photo: &tg.Photo{}}
	if info := classifyReplyMedia(empty); info.kind != kindNone {
		t.Errorf("empty photo: %v", info.kind)
	}

	// Photo without real photo object (PhotoEmpty).
	emptyPhoto := &tg.MessageMediaPhoto{Photo: &tg.PhotoEmpty{}}
	if info := classifyReplyMedia(emptyPhoto); info.kind != kindNone {
		t.Errorf("empty photo object: %v", info.kind)
	}
}

// ---------------------------------------------------------------- error mapping

func TestApiErrCard(t *testing.T) {
	ctx := &plugin.CommandContext{Lang: "zh-CN"}
	if s := apiErrCard(ctx, &apiError{status: http.StatusUnauthorized}); !strings.Contains(s, "密钥无效") {
		t.Errorf("401: %s", s)
	}
	if s := apiErrCard(ctx, &apiError{status: http.StatusTooManyRequests}); !strings.Contains(s, "频繁") {
		t.Errorf("429: %s", s)
	}
	s := apiErrCard(ctx, &apiError{status: http.StatusBadRequest, msg: "bad image"})
	if !strings.Contains(s, "API 请求错误") || !strings.Contains(s, "bad image") {
		t.Errorf("400: %s", s)
	}
	// 400 text must be Markdown-escaped.
	if s := apiErrCard(ctx, &apiError{status: http.StatusBadRequest, msg: "a *b* _c_"}); strings.Contains(s, "*b*") {
		t.Errorf("400 unescaped: %s", s)
	}
	// Connection refused.
	refused := errors.New("dial tcp 1.2.3.4:443: connect: connection refused")
	if s := apiErrCard(ctx, refused); !strings.Contains(s, "无法连接") {
		t.Errorf("refused: %s", s)
	}
	// DNS failure counts as unreachable too.
	if s := apiErrCard(ctx, errors.New("dial tcp: lookup api.example: no such host")); !strings.Contains(s, "无法连接") {
		t.Errorf("dns: %s", s)
	}
	// Generic error: escaped and capped.
	long := strings.Repeat("x", 500)
	if s := apiErrCard(ctx, errors.New(long)); len([]rune(s)) > 260 {
		t.Errorf("generic cap: %d runes", len([]rune(s)))
	}
}

func TestApiErrorStrings(t *testing.T) {
	if s := (&apiError{status: 500, msg: "boom"}).Error(); s != "HTTP 500: boom" {
		t.Errorf("status+msg: %s", s)
	}
	if s := (&apiError{status: 500}).Error(); s != "HTTP 500" {
		t.Errorf("status only: %s", s)
	}
	if s := (&apiError{msg: "boom"}).Error(); s != "boom" {
		t.Errorf("msg only: %s", s)
	}
}

// ---------------------------------------------------------------- providers

func TestParseContentParts(t *testing.T) {
	if s, err := parseContentParts([]byte(`"plain"`)); err != nil || s != "plain" {
		t.Errorf("string: %q %v", s, err)
	}
	arr := `[{"type":"text","text":"a"},{"type":"output_text","text":"b"},{"type":"image_url"}]`
	if s, err := parseContentParts([]byte(arr)); err != nil || s != "a\nb" {
		t.Errorf("parts: %q %v", s, err)
	}
	if _, err := parseContentParts(nil); err == nil {
		t.Error("empty should fail")
	}
	if _, err := parseContentParts([]byte(`42`)); err == nil {
		t.Error("number should fail")
	}
}

func TestExtractProviderError(t *testing.T) {
	if s := extractProviderError([]byte(`{"error":{"message":"nope"}}`)); s != "nope" {
		t.Errorf("openai shape: %s", s)
	}
	if s := extractProviderError([]byte(`{"message":"gemini nope"}`)); s != "gemini nope" {
		t.Errorf("gemini shape: %s", s)
	}
	if s := extractProviderError([]byte("plain text")); s != "plain text" {
		t.Errorf("plain: %s", s)
	}
	if s := extractProviderError([]byte(strings.Repeat("y", 500))); len(s) != 300 {
		t.Errorf("cap: %d", len(s))
	}
}

func TestOpenAIEndpointJoin(t *testing.T) {
	// base already trimmed by config(); join must not double-slash.
	base := strings.TrimRight(defaultBaseURL, "/")
	if strings.HasSuffix(base, "/") || !strings.HasPrefix(base, "https://") {
		t.Fatalf("bad base %q", base)
	}
	if ep := base + "/chat/completions"; ep != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("endpoint: %s", ep)
	}
}

// ---------------------------------------------------------------- settings stub

type emptySettings struct{}

func (emptySettings) Bool(string) bool       { return false }
func (emptySettings) Int(string) int         { return 0 }
func (emptySettings) Set(string, any) error  { return nil }
func (emptySettings) String(k string) string { return "" }

type mapSettings map[string]string

func (m mapSettings) Bool(string) bool       { return false }
func (m mapSettings) Int(string) int         { return 0 }
func (m mapSettings) Set(string, any) error  { return nil }
func (m mapSettings) String(k string) string { return m[k] }
