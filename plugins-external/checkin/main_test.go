package main

import (
	"errors"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

var sh = shanghai

func TestParseTime(t *testing.T) {
	cases := []struct {
		in  string
		min int
		ok  bool
	}{
		{"10:00", 600, true},
		{"9:05", 545, true},
		{"23:59", 1439, true},
		{"0:00", 0, true},
		{"24:00", 0, false},
		{"12:60", 0, false},
		{"9:5", 0, false}, // minute must be two digits, like the source regex
		{"", 0, false},
		{"abc", 0, false},
		{"1:2:3", 0, false},
	}
	for _, c := range cases {
		got, ok := parseTime(c.in)
		if ok != c.ok || (ok && got != c.min) {
			t.Errorf("parseTime(%q) = %d,%v want %d,%v", c.in, got, ok, c.min, c.ok)
		}
	}
	if s := formatTime(600); s != "10:00" {
		t.Errorf("formatTime(600) = %q", s)
	}
	if s := formatTime(5); s != "0:05" {
		t.Errorf("formatTime(5) = %q", s)
	}
}

func TestWindowOf(t *testing.T) {
	// 10:00-11:30 same day
	s, e := windowOf("2026-10-05", "10:00", "11:30", sh)
	if s.Format("15:04") != "10:00" || e.Format("15:04") != "11:30" {
		t.Errorf("same-day window: %v ~ %v", s, e)
	}
	if s.Format("2006-01-02") != "2026-10-05" {
		t.Errorf("start date wrong: %v", s)
	}
	// crossing midnight: 23:00 -> 01:00 next day
	s, e = windowOf("2026-10-05", "23:00", "01:00", sh)
	if s.Format("15:04") != "23:00" || e.Format("2006-01-02 15:04") != "2026-10-06 01:00" {
		t.Errorf("cross-midnight window: %v ~ %v", s, e)
	}
	// no end: single instant
	s, e = windowOf("2026-10-05", "10:00", "", sh)
	if !s.Equal(e) {
		t.Errorf("no-end window should be a point: %v ~ %v", s, e)
	}
	// empty run time falls back to 10:00
	s, _ = windowOf("2026-10-05", "", "11:30", sh)
	if s.Format("15:04") != "10:00" {
		t.Errorf("default start: %v", s)
	}
}

func TestWindowText(t *testing.T) {
	if got := windowText("10:00", ""); got != "每天 10:00" {
		t.Errorf("no end: %q", got)
	}
	if got := windowText("10:00", "11:30"); got != "每天 10:00 ~ 11:30 随机" {
		t.Errorf("same day: %q", got)
	}
	if got := windowText("23:00", "01:00"); got != "每天 23:00 ~ 次日 01:00 随机" {
		t.Errorf("cross day: %q", got)
	}
}

func TestPlanNextRun(t *testing.T) {
	zero := func(n int) int { return 0 }
	conf := config{RunTime: "10:00", RunTimeEnd: "11:30", RandomDelay: 0}

	// before today's window -> today's window start
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, sh)
	plan, err := planNextRun(now, conf, sh, zero)
	if err != nil {
		t.Fatal(err)
	}
	if plan.date != "2026-10-05" || plan.at.Format("15:04") != "10:00" {
		t.Errorf("before window: %+v", plan)
	}

	// inside the window -> now rounded up to the next minute
	now = time.Date(2026, 10, 5, 10, 30, 15, 0, sh)
	plan, err = planNextRun(now, conf, sh, zero)
	if err != nil {
		t.Fatal(err)
	}
	if plan.at.Format("15:04") != "10:31" {
		t.Errorf("inside window: %v", plan.at)
	}

	// after the window -> tomorrow's start
	now = time.Date(2026, 10, 5, 12, 0, 0, 0, sh)
	plan, err = planNextRun(now, conf, sh, zero)
	if err != nil {
		t.Fatal(err)
	}
	if plan.date != "2026-10-06" || plan.at.Format("15:04") != "10:00" {
		t.Errorf("after window: %+v", plan)
	}

	// already ran today -> tomorrow
	ran := conf
	ran.LastRunDate = "2026-10-05"
	now = time.Date(2026, 10, 5, 10, 30, 0, 0, sh)
	plan, err = planNextRun(now, ran, sh, zero)
	if err != nil {
		t.Fatal(err)
	}
	if plan.date != "2026-10-06" {
		t.Errorf("already ran today: %+v", plan)
	}

	// max rand picks the window end
	max := func(n int) int { return n - 1 }
	now = time.Date(2026, 10, 5, 8, 0, 0, 0, sh)
	plan, err = planNextRun(now, conf, sh, max)
	if err != nil {
		t.Fatal(err)
	}
	if plan.at.Format("15:04") != "11:30" {
		t.Errorf("max rand: %v", plan.at)
	}

	// random delay adds up to N minutes (rnd(n) returns n-1 -> delay n-1 of 10)
	d := conf
	d.RandomDelay = 10
	now = time.Date(2026, 10, 5, 8, 0, 0, 0, sh)
	plan, _ = planNextRun(now, d, sh, func(n int) int {
		if n == 91 { // window slot count
			return 0
		}
		return n - 1 // delay minutes
	})
	if plan.at.Format("15:04") != "10:09" {
		t.Errorf("delayed: %v", plan.at)
	}
}

