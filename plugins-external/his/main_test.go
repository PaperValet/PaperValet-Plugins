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
	// which is not a reply.
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
		args   []string
		reply  bool
		target string
		count  int
		ok     bool
	}{
		{nil, true, "", 30, true},
		{nil, false, "", 30, false},
		{[]string{"5"}, true, "", 5, true},
		{[]string{"500"}, true, "", 100, true},
		{[]string{"5"}, false, "5", 30, true},
		{[]string{"-1001234"}, true, "-1001234", 30, true},
		{[]string{"@bob"}, false, "@bob", 30, true},
		{[]string{"@"}, false, "@", 30, false},
		{[]string{"bob"}, false, "bob", 30, false},
		{[]string{"@bob", "10"}, false, "@bob", 10, true},
		{[]string{"@bob", "0"}, false, "", 30, false},
		{[]string{"@bob", "x"}, false, "", 30, false},
		{[]string{"a", "b", "c"}, true, "", 30, false},
	}
	for _, c := range cases {
		q, ok := parseArgs(c.args, c.reply)
		if ok != c.ok || (ok && (q.Target != c.target || q.Count != c.count)) {
			t.Errorf("%v reply=%v: got %+v ok=%v", c.args, c.reply, q, ok)
		}
	}
}

func TestPreview(t *testing.T) {
	doc := func(attrs ...tg.DocumentAttributeClass) *tg.MessageMediaDocument {
		return &tg.MessageMediaDocument{Document: &tg.Document{Attributes: attrs}}
	}
	cases := []struct {
		m    tg.MessageClass
		want string
	}{
		{&tg.Message{Message: "hi\n  there"}, "hi there"},
		{&tg.Message{Message: strings.Repeat("字", 60)}, strings.Repeat("字", 50) + "…"},
		{&tg.Message{Media: &tg.MessageMediaPhoto{}, Message: "cap"}, "[photo] cap"},
		{&tg.Message{Media: doc(&tg.DocumentAttributeSticker{})}, "[sticker]"},
		{&tg.Message{Media: doc(&tg.DocumentAttributeAudio{Voice: true})}, "[voice]"},
		{&tg.Message{Media: doc(&tg.DocumentAttributeVideo{}, &tg.DocumentAttributeAnimated{})}, "[GIF]"},
		{&tg.Message{Media: doc(&tg.DocumentAttributeVideo{RoundMessage: true})}, "[video message]"},
		{&tg.Message{Media: doc(&tg.DocumentAttributeFilename{FileName: "a.zip"})}, "[file]"},
		{&tg.Message{Media: &tg.MessageMediaDice{Emoticon: "🎲"}}, "[dice] 🎲"},
		{&tg.Message{}, "[empty]"},
		{&tg.MessageService{Action: &tg.MessageActionChatEditTitle{Title: "New"}}, "[renamed the group] New"},
		{&tg.MessageService{Action: &tg.MessageActionPinMessage{}}, "[pinned a message]"},
	}
	for i, c := range cases {
		got, ok := preview(en, c.m)
		if !ok || got != c.want {
			t.Errorf("case %d: %q", i, got)
		}
	}
	if _, ok := preview(en, &tg.MessageEmpty{}); ok {
		t.Error("empty message")
	}
}

func TestLinkBase(t *testing.T) {
	peer := &tg.InputPeerChannel{ChannelID: 42}
	if got := linkBase(peer, []tg.ChatClass{&tg.Channel{ID: 42}}); got != "https://t.me/c/42/" {
		t.Fatal(got)
	}
	if got := linkBase(peer, []tg.ChatClass{&tg.Channel{ID: 42, Username: "grp"}}); got != "https://t.me/grp/" {
		t.Fatal(got)
	}
	coll := &tg.Channel{ID: 42, Usernames: []tg.Username{{Username: "old"}, {Username: "nft", Active: true}}}
	if got := linkBase(peer, []tg.ChatClass{coll}); got != "https://t.me/nft/" {
		t.Fatal(got)
	}
	if got := linkBase(&tg.InputPeerChat{ChatID: 1}, nil); got != "" {
		t.Fatal(got)
	}
}

func TestTargetName(t *testing.T) {
	users := []tg.UserClass{&tg.User{ID: 7, FirstName: "Ann", Username: "ann"}, &tg.User{ID: 8, Self: true, FirstName: "Me"}}
	chats := []tg.ChatClass{&tg.Channel{ID: 9, Title: "Chan"}}
	if got := targetName(&tg.InputPeerUser{UserID: 7}, users, chats, ""); got != "Ann @ann" {
		t.Fatal(got)
	}
	if got := targetName(&tg.InputPeerSelf{}, users, chats, ""); got != "Me" {
		t.Fatal(got)
	}
	if got := targetName(&tg.InputPeerChannel{ChannelID: 9}, users, chats, ""); got != "Chan" {
		t.Fatal(got)
	}
	if got := targetName(&tg.InputPeerUser{UserID: 1}, nil, nil, "@x"); got != "@x" {
		t.Fatal(got)
	}
}

func TestChunk(t *testing.T) {
	lines := []string{"1. aaaa", "2. bbbb", "3. cccc"}
	out := chunk("H", lines, 12)
	if len(out) != 3 || !strings.HasPrefix(out[0], "H\n\n1.") || out[2] != "3. cccc" {
		t.Fatalf("%q", out)
	}
	if out := chunk("H", lines, 1000); len(out) != 1 || strings.Count(out[0], "\n") != 4 {
		t.Fatalf("%q", out)
	}
}
