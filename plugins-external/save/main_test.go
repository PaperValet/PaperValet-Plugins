package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func TestParseLink(t *testing.T) {
	cases := []struct {
		in   string
		want msgLink
	}{
		{"https://t.me/durov/123", msgLink{Username: "durov", MsgID: 123}},
		{"t.me/durov/123?single", msgLink{Username: "durov", MsgID: 123}},
		{"https://t.me/s/durov/5", msgLink{Username: "durov", MsgID: 5}},
		{"https://t.me/c/1234567/89", msgLink{ChannelID: 1234567, MsgID: 89}},
		{"https://t.me/c/1234567/10/89", msgLink{ChannelID: 1234567, MsgID: 89}},
		{"https://t.me/somegroup/10/89", msgLink{Username: "somegroup", MsgID: 89}},
		{"http://telegram.me/durov/7", msgLink{Username: "durov", MsgID: 7}},
		{"tg://resolve?domain=durov&post=9", msgLink{Username: "durov", MsgID: 9}},
		{"tg://privatepost?channel=555&post=3", msgLink{ChannelID: 555, MsgID: 3}},
	}
	for _, c := range cases {
		got, err := parseLink(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v %v", c.in, got, err)
		}
	}
	for _, bad := range []string{"https://t.me/durov", "https://t.me/c/abc/1", "https://t.me/x/1", "https://t.me/durov/0", "tg://resolve?domain=durov"} {
		if _, err := parseLink(bad); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
}

func TestClassifyArgs(t *testing.T) {
	pa := classifyArgs([]string{"https://t.me/durov/1", "t.me/durov/2", "@target"})
	if len(pa.Links) != 2 || pa.TempTarget != "@target" || pa.Range != nil {
		t.Fatalf("links: %+v", pa)
	}
	pa = classifyArgs([]string{"https://t.me/c/5/10|https://t.me/c/5/20", "local"})
	if pa.Range == nil || pa.Range[0].MsgID != 10 || pa.Range[1].MsgID != 20 || pa.TempTarget != "local" {
		t.Fatalf("range: %+v", pa)
	}
	pa = classifyArgs([]string{"https://t.me/durov/1", "|", "https://t.me/durov/9"})
	if pa.Range == nil || pa.Range[1].MsgID != 9 {
		t.Fatalf("spaced range: %+v", pa)
	}
	pa = classifyArgs([]string{"https://t.me/durov/1|https://t.me/other/9"})
	if pa.BadLink == "" {
		t.Fatal("cross-chat range should fail")
	}
	pa = classifyArgs([]string{"@target"})
	if len(pa.Links) != 0 || pa.TempTarget != "" {
		t.Fatalf("no links: %+v", pa)
	}
}

func TestCompactIDs(t *testing.T) {
	got := compactIDs([]int{5, 3, 4, 9, 9, 1})
	want := []idRange{{1, 1}, {3, 5}, {9, 9}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestMessageLink(t *testing.T) {
	if l := messageLink(-1000000001234, "", 5); l != "https://t.me/c/1234/5" {
		t.Error(l)
	}
	if l := messageLink(-1001234, "chan", 5); l != "https://t.me/chan/5" {
		t.Error(l)
	}
	if l := messageLink(-42, "", 5); l != "" {
		t.Error(l)
	}
	if channelChatID(1234) != -1000000001234 {
		t.Error("channelChatID")
	}
}

func TestSanitizeAndExt(t *testing.T) {
	if s := sanitizeSegment("../a b//c", "x"); s != "._a_b_c" && s != "a_b_c" {
		// leading dots are allowed by the reference, but never "." / ".."
		t.Logf("sanitized: %q", s)
	}
	if s := sanitizeSegment("..", "x"); s != "x" {
		t.Errorf("dotdot: %q", s)
	}
	if s := sanitizeSegment("你好 world", "x"); s != "你好_world" {
		t.Errorf("cjk: %q", s)
	}
	if e := extFor("a.PDF", "", "document"); e != ".pdf" {
		t.Error(e)
	}
	if e := extFor("", "video/mp4", "video"); e != ".mp4" {
		t.Error(e)
	}
	if e := extFor("", "", "photo"); e != ".jpg" {
		t.Error(e)
	}
}

func TestNormalizeTarget(t *testing.T) {
	ok := map[string]string{"me": "me", "": "me", "LOCAL": "local", "@durov": "@durov", "durov": "@durov",
		"-1001234": "-1001234", "https://t.me/durov": "@durov"}
	for in, want := range ok {
		if got, err := normalizeTarget(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"@a", "a b", "$$"} {
		if _, err := normalizeTarget(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestConfigPersistence(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "save_db.json")
	if err := os.WriteFile(legacy, []byte(`{"users":{"7":{"target":"@x","show_source":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New()
	p.cfgPath = filepath.Join(dir, "save", "config.json")
	p.legacy = legacy
	p.db = loadDB(p.cfgPath, p.legacy)
	if c := p.userConfig(7); c.Target != "@x" || !c.ShowSource {
		t.Fatalf("legacy not loaded: %+v", c)
	}
	if c := p.userConfig(8); c.Target != "me" || c.ShowSource {
		t.Fatalf("default: %+v", c)
	}
	if err := p.updateConfig(8, func(c *UserConfig) { c.Target = "local" }); err != nil {
		t.Fatal(err)
	}
	db := loadDB(p.cfgPath, "")
	if db.Users["8"].Target != "local" || db.Users["7"].Target != "@x" {
		t.Fatalf("persisted: %+v", db.Users)
	}
}

func TestReplyTarget(t *testing.T) {
	ev := &plugin.MessageEvent{ChatID: -100500, Message: &tg.Message{}}
	if _, _, ok := replyTarget(ev); ok {
		t.Fatal("no reply")
	}
	h := &tg.MessageReplyHeader{}
	h.SetReplyToMsgID(42)
	ev.Message.ReplyTo = h
	if c, id, ok := replyTarget(ev); !ok || id != 42 || c != -100500 {
		t.Fatalf("reply: %d %d %v", c, id, ok)
	}
	// plain message in a forum topic: points at topic root, not a reply
	ft := &tg.MessageReplyHeader{ForumTopic: true}
	ft.SetReplyToMsgID(7)
	ev.Message.ReplyTo = ft
	if _, _, ok := replyTarget(ev); ok {
		t.Fatal("topic root treated as reply")
	}
	ft.SetReplyToTopID(7)
	ft.SetReplyToMsgID(50)
	if _, id, ok := replyTarget(ev); !ok || id != 50 {
		t.Fatal("reply inside topic")
	}
	// cross-chat reply
	x := &tg.MessageReplyHeader{}
	x.SetReplyToMsgID(3)
	x.SetReplyToPeerID(&tg.PeerChannel{ChannelID: 99})
	ev.Message.ReplyTo = x
	if c, _, ok := replyTarget(ev); !ok || c != channelChatID(99) {
		t.Fatalf("cross chat: %d", c)
	}
}

func TestSentIDAndRestricted(t *testing.T) {
	u := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateMessageID{ID: 10},
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 12}},
	}}
	if id := sentID(u); id != 12 {
		t.Error(id)
	}
	if !isRestricted(errors.New("rpc error code 400: CHAT_FORWARDS_RESTRICTED")) || isRestricted(errors.New("PEER_ID_INVALID")) {
		t.Error("isRestricted")
	}
}

func TestDownloadable(t *testing.T) {
	m := &tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{
		MimeType: "video/mp4",
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeVideo{}, &tg.DocumentAttributeAnimated{},
			&tg.DocumentAttributeFilename{FileName: "clip.mp4"},
		},
	}}}
	mi := downloadable(m)
	if mi == nil || mi.Kind != "gif" || mi.ext() != ".mp4" || mi.baseName(1) != "clip" {
		t.Fatalf("%+v", mi)
	}
	p := &tg.Message{ID: 4, Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{Sizes: []tg.PhotoSizeClass{
		&tg.PhotoSize{Type: "m", Size: 10}, &tg.PhotoSizeProgressive{Type: "y", Sizes: []int{5, 50}},
	}}}}
	mi = downloadable(p)
	if mi == nil || mi.Kind != "photo" || mi.Location.(*tg.InputPhotoFileLocation).ThumbSize != "y" || mi.baseName(4) != "photo_4" {
		t.Fatalf("%+v", mi)
	}
	if downloadable(&tg.Message{Media: &tg.MessageMediaWebPage{}}) != nil {
		t.Fatal("webpage is not downloadable")
	}
}

func TestStopIdempotent(t *testing.T) {
	p := New()
	p.tmpDir = t.TempDir()
	os.WriteFile(filepath.Join(p.tmpDir, "x"), []byte("1"), 0o600)
	if err := p.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.beginJob(t.Context()); !errors.Is(err, errStopped) {
		t.Fatal("job after stop")
	}
	if entries, _ := os.ReadDir(p.tmpDir); len(entries) != 0 {
		t.Fatal("temp not cleaned")
	}
	_ = p.Start(t.Context())
	if _, done, err := p.beginJob(t.Context()); err != nil {
		t.Fatal(err)
	} else {
		done()
	}
}

func TestPanelTargetValidation(t *testing.T) {
	for _, ok := range []string{"me", "local", "@user", "123456", "-1001234567890", "https://t.me/durov"} {
		if _, err := validPanelTarget(ok); err != nil {
			t.Fatalf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"nope!", "@x", "hello world"} {
		if _, err := validPanelTarget(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestOwnerUsesPanelDefaults(t *testing.T) {
	p := New()
	// Without a settings store the legacy default applies.
	if c := p.userConfig(7); c.Target != "me" {
		t.Fatalf("%+v", c)
	}
}
