package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

var utc = time.UTC

func mustTask(t *testing.T, spec string, now time.Time) Task {
	t.Helper()
	sp, err := parseSpec(spec, utc)
	if err != nil {
		t.Fatalf("parseSpec(%q): %v", spec, err)
	}
	task, err := buildTask(sp, now, utc)
	if err != nil {
		t.Fatalf("buildTask(%q): %v", spec, err)
	}
	return task
}

func TestParseModes(t *testing.T) {
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, utc)
	cases := []struct {
		spec   string
		mode   string
		limit  int
		period int64
		at     time.Time
	}{
		{"every 1 minutes", modeInterval, -1, 60, time.Time{}},
		{"3 times 1 minutes", modeInterval, 3, 60, time.Time{}},
		{"every 1 hours 30 minutes", modeInterval, -1, 5400, time.Time{}},
		{"every 1h30m", modeInterval, -1, 5400, time.Time{}},
		{"every 2d", modeInterval, -1, 172800, time.Time{}},
		{"every 23:59:59 date", modeDaily, -1, 0, time.Time{}},
		{"every 08:00", modeDaily, -1, 0, time.Time{}},
		{"16:00:00 date", modeOnce, 1, 0, time.Date(2026, 3, 1, 16, 0, 0, 0, utc)},
		{"09:00", modeOnce, 1, 0, time.Date(2026, 3, 2, 9, 0, 0, 0, utc)},
		{"+5m", modeOnce, 1, 0, now.Add(5 * time.Minute)},
		{"10 seconds", modeOnce, 1, 0, now.Add(10 * time.Second)},
		{"2026-03-05 12:30", modeOnce, 1, 0, time.Date(2026, 3, 5, 12, 30, 0, 0, utc)},
	}
	for _, c := range cases {
		task := mustTask(t, c.spec, now)
		if task.Mode != c.mode || task.TimeLimit != c.limit || task.PeriodSec != c.period {
			t.Errorf("%q: got mode=%s limit=%d period=%d", c.spec, task.Mode, task.TimeLimit, task.PeriodSec)
		}
		if !c.at.IsZero() && task.At != c.at.Unix() {
			t.Errorf("%q: at=%v want %v", c.spec, time.Unix(task.At, 0).UTC(), c.at)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{"", "every", "5 parsecs", "25:00", "12:61:00 date", "every 0 seconds",
		"0 times 1 minutes", "every 2026-01-01 10:00", "+5m 10:00", "abc"} {
		if _, err := parseSpec(s, utc); err == nil {
			t.Errorf("%q: expected error", s)
		}
	}
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, utc)
	sp, err := parseSpec("2020-01-01 00:00", utc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildTask(sp, now, utc); err == nil {
		t.Error("past date should fail")
	}
}

func TestNextRun(t *testing.T) {
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, utc)
	iv := mustTask(t, "every 10 minutes", now)
	if got := nextRun(iv, now, utc); !got.Equal(now.Add(10 * time.Minute)) {
		t.Errorf("interval first: %v", got)
	}
	later := now.Add(25 * time.Minute)
	if got := nextRun(iv, later, utc); !got.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("interval later: %v", got)
	}
	d := mustTask(t, "every 09:30 date", now)
	if got := nextRun(d, now, utc); !got.Equal(time.Date(2026, 3, 2, 9, 30, 0, 0, utc)) {
		t.Errorf("daily: %v", got)
	}
	d2 := mustTask(t, "every 11:00", now)
	if got := nextRun(d2, now, utc); !got.Equal(time.Date(2026, 3, 1, 11, 0, 0, 0, utc)) {
		t.Errorf("daily same day: %v", got)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct{ in, spec, msg string }{
		{"every 1 minutes | hi | there", "every 1 minutes", "hi | there"},
		{"+5m hello world", "+5m", "hello world"},
		{"2026-01-01 12:00 happy\nnew year", "2026-01-01 12:00", "happy\nnew year"},
		{"18:00   dinner ", "18:00", "dinner"},
	}
	for _, c := range cases {
		spec, s, e, err := splitCommand(c.in)
		if err != nil || spec != c.spec || c.in[s:e] != c.msg {
			t.Errorf("%q: got %q %q %v", c.in, spec, c.in[s:e], err)
		}
	}
}

