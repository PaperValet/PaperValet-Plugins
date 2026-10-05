package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func zh(z, _ string) string { return z }
func en(_, e string) string { return e }

func TestEstimateRegDate(t *testing.T) {
	// Exact anchors round-trip.
	for _, a := range idDataPoints {
		want := time.Unix(a[1], 0).UTC().Format("2006-01")
		if got := estimateRegDate(a[0]); got != want {
			t.Errorf("anchor %d: got %q, want %q", a[0], got, want)
		}
	}
	// Below the first anchor is not a user; beyond the last extrapolates
	// past it (same as the ids.ts source).
	if got := estimateRegDate(-1001234); got != "" {
		t.Errorf("negative id should give no estimate, got %q", got)
	}
	if got := estimateRegDate(9_999_999_999); got <= "2026-01" {
		t.Errorf("beyond last anchor should extrapolate later, got %q", got)
	}
	// 100M sits halfway between the 2014-05 and 2016-01 anchors.
	wantTS := int64(1400000000 + (100000000-50000000)*(1451606400-1400000000)/(150000000-50000000))
	if got, want := estimateRegDate(100000000), time.Unix(wantTS, 0).UTC().Format("2006-01"); got != want {
		t.Errorf("midpoint = %q, want %q", got, want)
	}
}

