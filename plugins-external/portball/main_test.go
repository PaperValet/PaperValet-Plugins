package main

import (
	"testing"

	"github.com/gotd/td/tg"
)

func zh(z, _ string) string { return z }
func en(_, e string) string { return e }

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		secs int
		ok   bool
	}{
		{"300", 300, true},
		{"0", 0, true},
		{"5m", 300, true},
		{"5M", 300, true},
		{"1h30m", 5400, true},
		{"2d", 172800, true},
		{"1d2h3m4s", 93784, true},
		{"90s", 90, true},
		{"", 0, false},
		{"-5", 0, false},
		{"m", 0, false},
		{"1h30", 0, false},
		{"5x", 0, false},
		{"1m1m", 0, false},
		{"abc", 0, false},
		{"99999999999d", 0, false},
	}
	for _, c := range cases {
		secs, ok := parseDuration(c.in)
		if ok != c.ok || (ok && secs != c.secs) {
			t.Errorf("parseDuration(%q) = %d, %v; want %d, %v", c.in, secs, ok, c.secs, c.ok)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	if _, _, ok := splitArgs(nil); ok {
		t.Fatal("empty args should fail")
	}
	r, d, ok := splitArgs([]string{"10m"})
	if !ok || r != "" || d != "10m" {
		t.Fatalf("got %q %q %v", r, d, ok)
	}
	r, d, ok = splitArgs([]string{"发", "广告", "1h"})
	if !ok || r != "发 广告" || d != "1h" {
		t.Fatalf("got %q %q %v", r, d, ok)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		secs   int
		zh, en string
	}{
		{60, "1分钟", "1m"},
		{5400, "1小时30分钟", "1h 30m"},
		{93784, "1天2小时3分钟4秒", "1d 2h 3m 4s"},
		{86400, "1天", "1d"},
		{0, "0秒", "0s"},
	}
	for _, c := range cases {
		if got := formatDuration(zh, c.secs); got != c.zh {
			t.Errorf("zh %d = %q, want %q", c.secs, got, c.zh)
		}
		if got := formatDuration(en, c.secs); got != c.en {
			t.Errorf("en %d = %q, want %q", c.secs, got, c.en)
		}
	}
}

func TestRealReplyID(t *testing.T) {
	plain := &tg.Message{ReplyTo: &tg.MessageReplyHeader{ReplyToMsgID: 7}}
	if got := realReplyID(plain); got != 7 {
		t.Errorf("plain reply = %d", got)
	}
	root := &tg.Message{ReplyTo: &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 3}}
	if got := realReplyID(root); got != 0 {
		t.Errorf("topic root = %d, want 0", got)
	}
	inTopic := &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 9}
	inTopic.SetReplyToTopID(3)
	if got := realReplyID(&tg.Message{ReplyTo: inTopic}); got != 9 {
		t.Errorf("reply in topic = %d, want 9", got)
	}
	if got := realReplyID(&tg.Message{}); got != 0 {
		t.Errorf("no reply = %d", got)
	}
}

func TestResolveTarget(t *testing.T) {
	group := &tg.InputPeerChannel{ChannelID: 100, AccessHash: 1}
	bob := &tg.User{ID: 5, FirstName: "Bob"}
	bob.SetAccessHash(55)
	news := &tg.Channel{ID: 200, Title: "News"}
	news.SetAccessHash(22)
	ghost := &tg.User{ID: 6, Min: true, FirstName: "Min"}
	ghost.SetAccessHash(66)
	users := []tg.UserClass{bob, ghost}
	chats := []tg.ChatClass{news}

	got, err := resolveTarget(en, &tg.Message{FromID: &tg.PeerUser{UserID: 5}}, group, 1, users, chats)
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := got.peer.(*tg.InputPeerUser); !ok || u.AccessHash != 55 {
		t.Errorf("user peer = %#v", got.peer)
	}
	if _, err := resolveTarget(en, &tg.Message{FromID: &tg.PeerUser{UserID: 1}}, group, 1, users, chats); err == nil {
		t.Error("muting self should fail")
	}
	if _, err := resolveTarget(en, &tg.Message{FromID: &tg.PeerChannel{ChannelID: 100}}, group, 1, users, chats); err == nil {
		t.Error("anonymous admin should fail")
	}
	got, err = resolveTarget(en, &tg.Message{FromID: &tg.PeerChannel{ChannelID: 200}}, group, 1, users, chats)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := got.peer.(*tg.InputPeerChannel); !ok || c.AccessHash != 22 {
		t.Errorf("channel peer = %#v", got.peer)
	}

	got, err = resolveTarget(en, &tg.Message{ID: 42, FromID: &tg.PeerUser{UserID: 6}}, group, 1, users, chats)
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := got.peer.(*tg.InputPeerUserFromMessage); !ok || m.MsgID != 42 || m.UserID != 6 {
		t.Errorf("min user peer = %#v", got.peer)
	}
}