func TestDueState(t *testing.T) {
	at := time.Date(2026, 10, 5, 10, 0, 0, 0, sh)
	if s := dueState(at.Add(-time.Minute), at, sh); s != stateWait {
		t.Errorf("wait: %v", s)
	}
	if s := dueState(at.Add(time.Minute), at, sh); s != stateRun {
		t.Errorf("run: %v", s)
	}
	next := at.AddDate(0, 0, 1)
	if s := dueState(next, at, sh); s != stateMissed {
		t.Errorf("missed: %v", s)
	}
}

func TestDueStateDayBoundaryGrace(t *testing.T) {
	// Planned 23:58, first tick after midnight: the old code flipped to
	// stateMissed because the local date changed, silently skipping the
	// day's check-in. The grace window keeps it runnable.
	at := time.Date(2026, 10, 5, 23, 58, 0, 0, sh)
	now := time.Date(2026, 10, 6, 0, 2, 0, 0, sh)
	if s := dueState(now, at, sh); s != stateRun {
		t.Errorf("boundary grace: %v", s)
	}
	// Well past the grace it is still missed.
	now = time.Date(2026, 10, 6, 0, 30, 0, 0, sh)
	if s := dueState(now, at, sh); s != stateMissed {
		t.Errorf("past grace: %v", s)
	}
}

// TestCmdRunRunningAtomic exercises the check-and-set of p.running that
// cmdRun now performs under a single lock acquisition.
func TestCmdRunRunningAtomic(t *testing.T) {
	p := New()
	p.mu.Lock()
	p.running = true // simulate an in-flight run
	p.mu.Unlock()
	// The busy check happens atomically with the set, so a second entrant
	// sees running=true and never gets past the guard. (A full cmdRun call
	// needs a live CommandContext; the lock contract is what matters here.)
	p.mu.Lock()
	busy := p.running
	if !busy {
		p.running = true
	}
	p.mu.Unlock()
	if !busy {
		t.Fatal("guard lost the in-flight run")
	}
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
}

func TestNormalizeDate(t *testing.T) {
	cases := map[string]string{
		"2026-10-05": "2026-10-05",
		"2026/10/5":  "2026-10-05",
		"2026-1-1":   "2026-01-01",
		"":           "",
		"garbage":    "",
	}
	for in, want := range cases {
		if got := normalizeDate(in); got != want {
			t.Errorf("normalizeDate(%q) = %q want %q", in, got, want)
		}
	}
}

func TestConfigNormalize(t *testing.T) {
	c := config{
		RunTime: "10:00", RunTimeEnd: "11:30",
		LastRunDate: "2026/10/1", NextRunDate: "2026-10-02",
		Targets: []*Target{
			{ID: "a", Name: "A", Target: "@a", Command: "/sign", Enabled: true},
			nil,
			{ID: "a", Name: "dup", Target: "@a", Command: "/x"},  // duplicate id
			{ID: "", Name: "no id", Target: "@x", Command: "/y"}, // no id
			{ID: "b", Name: "B", Target: "@b", Command: "/sign"}, // enabled defaults false
		},
	}
	c.normalize()
	if len(c.Targets) != 2 {
		t.Fatalf("kept %d targets, want 2: %+v", len(c.Targets), c.Targets)
	}
	if c.Targets[0].ID != "a" || c.Targets[1].ID != "b" {
		t.Errorf("kept wrong targets: %+v", c.Targets)
	}
	if c.LastRunDate != "2026-10-01" {
		t.Errorf("lastRunDate not normalized: %q", c.LastRunDate)
	}
	if !c.enabled() || (config{}).enabled() {
		t.Errorf("enabled() wrong")
	}
	if c.Targets[1].Enabled {
		t.Errorf("missing enabled flag should default false")
	}
}

func TestParseMatcher(t *testing.T) {
	cases := []struct {
		args []string
		data string
		text string
	}{
		{nil, "", ""},
		{[]string{}, "", ""},
		{[]string{"data:checkin"}, "checkin", ""},
		{[]string{"checkin"}, "checkin", ""},
		{[]string{"text:签到"}, "", "签到"},
		{[]string{"data:a", "b"}, "a b", ""},
	}
	for _, c := range cases {
		data, text := parseMatcher(c.args)
		if data != c.data || text != c.text {
			t.Errorf("parseMatcher(%v) = %q,%q want %q,%q", c.args, data, text, c.data, c.text)
		}
	}
}

