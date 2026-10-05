package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func itoa(n int) string { return strconv.Itoa(n) }

func TestParsePeriod(t *testing.T) {
	now := time.Date(2025, time.October, 5, 12, 0, 0, 0, time.Local)
	// Sanity: plain empty arg in October 2025 is the current year (the
	// January special case is covered separately).
	if p, ok := parsePeriod("", now); !ok || p.Label != "2025" || p.From.Year() != 2025 {
		t.Fatalf("empty: %+v ok=%v", p, ok)
	}
	// January keeps last year as the year in review (source behavior).
	jan := time.Date(2025, time.January, 15, 0, 0, 0, 0, time.Local)
	if p, ok := parsePeriod("", jan); !ok || p.Label != "2024" {
		t.Fatalf("january: %+v ok=%v", p, ok)
	}
	if p, ok := parsePeriod("2025", now); !ok || p.Label != "2025" || p.From.Month() != time.January || p.To.Year() != 2026 {
		t.Fatalf("year: %+v ok=%v", p, ok)
	}
	if p, ok := parsePeriod("2025-06", now); !ok || p.Label != "2025-06" || p.From.Month() != time.June || p.To.Month() != time.July {
		t.Fatalf("month: %+v ok=%v", p, ok)
	}
	if p, ok := parsePeriod("202506", now); !ok || p.Label != "2025-06" {
		t.Fatalf("month compact: %+v ok=%v", p, ok)
	}
	if p, ok := parsePeriod("2025-06-01", now); !ok || p.Label != "2025-06-01" || p.To.Sub(p.From) != 24*time.Hour {
		t.Fatalf("day: %+v ok=%v", p, ok)
	}
	if p, ok := parsePeriod("all", now); !ok || !p.From.IsZero() || !p.To.IsZero() || p.Label != "all" {
		t.Fatalf("all: %+v ok=%v", p, ok)
	}
	if p, ok := parsePeriod("month", now); !ok || p.Label != "2025-10" {
		t.Fatalf("month keyword: %+v ok=%v", p, ok)
	}
	if _, ok := parsePeriod("banana", now); ok {
		t.Fatal("banana should not parse")
	}
	if _, ok := parsePeriod("2025-13", now); ok {
		t.Fatal("month 13 should not parse")
	}
}

func TestAggregateBinningAndRanking(t *testing.T) {
	// 2025-03-10 is a Monday (UTC-independent when constructed local).
	base := time.Date(2025, time.March, 10, 0, 0, 0, 0, time.Local)
	var samples []sample
	// alice (1): 3 messages at 09:xx Monday, 1 at 22:xx Tuesday
	for i := 0; i < 3; i++ {
		samples = append(samples, sample{Date: base.Add(9*time.Hour + time.Duration(i)*time.Minute).Unix(), Sender: 1, Kind: "text"})
	}
	samples = append(samples, sample{Date: base.AddDate(0, 0, 1).Add(22 * time.Hour).Unix(), Sender: 1, Kind: "photo"})
	// bob (2): 2 stickers late night
	for i := 0; i < 2; i++ {
		samples = append(samples, sample{Date: base.Add(2 * time.Hour).Unix(), Sender: 2, Kind: "sticker"})
	}
	// outside the period: 1999 message from eve
	samples = append(samples, sample{Date: time.Date(1999, time.January, 1, 0, 0, 0, 0, time.Local).Unix(), Sender: 3, Kind: "text"})

	from := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.Local)
	to := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.Local)
	r := aggregate(-1001234, "2025", samples, from, to)

	if r.Messages != 6 {
		t.Fatalf("messages = %d", r.Messages)
	}
	if r.Senders != 2 {
		t.Fatalf("senders = %d", r.Senders)
	}
	if r.Media != 3 || r.MediaMix["photo"] != 1 || r.MediaMix["sticker"] != 2 {
		t.Fatalf("media = %d mix = %v", r.Media, r.MediaMix)
	}
	// hour bins: 02 (x2), 09 (x3), 22 (x1)
	if r.Hours[2] != 2 || r.Hours[9] != 3 || r.Hours[22] != 1 {
		t.Fatalf("hours = %v", r.Hours)
	}
	// weekday bins: Monday 5 (3 morning + 2 late-night), Tuesday 1 (22:xx)
	if r.Weekdays[time.Monday] != 5 || r.Weekdays[time.Tuesday] != 1 {
		t.Fatalf("weekdays = %v", r.Weekdays)
	}
	// ranking: alice first (4), bob second (2)
	if len(r.Top) != 2 || r.Top[0].ID != 1 || r.Top[0].N != 4 || r.Top[1].ID != 2 || r.Top[1].N != 2 {
		t.Fatalf("top = %+v", r.Top)
	}
	// ties broken by id
	var tie []sample
	for i := 0; i < 2; i++ {
		tie = append(tie,
			sample{Date: base.Add(time.Duration(i) * time.Hour).Unix(), Sender: 9, Kind: "text"},
			sample{Date: base.Add(time.Duration(i) * time.Hour).Unix(), Sender: 5, Kind: "text"})
	}
	rt := aggregate(1, "2025", tie, from, to)
	if rt.Top[0].ID != 5 || rt.Top[1].ID != 9 {
		t.Fatalf("tie order = %+v", rt.Top)
	}
	// days: monday + tuesday = 2 distinct
	if r.Days != 2 {
		t.Fatalf("days = %d", r.Days)
	}
}

