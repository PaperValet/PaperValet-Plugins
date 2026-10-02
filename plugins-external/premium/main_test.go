package main

import (
	"strings"
	"testing"

	"github.com/gotd/td/tg"
)

func en(_, e string) string { return e }

func TestTally(t *testing.T) {
	var tl tally
	for _, u := range []*tg.User{{ID: 1, Bot: true}, {ID: 2, Deleted: true}, {ID: 3, Premium: true}, {ID: 4}, {ID: 5}, {ID: 6, Premium: true}, {ID: 6, Premium: true}} {
		tl.add(u)
	}
	if tl.Bots != 1 || tl.Deleted != 1 || tl.Users != 4 || tl.Premium != 2 || tl.Seen != 6 {
		t.Fatalf("%+v", tl)
	}
	if tl.percent() != "50.00" {
		t.Fatal(tl.percent())
	}
	if (tally{}).percent() != "0.00" {
		t.Fatal("zero users")
	}
	out := report(en, tl, true)
	if !strings.Contains(out, "50.00%") || !strings.Contains(out, "first 10k") {
		t.Fatal(out)
	}
}

func TestParticipantIDs(t *testing.T) {
	if channelParticipantID(&tg.ChannelParticipantAdmin{UserID: 7}) != 7 || channelParticipantID(&tg.ChannelParticipantLeft{}) != 0 {
		t.Fatal("channel ids")
	}
	if chatParticipantID(&tg.ChatParticipantCreator{UserID: 9}) != 9 {
		t.Fatal("chat ids")
	}
	m := usersByID([]tg.UserClass{&tg.User{ID: 1}, &tg.UserEmpty{ID: 2}})
	if len(m) != 1 || m[1] == nil {
		t.Fatal(m)
	}
}
