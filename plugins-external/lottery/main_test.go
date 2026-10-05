package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestPlugin builds a plugin whose store lives in dir.
func newTestPlugin(dir string) *LotteryPlugin {
	p := New()
	p.store = newStore(dir)
	p.store.data.Warehouses = map[string][]prizeItem{}
	return p
}

func TestDrawWinnersBasics(t *testing.T) {
	var parts []participant
	for i := int64(1); i <= 50; i++ {
		parts = append(parts, participant{UserID: i})
	}
	wins := drawWinners(parts, 5)
	if len(wins) != 5 {
		t.Fatalf("want 5 winners, got %d", len(wins))
	}
	seen := map[int64]bool{}
	for _, w := range wins {
		if seen[w.UserID] {
			t.Fatalf("duplicate winner %d", w.UserID)
		}
		seen[w.UserID] = true
	}
	// n larger than participants → all participants, no duplicates.
	wins = drawWinners(parts[:3], 10)
	if len(wins) != 3 {
		t.Fatalf("want 3 winners, got %d", len(wins))
	}
	if wins[0].UserID == wins[1].UserID || wins[1].UserID == wins[2].UserID {
		t.Fatal("duplicates in draw")
	}
	if drawWinners(nil, 5) != nil {
		t.Fatal("empty participants must yield no winners")
	}
	if drawWinners(parts, 0) != nil {
		t.Fatal("n=0 must yield no winners")
	}
}

// Fairness: every position must be reachable; a biased shuffle would skew
// the first participant's presence among winners.
func TestDrawWinnersFair(t *testing.T) {
	var parts []participant
	for i := int64(0); i < 10; i++ {
		parts = append(parts, participant{UserID: i})
	}
	hits := map[int64]int{}
	const rounds = 2000
	for i := 0; i < rounds; i++ {
		for _, w := range drawWinners(parts, 1) {
			hits[w.UserID]++
		}
	}
	// 200 rounds per participant expected; anything outside [40, 360]
	// (±82%) would be extraordinary for a fair RNG with 2000 draws.
	for id, n := range hits {
		if n < 140 || n > 260 {
			t.Fatalf("participant %d won %d/%d times — RNG looks biased", id, n, rounds)
		}
	}
}

func TestJoinDedupeAndCap(t *testing.T) {
	p := newTestPlugin(t.TempDir())
	l := &lottery{ID: 1, MaxUsers: 3, Status: "active"}
	now := time.Now()
	for i := int64(1); i <= 3; i++ {
		count, err := p.joinLocked(l, participant{UserID: i}, now)
		if err != joinOK {
			t.Fatalf("join %d: unexpected error %v", i, err)
		}
		if count != int(i) {
			t.Fatalf("count after join %d = %d", i, count)
		}
	}
	if _, err := p.joinLocked(l, participant{UserID: 1}, now); err != errDuplicate {
		t.Fatalf("duplicate join err = %v, want errDuplicate", err)
	}
	if _, err := p.joinLocked(l, participant{UserID: 9}, now); err != errFull {
		t.Fatalf("join over cap err = %v, want errFull", err)
	}
	if len(l.Participants) != 3 {
		t.Fatalf("participants = %d, want 3", len(l.Participants))
	}
}

