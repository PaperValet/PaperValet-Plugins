package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
	"github.com/gotd/td/tg"
)

// evFor builds a MessageEvent for chatID with the given text. chatID uses
// the MessageEvent form: users positive, basic groups -id, channels -100….
func evFor(chatID int64, text string) *plugin.MessageEvent {
	var peer tg.PeerClass
	switch {
	case chatID > 0:
		peer = &tg.PeerUser{UserID: chatID}
	case chatID <= -1000000000000:
		peer = &tg.PeerChannel{ChannelID: -chatID - 1000000000000}
	default:
		peer = &tg.PeerChat{ChatID: -chatID}
	}
	return plugin.EventFromMessage(&tg.Message{Message: text, PeerID: peer})
}

func TestParseTaskSimple(t *testing.T) {
	tk := newTask(1, -100123)
	if err := tk.parseTask("你好\n+++\n欢迎！"); err != nil {
		t.Fatal(err)
	}
	if tk.Key != "你好" || tk.Msg != "欢迎！" {
		t.Fatalf("got %+v", tk)
	}
	if !tk.Include || tk.Exact || tk.Regexp || tk.CaseSensitive || tk.IgnoreForward {
		t.Fatalf("defaults wrong: %+v", tk)
	}
	if !tk.Reply || tk.Delete || tk.Ban != 0 || tk.Restrict != 0 || tk.Cooldown != 0 {
		t.Fatalf("action defaults wrong: %+v", tk)
	}
}

func TestParseTaskFull(t *testing.T) {
	raw := "\\d{11}\n+++\n🚫 请勿发送手机号\n+++\nregexp case\n+++\nreply delete ban300 restrict600 cooldown60\n+++\n10\n+++\n5"
	tk := newTask(7, -42)
	if err := tk.parseTask(raw); err != nil {
		t.Fatal(err)
	}
	want := &task{
		ID: 7, ChatID: -42, Key: "\\d{11}", Msg: "🚫 请勿发送手机号",
		Include: true, Regexp: true, Exact: false, CaseSensitive: true,
		Reply: true, Delete: true, Ban: 300, Restrict: 600, Cooldown: 60,
		DelayDelete: 10, SourceDelayDelete: 5,
	}
	if !reflect.DeepEqual(tk, want) {
		t.Fatalf("got %+v want %+v", tk, want)
	}
}