func TestActiveUsernames(t *testing.T) {
	cases := []struct {
		name string
		u    *tg.User
		want []string
	}{
		{"plain", &tg.User{Username: "alice"}, []string{"alice"}},
		{"none", &tg.User{}, nil},
		{
			"collectible active merged",
			&tg.User{Username: "alice", Usernames: []tg.Username{
				{Username: "alice", Active: true},
				{Username: "frag", Active: false},
				{Username: "tips", Active: true},
			}},
			[]string{"alice", "tips"},
		},
		{
			"only inactive collectible",
			&tg.User{Usernames: []tg.Username{{Username: "frag", Active: false}}},
			nil,
		},
	}
	for _, c := range cases {
		got := activeUsernames(c.u)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}

func TestUserStatusText(t *testing.T) {
	cases := []struct {
		s    tg.UserStatusClass
		zh   string
		en   string
		okZH bool
		okEN bool
	}{
		{&tg.UserStatusOnline{}, "🟢 在线", "🟢 online", true, true},
		{&tg.UserStatusOffline{WasOnline: 1700000000},
			time.Unix(1700000000, 0).Format("2006-01-02 15:04"),
			time.Unix(1700000000, 0).Format("2006-01-02 15:04"), true, true},
		{&tg.UserStatusOffline{}, "离线", "offline", true, true},
		{&tg.UserStatusRecently{}, "最近", "recently", true, true},
		{&tg.UserStatusLastWeek{ByMe: true}, "一周内", "last week", true, true},
		{&tg.UserStatusLastMonth{}, "一月内", "last month", true, true},
		{&tg.UserStatusEmpty{}, "", "", false, false},
	}
	for _, c := range cases {
		zhText, zhOK := userStatusText(zh, c.s)
		enText, enOK := userStatusText(en, c.s)
		if zhText != c.zh || zhOK != c.okZH {
			t.Errorf("zh %+v: got (%q,%v)", c.s, zhText, zhOK)
		}
		if enText != c.en || enOK != c.okEN {
			t.Errorf("en %+v: got (%q,%v)", c.s, enText, enOK)
		}
	}
}

func TestUserBadges(t *testing.T) {
	u := &tg.User{Premium: true, Bot: true, Verified: true, Scam: true, Fake: true, Support: true}
	got := userBadges(zh, u)
	want := []string{"⭐ Premium", "🤖 机器人", "✅ 已验证", "🚩 诈骗", "❌ 虚假", "🛡 官方支持"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("badges = %v, want %v", got, want)
	}
	if b := userBadges(zh, &tg.User{Deleted: true}); strings.Join(b, "|") != "⚰️ 已注销" {
		t.Errorf("deleted badge = %v", b)
	}
	if b := userBadges(zh, &tg.User{}); len(b) != 0 {
		t.Errorf("no flags should give no badges, got %v", b)
	}
}

func TestUserDC(t *testing.T) {
	if got := userDC(&tg.User{Photo: &tg.UserProfilePhoto{DCID: 4}}); got != "DC4" {
		t.Errorf("photo dc = %q", got)
	}
	if got := userDC(&tg.User{Photo: &tg.UserProfilePhotoEmpty{}}); got != "-" {
		t.Errorf("empty photo dc = %q", got)
	}
	if got := userDC(&tg.User{}); got != "-" {
		t.Errorf("no photo dc = %q", got)
	}
}

func TestIsNumericAndParseID(t *testing.T) {
	for _, s := range []string{"123", "-100123", "+42", "0"} {
		if !isNumeric(s) {
			t.Errorf("isNumeric(%q) = false", s)
		}
	}
	for _, s := range []string{"", "abc", "@x", "12a", "--", "1.5"} {
		if isNumeric(s) {
			t.Errorf("isNumeric(%q) = true", s)
		}
	}
	cases := map[string]int64{"123": 123, "-1001234567890": -1001234567890, "0": 0, " 42": 42}
	for in, want := range cases {
		got, ok := parseID(in)
		if !ok || got != want {
			t.Errorf("parseID(%q) = (%d,%v), want (%d,true)", in, got, ok, want)
		}
	}
	if _, ok := parseID("abc"); ok {
		t.Error("parseID(abc) should fail")
	}
}

func TestRealReplyID(t *testing.T) {
	cases := []struct {
		name string
		msg  *tg.Message
		want int
	}{
		{"no reply", &tg.Message{}, 0},
		{"plain reply", &tg.Message{ReplyTo: &tg.MessageReplyHeader{ReplyToMsgID: 7}}, 7},
		{"forum topic header only", &tg.Message{ReplyTo: &tg.MessageReplyHeader{ReplyToMsgID: 1, ForumTopic: true}}, 0},
		{"forum real reply", func() *tg.Message {
			h := &tg.MessageReplyHeader{ReplyToMsgID: 9, ForumTopic: true}
			h.SetReplyToTopID(2)
			return &tg.Message{ReplyTo: h}
		}(), 9},
		{"forum topic root with top id", func() *tg.Message {
			h := &tg.MessageReplyHeader{ReplyToMsgID: 1, ForumTopic: true}
			h.SetReplyToTopID(1)
			return &tg.Message{ReplyTo: h}
		}(), 1},
	}
	for _, c := range cases {
		if got := realReplyID(c.msg); got != c.want {
			t.Errorf("%s: realReplyID = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestDisplayName(t *testing.T) {
	cases := map[string]*tg.User{
		"Li Lei":          {FirstName: "Li", LastName: "Lei"},
		"@bob":            {Username: "bob"},
		"User 42":         {ID: 42},
		"Deleted Account": {Deleted: true},
	}
	for want, u := range cases {
		if got := displayName(u); got != want {
			t.Errorf("displayName = %q, want %q", got, want)
		}
	}
}

func TestChatParticipantDate(t *testing.T) {
	cases := []tg.ChatParticipantClass{
		&tg.ChatParticipant{Date: 111},
		&tg.ChatParticipantAdmin{Date: 222},
		&tg.ChatParticipantCreator{},
	}
	want := []int{111, 222, 0}
	for i, pc := range cases {
		if got := chatParticipantDate(pc); got != want[i] {
			t.Errorf("chatParticipantDate(%T) = %d, want %d", pc, got, want[i])
		}
	}
}

func TestChannelHandles(t *testing.T) {
	ch := &tg.Channel{Username: "main", Usernames: []tg.Username{
		{Username: "main", Active: true},
		{Username: "old", Active: false},
		{Username: "alt", Active: true},
	}}
	got := channelHandles(ch)
	if len(got) != 2 || !strings.Contains(got[0], "@main") || !strings.Contains(got[1], "@alt") {
		t.Errorf("channelHandles = %v", got)
	}
	if h := activeChannelHandle(&tg.Channel{}); h != "" {
		t.Errorf("no handle expected, got %q", h)
	}
	if h := activeChannelHandle(&tg.Channel{Usernames: []tg.Username{{Username: "x", Active: true}}}); h != "x" {
		t.Errorf("fallback handle = %q", h)
	}
}

func TestMyRoleLine(t *testing.T) {
	if line, ok := myRoleLine(zh, &tg.Channel{Creator: true}); !ok || !strings.Contains(line, "创建者") {
		t.Errorf("creator line = (%q,%v)", line, ok)
	}
	if line, ok := myRoleLine(en, &tg.Channel{AdminRights: tg.ChatAdminRights{BanUsers: true}}); !ok || !strings.Contains(line, "admin") {
		t.Errorf("admin line = (%q,%v)", line, ok)
	}
	if _, ok := myRoleLine(zh, &tg.Channel{}); ok {
		t.Error("plain member snapshot should not claim a role")
	}
	if line, ok := myRoleLine(zh, &tg.Channel{Left: true}); !ok || !strings.Contains(line, "不在群里") {
		t.Errorf("left line = (%q,%v)", line, ok)
	}
}

func TestMyBasicRoleLine(t *testing.T) {
	cf := &tg.ChatFull{Participants: &tg.ChatParticipants{Participants: []tg.ChatParticipantClass{
		&tg.ChatParticipant{UserID: 1, Date: 5},
		&tg.ChatParticipantAdmin{UserID: 2},
		&tg.ChatParticipantCreator{UserID: 3},
	}}}
	if line, ok := myBasicRoleLine(zh, cf, 2); !ok || !strings.Contains(line, "管理员") {
		t.Errorf("admin = (%q,%v)", line, ok)
	}
	if line, ok := myBasicRoleLine(zh, cf, 3); !ok || !strings.Contains(line, "创建者") {
		t.Errorf("creator = (%q,%v)", line, ok)
	}
	if line, ok := myBasicRoleLine(zh, cf, 1); !ok || !strings.Contains(line, "成员") {
		t.Errorf("member = (%q,%v)", line, ok)
	}
	if _, ok := myBasicRoleLine(zh, cf, 9); ok {
		t.Error("non-member should give no line")
	}
	if _, ok := myBasicRoleLine(zh, &tg.ChatFull{}, 1); ok {
		t.Error("forbidden participants should give no line")
	}
}

func TestChatBadges(t *testing.T) {
	if b := chatBadges(zh, true, false, false, true); strings.Join(b, " ") != "✅ 已验证" {
		t.Errorf("verified megagroup badges = %v", b)
	}
	if b := chatBadges(en, false, false, false, false); strings.Join(b, " ") != "Channel" {
		t.Errorf("plain channel badge = %v", b)
	}
	if b := chatBadges(en, false, false, false, true); len(b) != 0 {
		t.Errorf("plain megagroup should have no badges, got %v", b)
	}
	if b := chatBadges(zh, false, true, false, false); !strings.Contains(strings.Join(b, " "), "诈骗") {
		t.Errorf("scam badge = %v", b)
	}
}

func TestClipAndOneLine(t *testing.T) {
	if got := clip("hello", 10); got != "hello" {
		t.Errorf("short clip = %q", got)
	}
	if got := clip(strings.Repeat("字", 5), 3); got != "字字字…" {
		t.Errorf("rune clip = %q", got)
	}
	if got := oneLine(" a \n b\t c "); got != "a b c" {
		t.Errorf("oneLine = %q", got)
	}
}

func TestHelpTextBilingual(t *testing.T) {
	if h := helpText(zh); !strings.Contains(h, "信息查询") || !strings.Contains(h, "whois") {
		t.Error("zh help missing pieces")
	}
	if h := helpText(en); !strings.Contains(h, "Whois") || !strings.Contains(h, "username") {
		t.Error("en help missing pieces")
	}
}

func TestFailFloodWait(t *testing.T) {
	err := &tgerr.Error{Type: "FLOOD_WAIT", Argument: 30, Code: 420}
	out := fail(zh, err)
	if !strings.Contains(out, "31") || !strings.Contains(out, "请求过于频繁") {
		t.Errorf("flood wait zh = %q", out)
	}
	if out := fail(en, &tgerr.Error{Type: "USERNAME_NOT_OCCUPIED"}); !strings.Contains(out, "does not exist") {
		t.Errorf("username error en = %q", out)
	}
	if out := fail(zh, errors.New("普通错误 *bold* [x]")); !strings.Contains(out, `普通错误 \*bold\* \[x\]`) {
		t.Errorf("plain error should be escaped: %q", out)
	}
}

func TestPeerUser(t *testing.T) {
	if u, ok := peerUser(&tg.InputPeerUser{UserID: 7, AccessHash: 9}); !ok || u.ID != 7 || u.AccessHash != 9 {
		t.Errorf("InputPeerUser conversion failed: %+v %v", u, ok)
	}
	if u, ok := peerUser(&tg.InputPeerSelf{}); !ok || !u.Self {
		t.Errorf("InputPeerSelf conversion failed: %+v %v", u, ok)
	}
	if _, ok := peerUser(&tg.InputPeerChannel{ChannelID: 5}); ok {
		t.Error("channel peer is not a user")
	}
}