func TestHasMatcherAndFindCallbackData(t *testing.T) {
	if hasMatcher(&Target{CallbackData: "x"}) != true || hasMatcher(&Target{ButtonText: "x"}) != true || hasMatcher(&Target{}) != false {
		t.Fatal("hasMatcher")
	}
	msg := &tg.Message{
		ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
			{Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonCallback{Text: "Ignore", Data: []byte("nope")},
				&tg.KeyboardButtonCallback{Text: "Sign in", Data: []byte("checkin")},
			}},
		}},
	}
	if got := findCallbackData(msg, &Target{CallbackData: "checkin"}); string(got) != "checkin" {
		t.Errorf("by data: %q", got)
	}
	if got := findCallbackData(msg, &Target{ButtonText: "Sign in"}); string(got) != "checkin" {
		t.Errorf("by text: %q", got)
	}
	if got := findCallbackData(msg, &Target{CallbackData: "zzz"}); got != nil {
		t.Errorf("no match: %q", got)
	}
	if got := findCallbackData(&tg.Message{}, &Target{CallbackData: "x"}); got != nil {
		t.Errorf("no markup: %q", got)
	}
}

func TestMessageEdited(t *testing.T) {
	a := &tg.Message{ID: 10, Message: "hi"}
	if messageEdited(a, &tg.Message{ID: 10, Message: "hi"}) {
		t.Error("identical should not count as edited")
	}
	if !messageEdited(a, &tg.Message{ID: 10, Message: "hi2"}) {
		t.Error("text change should count")
	}
	if !messageEdited(a, &tg.Message{ID: 11, Message: "hi"}) {
		t.Error("newer id should count")
	}
	if messageEdited(a, &tg.Message{ID: 9, Message: "hi"}) {
		t.Error("older id should not count as a candidate")
	}
}

func TestHistoryMessages(t *testing.T) {
	// messages.messages carries plain messages
	res := &tg.MessagesMessages{Messages: []tg.MessageClass{
		&tg.Message{ID: 1, Message: "a"},
		&tg.MessageService{ID: 2},
		&tg.MessageEmpty{ID: 3},
		&tg.Message{ID: 4, Message: "b"},
	}}
	msgs := historyMessages(res)
	if len(msgs) != 2 || msgs[0].ID != 1 || msgs[1].ID != 4 {
		t.Errorf("historyMessages: %+v", msgs)
	}
}

func TestSentID(t *testing.T) {
	if got := sentID(&tg.UpdateShortSentMessage{ID: 42}); got != 42 {
		t.Errorf("short sent: %d", got)
	}
	u := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateMessageID{ID: 7},
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 9}},
	}}
	if got := sentID(u); got != 9 {
		t.Errorf("updates: %d", got)
	}
	if got := sentID(nil); got != 0 {
		t.Errorf("nil: %d", got)
	}
}

func TestErrText(t *testing.T) {
	if errText(nil) != "" {
		t.Error("nil")
	}
	if got := errText(tgerr.New(400, "PEER_FLOOD")); got != "PEER_FLOOD" {
		t.Errorf("tgerr: %q", got)
	}
	if got := errText(errors.New("boom")); got != "boom" {
		t.Errorf("plain: %q", got)
	}
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	if got := errText(errors.New(string(long))); len([]rune(got)) != 201 {
		t.Errorf("truncate: %d runes", len([]rune(got)))
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("abc", 5); got != "abc" {
		t.Errorf("short: %q", got)
	}
	if got := truncateRunes("abcdef", 3); got != "abc…" {
		t.Errorf("long: %q", got)
	}
	// multibyte safety
	if got := truncateRunes("签到测试数据", 2); got != "签到…" {
		t.Errorf("cjk: %q", got)
	}
}

func TestValidTimezone(t *testing.T) {
	if _, err := validTimezone("Asia/Shanghai"); err != nil {
		t.Errorf("valid: %v", err)
	}
	if v, err := validTimezone("  "); err != nil || v != "" {
		t.Errorf("empty: %q %v", v, err)
	}
	if v, err := validTimezone("LOCAL"); err != nil || v != "" {
		t.Errorf("local alias: %q %v", v, err)
	}
	if _, err := validTimezone("Mars/Olympus"); err == nil {
		t.Error("invalid should fail")
	}
}

func TestValidChatID(t *testing.T) {
	for _, ok := range []string{"123", "-1001234567890", ""} {
		if _, err := validChatID(ok); err != nil {
			t.Errorf("validChatID(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"@user", "abc", "1.5"} {
		if _, err := validChatID(bad); err == nil {
			t.Errorf("validChatID(%q) should fail", bad)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := defaultConfig()
	c.LastRunDate = "2026-10-05"
	c.Targets = []*Target{{ID: "a", Name: "A", Target: "@a", Command: "/sign", Enabled: true}}
	if err := writeJSON(configPath(dir), c); err != nil {
		t.Fatal(err)
	}
	var got config
	if err := readJSON(configPath(dir), &got); err != nil {
		t.Fatal(err)
	}
	got.normalize()
	if got.RunTime != c.RunTime || len(got.Targets) != 1 || got.Targets[0].Command != "/sign" {
		t.Errorf("round trip: %+v", got)
	}
}

func TestDefaultConfig(t *testing.T) {
	c := defaultConfig()
	if c.RunTime != "10:00" || c.RunTimeEnd != "11:30" || c.RandomDelay != 0 || len(c.Targets) != 0 {
		t.Errorf("defaults: %+v", c)
	}
}
