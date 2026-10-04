package main

import (
	"strings"
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
		{"5m", 300, true},
		{"5M", 300, true},
		{"1h30m", 5400, true},
		{"2d", 172800, true},
		{"1d2h3m4s", 93784, true},
		{"90s", 90, true},
		// a bare number is a user id, not a duration
		{"300", 0, false},
		{"", 0, false},
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

func TestTimedSeconds(t *testing.T) {
	secs, found, bad := timedSeconds([]string{"12345", "true"})
	if found || bad != "" || secs != 0 {
		t.Errorf("bare id: got %d %v %q", secs, found, bad)
	}
	secs, found, bad = timedSeconds([]string{"1h30m"})
	if !found || secs != 5400 || bad != "" {
		t.Errorf("duration: got %d %v %q", secs, found, bad)
	}
	secs, found, bad = timedSeconds([]string{"@someone", "10m"})
	if !found || secs != 600 {
		t.Errorf("after username: got %d %v %q", secs, found, bad)
	}
	_, found, bad = timedSeconds([]string{"1h30"})
	if found || bad != "1h30" {
		t.Errorf("bad duration should surface: got %v %q", found, bad)
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

func TestRights(t *testing.T) {
	ban := banRights(42)
	if !ban.ViewMessages || ban.UntilDate != 42 {
		t.Errorf("banRights = %+v", ban)
	}
	mute := fullMuteRights(0)
	if mute.ViewMessages || !mute.SendMessages || !mute.SendDocs || mute.UntilDate != 0 {
		t.Errorf("fullMuteRights = %+v", mute)
	}
}

func TestManageable(t *testing.T) {
	ch := &tg.Channel{ID: 1, Title: "creator"}
	ch.SetCreator(true)
	if g, ok := manageable(ch, 0); !ok || g.Kind != "channel" {
		t.Errorf("creator channel: %v %v", g, ok)
	}
	admin := &tg.Channel{ID: 2, Title: "admin"}
	r := tg.ChatAdminRights{BanUsers: true}
	admin.SetAdminRights(r)
	if _, ok := manageable(admin, 0); !ok {
		t.Error("ban admin should manage")
	}
	plain := &tg.Channel{ID: 3, Title: "member"}
	if _, ok := manageable(plain, 0); ok {
		t.Error("plain member should not manage")
	}
	bc := &tg.Channel{ID: 4, Title: "broadcast"}
	bc.SetBroadcast(true)
	bc.SetCreator(true)
	if _, ok := manageable(bc, 0); ok {
		t.Error("broadcast channel should not manage")
	}
	basic := &tg.Chat{ID: 5, Title: "basic"}
	if _, ok := manageable(basic, 0); ok {
		t.Error("basic group snapshot never qualifies")
	}
}

func TestBatchOutcome(t *testing.T) {
	o := newOutcome()
	o.fail("USER_NOT_PARTICIPANT")
	o.fail("USER_NOT_PARTICIPANT")
	o.fail("CHANNEL_INVALID")
	if o.failed != 3 || o.topReasons(2) != "USER_NOT_PARTICIPANT×2、CHANNEL_INVALID×1" {
		t.Errorf("outcome = %+v top=%q", o, o.topReasons(2))
	}
}

func TestSplitDialogs(t *testing.T) {
	d := &tg.MessagesDialogs{}
	if page, ok := splitDialogs(d); !ok || page.dialogs != nil {
		t.Errorf("empty dialogs: %v %v", page, ok)
	}
	s := &tg.MessagesDialogsSlice{Count: 3, Dialogs: []tg.DialogClass{&tg.Dialog{}}}
	page, ok := splitDialogs(s)
	if !ok || len(page.dialogs) != 1 {
		t.Errorf("slice: %v %v", page, ok)
	}
	if _, ok := splitDialogs(&tg.MessagesDialogsNotModified{}); ok {
		t.Error("not modified should not unpack")
	}
}

func TestPeerInputOf(t *testing.T) {
	chats := map[int64]tg.ChatClass{}
	if _, ok := peerInputOf(&tg.PeerChannel{ChannelID: 9}, chats).(*tg.InputPeerChannel); !ok {
		t.Error("channel offset peer")
	}
	if _, ok := peerInputOf(&tg.PeerUser{UserID: 3}, chats).(*tg.InputPeerUser); !ok {
		t.Error("user offset peer")
	}
	if _, ok := peerInputOf(&tg.PeerChat{ChatID: 4}, chats).(*tg.InputPeerChat); !ok {
		t.Error("chat offset peer")
	}
}

func TestFormatDuration(t *testing.T) {
	if got := formatDuration(zh, 5400); got != "1小时30分钟" {
		t.Errorf("zh 5400 = %q", got)
	}
	if got := formatDuration(en, 5400); got != "1h 30m" {
		t.Errorf("en 5400 = %q", got)
	}
	if got := formatDuration(en, 86400); got != "1d" {
		t.Errorf("en 86400 = %q", got)
	}
}

func TestErrCode(t *testing.T) {
	if got := errCode(errBasicNoUnban); got != "BASIC_GROUP_NO_UNBAN" {
		t.Errorf("errCode(basic) = %q", got)
	}
	long := errCode(errStringErr(strings.Repeat("x", 200)))
	if len(long) > 60 {
		t.Errorf("errCode clips to 60, got %d", len(long))
	}
}

type errStringErr string

func (e errStringErr) Error() string { return string(e) }