func TestAggregateAllUnbounded(t *testing.T) {
	var samples []sample
	for i := 0; i < 5; i++ {
		samples = append(samples, sample{Date: time.Date(2001+i, 1, 1, 0, 0, 0, 0, time.Local).Unix(), Sender: 1, Kind: "text"})
	}
	r := aggregate(1, "all", samples, time.Time{}, time.Time{})
	if r.Messages != 5 || r.Days != 5 {
		t.Fatalf("all: %+v", r)
	}
}

func TestMediaKind(t *testing.T) {
	cases := []struct {
		msg  *tg.Message
		kind string
	}{
		{&tg.Message{Message: "hi"}, "text"},
		{&tg.Message{Media: &tg.MessageMediaPhoto{}}, "photo"},
		{&tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{
			MimeType: "video/mp4",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeVideo{},
			},
		}}}, "video"},
		{&tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{
			MimeType: "audio/ogg",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeAudio{Voice: true},
			},
		}}}, "voice"},
		{&tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{
			MimeType: "image/webp",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeSticker{},
			},
		}}}, "sticker"},
		{&tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{
			MimeType: "image/gif",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeAnimated{},
			},
		}}}, "gif"},
		{&tg.Message{Media: &tg.MessageMediaWebPage{}}, "text"},
	}
	for i, c := range cases {
		if k, _ := mediaKind(c.msg); k != c.kind {
			t.Fatalf("case %d: got %s want %s", i, k, c.kind)
		}
	}
}

func TestToSamples(t *testing.T) {
	msgs := []*tg.Message{
		{ID: 1, Date: 100, FromID: &tg.PeerUser{UserID: 7}, Message: "x"},
		{ID: 2, Date: 200, PeerID: &tg.PeerUser{UserID: 9}}, // channel-style, sender 0
	}
	got := toSamples(msgs, map[int64]string{7: "alice"})
	if len(got) != 2 || got[0].Sender != 7 || got[0].Kind != "text" || got[1].Sender != 9 {
		t.Fatalf("samples = %+v", got)
	}
}

func TestSplitMessage(t *testing.T) {
	// short message stays whole
	if parts := splitMessage("hello"); len(parts) != 1 {
		t.Fatalf("short: %d", len(parts))
	}
	// a long message splits on line boundaries with part markers
	line := strings.Repeat("x", 100)
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString(line + "\n")
	}
	parts := splitMessage(b.String())
	if len(parts) < 2 {
		t.Fatalf("long: %d parts", len(parts))
	}
	for i, p := range parts {
		if len([]rune(p)) > reportMaxRunes+20 {
			t.Fatalf("part %d too long: %d", i, len([]rune(p)))
		}
		if !strings.Contains(p, "("+itoa(i+1)+"/"+itoa(len(parts))+")") {
			t.Fatalf("part %d missing marker", i)
		}
	}
	joined := strings.Join(parts, "")
	for i := range parts {
		joined = strings.Replace(joined, "("+itoa(i+1)+"/"+itoa(len(parts))+")", "", 1)
	}
	if strings.ReplaceAll(joined, "\n", "") != strings.ReplaceAll(b.String(), "\n", "") {
		t.Fatal("content lost while splitting")
	}
}

