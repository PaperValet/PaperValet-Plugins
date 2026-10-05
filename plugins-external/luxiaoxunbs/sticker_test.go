package main

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestPickStickerByHour(t *testing.T) {
	mk := func(id int64) *tg.Document {
		d := &tg.Document{ID: id, AccessHash: 1, FileReference: []byte{1}}
		return d
	}
	set := []*tg.Document{mk(10), mk(11), mk(12)}
	// hourIndex(9:00, 12) = 8 -> 8 % 3 = 2
	if got := pickSticker(set, 8); got == nil || got.ID != 12 {
		t.Fatalf("pickSticker(8) = %v, want doc 12", got)
	}
	if got := pickSticker(set, 3); got == nil || got.ID != 10 {
		t.Fatalf("pickSticker(3) = %v, want doc 10 (sticker 1)", got)
	}
	if got := pickSticker(nil, 8); got != nil {
		t.Fatalf("pickSticker(nil) = %v, want nil", got)
	}
	if got := pickSticker(set, -1); got != nil {
		t.Fatalf("pickSticker(-1) = %v, want nil", got)
	}
}

func TestFatalSendError(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"rpc error: CHAT_WRITE_FORBIDDEN", true},
		{"rpc error: CHAT_NOT_FOUND", true},
		{"rpc error: USER_IS_BLOCKED", true},
		{"rpc error: PEER_ID_INVALID", true},
		{"rpc error: CHANNEL_PRIVATE", true},
		{"rpc error: FLOOD_WAIT_30", false},
		{"some transient error", false},
		{"", false},
	}
	for _, c := range cases {
		if got := fatalSendError(c.err); got != c.want {
			t.Errorf("fatalSendError(%q) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestIsAdmin(t *testing.T) {
	yes := []tg.ChannelParticipantClass{
		&tg.ChannelParticipantCreator{UserID: 7},
		&tg.ChannelParticipantAdmin{UserID: 7},
	}
	no := []tg.ChannelParticipantClass{
		&tg.ChannelParticipant{UserID: 7},
		&tg.ChannelParticipantLeft{},
	}
	for _, p := range yes {
		if !isAdmin(p) {
			t.Errorf("isAdmin(%T) = false, want true", p)
		}
	}
	for _, p := range no {
		if isAdmin(p) {
			t.Errorf("isAdmin(%T) = true, want false", p)
		}
	}
}

func TestChatAdminBasic(t *testing.T) {
	// Basic-group participant list: creator/admin pass, member fails.
	yes := []tg.ChatParticipantClass{
		&tg.ChatParticipantCreator{UserID: 7},
		&tg.ChatParticipantAdmin{UserID: 7},
	}
	no := []tg.ChatParticipantClass{
		&tg.ChatParticipant{UserID: 7},
	}
	for _, p := range yes {
		if !isChatAdmin(p, 7) {
			t.Errorf("isChatAdmin(%T) = false, want true", p)
		}
	}
	for _, p := range no {
		if isChatAdmin(p, 7) {
			t.Errorf("isChatAdmin(%T) = true, want false", p)
		}
	}
	// wrong user id never matches
	if isChatAdmin(&tg.ChatParticipantAdmin{UserID: 9}, 7) {
		t.Error("admin with different user id must not pass")
	}
}
