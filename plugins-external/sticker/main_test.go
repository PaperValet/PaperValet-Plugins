package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
	"github.com/gotd/td/tg"
)

// pluginCtx builds a minimal CommandContext for formatting-only tests.
func pluginCtx() plugin.CommandContext {
	return plugin.CommandContext{Lang: "zh-CN"}
}

func TestParseSub(t *testing.T) {
	cases := []struct {
		args []string
		want subKind
		pack string
	}{
		{nil, subFav, ""},
		{[]string{}, subFav, ""},
		{[]string{"help"}, subHelp, ""},
		{[]string{"h"}, subHelp, ""},
		{[]string{"to", "MyPack"}, subFavTo, "MyPack"},
		{[]string{"to"}, subUnknown, ""},
		{[]string{"cancel"}, subCancel, ""},
		{[]string{"status"}, subStatus, ""},
		{[]string{"MyPack"}, subPack, "MyPack"},
		{[]string{"pic"}, subPic, ""},
		{[]string{"pic", "😎"}, subPic, ""},
		{[]string{"pic", "batch"}, subPicBatch, ""},
		{[]string{"pic", "help"}, subUnknown, ""},
		{[]string{"topng"}, subToPic, ""},
		{[]string{"topng", "png"}, subToPic, ""},
		{[]string{"tojpg"}, subToPic, ""},
	}
	for _, c := range cases {
		got := parseSub(c.args)
		if got.kind != c.want {
			t.Errorf("parseSub(%v) kind = %d, want %d", c.args, got.kind, c.want)
		}
		if got.pack != c.pack {
			t.Errorf("parseSub(%v) pack = %q, want %q", c.args, got.pack, c.pack)
		}
	}
}

func TestParseToPicArgs(t *testing.T) {
	cases := []struct {
		args   []string
		ok     bool
		format string
		doc    bool
		transp bool
	}{
		{[]string{"topng"}, true, "jpg", false, false},
		{[]string{"topng", "png"}, true, "png", false, false},
		{[]string{"topng", "transparent"}, true, "png", false, true},
		{[]string{"topng", "trans"}, true, "png", false, true},
		{[]string{"topng", "doc"}, true, "jpg", true, false},
		{[]string{"topng", "doc", "png"}, true, "png", true, false},
		{[]string{"topng", "doc", "png", "transparent"}, true, "png", true, true},
		{[]string{"topng", "png", "jpg"}, true, "jpg", false, false},
		{[]string{"topng", "jpg"}, true, "jpg", false, false},
		{[]string{"topng", "bogus"}, false, "", false, false},
		{[]string{"tojpg"}, true, "jpg", false, false},
	}
	for _, c := range cases {
		opts, ok := parseToPicArgs(c.args)
		if ok != c.ok {
			t.Errorf("parseToPicArgs(%v) ok = %v, want %v", c.args, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if opts.format != c.format || opts.doc != c.doc || opts.transp != c.transp {
			t.Errorf("parseToPicArgs(%v) = %+v, want format=%s doc=%v transp=%v",
				c.args, opts, c.format, c.doc, c.transp)
		}
	}
}

func TestValidPackName(t *testing.T) {
	valid := []string{"a", "abc", "MyPack", "user_static_1", "a12345678901234567890"}
	invalid := []string{"", "1abc", "_abc", "ab-cd", "ab cd", strings.Repeat("a", 65), "pack.name"}
	for _, s := range valid {
		if !validPackName(s) {
			t.Errorf("validPackName(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if validPackName(s) {
			t.Errorf("validPackName(%q) = true, want false", s)
		}
	}
}

func TestStickerClassification(t *testing.T) {
	d := &tg.Document{MimeType: "application/x-tgsticker"}
	if classifySticker(d) != kindAnimated {
		t.Error("x-tgsticker should be animated")
	}
	d = &tg.Document{MimeType: "video/webm"}
	if classifySticker(d) != kindVideo {
		t.Error("video/webm should be video")
	}
	d = &tg.Document{MimeType: "image/webp"}
	if classifySticker(d) != kindStatic {
		t.Error("image/webp should be static")
	}
	if kindSuffix(kindAnimated) != "_animated" || kindSuffix(kindVideo) != "_video" || kindSuffix(kindStatic) != "_static" {
		t.Error("kindSuffix mismatch")
	}
}

func TestStickerAttrAndDoc(t *testing.T) {
	doc := &tg.Document{
		MimeType: "image/webp",
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeSticker{Alt: "🔥", Stickerset: &tg.InputStickerSetEmpty{}},
		},
	}
	msg := &tg.Message{Media: &tg.MessageMediaDocument{Document: doc}}
	if !isStickerMsg(msg) {
		t.Fatal("isStickerMsg should be true")
	}
	got, ok := docOf(msg)
	if !ok || got != doc {
		t.Fatal("docOf should return the document")
	}
	attr, has := stickerAttr(doc)
	if !has || attr.Alt != "🔥" {
		t.Fatalf("stickerAttr = %+v", attr)
	}
	// plain photo message is not a sticker
	photo := &tg.Message{Media: &tg.MessageMediaPhoto{}}
	if isStickerMsg(photo) {
		t.Error("photo message must not be a sticker")
	}
	if _, ok := docOf(nil); ok {
		t.Error("docOf(nil) should be false")
	}
}

func TestIsPhotoLike(t *testing.T) {
	if !isPhotoLike(&tg.MessageMediaPhoto{}) {
		t.Error("photo should be photo-like")
	}
	img := &tg.MessageMediaDocument{Document: &tg.Document{MimeType: "image/jpeg"}}
	if !isPhotoLike(img) {
		t.Error("image/jpeg document should be photo-like")
	}
	stickerDoc := &tg.Document{
		MimeType: "image/webp",
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeSticker{Alt: "🔥", Stickerset: &tg.InputStickerSetEmpty{}},
		},
	}
	if isPhotoLike(&tg.MessageMediaDocument{Document: stickerDoc}) {
		t.Error("a sticker must not be photo-like")
	}
	if isPhotoLike(&tg.MessageMediaDocument{Document: &tg.Document{MimeType: "video/mp4"}}) {
		t.Error("video must not be photo-like")
	}
}

func TestMediaLocation(t *testing.T) {
	photo := &tg.MessageMediaPhoto{Photo: &tg.Photo{
		ID:            1,
		AccessHash:    2,
		FileReference: []byte("ref"),
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "x", W: 100, H: 80, Size: 1234},
			&tg.PhotoSize{Type: "s", W: 40, H: 40, Size: 100},
		},
	}}
	loc, ok := mediaLocation(photo)
	if !ok {
		t.Fatal("photo location expected")
	}
	pl, isPhoto := loc.(*tg.InputPhotoFileLocation)
	if !isPhoto || pl.ThumbSize != "x" {
		t.Fatalf("largest size not chosen: %+v", pl)
	}
	doc := &tg.MessageMediaDocument{Document: &tg.Document{ID: 7, AccessHash: 8, FileReference: []byte("r")}}
	loc, ok = mediaLocation(doc)
	if !ok {
		t.Fatal("document location expected")
	}
	if _, isDoc := loc.(*tg.InputDocumentFileLocation); !isDoc {
		t.Fatalf("document location type: %T", loc)
	}
	if _, ok := mediaLocation(nil); ok {
		t.Error("nil media should have no location")
	}
}

