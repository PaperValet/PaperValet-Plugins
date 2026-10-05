package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		in   []string
		want yvluArgs
	}{
		{nil, yvluArgs{count: 1}},
		{[]string{""}, yvluArgs{count: 1}},
		{[]string{"3"}, yvluArgs{count: 3}},
		{[]string{"r"}, yvluArgs{count: 1, withReply: true}},
		{[]string{"r", "2"}, yvluArgs{count: 2, withReply: true}},
		{[]string{"r", "webp"}, yvluArgs{count: 1, withReply: true, format: "webp"}},
		{[]string{"r", "image", "3"}, yvluArgs{count: 3, withReply: true, format: "image"}},
		{[]string{"r", "png", "4"}, yvluArgs{count: 4, withReply: true, format: "image"}},
		{[]string{"s"}, yvluArgs{count: 1, save: true}},
		{[]string{"webp"}, yvluArgs{count: 1, format: "webp"}},
		{[]string{"image"}, yvluArgs{count: 1, format: "image"}},
		{[]string{"png"}, yvluArgs{count: 1, format: "image"}},
		{[]string{"stories"}, yvluArgs{count: 1, format: "stories"}},
		{[]string{"stories", "5"}, yvluArgs{count: 5, format: "stories"}},
		{[]string{"WEBP", "2"}, yvluArgs{count: 2, format: "webp"}},
		{[]string{"xyz"}, yvluArgs{count: 1, help: true}},
		{[]string{"0"}, yvluArgs{count: 1}},
		{[]string{"99"}, yvluArgs{count: 99}}, // over-max reported by the handler
	}
	for _, c := range cases {
		if got := parseArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseArgs(%v) = %+v want %+v", c.in, got, c.want)
		}
	}
}

func TestNormalizeFormat(t *testing.T) {
	for in, want := range map[string]string{
		"webp": "webp", "WEBP": "webp",
		"image": "image", "png": "image", "PNG": "image",
		"stories": "stories", "": "", "gif": "", "jpg": "",
	} {
		if got := normalizeFormat(in); got != want {
			t.Errorf("normalizeFormat(%q)=%q want %q", in, got, want)
		}
	}
}

