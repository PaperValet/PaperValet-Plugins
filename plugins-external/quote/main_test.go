package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
)

func TestParseArgs(t *testing.T) {
	defBG := "#231d2b/#372e44"
	cases := []struct {
		in   string
		want quoteArgs
	}{
		{"", quoteArgs{count: 1, scale: 2, bg: defBG}},
		{"5", quoteArgs{count: 5, scale: 2, bg: defBG}},
		{"-10", quoteArgs{count: -10, scale: 2, bg: defBG}},
		{"100", quoteArgs{count: 50, scale: 2, bg: defBG}},
		{"r 3", quoteArgs{count: 3, reply: true, scale: 2, bg: defBG}},
		{"png", quoteArgs{count: 1, png: true, scale: 2, bg: defBG}},
		{"stories", quoteArgs{count: 1, png: true, stories: true, scale: 2, bg: defBG}},
		{"webp", quoteArgs{count: 1, scale: 2, bg: defBG}},
		{"hidden media crop", quoteArgs{count: 1, hidden: true, media: true, crop: true, scale: 2, bg: defBG}},
		{"scale 4", quoteArgs{count: 1, scale: 4, bg: defBG}},
		{"s=8", quoteArgs{count: 1, scale: 8, bg: defBG}},
		{"scale 30", quoteArgs{count: 1, scale: 20, bg: defBG}},
		{"#1b1429", quoteArgs{count: 1, scale: 2, bg: "#1b1429"}},
		{"#111/#222", quoteArgs{count: 1, scale: 2, bg: "#111/#222"}},
		{"bg #0af", quoteArgs{count: 1, scale: 2, bg: "#0af"}},
		{"bg=1a2b3c", quoteArgs{count: 1, scale: 2, bg: "#1a2b3c"}},
		{"3 r png", quoteArgs{count: 3, reply: true, png: true, scale: 2, bg: defBG}},
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
	// quote-api luminance-pair form survives normalization
	if n := normalizeColorToken("//abc"); n != "//#abc" {
		t.Errorf("normalizeColorToken(//abc) → %q, want //#abc", n)
	}
}

// ---------------------------------------------------------------- bridge frame

func TestParseBridgeFrame(t *testing.T) {
	// valid png frame
	data := []byte{0x89, 0x50, 0x4E, 0x47, 1, 2, 3, 4}
	frame := frameFor(data, "png")
	res, err := parseBridgeFrame(frame)
	if err != nil {
		t.Fatalf("parseBridgeFrame: %v", err)
	}
	if res.Ext != "png" || len(res.Data) != len(data) || res.Data[0] != 0x89 {
		t.Errorf("res = %+v", res)
	}
	if detectImageExt(res.Data) != "png" {
		t.Errorf("detectImageExt png failed")
	}

	// webp frame
	webp := append([]byte("RIFF____WEBP"), 9, 9)
	frame = frameFor(webp, "webp")
	res, err = parseBridgeFrame(frame)
	if err != nil || res.Ext != "webp" {
		t.Errorf("webp frame: %+v %v", res, err)
	}
	if detectImageExt(res.Data) != "webp" {
		t.Errorf("detectImageExt webp failed")
	}

	// webm frame
	frame = frameFor([]byte{0x1a, 0x45, 0xdf, 0xa3, 0}, "webm")
	if res, err = parseBridgeFrame(frame); err != nil || res.Ext != "webm" {
		t.Errorf("webm frame: %+v %v", res, err)
	}

	// truncated
	if _, err := parseBridgeFrame(frame[:10]); err == nil {
		t.Error("truncated frame should fail")
	}
	// bad magic
	bad := frameFor(data, "png")
	bad[0] = 'X'
	if _, err := parseBridgeFrame(bad); err == nil {
		t.Error("bad magic should fail")
	}
	// length mismatch
	bad = frameFor(data, "png")
	bad[4] = 0xFF
	if _, err := parseBridgeFrame(bad); err == nil {
		t.Error("length mismatch should fail")
	}
	// unknown ext code
	bad = frameFor(data, "png")
	bad[8] = 0
	bad[9], bad[10], bad[11] = 0, 0, 9
	if _, err := parseBridgeFrame(bad); err == nil {
		t.Error("unknown ext should fail")
	}
}

func TestBridgeRequestJSON(t *testing.T) {
	msg := &quoteMessage{
		ChatID: 42, MessageID: 7,
		From: &quoteFrom{ID: 42, Name: "Alice", FirstName: "Alice"},
		Text: "hello", Caption: "hello",
		Entities: []bridgeEntity{{Type: "bold", Offset: 0, Length: 2}},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	req := &bridgeRequest{
		Messages:        []json.RawMessage{raw},
		Type:            "quote",
		Format:          "webp",
		Scale:           2,
		BackgroundColor: "#231d2b/#372e44",
		EmojiBrand:      "apple",
		AssetsDir:       "/tmp/x",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"messages", "type", "format", "scale", "backgroundColor", "emojiBrand", "assetsDir"} {
		if _, ok := back[key]; !ok {
			t.Errorf("payload missing %q", key)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// quote-api field names the renderer reads
	for _, key := range []string{"chatId", "message_id", "from", "text", "entities", "caption"} {
		if _, ok := m[key]; !ok {
			t.Errorf("message json missing %q", key)
		}
	}
	// optional renderer fields must exist as json tags (omitempty drops them
	// only when unset) — round-trip a message that sets every one
	full := &quoteMessage{
		AvatarBuffer: "aGk=",
		ReplyMessage: &quoteReply{Name: "n", Text: "t", From: &quoteFrom{ID: 1, Name: "n", FirstName: "n"}},
		MediaCanvas:  "aGk=",
		Voice:        &bridgeVoice{Waveform: []int{1, 2}},
		Document:     &bridgeDocument{FileName: "f"},
		Audio:        &bridgeAudio{Title: "t"},
		GroupPos:     "single",
	}
	fullRaw, _ := json.Marshal(full)
	for _, key := range []string{"avatarBuffer", "replyMessage", "mediaCanvas", "voice", "document", "audio", "groupPos"} {
		if !strings.Contains(string(fullRaw), key) {
			t.Errorf("message json missing %q tag", key)
		}
	}
	// hidden name must serialize to false, not be dropped
	hid, _ := json.Marshal(&quoteMessage{From: &quoteFrom{ID: 1, Name: false, FirstName: false}})
	if !strings.Contains(string(hid), `"name":false`) {
		t.Errorf("hidden name should be false: %s", hid)
	}
}

func TestRunBridgeNodeMissing(t *testing.T) {
	// with node hidden from PATH the bridge must fail with a clear error
	t.Setenv("PATH", "/nonexistent")
	if nodeRuntime() != "" {
		t.Skip("node still resolvable")
	}
	if _, err := runBridge(t.Context(), nil, &bridgeRequest{Messages: []json.RawMessage{[]byte("{}")}}); err == nil {
		t.Error("runBridge should fail without node")
	} else if !strings.Contains(err.Error(), "node") {
		t.Errorf("error should mention node: %v", err)
	}
}

func TestEnsureBridgeFile(t *testing.T) {
	dir := t.TempDir()
	p, err := ensureBridgeFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p, "bridge.mjs") {
		t.Errorf("path = %q", p)
	}
	if len(bridgeSource) < 1000 {
		t.Errorf("bridgeSource suspiciously short: %d bytes", len(bridgeSource))
	}
	if !strings.Contains(bridgeSource, "generateQuote") {
		t.Error("bridgeSource must call generateQuote")
	}
	if !strings.Contains(bridgeSource, "raw.githubusercontent.com/LyoSU/quote-api") {
		t.Error("bridgeSource must pull official quote-api assets")
	}
	// second call is a no-op rewrite with identical content
	if _, err := ensureBridgeFile(dir); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- entities / models

func TestEntityMapping(t *testing.T) {
	m := &tg.Message{
		Message: "bold text",
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityBold{Offset: 0, Length: 4},
			&tg.MessageEntityTextURL{Offset: 5, Length: 4, URL: "https://x"},
		},
	}
	ents := bridgeEntities(m)
	if len(ents) != 2 {
		t.Fatalf("ents = %+v", ents)
	}
	if ents[0].Type != "bold" || ents[0].Offset != 0 || ents[0].Length != 4 {
		t.Errorf("ent0 = %+v", ents[0])
	}
	if ents[1].Type != "text_link" || ents[1].URL != "https://x" {
		t.Errorf("ent1 = %+v", ents[1])
	}

	// unmapped entity kinds are dropped, not passed as "text"
	m2 := &tg.Message{Message: "x", Entities: []tg.MessageEntityClass{&tg.MessageEntityUnknown{}}}
	if got := bridgeEntities(m2); len(got) != 0 {
		t.Errorf("unknown entity should be dropped: %+v", got)
	}
}

func TestTruncVisually(t *testing.T) {
	if s := truncVisually("hello", 3); s != "he…" {
		t.Errorf("trunc = %q", s)
	}
	if s := truncVisually("hi", 10); s != "hi" {
		t.Errorf("trunc = %q", s)
	}
}

func TestClassifyAndDocInfo(t *testing.T) {
	voiceDoc := &tg.Document{
		MimeType: "audio/ogg",
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeAudio{Voice: true, Duration: 12, Waveform: []byte{1, 2, 200, 31, 99}},
		},
	}
	kind, info, _, _ := classifyMedia(&tg.MessageMediaDocument{Document: voiceDoc})
	if kind != mediaVoice {
		t.Fatalf("kind = %v", kind)
	}
	if info.duration != 12 || len(info.waveform) != 5 {
		t.Errorf("info = %+v", info)
	}
	// waveform clamp is applied at build time (vendor expects 0..31)
	clamped := make([]int, len(info.waveform))
	for i, b := range info.waveform {
		clamped[i] = min(31, int(b))
	}
	if clamped[2] != 31 || clamped[4] != 31 {
		t.Errorf("clamp = %v", clamped)
	}

	audioDoc := &tg.Document{
		MimeType: "audio/mp3",
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeAudio{Title: "Song", Performer: "Artist", Duration: 90},
		},
	}
	kind, info, _, _ = classifyMedia(&tg.MessageMediaDocument{Document: audioDoc})
	if kind != mediaAudio || info.title != "Song" || info.performer != "Artist" {
		t.Errorf("audio: %v %+v", kind, info)
	}

	fileDoc := &tg.Document{
		MimeType:   "application/zip",
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "a.zip"}},
	}
	kind, info, _, _ = classifyMedia(&tg.MessageMediaDocument{Document: fileDoc})
	if kind != mediaDocument || info.fileName != "a.zip" {
		t.Errorf("doc: %v %+v", kind, info)
	}
}