func TestBgHex(t *testing.T) {
	if bgHex("white") != "white" || bgHex("black") != "black" {
		t.Error("named backgrounds")
	}
	if bgHex("transparent") != "0x00000000@0" {
		t.Error("transparent background")
	}
}

func TestFfmpegWebpArgs(t *testing.T) {
	s := convSettings{emoji: "🙂", size: 512, quality: 90, bg: "transparent"}
	args := ffmpegWebpArgs("/tmp/in.jpg", "/tmp/out.webp", s)
	joined := strings.Join(args, " ")
	for _, want := range []string{"libwebp", "-q:v 90", "scale=512:512", "format=yuva420p", "pad=512:512"} {
		if !strings.Contains(joined, want) {
			t.Errorf("webp args missing %q: %s", want, joined)
		}
	}
	s.bg = "white"
	args = ffmpegWebpArgs("/tmp/in.jpg", "/tmp/out.webp", s)
	if !strings.Contains(strings.Join(args, " "), "color=white") {
		t.Error("white background not used")
	}
}

func TestFfmpegToPicArgs(t *testing.T) {
	// JPG: flatten alpha onto white.
	args := ffmpegToPicArgs("/tmp/in.webp", "/tmp/out.jpg", false)
	j := strings.Join(args, " ")
	for _, want := range []string{"color=white", "scale2ref", "overlay", "-q:v 2"} {
		if !strings.Contains(j, want) {
			t.Errorf("jpg args missing %q: %s", want, j)
		}
	}
	// PNG flattened.
	args = ffmpegToPicArgs("/tmp/in.webp", "/tmp/out.png", false)
	if !strings.Contains(strings.Join(args, " "), "scale2ref") {
		t.Error("flattened png should composite onto white")
	}
	// PNG transparent: plain copy.
	args = ffmpegToPicArgs("/tmp/in.webp", "/tmp/out.png", true)
	j = strings.Join(args, " ")
	if strings.Contains(j, "scale2ref") || strings.Contains(j, "color=white") {
		t.Errorf("transparent png must not flatten: %s", j)
	}
	if !strings.Contains(j, "/tmp/out.png") {
		t.Error("output missing")
	}
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if got := loadConfig(path); got != (config{}) {
		t.Errorf("missing file should give defaults, got %+v", got)
	}
	cfg := config{DefaultPack: "my_pack"}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if got := loadConfig(path); got != cfg {
		t.Errorf("round trip = %+v, want %+v", got, cfg)
	}
	// mode 0600
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", st.Mode().Perm())
	}
	// corrupt file → defaults, no error
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(path); got != (config{}) {
		t.Errorf("corrupt file should give defaults, got %+v", got)
	}
}

func TestHelpText(t *testing.T) {
	// helpText only formats strings; make sure it does not panic with a
	// minimal context and contains the subcommands.
	ctx := pluginCtx()
	h := helpText(&ctx)
	for _, want := range []string{"sticker", "topng", "cancel", "batch"} {
		if !strings.Contains(h, want) {
			t.Errorf("help missing %q", want)
		}
	}
}

func TestRandomEmoji(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		e := randomEmoji()
		if e == "" {
			t.Fatal("empty emoji")
		}
		seen[e] = true
	}
	for _, e := range baseEmojis {
		_ = e
	}
	if len(seen) == 0 {
		t.Fatal("no emoji generated")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("abc", 5) != "abc" {
		t.Error("short string should be unchanged")
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate = %q", got)
	}
}