func TestParseTaskExactWinsOverInclude(t *testing.T) {
	// The source's exact branch un-sets include, so "include exact" never
	// conflicts: exact wins (the source's conflict check is unreachable).
	tk := newTask(1, 1)
	if err := tk.parseTask("a\n+++\nb\n+++\ninclude exact"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tk.Exact || tk.Include {
		t.Fatalf("exact must win: %+v", tk)
	}
}

func TestParseTaskErrors(t *testing.T) {
	cases := []string{
		"only-one-segment",
		"\n+++\nb", // empty key
		"a\n+++\n", // empty msg
		"a\n+++\nb\n+++\nbogus",
		"a\n+++\nb\n+++\ninclude\n+++\nnonsense",
		strings.Repeat("x\n+++\n", 6) + "extra", // 7 segments
	}
	for _, raw := range cases {
		if err := (newTask(1, 1)).parseTask(raw); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}

func TestParseTaskNegativeRejected(t *testing.T) {
	// "ban-5" parses the number -5 → negative must be rejected.
	if err := (newTask(1, 1)).parseTask("a\n+++\nb\n+++\ninclude\n+++\nban-5"); err == nil {
		t.Fatal("negative ban accepted")
	}
}

func TestParseTaskIgnoresBlankTail(t *testing.T) {
	tk := newTask(1, 1)
	if err := tk.parseTask("a\n+++\nb\n+++\n\n+++\n\n+++\n"); err != nil {
		t.Fatal(err)
	}
	if tk.DelayDelete != 0 || tk.SourceDelayDelete != 0 {
		t.Fatalf("blank tail must stay 0: %+v", tk)
	}
}

func TestMatchesIncludeExactCase(t *testing.T) {
	base := func() *task { return &task{Key: "Hello", Msg: "x", Include: true, Reply: true} }

	tk := base() // include, case-insensitive (default)
	for _, s := range []string{"Hello", "hello world", "say HELLO!", "heLLo"} {
		if !tk.matches(s) {
			t.Fatalf("include: %q should match", s)
		}
	}
	if tk.matches("hell") {
		t.Fatal("substring must not match")
	}
	if tk.matches("") {
		t.Fatal("empty text must not match")
	}

	tk = base()
	tk.CaseSensitive = true
	if !tk.matches("Hello there") {
		t.Fatal("case-sensitive match failed")
	}
	if tk.matches("hello there") {
		t.Fatal("case mismatch must not match")
	}

	tk = base()
	tk.Include, tk.Exact = false, true
	if !tk.matches("hello") || tk.matches("hello!") {
		t.Fatalf("exact broken: %+v", tk)
	}
}

func TestMatchesRegexp(t *testing.T) {
	tk := &task{Key: `\d{11}`, Msg: "x", Regexp: true, Include: false, Reply: true}
	if !tk.matches("call me 13800138000 now") {
		t.Fatal("regexp should match")
	}
	if tk.matches("no digits here") {
		t.Fatal("regexp should not match")
	}
	tk2 := &task{Key: `abc`, Msg: "x", Regexp: true, Include: false, Reply: true}
	if !tk2.matches("xyz ABC") {
		t.Fatal("regexps must be case-insensitive by default")
	}
	tk2.CaseSensitive = true
	if tk2.matches("xyz ABC") {
		t.Fatal("case-sensitive regexp must not match")
	}
	tk3 := &task{Key: `(?i)abc`, Msg: "x", Regexp: true, Include: false, Reply: true}
	if !tk3.matches("xyz ABC") {
		t.Fatal("inline flags must work")
	}
	// broken regex never matches and never panics
	bad := &task{Key: "[", Msg: "x", Regexp: true, Include: false, Reply: true}
	if bad.matches("anything [") {
		t.Fatal("broken regexp must not match")
	}
}

func TestRenderVariables(t *testing.T) {
	tk := &task{Msg: "hi $mention id=$code_id name=$code_name dd=$delay_delete"}
	got := tk.render(42, "Ann")
	if !strings.Contains(got, "[Ann](tg://user?id=42)") {
		t.Fatalf("mention broken: %q", got)
	}
	if !strings.Contains(got, "id=42") || !strings.Contains(got, "name=Ann") {
		t.Fatalf("vars broken: %q", got)
	}
	if strings.Contains(got, "$delay_delete") {
		t.Fatalf("delay var must be emptied: %q", got)
	}
	tk.DelayDelete = 30
	if got := tk.render(42, "Ann"); !strings.Contains(got, "dd=30") {
		t.Fatalf("delay var not substituted: %q", got)
	}
	// no user identity → vars removed
	if got := tk.render(0, ""); strings.Contains(got, "$") {
		t.Fatalf("channel post must strip vars: %q", got)
	}
	// name fallback to the id
	if got := tk.render(9, ""); !strings.Contains(got, "name=9") {
		t.Fatalf("name fallback broken: %q", got)
	}
}

func TestCooldownLogic(t *testing.T) {
	p := New()
	p.cool = map[int]time.Time{}
	p.inFlight = map[int]bool{}
	p.tasks = []*task{
		{ID: 1, ChatID: -10, Key: "a", Msg: "x", Include: true, Reply: true, Cooldown: 60},
		{ID: 2, ChatID: -10, Key: "b", Msg: "y", Include: true, Reply: true},
	}
	e := evFor(-10, "a b")
	if ids := p.matchingLocked(e); !equalIDs(ids, []int{1, 2}) {
		t.Fatalf("first fire: got %v", ids)
	}
	// task 1 is on cooldown now, task 2 still fires
	if ids := p.matchingLocked(e); !equalIDs(ids, []int{2}) {
		t.Fatalf("cooldown: got %v", ids)
	}
	// cooldown expiry (task 1 fires and re-enters cooldown)
	p.cool[1] = time.Now().Add(-time.Second)
	if ids := p.matchingLocked(e); !equalIDs(ids, []int{1, 2}) {
		t.Fatalf("after expiry: got %v", ids)
	}
	// both tasks match, but 1 is on cooldown again and 2 is in flight
	p.inFlight[2] = true
	if ids := p.matchingLocked(e); !equalIDs(ids, []int{}) || len(ids) != 0 {
		t.Fatalf("in-flight: got %v", ids)
	}
	p.cool[1] = time.Now().Add(-time.Second)
	if ids := p.matchingLocked(e); !equalIDs(ids, []int{1}) {
		t.Fatalf("in-flight skips only 2: got %v", ids)
	}
	// cooldown is per task, not per chat: a task without cooldown always fires
	p2 := New()
	p2.cool, p2.inFlight = map[int]time.Time{}, map[int]bool{}
	p2.tasks = []*task{{ID: 1, ChatID: -10, Key: "a", Msg: "x", Include: true, Reply: true}}
	e2 := evFor(-10, "a")
	for i := 0; i < 3; i++ {
		if ids := p2.matchingLocked(e2); !equalIDs(ids, []int{1}) {
			t.Fatalf("no-cooldown fire %d: got %v", i, ids)
		}
	}
}

func TestMatchingIgnoresOtherChatsAndAlias(t *testing.T) {
	p := New()
	p.cool, p.inFlight = map[int]time.Time{}, map[int]bool{}
	p.tasks = []*task{
		{ID: 1, ChatID: -10, Key: "a", Msg: "x", Include: true, Reply: true},
		{ID: 2, ChatID: -20, Key: "a", Msg: "other chat", Include: true, Reply: true},
	}
	// task 2's chat is not the message's chat → only task 1 matches
	if ids := p.matchingLocked(evFor(-10, "a")); !equalIDs(ids, []int{1}) {
		t.Fatalf("chat scoping: got %v", ids)
	}
	// alias: -30 inherits -20's rules
	p.alias = map[int64]int64{-30: -20}
	if ids := p.matchingLocked(evFor(-30, "a")); !equalIDs(ids, []int{2}) {
		t.Fatalf("alias: got %v", ids)
	}
}

func TestIgnoreForward(t *testing.T) {
	p := New()
	p.cool, p.inFlight = map[int]time.Time{}, map[int]bool{}
	p.tasks = []*task{{ID: 1, ChatID: -10, Key: "a", Msg: "x", Include: true, Reply: true, IgnoreForward: true}}
	msg := &tg.Message{Message: "a", PeerID: &tg.PeerChat{ChatID: 10}}
	ev := plugin.EventFromMessage(msg)
	if ids := p.matchingLocked(ev); !equalIDs(ids, []int{1}) {
		t.Fatalf("plain message must match: %v", ids)
	}
	msg.FwdFrom = tg.MessageFwdHeader{Date: 1}
	if ids := p.matchingLocked(ev); len(ids) != 0 {
		t.Fatalf("forwarded must be skipped: %v", ids)
	}
	p.tasks[0].IgnoreForward = false
	if ids := p.matchingLocked(ev); !equalIDs(ids, []int{1}) {
		t.Fatalf("forward ok when allowed: %v", ids)
	}
}

func TestParseTaskIDs(t *testing.T) {
	ids, err := parseTaskIDs("1, 2 ,3")
	if err != nil || !reflect.DeepEqual(ids, []int{1, 2, 3}) {
		t.Fatalf("got %v %v", ids, err)
	}
	if _, err := parseTaskIDs("1,x"); err == nil {
		t.Fatal("bad id accepted")
	}
	if _, err := parseTaskIDs(""); err == nil {
		t.Fatal("empty accepted")
	}
}

// loadForTest is Start's persistence load without the listener.
func (p *KeywordPlugin) loadForTest() error {
	return p.load(filepath.Join(p.dir, tasksFile))
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := New()
	p.dir = dir
	p.tasks = []*task{{
		ID: 3, ChatID: -100999, Key: "k", Msg: "m", Include: false, Exact: true, CaseSensitive: true,
		Reply: true, Delete: true, Ban: 10, Restrict: 20, Cooldown: 30, DelayDelete: 40, SourceDelayDelete: 50,
	}}
	p.nextID = 4
	p.alias = map[int64]int64{-10: -20}
	if err := p.saveLocked(); err != nil {
		t.Fatal(err)
	}
	q := New()
	q.dir = dir
	if err := q.loadForTest(); err != nil {
		t.Fatal(err)
	}
	if len(q.tasks) != 1 || !reflect.DeepEqual(q.tasks[0], p.tasks[0]) {
		t.Fatalf("tasks round trip: %+v", q.tasks)
	}
	if q.nextID != 4 || q.alias[-10] != -20 {
		t.Fatalf("meta round trip: nextID=%d alias=%v", q.nextID, q.alias)
	}
}

func TestClipLinesAndTruncate(t *testing.T) {
	s := "> `aaa`\n> `bbb`\n> `ccc`"
	if got := clipLines(s, 12); got != "> `aaa`\n…" {
		t.Fatalf("clipLines: %q", got)
	}
	if clipLines(s, 100) != s {
		t.Fatal("short text changed")
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Fatalf("truncate: %q", got)
	}
}

func TestFlagsRoundTrip(t *testing.T) {
	raw := "\\d{11}\n+++\nno phones\n+++\nregexp case\n+++\nreply delete ban300 restrict600 cooldown60\n+++\n10\n+++\n5"
	tk := newTask(1, 1)
	if err := tk.parseTask(raw); err != nil {
		t.Fatal(err)
	}
	back := newTask(1, 1)
	if err := back.parseTask(tk.flags()); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	back.ID, back.ChatID = tk.ID, tk.ChatID
	if !semanticallyEqual(tk, back) {
		t.Fatalf("flags round trip:\n%+v\n%+v", tk, back)
	}
}

func semanticallyEqual(a, b *task) bool {
	return a.Key == b.Key && a.Msg == b.Msg && a.Include == b.Include && a.Regexp == b.Regexp &&
		a.Exact == b.Exact && a.CaseSensitive == b.CaseSensitive && a.IgnoreForward == b.IgnoreForward &&
		a.Reply == b.Reply && a.Delete == b.Delete && a.Ban == b.Ban && a.Restrict == b.Restrict &&
		a.Cooldown == b.Cooldown && a.DelayDelete == b.DelayDelete && a.SourceDelayDelete == b.SourceDelayDelete
}

func equalIDs(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