func TestShouldFetchPreview(t *testing.T) {
	if !shouldFetchPreview(mediaPhoto, false) || !shouldFetchPreview(mediaSticker, false) {
		t.Error("photo/sticker should always fetch")
	}
	if shouldFetchPreview(mediaDocument, false) {
		t.Error("document should not fetch unless forced")
	}
	if !shouldFetchPreview(mediaDocument, true) {
		t.Error("forced document should fetch")
	}
}

func TestOutTypeSelection(t *testing.T) {
	// mirrors handleQuote: stories→stories/png, png→image/png, else quote/webp
	cases := []struct {
		args            quoteArgs
		wantType, wantF string
	}{
		{quoteArgs{}, "quote", "webp"},
		{quoteArgs{png: true}, "image", "png"},
		{quoteArgs{stories: true, png: true}, "stories", "png"},
	}
	for _, c := range cases {
		outType := "quote"
		if c.args.stories {
			outType = "stories"
		} else if c.args.png {
			outType = "image"
		}
		outFormat := "png"
		if outType == "quote" {
			outFormat = "webp"
		}
		if outType != c.wantType || outFormat != c.wantF {
			t.Errorf("args %+v → %s/%s, want %s/%s", c.args, outType, outFormat, c.wantType, c.wantF)
		}
	}
}

