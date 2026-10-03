package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func en(_, e string) string { return e }
func zh(z, _ string) string { return z }

func TestPageIDs(t *testing.T) {
	msgs := []tg.MessageClass{
		&tg.Message{ID: 50}, &tg.MessageService{ID: 49}, &tg.Message{ID: 48},
		&tg.MessageEmpty{ID: 47}, &tg.Message{ID: 46},
	}
	ids, next := pageIDs(msgs, 48)
	if !slices.Equal(ids, []int{50, 49, 46}) || next != 46 {
		t.Fatal(ids, next)
	}
	if ids, next := pageIDs(nil, 1); ids != nil || next != 0 {
		t.Fatal("empty page", ids, next)
	}
	// A page holding only the command message still advances.
	if ids, next := pageIDs([]tg.MessageClass{&tg.Message{ID: 9}}, 9); len(ids) != 0 || next != 9 {
		t.Fatal(ids, next)
	}
}

func TestGroupOf(t *testing.T) {
	cases := []struct {
		chat tg.ChatClass
		ok   bool
		wipe bool
	}{
		{&tg.Channel{Megagroup: true, Creator: true}, true, true},
		{&tg.Channel{Megagroup: true, AdminRights: tg.ChatAdminRights{BanUsers: true, DeleteMessages: true}}, true, true},
		{&tg.Channel{Megagroup: true, AdminRights: tg.ChatAdminRights{BanUsers: true}}, true, false},
		{&tg.Channel{Megagroup: true}, true, false},
		{&tg.Channel{Broadcast: true, Creator: true}, false, false},
		{&tg.Channel{Megagroup: true, Left: true}, false, false},
		{&tg.Chat{Creator: true}, true, true},
		{&tg.Chat{Deactivated: true, Creator: true}, false, false},
		{&tg.ChatForbidden{}, false, false},
	}
	for i, c := range cases {
		if ch, ok := c.chat.(*tg.Channel); ok && (ch.AdminRights != tg.ChatAdminRights{}) {
			ch.SetAdminRights(ch.AdminRights)
		}
		if ch, ok := c.chat.(*tg.Chat); ok && (ch.AdminRights != tg.ChatAdminRights{}) {
			ch.SetAdminRights(ch.AdminRights)
		}
		g := groupOf(c.chat)
		if (g != nil) != c.ok {
			t.Fatalf("case %d: group %v", i, g)
		}
		if g != nil && g.canWipe() != c.wipe {
			t.Fatalf("case %d: canWipe %v", i, g.canWipe())
		}
	}
}

func TestMuteRightsKeepsReading(t *testing.T) {
	r := muteRights()
	if r.ViewMessages || r.UntilDate != 0 || !r.SendMessages || !r.SendPlain || !r.InviteUsers {
		t.Fatalf("%+v", r)
	}
}

func TestFatal(t *testing.T) {
	if !fatal(tgerr.New(400, "CHAT_ADMIN_REQUIRED")) || !fatal(tgerr.New(420, "FLOOD_WAIT_300")) {
		t.Fatal("fatal errors")
	}
	if fatal(tgerr.New(400, "MESSAGE_DELETE_FORBIDDEN")) || fatal(errors.New("x")) {
		t.Fatal("non-fatal errors")
	}
}

func TestTexts(t *testing.T) {
	if s := warning(en); !strings.Contains(s, "paolu confirm") {
		t.Fatal(s)
	}
	ok := summary(en, "My_Group", result{Muted: true, Deleted: 120})
	if !strings.Contains(ok, "Wipe finished") || !strings.Contains(ok, "Deleted 120") || strings.Contains(ok, "could not") {
		t.Fatal(ok)
	}
	bad := summary(zh, "", result{MuteErr: tgerr.New(400, "CHAT_ADMIN_REQUIRED"), Deleted: 3, Failed: 2, Err: tgerr.New(420, "FLOOD_WAIT_120")})
	for _, want := range []string{"未完全成功", "管理员权限不足", "2 条删不掉", "121 秒"} {
		if !strings.Contains(bad, want) {
			t.Fatalf("missing %q in %s", want, bad)
		}
	}
	if p := progress(en, &result{Muted: true, Deleted: 5}); !strings.Contains(p, "Deleted 5") {
		t.Fatal(p)
	}
}