func TestValidateStickerSet(t *testing.T) {
	if s, err := validateStickerSet(" my_pack_01 "); err != nil || s != "my_pack_01" {
		t.Fatalf("trim+valid failed: %q %v", s, err)
	}
	if s, err := validateStickerSet(""); err != nil || s != "" {
		t.Fatalf("empty must be allowed: %q %v", s, err)
	}
	for _, bad := range []string{"my pack", "my-pack", "中文", strings.Repeat("a", 65), "a.b"} {
		if _, err := validateStickerSet(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestImageKind(t *testing.T) {
	webp := append([]byte("RIFF"), make([]byte, 4)...)
	webp = append(webp, []byte("WEBP")...)
	webp = append(webp, 1, 2, 3, 4)
	if k, err := imageKind(webp); err != nil || k != "webp" {
		t.Fatalf("webp: %q %v", k, err)
	}
	if k, err := imageKind([]byte("\x89PNG\r\n\x1a\nxxxx")); err != nil || k != "png" {
		t.Fatalf("png: %q %v", k, err)
	}
	if k, err := imageKind([]byte{0x1a, 0x45, 0xdf, 0xa3, 0}); err != nil || k != "webm" {
		t.Fatalf("webm: %q %v", k, err)
	}
	if _, err := imageKind([]byte("<html>oops</html>")); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := imageKind(nil); err == nil {
		t.Fatal("empty accepted")
	}
}

func TestConvertEntities(t *testing.T) {
	ents := []tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 0, Length: 2},
		&tg.MessageEntityItalic{Offset: 3, Length: 1},
		&tg.MessageEntityStrike{Offset: 5, Length: 2},
		&tg.MessageEntityUnderline{Offset: 5, Length: 2},
		&tg.MessageEntityCode{Offset: 8, Length: 3},
		&tg.MessageEntityPre{Offset: 12, Length: 4, Language: "go"},
		&tg.MessageEntityCustomEmoji{Offset: 20, Length: 2, DocumentID: 123456789},
		&tg.MessageEntityTextURL{Offset: 22, Length: 3, URL: "https://x"},
		&tg.MessageEntityMentionName{Offset: 26, Length: 2, UserID: 42},
		&tg.MessageEntitySpoiler{Offset: 28, Length: 1},
		&tg.MessageEntityBlockquote{Offset: 29, Length: 3},
		&tg.MessageEntityUnknown{Offset: 33, Length: 1}, // unmapped: dropped
		&tg.MessageEntityBold{Offset: 40, Length: 0},    // zero length: dropped
	}
	got := convertEntities(ents)
	if len(got) != 11 {
		t.Fatalf("got %d entities: %+v", len(got), got)
	}
	type flat struct {
		o, l int
		typ  string
		lang string
		ceid string
		url  string
		uid  int64
	}
	want := []flat{
		{0, 2, "bold", "", "", "", 0},
		{3, 1, "italic", "", "", "", 0},
		{5, 2, "strikethrough", "", "", "", 0},
		{5, 2, "underline", "", "", "", 0},
		{8, 3, "code", "", "", "", 0},
		{12, 4, "pre", "go", "", "", 0},
		{20, 2, "custom_emoji", "", "123456789", "", 0},
		{22, 3, "text_link", "", "", "https://x", 0},
		{26, 2, "text_mention", "", "", "", 42},
		{28, 1, "spoiler", "", "", "", 0},
		{29, 3, "blockquote", "", "", "", 0},
	}
	for i, w := range want {
		g := got[i]
		if g.Offset != w.o || g.Length != w.l || g.Type != w.typ ||
			g.Language != w.lang || g.CustomEmojiID != w.ceid || g.URL != w.url ||
			(g.User != nil && g.User.ID != w.uid) || (g.User == nil && w.uid != 0) {
			t.Errorf("entity %d: got %+v want %+v", i, g, w)
		}
	}
}

func TestBuildRequest(t *testing.T) {
	items := []quoteMessage{{Text: "hi"}}
	// default / webp
	req := buildRequest("", items)
	if req.Type != "quote" || req.Format != "webp" || req.Width != 512 || req.Height != 768 {
		t.Fatalf("default: %+v", req)
	}
	req = buildRequest("webp", items)
	if req.Type != "quote" || req.Format != "webp" {
		t.Fatalf("webp: %+v", req)
	}
	// image
	req = buildRequest("image", items)
	if req.Type != "image" || req.Format != "png" || req.Width != 512 {
		t.Fatalf("image: %+v", req)
	}
	// stories
	req = buildRequest("stories", items)
	if req.Type != "stories" || req.Format != "png" || req.Width != 360 || req.Height != 640 {
		t.Fatalf("stories: %+v", req)
	}
}

func TestQuoteJSONShape(t *testing.T) {
	// from omits empty fields; entities use the quote-api names.
	u := &quoteUser{ID: 7, Name: "Ann"}
	m := quoteMessage{
		From: u, Text: "hi",
		Entities: []quoteEntity{{Offset: 0, Length: 2, Type: "bold"}},
	}
	req := buildRequest("", []quoteMessage{m})
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{
		`"type":"quote"`, `"format":"webp"`, `"backgroundColor":"#1b1429"`,
		`"width":512`, `"height":768`, `"scale":2`, `"emojiBrand":"apple"`,
		`"messages":[{`, `"from":{"id":7,"name":"Ann"}`, `"text":"hi"`,
		`"entities":[{"offset":0,"length":2,"type":"bold"}]`, `"avatar":false`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("json missing %s in %s", want, s)
		}
	}
	if strings.Contains(s, "photo") && !strings.Contains(s, `"photo":{`) {
		t.Errorf("unexpected photo field: %s", s)
	}
}

