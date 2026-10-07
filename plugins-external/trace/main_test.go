package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func TestParseReactionsStandard(t *testing.T) {
	text := ".trace 👍👎 ❤️ x 🥰👍"
	got := parseReactions(text, argsStart(text, 1), nil, false)
	want := []string{"👍", "👎", "❤", "🥰"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestParseReactionsZWJAndSkin(t *testing.T) {
	text := "❤️‍🔥👍🏻⚡️"
	got := parseReactions(text, 0, nil, false)
	want := []string{"❤\u200d🔥", "👍", "⚡"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestParseReactionsCustom(t *testing.T) {
	// ".trace 😀👍" with the 😀 being a custom emoji entity
	text := ".trace 😀👍"
	off := utf16Len(".trace ")
	ents := []tg.MessageEntityClass{&tg.MessageEntityCustomEmoji{Offset: off, Length: 2, DocumentID: 12345}}
	got := parseReactions(text, argsStart(text, 1), ents, true)
	if want := []string{"12345", "👍"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("premium: got %q want %q", got, want)
	}
	got = parseReactions(text, argsStart(text, 1), ents, false)
	if want := []string{"👍"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no premium: got %q want %q", got, want)
	}
}

func TestArgsStart(t *testing.T) {
	s := ".trace kw add hello  👍"
	if i := argsStart(s, 4); s[i:] != "👍" {
		t.Fatalf("got %q", s[i:])
	}
	if argsStart(".trace", 2) != -1 {
		t.Fatal("expected -1")
	}
}

func TestMatchKeyword(t *testing.T) {
	kws := map[string][]string{"foo": {"👍"}, "bar": {"👎"}}
	if k, ok := matchKeyword("a bar foo", kws); !ok || k != "bar" {
		t.Fatalf("got %q %v", k, ok)
	}
	if _, ok := matchKeyword("nothing", kws); ok {
		t.Fatal("unexpected match")
	}
}

func TestToTG(t *testing.T) {
	r := toTG([]string{"👍", "987"})
	if e, ok := r[0].(*tg.ReactionEmoji); !ok || e.Emoticon != "👍" {
		t.Fatalf("bad %T", r[0])
	}
	if c, ok := r[1].(*tg.ReactionCustomEmoji); !ok || c.DocumentID != 987 {
		t.Fatalf("bad %T", r[1])
	}
}

func TestPremiumRequired(t *testing.T) {
	if !premiumRequired(tgerr.New(400, "PREMIUM_ACCOUNT_REQUIRED")) {
		t.Fatal("premium required not detected")
	}
	if !premiumRequired(tgerr.New(400, "CUSTOM_EMOJI_INVALID")) {
		t.Fatal("custom emoji invalid not detected")
	}
	if premiumRequired(tgerr.New(400, "REACTION_INVALID")) {
		t.Fatal("reaction invalid misdetected")
	}
	if premiumRequired(errors.New("boom")) {
		t.Fatal("plain error misdetected")
	}
}

func TestMapToStandard(t *testing.T) {
	alts := func(id string) string {
		switch id {
		case "111":
			return "❤️" // variant selector must be cleaned
		case "222":
			return "🫠" // not a usable standard reaction → 👍
		default:
			return "" // lookup failed → 👍
		}
	}
	got := mapToStandard([]string{"111", "👍", "222", "333", "111"}, alts)
	if want := []string{"❤", "👍"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	// altOf nil (no lookup possible) still downgrades to 👍.
	if got := mapToStandard([]string{"111"}, nil); !reflect.DeepEqual(got, []string{"👍"}) {
		t.Fatalf("nil altOf: got %q", got)
	}
}

func TestReplyMsgID(t *testing.T) {
	ev := &plugin.MessageEvent{Message: &tg.Message{}}
	if got := replyMsgID(ev); got != 0 {
		t.Fatal("no reply header")
	}
	if got := replyMsgID(nil); got != 0 {
		t.Fatal("nil event")
	}
	h := &tg.MessageReplyHeader{}
	h.SetReplyToMsgID(42)
	ev.Message.ReplyTo = h
	if got := replyMsgID(ev); got != 42 {
		t.Fatalf("plain reply: %d", got)
	}
	// Plain message inside a forum topic: header points at the topic root,
	// which is not a reply — must not be treated as one.
	ft := &tg.MessageReplyHeader{ForumTopic: true}
	ft.SetReplyToMsgID(7)
	ev.Message.ReplyTo = ft
	if got := replyMsgID(ev); got != 0 {
		t.Fatalf("topic root treated as reply: %d", got)
	}
	// A real reply inside a topic carries replyToTopID as well.
	ft.SetReplyToTopID(7)
	ft.SetReplyToMsgID(50)
	if got := replyMsgID(ev); got != 50 {
		t.Fatalf("real reply in topic: %d", got)
	}
}

func TestSenderKey(t *testing.T) {
	m := &tg.Message{FromID: &tg.PeerUser{UserID: 5}, PeerID: &tg.PeerChannel{ChannelID: 1}}
	if senderKey(m) != 5 {
		t.Fatal("user")
	}
	m = &tg.Message{PeerID: &tg.PeerUser{UserID: 7}}
	if senderKey(m) != 7 {
		t.Fatal("private")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	in := traceDB{Users: map[string]*userEntry{"1": {Name: "a", Reactions: []string{"👍"}}}, Keywords: map[string][]string{"k": {"🔥"}}}
	if err := writeJSON(path, in); err != nil {
		t.Fatal(err)
	}
	var out traceDB
	if err := readJSON(path, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("got %+v", out)
	}
}

func TestClipLines(t *testing.T) {
	s := "> `aaa`\n> `bbb`\n> `ccc`"
	if got := clipLines(s, 12); got != "> `aaa`\n…" {
		t.Fatalf("got %q", got)
	}
	if clipLines(s, 100) != s {
		t.Fatal("short text changed")
	}
}

func TestKwTokenStable(t *testing.T) {
	if kwToken("你好") != kwToken("你好") || kwToken("a") == kwToken("b") {
		t.Fatal("token not stable or collides")
	}
	if len("p:trace:k:"+kwToken(strings.Repeat("长", 200))) > 64 {
		t.Fatal("callback data too long")
	}
}