func TestSliceEntities(t *testing.T) {
	ents := []tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 0, Length: 7},   // in command part
		&tg.MessageEntityBold{Offset: 10, Length: 4},  // inside message
		&tg.MessageEntityItalic{Offset: 8, Length: 4}, // straddles start
	}
	got := sliceEntities(ents, 10, 20)
	if len(got) != 2 {
		t.Fatalf("got %d entities", len(got))
	}
	if b := got[0].(*tg.MessageEntityBold); b.Offset != 0 || b.Length != 4 {
		t.Errorf("bold: %+v", b)
	}
	if i := got[1].(*tg.MessageEntityItalic); i.Offset != 0 || i.Length != 2 {
		t.Errorf("italic: %+v", i)
	}
	if ents[1].(*tg.MessageEntityBold).Offset != 10 {
		t.Error("original mutated")
	}
	enc := encodeEntities(got)
	dec := decodeEntities(enc)
	if len(dec) != 2 || dec[0].(*tg.MessageEntityBold).Length != 4 {
		t.Errorf("roundtrip failed: %v", dec)
	}
	if utf16Len("a😀b") != 4 {
		t.Error("utf16Len")
	}
}

func TestPersistenceAndLifecycle(t *testing.T) {
	dir := t.TempDir()
	p := New()
	p.dir = dir
	now := time.Now()
	p.tasks = []*Task{
		{ID: 1, ChatID: 5, Msg: "a", Mode: modeInterval, PeriodSec: 60, Anchor: now.Unix(), TimeLimit: -1},
		{ID: 2, ChatID: 5, Msg: "b", Mode: modeOnce, At: now.Add(-48 * time.Hour).Unix(), TimeLimit: 1}, // stale
		{ID: 3, ChatID: 6, Msg: "c", Mode: modeDaily, Hour: 1, TimeLimit: 2, Pause: true},
		{ID: 4, ChatID: 6, Msg: "d", Mode: modeOnce, At: now.Add(-time.Minute).Unix(), TimeLimit: 1, Peer: &peerRef{Type: "chat", ID: 6}},
	}
	p.nextID = 5
	if err := p.saveLocked(); err != nil {
		t.Fatal(err)
	}

	q := New()
	q.dir = dir
	if err := q.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	n := len(q.tasks)
	_, paused := q.next[3]
	_, active := q.next[1]
	at4 := q.next[4]
	nextID := q.nextID
	q.mu.Unlock()
	if n != 3 || paused || !active || nextID != 5 {
		t.Errorf("restore: n=%d paused=%v active=%v nextID=%d", n, paused, active, nextID)
	}
	if at4.After(time.Now()) {
		t.Errorf("overdue one-shot should fire now, got %v", at4)
	}
	// no API: due task is postponed, not dropped
	time.Sleep(50 * time.Millisecond)
	q.mu.Lock()
	_, still := q.next[4]
	q.mu.Unlock()
	if !still {
		t.Error("task 4 dropped without api")
	}
	// simulate successful runs
	q.afterRun(4, nil)
	q.afterRun(1, nil)
	q.mu.Lock()
	if q.indexLocked(4) >= 0 {
		t.Error("one-shot not removed after run")
	}
	if i := q.indexLocked(1); i < 0 || q.tasks[i].Count != 1 {
		t.Error("interval count not updated")
	}
	q.mu.Unlock()

	done := make(chan struct{})
	go func() { _ = q.Stop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
	var sf storeFile
	if err := readJSON(filepath.Join(dir, tasksFile), &sf); err != nil || len(sf.Tasks) != 2 {
		t.Fatalf("persisted: %v %d", err, len(sf.Tasks))
	}
}

func TestTimeLimitFinishes(t *testing.T) {
	p := New()
	p.dir = t.TempDir()
	p.tasks = []*Task{{ID: 1, Msg: "x", Mode: modeInterval, PeriodSec: 1, Anchor: time.Now().Unix(), TimeLimit: 2}}
	p.next[1] = time.Now()
	p.afterRun(1, nil)
	if len(p.tasks) != 1 || p.tasks[0].TimeLimit != 1 {
		t.Fatal("first run")
	}
	p.afterRun(1, nil)
	if len(p.tasks) != 0 {
		t.Fatal("task should be removed after limit")
	}
}

func TestValidTimezone(t *testing.T) {
	if got, err := validTimezone(" Asia/Shanghai "); err != nil || got != "Asia/Shanghai" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := validTimezone(""); err != nil || got != "" {
		t.Fatal("empty must clear")
	}
	if got, err := validTimezone("local"); err != nil || got != "" {
		t.Fatal("local must clear")
	}
	if _, err := validTimezone("Mars/Olympus"); err == nil {
		t.Fatal("bad zone accepted")
	}
}
