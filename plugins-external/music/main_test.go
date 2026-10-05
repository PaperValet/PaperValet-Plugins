package main

import (
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func en(_, e string) string { return e }

func TestRealReply(t *testing.T) {
	ev := &plugin.MessageEvent{Message: &tg.Message{}}
	if got := realReply(ev); got != 0 {
		t.Fatal("no reply header")
	}
	if got := realReply(nil); got != 0 {
		t.Fatal("nil event")
	}
	h := &tg.MessageReplyHeader{}
	h.SetReplyToMsgID(42)
	ev.Message.ReplyTo = h
	if got := realReply(ev); got != 42 {
		t.Fatalf("plain reply: %d", got)
	}
	// Plain message inside a forum topic: header points at the topic root,
	// the song must not be attached to it.
	ft := &tg.MessageReplyHeader{ForumTopic: true}
	ft.SetReplyToMsgID(7)
	ev.Message.ReplyTo = ft
	if got := realReply(ev); got != 0 {
		t.Fatalf("topic root treated as reply: %d", got)
	}
	ft.SetReplyToTopID(7)
	ft.SetReplyToMsgID(50)
	if got := realReply(ev); got != 50 {
		t.Fatalf("real reply in topic: %d", got)
	}
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		in   string
		want request
	}{
		{"晴天 周杰伦", request{Query: "晴天 周杰伦"}},
		{"qq 晴天", request{Plat: "qq", Query: "晴天"}},
		{"qq", request{Query: "qq"}}, // a lone platform word is a song name
		{"search @vmomo 晴天", request{Search: true, Source: sourceByKey("vmomo"), Query: "晴天"}},
		{"@Music163DownBot kugou 晴天", request{Source: sourceByKey("down"), Plat: "kugou", Query: "晴天"}},
		{"3", request{Pick: 3, Query: "3"}},
		{"search 3", request{Search: true, Query: "3"}},
		{"186016", request{Query: "186016"}},
		{"@nobody hello", request{Query: "@nobody hello"}},
	}
	for _, c := range cases {
		got := parseArgs(strings.Fields(c.in))
		if got != c.want {
			t.Errorf("%q: got %+v want %+v", c.in, got, c.want)
		}
	}
}

func TestRoute(t *testing.T) {
	down, vm := sourceByKey("down"), sourceByKey("vmomo")
	if s, tok, ok := route(request{Plat: "qq", Query: "x"}, vm); !ok || s != down || tok != "qq" {
		t.Fatalf("fallback: %v %q %v", s, tok, ok)
	}
	if s, _, ok := route(request{Query: "x"}, vm); !ok || s != vm {
		t.Fatal("default")
	}
	if _, _, ok := route(request{Source: vm, Plat: "qq", Query: "x"}, down); ok {
		t.Fatal("vmomo has no platforms")
	}
}

func TestCommands(t *testing.T) {
	down, n163, v1 := sourceByKey("down"), sourceByKey("163"), sourceByKey("v1")
	if got := down.get("晴天", "qqmusic", "lossless"); got != "/music 晴天 qqmusic lossless" {
		t.Error(got)
	}
	if got := down.get("晴天", "", ""); got != "/music 晴天" {
		t.Error(got)
	}
	if got := n163.get("https://music.163.com/#/song?id=186016", "", ""); got != "/music 186016" {
		t.Error(got)
	}
	if got := n163.get("晴天", "", ""); got != "/search 晴天" {
		t.Error(got)
	}
	if got := v1.get("晴天", "kugou", ""); got != "/kugou 晴天" {
		t.Error(got)
	}
}

func TestListTitles(t *testing.T) {
	vmomo := "🎯 🤩 ad line 5000万U\n\n1️⃣ 晴天(深情版) - Lucky小爱\n2️⃣ 晴天 - 周杰伦\n🔟 晴天 (Live) - 周杰伦\n\n🎗[ 晴天 ] Page : 1"
	if got := listTitles(vmomo); len(got) != 3 || got[1] != "晴天 - 周杰伦" {
		t.Fatalf("%q", got)
	}
	down := "🎵 网易云音乐 搜索结果\n第 1/6 页\n\n1. 「晴天」 - 周杰伦\n2. 「晴天」 - 蓝心羽"
	if got := listTitles(down); len(got) != 2 || got[0] != "「晴天」 - 周杰伦" {
		t.Fatalf("%q", got)
	}
}

func TestPickButtonsAndAudio(t *testing.T) {
	m := &tg.Message{ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonCallback{Data: []byte("play1")}, &tg.KeyboardButtonURL{URL: "https://ad"}}},
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonCallback{Data: []byte("pageTo2")}, &tg.KeyboardButtonCallback{Data: []byte("play2")}}},
	}}}
	if b := pickButtons(m, "play"); len(b) != 2 || string(b[1].Data) != "play2" {
		t.Fatalf("%v", b)
	}
	vk := &tg.Message{ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonCallback{Data: []byte("af:+:1")}}}}}}
	if len(pickButtons(vk, "a:")) != 0 {
		t.Fatal("af: is not a pick button")
	}
	voice := &tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{MimeType: "audio/ogg", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{Voice: true}}}}}
	if d, _ := audioDoc(voice); d != nil {
		t.Fatal("voice is not a song")
	}
	song := &tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{MimeType: "audio/x-flac", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{Title: "晴天", Performer: "周杰伦"}}}}}
	d, au := audioDoc(song)
	if d == nil || captionFor(au) != "🎵 **晴天** - 周杰伦" {
		t.Fatal(captionFor(au))
	}
}

func TestIsFailure(t *testing.T) {
	for _, s := range []string{"获取歌曲下载链接失败", "This track is currently unavailable", "未找到相关歌曲或发生错误[0]"} {
		if !isFailure(s) {
			t.Error(s)
		}
	}
	if isFailure("正在下载…") {
		t.Error("progress")
	}
}

func TestRenderList(t *testing.T) {
	l := &lastList{src: sourceByKey("vmomo"), titles: []string{"a"}, buttons: make([]*tg.KeyboardButtonCallback, 2)}
	out := renderList(en, l)
	if !strings.Contains(out, "1. a") || !strings.Contains(out, "2. Track 2") {
		t.Fatal(out)
	}
}

func TestPlatformChoices(t *testing.T) {
	if got := platformChoices(); len(got) != len(platformNames)+1 || got[1].Label == "" {
		t.Fatal(got)
	}
	for _, n := range platformNames {
		if _, ok := platformLabels[n]; !ok {
			t.Error("no label for", n)
		}
		if _, ok := platformAliases[n]; !ok {
			t.Error("name not typeable:", n)
		}
	}
}