func TestWarehousePrizes(t *testing.T) {
	p := newTestPlugin(t.TempDir())
	if !p.createWarehouseLocked("vip") {
		t.Fatal("create warehouse failed")
	}
	if p.createWarehouseLocked("vip") {
		t.Fatal("duplicate create must fail")
	}
	p.addPrizeLocked("vip", "月卡", 2)
	p.addPrizeLocked("vip", "月卡", 3) // same text adds stock
	p.addPrizeLocked("vip", "年卡", 1)
	got := p.prizesInStockLocked("vip")
	if len(got) != 2 {
		t.Fatalf("prize kinds = %d, want 2", len(got))
	}
	if got[0].Text != "月卡" || got[0].Stock != 5 {
		t.Fatalf("first prize = %+v, want 月卡 x5 (order preserved)", got[0])
	}
	// Consume in order: 月卡 x5 (stock merged) then 年卡 x1.
	order := []string{"月卡", "月卡", "月卡", "月卡", "月卡", "年卡"}
	for i, want := range order {
		name, ok := p.nextPrizeLocked("vip")
		if !ok || name != want {
			t.Fatalf("draw %d = %q ok=%v, want %q", i, name, ok, want)
		}
	}
	if _, ok := p.nextPrizeLocked("vip"); ok {
		t.Fatal("empty warehouse still returned a prize")
	}
	if n := p.clearWarehouseLocked("vip"); n != 2 {
		t.Fatalf("clear removed %d kinds, want 2", n)
	}
	if len(p.prizesInStockLocked("vip")) != 0 {
		t.Fatal("cleared warehouse still has prizes")
	}
}

func TestWarehouseByNameOrIndex(t *testing.T) {
	names := []string{"default", "vip", "wux"}
	if n, ok := warehouseByNameOrIndex("2", names); !ok || n != "vip" {
		t.Fatalf("index lookup = %q %v", n, ok)
	}
	if n, ok := warehouseByNameOrIndex("vip", names); !ok || n != "vip" {
		t.Fatalf("name lookup = %q %v", n, ok)
	}
	if _, ok := warehouseByNameOrIndex("9", names); ok {
		t.Fatal("out-of-range index must not match")
	}
	if _, ok := warehouseByNameOrIndex("nope", names); ok {
		t.Fatal("unknown name must not match")
	}
}

func TestExpireClaims(t *testing.T) {
	p := newTestPlugin(t.TempDir())
	now := time.Now()
	l := &lottery{Winners: []winner{
		{UserID: 1, Status: "pending", ExpiresAt: now.Add(-time.Hour).Unix()},
		{UserID: 2, Status: "pending", ExpiresAt: now.Add(time.Hour).Unix()},
		{UserID: 3, Status: "sent", ExpiresAt: now.Add(-time.Hour).Unix()},
	}}
	p.store.data.Lotteries = append(p.store.data.Lotteries, l)
	if n := p.expireClaimsLocked(now); n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}
	if l.Winners[0].Status != "expired" || l.Winners[1].Status != "pending" || l.Winners[2].Status != "sent" {
		t.Fatalf("statuses = %v", l.Winners)
	}
}

func TestParseAt(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.Local)
	at, err := parseAt("21:00", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 5, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("21:00 today = %v, want %v", at, want)
	}
	at, err = parseAt("09:00", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 6, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("09:00 rolled to tomorrow = %v, want %v", at, want)
	}
	if _, err = parseAt("2026-12-31 23:59:59", now); err != nil {
		t.Fatalf("full datetime failed: %v", err)
	}
	for _, bad := range []string{"", "banana", "25:99", "2026-12-31"} {
		if _, err := parseAt(bad, now); err == nil {
			t.Fatalf("parseAt(%q) unexpectedly succeeded", bad)
		}
	}
}

