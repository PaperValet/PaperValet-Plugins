package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"30s", 30}, {"5m", 300}, {"2h", 7200}, {"1d", 86400},
		{"30 seconds", 30}, {"5 minutes", 300}, {"5 minute", 300},
		{"2 hours", 7200}, {"1 days", 86400}, {"1 day", 86400},
		{"30sec", 30}, {"5min", 300}, {"2hr", 7200}, {"1D", 86400},
		{"30 S", 30}, {"  2 h ", 7200},
		{"30秒", 30}, {"5分", 300}, {"5分钟", 300}, {"2小时", 7200}, {"2时", 7200}, {"1天", 86400},
		{"5 分钟", 300}, {"30 秒", 30},
		{"", 0}, {"abc", 0}, {"5", 0}, {"s5", 0}, {"5x", 0}, {"五分钟", 0}, {"-5s", 0},
		{"5seconds", 5}, // tight English also works (spaces stripped)
	}
	for _, c := range cases {
		got := parseDuration(c.in)
		if got != c.want {
			t.Errorf("parseDuration(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestMatchRule(t *testing.T) {
	rules := []CmdRule{
		{ID: "1", Command: "tpm", Delay: 120, Parameters: []string{"s", "search"}},
		{ID: "2", Command: "tpm", Delay: 10},
		{ID: "3", Command: "eat", Delay: 10, ExactMatch: true},
		{ID: "4", Command: "eat", Delay: 60},
	}
	// parameter rule wins over the plain rule
	if r := matchRule(rules, "tpm", []string{"search", "x"}); r == nil || r.ID != "1" {
		t.Errorf("param rule not matched: %+v", r)
	}
	if r := matchRule(rules, "tpm", []string{}); r == nil || r.ID != "2" {
		t.Errorf("plain rule not matched: %+v", r)
	}
	// unmatched param falls back to plain rule
	if r := matchRule(rules, "tpm", []string{"other"}); r == nil || r.ID != "2" {
		t.Errorf("fallback to plain rule failed: %+v", r)
	}
	// exact wins over normal for bare calls
	if r := matchRule(rules, "eat", nil); r == nil || r.ID != "3" {
		t.Errorf("exact rule not matched: %+v", r)
	}
	// exact does not match calls with args
	if r := matchRule(rules, "eat", []string{"set"}); r == nil || r.ID != "4" {
		t.Errorf("exact should not match args: %+v", r)
	}
	if r := matchRule(rules, "nope", nil); r != nil {
		t.Errorf("unknown command matched: %+v", r)
	}
}

func TestRuleKey(t *testing.T) {
	a := CmdRule{Command: "c", Parameters: []string{"x"}}
	b := CmdRule{Command: "c", Parameters: []string{"x"}}
	if ruleKey(a) != ruleKey(b) {
		t.Error("same rules should have same key")
	}
	b.ExactMatch = true
	if ruleKey(a) == ruleKey(b) {
		t.Error("exactMatch must change the key")
	}
}

func TestFormatSeconds(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{30, "30s"}, {60, "1m"}, {3600, "1h"}, {86400, "1d"},
		{90061, "1d 1h 1m 1s"}, {0, "0s"},
	}
	for _, c := range cases {
		if got := formatSeconds(c.in); got != c.want {
			t.Errorf("formatSeconds(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitPrefix(t *testing.T) {
	pre, rest := splitPrefix(".ping x", []string{".", "!"})
	if pre != "." || rest != "ping x" {
		t.Errorf("splitPrefix = %q %q", pre, rest)
	}
	if pre, rest = splitPrefix("hello", []string{".", "!"}); pre != "" || rest != "hello" {
		t.Errorf("no prefix: %q %q", pre, rest)
	}
}

func TestPrunePending(t *testing.T) {
	now := time.Now()
	list := []PendingDel{
		{CID: 1, MID: 10, At: now.Add(time.Hour).Unix()},
		{CID: 1, MID: 10, At: now.Add(time.Hour).Unix()},      // dup
		{CID: 2, MID: 0, At: now.Add(time.Hour).Unix()},       // invalid
		{CID: 0, MID: 5, At: now.Add(time.Hour).Unix()},       // invalid
		{CID: 3, MID: 7, At: now.Add(-25 * time.Hour).Unix()}, // too old
		{CID: 4, MID: 8, At: now.Add(-10 * time.Hour).Unix()}, // overdue but kept
	}
	got := prunePending(list, now)
	if len(got) != 2 {
		t.Fatalf("prunePending kept %d, want 2: %+v", len(got), got)
	}
	if got[0].CID != 1 || got[1].CID != 4 {
		t.Errorf("prunePending order/content wrong: %+v", got)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// rules seed on first run
	p := &AutoDelPlugin{dir: dir, ttl: map[string]int{}}
	if err := p.loadRulesLocked(); err != nil {
		t.Fatal(err)
	}
	if len(p.rules) == 0 {
		t.Fatal("default rules not seeded")
	}
	for _, r := range p.rules {
		if r.ID == "" {
			t.Fatal("rule without ID after seeding")
		}
	}
	// persisted file exists and reloads identically
	q := &AutoDelPlugin{dir: dir}
	if err := q.loadRulesLocked(); err != nil {
		t.Fatal(err)
	}
	if len(q.rules) != len(p.rules) {
		t.Fatalf("rules not persisted: %d vs %d", len(q.rules), len(p.rules))
	}
	// broken rule entries are dropped, ids repaired
	bad := `{"rules":[{"id":"","command":"","delay":5},{"id":"9","command":"x","delay":0},{"command":"y","delay":7}]}`
	if err := os.WriteFile(filepath.Join(dir, rulesFile), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &AutoDelPlugin{dir: dir}
	if err := r.loadRulesLocked(); err != nil {
		t.Fatal(err)
	}
	if len(r.rules) != 1 || r.rules[0].Command != "y" || r.rules[0].ID == "" {
		t.Fatalf("repair failed: %+v", r.rules)
	}

	// TTL load/save
	if err := os.WriteFile(filepath.Join(dir, settingsFile), []byte(`{"0":300,"12345":10,"999":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ttl, err := loadTTL(filepath.Join(dir, settingsFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(ttl) != 2 || ttl["0"] != 300 || ttl["12345"] != 10 {
		t.Fatalf("loadTTL wrong: %+v", ttl)
	}

	// pending round trip
	pds := []PendingDel{{CID: 42, MID: 7, At: 12345, Resp: true}}
	if err := writeJSON(filepath.Join(dir, pendingFile), struct {
		Pending []PendingDel `json:"pending"`
	}{pds}); err != nil {
		t.Fatal(err)
	}
	back, err := loadPending(filepath.Join(dir, pendingFile))
	if err != nil || len(back) != 1 || back[0] != pds[0] {
		t.Fatalf("pending round trip failed: %+v %v", back, err)
	}
	// empty/missing file → no pending
	if back, err = loadPending(filepath.Join(dir, "missing.json")); err != nil || back != nil {
		t.Fatalf("missing pending file: %+v %v", back, err)
	}
}

func TestSchedulePersistsAndDedupes(t *testing.T) {
	dir := t.TempDir()
	p := &AutoDelPlugin{dir: dir, ttl: map[string]int{}}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer p.cancel()

	p.mu.Lock()
	p.pending = append(p.pending, PendingDel{CID: 1, MID: 1, At: 1})
	if err := p.savePendingLocked(); err != nil {
		t.Fatal(err)
	}
	p.mu.Unlock()

	back, err := loadPending(filepath.Join(dir, pendingFile))
	if err != nil || len(back) != 1 || back[0].CID != 1 || back[0].MID != 1 {
		t.Fatalf("pending not persisted: %+v %v", back, err)
	}
}

func TestDefaultRulesNoDupes(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range defaultRules() {
		if seen[ruleKey(r)] {
			t.Errorf("duplicate default rule: %+v", r)
		}
		seen[ruleKey(r)] = true
		if r.Delay < 1 {
			t.Errorf("rule with bad delay: %+v", r)
		}
		if _, err := strconv.Atoi(r.ID); err != nil {
			t.Errorf("non-numeric id: %+v", r)
		}
	}
}
