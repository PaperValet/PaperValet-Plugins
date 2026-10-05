package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSplitNameItem(t *testing.T) {
	cases := []struct {
		item        string
		first, last string
		setLast     bool
	}{
		{"张三|李", "张三", "李", true},
		{"张三|", "张三", "", true},
		{"张三", "张三", "", false},
		{"|李", "", "李", true},
		{"A|B|C", "A", "B|C", true},
	}
	for _, c := range cases {
		f, l, s := splitNameItem(c.item)
		if f != c.first || l != c.last || s != c.setLast {
			t.Errorf("splitNameItem(%q) = (%q,%q,%v), want (%q,%q,%v)", c.item, f, l, s, c.first, c.last, c.setLast)
		}
	}
}

func TestValidateItem(t *testing.T) {
	if err := validateItem("张三", targetName); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
	if err := validateItem("张三|李", targetName); err != nil {
		t.Errorf("valid full name rejected: %v", err)
	}
	if err := validateItem("|李", targetName); err == nil {
		t.Error("empty first name accepted")
	}
	long := make([]rune, 65)
	for i := range long {
		long[i] = '张'
	}
	if err := validateItem(string(long), targetName); err == nil {
		t.Error("over-long name accepted")
	}
	if err := validateItem("在摸鱼", targetBio); err != nil {
		t.Errorf("valid bio rejected: %v", err)
	}
	bio := make([]rune, 71)
	if err := validateItem(string(bio), targetBio); err == nil {
		t.Error("over-long bio accepted")
	}
	for _, u := range []string{"abc", "ab", "1abcd", "abcd", "overly_long_username_xxxxxxxxxxxx"} {
		if err := validateItem(u, targetUsername); err == nil {
			t.Errorf("invalid username accepted: %q", u)
		}
	}
	for _, u := range []string{"valid_user01", "user_01", "A1234567890123456789012345678"} {
		if err := validateItem(u, targetUsername); err != nil {
			t.Errorf("valid username rejected: %q: %v", u, err)
		}
	}
}

func TestParseItems(t *testing.T) {
	// Target is name: usernames are ordinary text there, "bad__u" is a
	// valid name; only the duplicate and the empty-line handling matter.
	added, dup, invalid := parseItems("张三|李\n\n张三|李\n摸鱼中\nbad__u\n张三|李\r\n", targetName,
		[]string{"张三"})
	if len(added) != 3 || added[0] != "张三|李" || added[1] != "摸鱼中" || added[2] != "bad__u" {
		t.Errorf("added = %v, want [张三|李 摸鱼中 bad__u]", added)
	}
	if len(dup) != 2 || dup[0] != "张三|李" {
		t.Errorf("dup = %v, want two 张三|李", dup)
	}
	if len(invalid) != 0 {
		t.Errorf("invalid = %v, want none", invalid)
	}
}

func TestParseItemsUsername(t *testing.T) {
	_, _, invalid := parseItems("good_user\nab\n1abc\n", targetUsername, nil)
	if len(invalid) != 2 {
		t.Errorf("invalid = %v, want [ab 1abc]", invalid)
	}
	added, _, _ := parseItems("good_user\n", targetUsername, nil)
	if len(added) != 1 || added[0] != "good_user" {
		t.Errorf("added = %v", added)
	}
}

func TestParseItemsCap(t *testing.T) {
	existing := make([]string, 99)
	lines := "a\nb\nc\nd"
	added, _, _ := parseItems(lines, targetBio, existing)
	if len(added) != 1 {
		t.Errorf("cap not enforced: added = %v", added)
	}
}

func TestNextIndex(t *testing.T) {
	if got := nextIndex(0, 0, false, nil); got != 0 {
		t.Errorf("nextIndex(0) = %d", got)
	}
	if got := nextIndex(1, 0, true, nil); got != 0 {
		t.Errorf("nextIndex(1) = %d", got)
	}
	// Sequential wraps.
	if got := nextIndex(3, 2, false, nil); got != 0 {
		t.Errorf("sequential wrap = %d, want 0", got)
	}
	if got := nextIndex(3, 0, false, nil); got != 1 {
		t.Errorf("sequential = %d, want 1", got)
	}
	// Random never returns the current index and stays in range.
	seen := map[int]bool{}
	for i := 0; i < 100; i++ {
		got := nextIndex(3, 1, true, func(n int) int { return i % n })
		if got == 1 || got < 0 || got > 2 {
			t.Fatalf("random index %d out of range or repeats current", got)
		}
		seen[got] = true
	}
	if !seen[0] || !seen[2] {
		t.Errorf("random never reached both alternatives: %v", seen)
	}
}

