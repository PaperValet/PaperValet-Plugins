package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestHourIndexMatchesSource(t *testing.T) {
	// hour references from the TS source: h-1, +1 past :30, 12h clock,
	// -1 (00:xx first half) maps to 11, index clamped by set size.
	cases := []struct {
		hhmm string
		want int
	}{
		{"00:05", 11}, // 0-1 = -1 -> 11
		{"00:45", 0},  // 0-1+1 = 0
		{"01:05", 0},  // 1-1 = 0
		{"01:45", 1},
		{"11:05", 10},
		{"11:45", 11},
		{"12:05", 11},
		{"12:45", 0},
		{"13:05", 0},
		{"23:05", 10},
		{"23:45", 11},
	}
	for _, c := range cases {
		var hh, mm int
		fmt.Sscanf(c.hhmm, "%d:%d", &hh, &mm)
		now := time.Date(2026, 1, 1, hh, mm, 0, 0, time.UTC)
		if got := hourIndex(now, 12); got != c.want {
			t.Errorf("hourIndex(%s) = %d, want %d", c.hhmm, got, c.want)
		}
	}
}

func TestHourIndexWrapsSmallSets(t *testing.T) {
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC) // 9h -> index 8
	if got := hourIndex(now, 5); got != 3 {            // 8 % 5
		t.Errorf("hourIndex 9:00 set=5 = %d, want 3", got)
	}
}

func TestNextHourly(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 31, 0, 0, time.UTC)
	if got := nextHourly(base); got.Minute() != 0 || got.Sub(base) != 29*time.Minute {
		t.Errorf("nextHourly(10:31) = %v, want 11:00", got)
	}
	at11 := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	if !nextHourly(at11).After(at11) {
		t.Errorf("nextHourly(11:00:00) must move to the next hour")
	}
}

func TestStoreRoundTripAndMigration(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/subscriptions.json"

	st, err := load(path)
	if err != nil || len(st.Subs) != 0 {
		t.Fatalf("load missing file: %v %v", st, err)
	}
	st.Subs["-100123"] = 1700000000
	st.Last["-100123"] = 42
	if err := save(path, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Subs["-100123"] != 1700000000 || got.Last["-100123"] != 42 || len(got.Subs) != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	// TeleBox lowdb shape migrates in.
	if err := saveRaw(path, []byte(`{"subscriptions":["-100123","777"],"lastMessages":{"-100123":15}}`)); err != nil {
		t.Fatal(err)
	}
	mig, err := load(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(mig.Subs) != 2 || mig.Last["-100123"] != 15 {
		t.Fatalf("migration mismatch: %+v", mig)
	}

	// Corrupt file falls back to defaults instead of erroring the plugin.
	if err := saveRaw(path, []byte(`{oops`)); err != nil {
		t.Fatal(err)
	}
	bad, err := load(path)
	if err != nil || len(bad.Subs) != 0 {
		t.Fatalf("corrupt file: %v %v", bad, err)
	}
}

func TestParseSubCommand(t *testing.T) {
	cases := []struct {
		arg  string
		want string
	}{
		{"", "help"},
		{"sub", "sub"},
		{"SUB", "sub"},
		{"订阅", "sub"},
		{"unsub", "unsub"},
		{"退订", "unsub"},
		{"list", "list"},
		{"列表", "list"},
		{"reload", "reload"},
		{"重载", "reload"},
		{"help", "help"},
		{"帮助", "help"},
		{"whatever", "help"},
	}
	for _, c := range cases {
		if got := subCommand(c.arg); got != c.want {
			t.Errorf("subCommand(%q) = %q, want %q", c.arg, got, c.want)
		}
	}
}

// saveRaw is a test helper writing bytes straight to disk.
func saveRaw(path string, b []byte) error {
	return os.WriteFile(path, b, 0o600)
}

// TestSendAllLazyLoadLockBalance drives the first-tick lazy-load path of
// sendAll: the function must not unlock p.mu twice (the historical bug
// panicked with "sync: unlock of unlocked mutex" on the very first hourly
// report). With no API client the call returns an error before sending, but
// only after the lock dance completed exactly once, which this test asserts
// by locking afterwards — a double unlock would have left the mutex in an
// inconsistent state or panicked here already.
func TestSendAllLazyLoadLockBalance(t *testing.T) {
	p := New()
	p.dir = t.TempDir()
	st, err := load(p.dir + "/subscriptions.json")
	if err != nil {
		t.Fatal(err)
	}
	st.Subs["777"] = time.Now().Unix()
	p.state = st

	// Not loaded and no docs: the lazy-load branch runs, fails for lack of
	// an API client, and must leave p.mu properly balanced.
	if err := p.sendAll(context.Background()); err == nil {
		t.Fatalf("expected no-api error, got nil")
	}
	p.mu.Lock()
	loaded, docs := p.loaded, len(p.docs)
	p.mu.Unlock()
	if loaded || docs != 0 {
		t.Fatalf("state changed without api: loaded=%v docs=%d", loaded, docs)
	}

	// With docs already present the non-lazy path runs; also must balance.
	p.mu.Lock()
	p.loaded = true
	p.docs = []*tg.Document{{ID: 1, AccessHash: 1, FileReference: []byte{1}}}
	p.mu.Unlock()
	if err := p.sendAll(context.Background()); err == nil {
		t.Fatalf("expected no-api error, got nil")
	}
	// The mutex must still be lockable and consistent.
	p.mu.Lock()
	p.mu.Unlock()
}

// TestSentMessageIDUpdateMessageID covers the UpdateMessageID reply shape
// (channels answer messages.sendMessage with an updates box containing only
// UpdateMessageID): newID must be non-zero so the previous sticker gets
// deleted next hour.
func TestSentMessageIDUpdateMessageID(t *testing.T) {
	box := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateMessageID{ID: 77},
	}}
	if got := sentMessageID(box); got != 77 {
		t.Fatalf("updates box with only UpdateMessageID: got %d, want 77", got)
	}
	// UpdateMessageID wins over an empty message placeholder in the box.
	mixed := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateNewMessage{Message: &tg.MessageEmpty{ID: 0}},
		&tg.UpdateMessageID{ID: 88},
	}}
	if got := sentMessageID(mixed); got != 88 {
		t.Fatalf("mixed box: got %d, want 88", got)
	}
}