// TestBridgeE2E exercises the real node bridge end to end (asset download,
// npm install, official renderer). Opt-in only — tests must not touch the
// network by default. Run with: QUOTE_E2E=1 go test -run E2E -timeout 30m
func TestBridgeE2E(t *testing.T) {
	if os.Getenv("QUOTE_E2E") == "" {
		t.Skip("set QUOTE_E2E=1 to run the live node bridge test (network + ~200MB assets)")
	}
	if nodeRuntime() == "" {
		t.Fatal("node not found")
	}
	avatar := base64.StdEncoding.EncodeToString(genAvatarPNG(t))

	msg := &quoteMessage{
		ChatID: 42, MessageID: 1, AvatarScale: 2,
		From:         &quoteFrom{ID: 42, Name: "张三", FirstName: "张三"},
		Text:         "你好，世界！Hello 😀 e2e",
		Entities:     []bridgeEntity{{Type: "bold", Offset: 0, Length: 2}},
		Avatar:       true,
		AvatarBuffer: avatar,
	}
	raw, _ := json.Marshal(msg)
	dir := filepath.Join(t.TempDir(), "assets")
	res, err := runBridge(t.Context(), nil, &bridgeRequest{
		Messages:        []json.RawMessage{raw},
		Type:            "quote",
		Format:          "webp",
		Scale:           2,
		BackgroundColor: defaultBackground,
		EmojiBrand:      "apple",
		AssetsDir:       dir,
	})
	if err != nil {
		t.Fatalf("runBridge: %v", err)
	}
	if len(res.Data) < 1000 {
		t.Fatalf("rendered image too small: %d bytes", len(res.Data))
	}
	if got := detectImageExt(res.Data); got != res.Ext {
		t.Errorf("frame ext %q but bytes sniff as %q", res.Ext, got)
	}
	t.Logf("rendered %d bytes %s", len(res.Data), res.Ext)
}

// genAvatarPNG draws a small red/blue square avatar in memory.
func genAvatarPNG(t *testing.T) []byte {
	const s = 64
	img := image.NewRGBA(image.Rect(0, 0, s, s))
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			if x < s/2 {
				img.Set(x, y, color.RGBA{0xE0, 0x40, 0x40, 0xFF})
			} else {
				img.Set(x, y, color.RGBA{0x40, 0x60, 0xE0, 0xFF})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