func TestHashString(t *testing.T) {
	// matches Java String.hashCode for ASCII input
	want := int32(0)
	for _, c := range []byte("yvlu") {
		want = want<<5 - want + int32(c)
	}
	if got := hashString("yvlu"); got != want {
		t.Fatalf("hashString=%d want %d", got, want)
	}
	if hashString("") != 0 {
		t.Fatal("empty hash must be 0")
	}
}

func TestFwdLabel(t *testing.T) {
	if fwdLabel(&tg.MessageFwdHeader{FromName: "X", PostAuthor: "Y"}) != "X" {
		t.Fatal("FromName should win")
	}
	if fwdLabel(&tg.MessageFwdHeader{PostAuthor: "Y"}) != "Y" {
		t.Fatal("PostAuthor fallback failed")
	}
	if fwdLabel(&tg.MessageFwdHeader{}) != "" {
		t.Fatal("empty header should give empty label")
	}
}

func TestFillAdvancedVoiceWaveform(t *testing.T) {
	msg := &tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeAudio{Voice: true, Duration: 7, Waveform: []byte{31 << 3, 255, 5 << 3}},
		},
	}}}
	var item quoteMessage
	fillAdvanced(&item, msg)
	if item.Voice == nil {
		t.Fatal("voice missing")
	}
	if item.Voice.Duration != 7 {
		t.Fatalf("duration=%d", item.Voice.Duration)
	}
	for i, w := range item.Voice.Waveform {
		if w < 0 || w > 31 {
			t.Fatalf("waveform[%d]=%d out of range", i, w)
		}
	}
	if item.Voice.Waveform[0] != 31 || item.Voice.Waveform[1] != 31 || item.Voice.Waveform[2] != 5 {
		t.Fatalf("waveform=%v", item.Voice.Waveform)
	}
}

func TestFillAdvancedKinds(t *testing.T) {
	audioMsg := &tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeAudio{Title: "Song", Performer: "Artist", Duration: 100},
		},
	}}}
	var item quoteMessage
	fillAdvanced(&item, audioMsg)
	if item.Audio == nil || item.Audio.Title != "Song" || item.Audio.Performer != "Artist" {
		t.Fatalf("audio: %+v", item.Audio)
	}

	gifMsg := &tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeVideo{Duration: 3},
			&tg.DocumentAttributeAnimated{},
		},
	}}}
	item = quoteMessage{}
	fillAdvanced(&item, gifMsg)
	if item.MediaType != "gif" || item.MediaDuration != 3 {
		t.Fatalf("gif: %+v", item)
	}

	docMsg := &tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: "file.pdf"},
		},
	}}}
	item = quoteMessage{}
	fillAdvanced(&item, docMsg)
	if item.Document == nil || item.Document.FileName != "file.pdf" {
		t.Fatalf("document: %+v", item.Document)
	}

	plain := &tg.Message{}
	item = quoteMessage{}
	fillAdvanced(&item, plain)
	if item.Voice != nil || item.Audio != nil || item.Document != nil || item.MediaType != "" {
		t.Fatalf("plain message produced fields: %+v", item)
	}
}

func TestMediaDataURIMagicHelpers(t *testing.T) {
	if !isGzip([]byte{0x1f, 0x8b, 0x00}) {
		t.Fatal("gzip magic failed")
	}
	if isGzip([]byte{0x00, 0x01}) {
		t.Fatal("gzip false positive")
	}
	if !isMp4([]byte{0, 0, 0, 0, 'f', 't', 'y', 'p'}) {
		t.Fatal("mp4 magic failed")
	}
	if isMp4([]byte{0, 0, 0, 0, 'f', 't', 'y', 'q'}) {
		t.Fatal("mp4 false positive")
	}
}

func TestTruncRunes(t *testing.T) {
	if got := truncRunes("你好世界", 2); got != "你好" {
		t.Fatalf("got %q", got)
	}
	if got := truncRunes("ab", 5); got != "ab" {
		t.Fatalf("got %q", got)
	}
}