func TestNextDue(t *testing.T) {
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

	// First fire with an interval is immediate.
	d, err := nextDue(time.Time{}, 60, "", now)
	if err != nil || !d.Equal(now) {
		t.Errorf("first interval fire = %v, %v; want now", d, err)
	}
	// Interval fires every interval minutes.
	d, err = nextDue(now.Add(-30*time.Minute), 60, "", now)
	if err != nil || d.Sub(now) != 30*time.Minute {
		t.Errorf("interval due = %v, %v", d, err)
	}
	// Interval clamped to a minimum of one minute.
	d, _ = nextDue(now.Add(-2*time.Second), 0, "", now)
	if d.Sub(now) > time.Minute {
		t.Errorf("min interval not clamped: %v", d)
	}
	// Cron wins over the interval: 0 9 * * * fired at 10:00 → next 09:00.
	d, err = nextDue(now.Add(-time.Hour), 5, "0 9 * * *", now)
	want := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	if err != nil || !d.Equal(want) {
		t.Errorf("cron due = %v, %v; want %v", d, err, want)
	}
	// First cron fire after zero lastFire.
	d, err = nextDue(time.Time{}, 5, "30 10 * * *", now)
	want = now.Add(30 * time.Minute)
	if err != nil || !d.Equal(want) {
		t.Errorf("first cron due = %v, %v; want %v", d, err, want)
	}
	// Bad cron errors out.
	if _, err := nextDue(time.Time{}, 5, "not a cron", now); err == nil {
		t.Error("invalid cron accepted")
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := newStateStore(filepath.Join(dir, "state.json"))
	if err := s.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := s.items(targetName); len(got) != 0 {
		t.Errorf("fresh store has items: %v", got)
	}
	if n, err := s.addItems(targetName, []string{"A", "B"}, nil, nil); err != nil || n != 2 {
		t.Fatalf("addItems: %d, %v", n, err)
	}
	if _, err := s.deleteItem(targetName, 3); !errors.Is(err, os.ErrInvalid) {
		t.Errorf("delete out of range: %v", err)
	}
	removed, err := s.deleteItem(targetName, 1)
	if err != nil || removed != "A" {
		t.Errorf("deleteItem = %q, %v", removed, err)
	}
	if err := s.setIndex(targetBio, 4); err != nil {
		t.Fatalf("setIndex: %v", err)
	}
	if got := s.curIndex(targetBio); got != 0 {
		t.Errorf("curIndex clamp failed: %d", got)
	}
	if err := s.setLastFire(1234); err != nil {
		t.Fatalf("setLastFire: %v", err)
	}

	// Reload from disk.
	s2 := newStateStore(filepath.Join(dir, "state.json"))
	if err := s2.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := s2.items(targetName); len(got) != 1 || got[0] != "B" {
		t.Errorf("reload items = %v", got)
	}
	if unix, _ := s2.lastFire(); unix != 1234 {
		t.Errorf("reload lastFire = %d", unix)
	}

	// Origin capture keeps the first value only.
	if err := s.setOrigin(originInfo{First: "张三", Last: "李", SetLast: true, Bio: "hi"}); err != nil {
		t.Fatalf("setOrigin: %v", err)
	}
	if err := s.setOrigin(originInfo{First: "new", Bio: "newbio"}); err != nil {
		t.Fatalf("setOrigin 2: %v", err)
	}
	o := s.origin()
	if o.First != "张三" || o.Bio != "hi" {
		t.Errorf("origin overwritten: %+v", o)
	}

	// An explicitly captured empty bio is a real restorable value.
	s3 := newStateStore(filepath.Join(dir, "state2.json"))
	if err := s3.load(); err != nil {
		t.Fatalf("load 3: %v", err)
	}
	if err := s3.setOrigin(originInfo{Bio: "", CapturedBio: true}); err != nil {
		t.Fatalf("setOrigin 3: %v", err)
	}
	o = s3.origin()
	if !o.CapturedBio || o.Bio != "" {
		t.Errorf("empty bio not captured: %+v", o)
	}
	if err := s3.setOrigin(originInfo{Bio: "later"}); err != nil {
		t.Fatalf("setOrigin 4: %v", err)
	}
	if o = s3.origin(); o.Bio != "" || !o.CapturedBio {
		t.Errorf("captured empty bio overwritten: %+v", o)
	}
}

func TestNameRequest(t *testing.T) {
	req := nameRequest("张三", "李", true)
	if f, ok := req.GetFirstName(); !ok || f != "张三" {
		t.Errorf("firstName = %q, %v", f, ok)
	}
	if l, ok := req.GetLastName(); !ok || l != "李" {
		t.Errorf("lastName = %q, %v", l, ok)
	}
	if _, ok := req.GetAbout(); ok {
		t.Error("name request must not set about")
	}
	req = nameRequest("Only", "", false)
	if _, ok := req.GetLastName(); ok {
		t.Error("lastName set when not requested")
	}
	b := bioRequest("摸鱼中")
	if a, ok := b.GetAbout(); !ok || a != "摸鱼中" {
		t.Errorf("about = %q, %v", a, ok)
	}
	r := restoreNameRequest("张三", "")
	if l, ok := r.GetLastName(); !ok || l != "" {
		t.Errorf("restore lastName = %q, %v (must clear)", l, ok)
	}
}

func TestValidCronSetting(t *testing.T) {
	if _, err := validCronSetting(""); err != nil {
		t.Errorf("empty cron rejected: %v", err)
	}
	if _, err := validCronSetting("0 9 * * *"); err != nil {
		t.Errorf("valid cron rejected: %v", err)
	}
	if _, err := validCronSetting("junk"); err == nil {
		t.Error("invalid cron accepted")
	}
}

func TestClipLines(t *testing.T) {
	// Under the cap: untouched.
	s := "第一行\n第二行\n第三行"
	if got := clipLines(s, 3900); got != s {
		t.Errorf("short text changed: %q", got)
	}
	// Over the cap: cut on a line boundary, never mid-line.
	long := ""
	for i := 0; i < 200; i++ {
		long += "条目" + strconv.Itoa(i) + " `代码` 一些内容让这行变长一点\n"
	}
	got := clipLines(long, 3900)
	if n := len([]rune(got)); n > 3902 { // cut + "\n…"
		t.Errorf("clipLines returned %d runes, want <= 3902", n)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n…"), "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "条目") && !strings.HasSuffix(l, "一点") {
			t.Errorf("line cut in half: %q", l)
		}
	}
	if !strings.HasSuffix(got, "\n…") {
		t.Errorf("missing ellipsis tail: %q", got[len(got)-10:])
	}
	// A single huge line still gets truncated.
	one := strings.Repeat("长", 5000)
	if got := clipLines(one, 3900); len([]rune(got)) != 3902 { // 3900 cut + "\n…"
		t.Errorf("hard cut = %d runes, want 3902", len([]rune(got)))
	}
}

func TestErrTextFallbackBilingual(t *testing.T) {
	// Unknown errors keep the raw detail but gain a localized prefix and
	// Markdown escaping instead of dumping raw English text.
	zh := errText(func(z, _ string) string { return z }, errors.New("weird *failure*"))
	if !strings.HasPrefix(zh, "操作失败：") {
		t.Errorf("zh fallback = %q, want localized prefix", zh)
	}
	if strings.Contains(zh, "*failure*") {
		t.Errorf("zh fallback not escaped: %q", zh)
	}
	en := errText(func(_, e string) string { return e }, errors.New("weird *failure*"))
	if !strings.HasPrefix(en, "Operation failed: ") {
		t.Errorf("en fallback = %q, want localized prefix", en)
	}
}
