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
	"unicode/utf8"

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

func TestBridgeErrMsg(t *testing.T) {
	// the FATAL one-liner beats any stack frame
	stderr := "[quote-bridge] first run: downloading quote-api vendor + assets (33 files)…\n" +
		"[quote-bridge] assets ready\n" +
		"[quote-bridge] FATAL TypeError [ERR_INVALID_ARG_VALUE]: The argument 'filename' must be an absolute path. Received 'data/quote/vendor-src/generate.js'\n" +
		"    at createRequire (node:internal/modules/cjs/loader:2039:13)\n" +
		"    at main (file:///root/.pvtest/data/quote/bridge.mjs:166:25)\n"
	want := "[quote-bridge] FATAL TypeError [ERR_INVALID_ARG_VALUE]: The argument 'filename' must be an absolute path. Received 'data/quote/vendor-src/generate.js'"
	if got := bridgeErrMsg(stderr); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// no FATAL: fall back to the last non-empty line
	if got := bridgeErrMsg("warn\nError: boom\n    at foo (bar.js:1:1)\n\n"); got != "at foo (bar.js:1:1)" {
		t.Errorf("fallback got %q", got)
	}
	if got := bridgeErrMsg("   \n"); got != "" {
		t.Errorf("empty stderr got %q", got)
	}
	// over-long FATAL line is clipped on a rune boundary
	if got := bridgeErrMsg(fatalMark + " " + strings.Repeat("喵", 500)); len([]rune(got)) != 400 {
		t.Errorf("clip got %d runes, want 400", len([]rune(got)))
	}
}

func TestAbsDataDir(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// a relative DataDir (what the host returns) must resolve against this
	// process's cwd — otherwise the node child re-resolves it against its
	// own cwd and nests data/quote/data/quote.
	host := stubHost{dir: filepath.Join("data", "quote")}
	if got, want := absDataDir(host), filepath.Join(cwd, "data", "quote"); got != want {
		t.Errorf("absDataDir = %q, want %q", got, want)
	}
	// absolute values pass through untouched
	host.dir = "/var/lib/quote"
	if got := absDataDir(host); got != "/var/lib/quote" {
		t.Errorf("absolute pass-through = %q", got)
	}
}

type stubHost struct{ dir string }

func (h stubHost) DataDir(string) (string, error) { return h.dir, nil }

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
	if !strings.Contains(bridgeSource, "path.resolve(req.assetsDir") {
		t.Error("bridgeSource must absolutize the assets dir (a relative value nests)")
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
	ents := bridgeEntities(m, nil)
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
	if got := bridgeEntities(m2, nil); len(got) != 0 {
		t.Errorf("unknown entity should be dropped: %+v", got)
	}

	// custom emoji carry their id and pre-downloaded buffer
	m3 := &tg.Message{Message: "hi", Entities: []tg.MessageEntityClass{
		&tg.MessageEntityCustomEmoji{Offset: 0, Length: 2, DocumentID: 1234},
	}}
	got3 := bridgeEntities(m3, map[int64]string{1234: "aGk="})
	if len(got3) != 1 || got3[0].Type != "custom_emoji" || got3[0].CustomEmojiID != "1234" || got3[0].CustomEmojiBuffer != "aGk=" {
		t.Errorf("custom emoji entity = %+v", got3)
	}
	// without a buffer the id still rides along but the buffer stays empty
	got4 := bridgeEntities(m3, nil)
	if len(got4) != 1 || got4[0].CustomEmojiID != "1234" || got4[0].CustomEmojiBuffer != "" {
		t.Errorf("custom emoji entity without buffer = %+v", got4)
	}
}