func TestRenderChat(t *testing.T) {
	tl := func(zh, en string) string { return en }
	r := &Report{
		ChatID: -1001234, Period: "2025", Title: "Test Group",
		Messages: 100, Senders: 8, Media: 30, Days: 10,
		Top:   []senderCount{{ID: 1, N: 50}, {ID: 2, N: 20}},
		Names: map[int64]string{1: "Alice_B", 2: "Bob"},
		First: time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC).Unix(),
		Last:  time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC).Unix(),
	}
	r.Hours[14] = 42
	r.Weekdays[time.Friday] = 60
	r.MediaMix = map[string]int{"photo": 20, "sticker": 10}
	parts := renderChat(tl, r, "")
	s := strings.Join(parts, "\n---\n")
	for _, want := range []string{"Test Group", "100", "Alice\\_B", "14:00", "Friday", "photo", "10/day"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	// empty period
	parts = renderChat(tl, &Report{Period: "2025"}, "")
	if !strings.Contains(parts[0], "No messages") {
		t.Fatalf("empty: %s", parts[0])
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey(-1001234, "2025", "zh-CN")
	if _, hit := loadCacheEntry(dir, key); hit {
		t.Fatal("cache should start empty")
	}
	r := &Report{ChatID: -1001234, Period: "2025", Messages: 5, Days: 1,
		Top: []senderCount{{ID: 1, N: 5}}, MediaMix: map[string]int{"photo": 2}, Names: map[int64]string{1: "a"}}
	saveCacheEntry(dir, key, &cacheEntry{GeneratedAt: 123, Report: r})
	e, hit := loadCacheEntry(dir, key)
	if !hit || e.Report == nil || e.Report.Messages != 5 || e.Report.Top[0].N != 5 || e.Report.MediaMix["photo"] != 2 {
		t.Fatalf("cache read: %+v hit=%v", e, hit)
	}
	// eviction keeps only cacheLimit entries
	for i := 0; i < cacheLimit+5; i++ {
		saveCacheEntry(dir, cacheKey(int64(i), "x", "zh"), &cacheEntry{Report: r})
	}
	if e, hit := loadCacheEntry(dir, key); hit {
		_ = e
		// oldest entry (saved first) must have been evicted
		t.Fatal("old entry not evicted")
	}
}

func TestStateAndRunDays(t *testing.T) {
	dir := t.TempDir()
	s := loadState(dir + "/stats.json")
	if s.StartTime == 0 {
		t.Fatal("startTime must default to now")
	}
	s.ReportCount = 3
	if err := writeJSON(dir+"/stats.json", s); err != nil {
		t.Fatal(err)
	}
	if s2 := loadState(dir + "/stats.json"); s2.ReportCount != 3 {
		t.Fatalf("state: %+v", s2)
	}
}

func TestHourBar(t *testing.T) {
	if hourBar(0, 10) != "" {
		t.Fatal("zero count must render empty bar")
	}
	if hourBar(10, 10) != strings.Repeat("▇", barWidth) {
		t.Fatal("full bar")
	}
	if hourBar(1, 100) != "▇" {
		t.Fatal("small nonzero count gets one cell")
	}
}

func TestHourHistogramMergesPairs(t *testing.T) {
	tl := func(zh, en string) string { return en }
	r := &Report{}
	r.Hours[3] = 10 // odd-hour bucket: must drive the 02–04 bar
	r.Hours[4] = 5
	s := hourHistogram(tl, r)
	// 12 rows for 24 buckets, 2h each
	if got := strings.Count(s, "\n"); got != 13 { // 12 rows + peak line
		t.Fatalf("rows = %d:\n%s", got, s)
	}
	for h := 0; h < 24; h += 2 {
		if !strings.Contains(s, fmt.Sprintf("%02d–%02d", h, h+2)) {
			t.Fatalf("missing %02d–%02d row:\n%s", h, h+2, s)
		}
	}
	// 02–04 bar spans both hours (10+5=15 = max → full width)
	if !strings.Contains(s, "02–04 "+strings.Repeat("▇", barWidth)) {
		t.Fatalf("02–04 bar not merged/max:\n%s", s)
	}
	// empty pairs render no bar cells
	if strings.Contains(s, "06–08 ▇") {
		t.Fatalf("empty bucket drew a bar:\n%s", s)
	}
	// peak line still reports the exact odd hour
	if !strings.Contains(s, "03:00") {
		t.Fatalf("peak hour line missing:\n%s", s)
	}
}

func TestStripRefresh(t *testing.T) {
	cases := []struct {
		in      []string
		out     []string
		refresh bool
	}{
		{nil, nil, false},
		{[]string{}, []string{}, false},
		{[]string{"2025"}, []string{"2025"}, false},
		{[]string{"refresh"}, []string{}, true},
		{[]string{"refresh", "2025"}, []string{"2025"}, true},
		{[]string{"2025", "refresh"}, []string{"2025"}, true},
		{[]string{"2025", "Refresh", "refresh"}, []string{"2025"}, true},
		{[]string{"2025", " refresh "}, []string{"2025"}, true},
		{[]string{"all", "REFRESH"}, []string{"all"}, true},
		{[]string{"refreshing"}, []string{"refreshing"}, false},
	}
	for i, c := range cases {
		refresh := false
		got := stripRefresh(c.in, &refresh)
		if refresh != c.refresh {
			t.Fatalf("case %d: refresh = %v, want %v", i, refresh, c.refresh)
		}
		if len(got) != len(c.out) {
			t.Fatalf("case %d: args = %v, want %v", i, got, c.out)
		}
		for j := range got {
			if got[j] != c.out[j] {
				t.Fatalf("case %d: args = %v, want %v", i, got, c.out)
			}
		}
	}
}