func TestParsePrizeAdd(t *testing.T) {
	cases := []struct {
		raw      string
		wh, text string
		stock    string
		wantErr  bool
	}{
		{`prize add vip "VIP 一个月" 3`, "vip", "VIP 一个月", "3", false},
		{`prize add vip 红包 5`, "vip", "红包", "5", false},
		{`prize add default "大 奖" 10`, "default", "大 奖", "10", false},
		{`prize add vip`, "", "", "", true},
		{`prize add vip "unbalanced 3`, "", "", "", true},
		{`prize add vip 红包`, "", "", "", true},
	}
	for _, c := range cases {
		f, err := parsePrizeAdd(c.raw)
		if c.wantErr {
			if err == nil {
				t.Fatalf("parsePrizeAdd(%q) succeeded, want error", c.raw)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parsePrizeAdd(%q): %v", c.raw, err)
		}
		if f[0] != c.wh || f[1] != c.text || f[2] != c.stock {
			t.Fatalf("parsePrizeAdd(%q) = %v, want [%q %q %q]", c.raw, f, c.wh, c.text, c.stock)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := newTestPlugin(dir)
	p.createWarehouseLocked("vip")
	p.addPrizeLocked("vip", "月卡", 2)
	p.store.data.NextID = 7
	l := &lottery{
		ID: 8, ChatID: -100123, Title: "新年抽奖", Keyword: "抽奖",
		MaxUsers: 100, WinnerCnt: 5, Warehouse: "vip", CreatorID: 42,
		CreatedAt: time.Now().Unix(), Status: "active", AutoDrawAt: time.Now().Add(time.Hour).Unix(),
		Participants: []participant{{UserID: 1, Username: "alice", FirstName: "Alice"}},
		Winners:      []winner{{UserID: 1, Prize: "月卡", Status: "pending"}},
	}
	p.store.data.Lotteries = append(p.store.data.Lotteries, l)
	if err := p.store.save(); err != nil {
		t.Fatal(err)
	}
	if err := p.store.load(); err != nil {
		t.Fatal(err)
	}
	if len(p.store.data.Lotteries) != 1 || p.store.data.Lotteries[0].ID != 8 {
		t.Fatalf("round trip lost lottery: %+v", p.store.data.Lotteries)
	}
	if p.store.data.NextID != 7 || len(p.store.data.Warehouses["vip"]) != 1 {
		t.Fatalf("round trip lost state: next=%d wh=%v", p.store.data.NextID, p.store.data.Warehouses)
	}
	got := p.store.data.Lotteries[0]
	if got.Participants[0].Username != "alice" || got.Winners[0].Prize != "月卡" {
		t.Fatalf("nested data lost: %+v", got)
	}
	// Permissions on the state file.
	fi, err := os.Stat(filepath.Join(dir, storeFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestStoreLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, storeFile), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newTestPlugin(dir)
	if err := p.store.load(); err == nil {
		t.Fatal("corrupt state must fail to load")
	}
}

func TestWinnerName(t *testing.T) {
	cases := []struct {
		w    winner
		want string
	}{
		{winner{UserID: 1, FirstName: "A", LastName: "B", Username: "u"}, "A B @u"},
		{winner{UserID: 2, FirstName: "A"}, "A"},
		{winner{UserID: 3, Username: "u"}, "@u"},
		{winner{UserID: 4}, "用户 4"},
	}
	for _, c := range cases {
		if got := winnerName(c.w); got != c.want {
			t.Fatalf("winnerName(%+v) = %q, want %q", c.w, got, c.want)
		}
	}
}

func TestJSONTagStability(t *testing.T) {
	// The state file's field names must stay stable across versions.
	b, err := json.Marshal(lottery{AutoDrawAt: 1, MessageID: 2, Winners: []winner{{UserID: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, key := range []string{`"id"`, `"chat_id"`, `"title"`, `"keyword"`, `"max_participants"`, `"winner_count"`, `"prize_warehouse"`, `"creator_id"`, `"status"`, `"auto_draw_at"`, `"participants"`, `"winners"`} {
		if !strings.Contains(s, key) {
			t.Fatalf("lottery JSON missing stable key %s in %s", key, s)
		}
	}
}

func TestRandIntnBounds(t *testing.T) {
	for _, n := range []int{1, 2, 3, 10, 100, 1000} {
		for i := 0; i < 200; i++ {
			if v := randIntn(n); v < 0 || v >= n {
				t.Fatalf("randIntn(%d) = %d out of range", n, v)
			}
		}
	}
	if v := randIntn(0); v != 0 {
		t.Fatalf("randIntn(0) = %d", v)
	}
}

func TestIsCommandPrefixSkip(t *testing.T) {
	p := newTestPlugin(t.TempDir())
	// No host wired: prefixes list is empty, nothing is a command.
	if p.isCommand("抽奖") {
		t.Fatal("plain keyword treated as command without host")
	}
}