func TestEmojiStatusOf(t *testing.T) {
	if got := emojiStatusOf(nil); got != 0 {
		t.Errorf("nil user = %d", got)
	}
	if got := emojiStatusOf(&tg.User{ID: 1}); got != 0 {
		t.Errorf("no status = %d", got)
	}
	u := &tg.User{ID: 1, EmojiStatus: &tg.EmojiStatus{DocumentID: 42}}
	if got := emojiStatusOf(u); got != 42 {
		t.Errorf("status = %d", got)
	}
	bufs := map[int64]string{42: "aGk="}
	pay := emojiStatusPayload(u, bufs)
	if pay == nil || pay.CustomEmojiID != "42" || pay.CustomEmojiBuffer != "aGk=" {
		t.Fatalf("payload = %+v", pay)
	}
	// a status whose image is missing must not render a placeholder glyph
	if pay := emojiStatusPayload(u, nil); pay != nil {
		t.Errorf("missing buffer should drop the status: %+v", pay)
	}
	if pay := emojiStatusPayload(&tg.User{ID: 2}, bufs); pay != nil {
		t.Errorf("statusless user should have no payload: %+v", pay)
	}
}

func TestUserMapOf(t *testing.T) {
	m := userMapOf([]tg.UserClass{&tg.User{ID: 7, FirstName: "A"}, nil, &tg.UserEmpty{}})
	if len(m) != 1 || m[7] == nil {
		t.Fatalf("map = %+v", m)
	}
	if userName(m[7]) != "A" {
		t.Errorf("name = %q", userName(m[7]))
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

func TestChannelNames(t *testing.T) {
	chats := []tg.ChatClass{
		&tg.Channel{ID: 100, Title: "News"},
		&tg.Chat{ID: 200, Title: "Old Group"},
		&tg.ChannelForbidden{ID: 300},
	}
	m := channelNames(chats)
	if m[100] != "News" || m[200] != "Old Group" {
		t.Errorf("channelNames = %v", m)
	}
	if _, ok := m[300]; ok {
		t.Error("forbidden channel should not be named")
	}
}

func TestLimitedWriter(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitedWriter{w: &buf, max: 10}
	if _, err := lw.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := lw.Write([]byte("67890")); err != nil {
		t.Fatal(err)
	}
	if _, err := lw.Write([]byte("x")); err != errTooLarge {
		t.Fatalf("want errTooLarge, got %v", err)
	}
	if buf.String() != "1234567890" {
		t.Errorf("buf = %q", buf.String())
	}
}

func TestErrBufferRuneSafe(t *testing.T) {
	var e errBuffer
	// >8192 bytes ending mid-rune: the kept window must start on a rune
	// boundary so String() is valid UTF-8.
	pattern := []byte("你好世界") // 12 bytes
	for written := 0; written < 8192+24; written += len(pattern) {
		e.Write(pattern)
	}
	e.Write([]byte{0xe4, 0xbd}) // partial 你
	if !utf8.ValidString(e.String()) {
		t.Error("errBuffer output is not valid UTF-8")
	}
	if len(e.b) > 8192 {
		t.Errorf("kept %d bytes, want <= 8192", len(e.b))
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
	// waveform decode: bytes are 5-bit amplitudes shifted left by 3
	decoded := make([]int, len(info.waveform))
	for i, b := range info.waveform {
		decoded[i] = min(31, int(b)>>3)
	}
	if decoded[0] != 0 || decoded[2] != 25 || decoded[3] != 3 || decoded[4] != 12 {
		t.Errorf("decode = %v", decoded)
	}
	// every value stays in 0..31 regardless of the byte
	for _, v := range decoded {
		if v < 0 || v > 31 {
			t.Fatalf("waveform value %d out of range", v)
		}
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

	// regression: a relative assets dir used to be re-resolved against the
	// node child's own cwd (nesting data/quote/data/quote) and crash
	// createRequire with a relative vendor script path. It must render the
	// same assets dir as the absolute run above.
	t.Chdir(filepath.Dir(dir))
	res, err = runBridge(t.Context(), nil, &bridgeRequest{
		Messages:        []json.RawMessage{raw},
		Type:            "quote",
		Format:          "webp",
		Scale:           2,
		BackgroundColor: defaultBackground,
		EmojiBrand:      "apple",
		AssetsDir:       "assets",
	})
	if err != nil {
		t.Fatalf("runBridge with relative assets dir: %v", err)
	}
	if len(res.Data) < 1000 {
		t.Fatalf("relative-dir render too small: %d bytes", len(res.Data))
	}
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
