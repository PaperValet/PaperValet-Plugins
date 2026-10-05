package main

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseCronExpr(t *testing.T) {
	// 6-field expressions parse.
	for _, ok := range []string{
		"0 0 2 * * *",
		"*/30 * * * * *",
		"0 30 2 * * 1-5",
		"15 0 0 1 1 *",
		"0 0 12 */2 * mon",
	} {
		if _, err := parseCronExpr(ok); err != nil {
			t.Errorf("parseCronExpr(%q): %v", ok, err)
		}
	}
	// 5-field and 7-field do not.
	for _, bad := range []string{
		"0 0 2 * *",
		"0 0 2 * * * *",
		"* * * *",
		"60 0 0 * * *",
		"0 0 25 * * *",
		"",
	} {
		if _, err := parseCronExpr(bad); err == nil {
			t.Errorf("parseCronExpr(%q): expected error", bad)
		}
	}
}

func TestParseCronFromArgs(t *testing.T) {
	expr, rest, err := parseCronFromArgs([]string{"0", "0", "2", "*", "*", "*", "@durov", "note"})
	if err != nil {
		t.Fatalf("parseCronFromArgs: %v", err)
	}
	if expr != "0 0 2 * * *" || rest[0] != "@durov" || rest[1] != "note" {
		t.Errorf("got expr=%q rest=%v", expr, rest)
	}
	if _, _, err := parseCronFromArgs([]string{"0", "0", "2", "*"}); err == nil {
		t.Errorf("expected error for 4 args")
	}
	if _, _, err := parseCronFromArgs([]string{"x", "0", "2", "*", "*", "*"}); err == nil {
		t.Errorf("expected error for bad field")
	}
}

func TestParseChatArg(t *testing.T) {
	cases := []struct {
		in      string
		chat    string
		replyTo int
		wantErr bool
	}{
		{"-1001234567890", "-1001234567890", 0, false},
		{"-1001234567890|42", "-1001234567890", 42, false},
		{"@durov|5", "@durov", 5, false},
		{"12345｜7", "12345", 7, false}, // full-width separator
		{"@durov|0", "", 0, true},      // reply id must be positive
		{"@durov|x", "", 0, true},
		{"||", "", 0, true},
	}
	for _, c := range cases {
		chat, replyTo, err := parseChatArg(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseChatArg(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseChatArg(%q): %v", c.in, err)
			continue
		}
		if chat != c.chat || replyTo != c.replyTo {
			t.Errorf("parseChatArg(%q): got %q,%d want %q,%d", c.in, chat, replyTo, c.chat, c.replyTo)
		}
	}
}

func TestParseChatID(t *testing.T) {
	if id, ok := parseChatID("-1001234567890"); !ok || id != -1001234567890 {
		t.Errorf("parseChatID(-100…): got %d,%v", id, ok)
	}
	if id, ok := parseChatID("12345"); !ok || id != 12345 {
		t.Errorf("parseChatID(12345): got %d,%v", id, ok)
	}
	if _, ok := parseChatID("@durov"); ok {
		t.Errorf("parseChatID(@durov): expected ok=false")
	}
	if _, ok := parseChatID("12x4"); ok {
		t.Errorf("parseChatID(12x4): expected ok=false")
	}
}

func TestTryParseRegex(t *testing.T) {
	re, err := tryParseRegex("/^test/i")
	if err != nil {
		t.Fatalf("tryParseRegex(/^test/i): %v", err)
	}
	if !re.MatchString("TEST line") {
		t.Errorf("flag i not applied")
	}
	if re.MatchString("no match here") {
		t.Errorf("unexpected match")
	}
	if _, err := tryParseRegex("/x/q"); err == nil {
		t.Errorf("expected error for unsupported flag")
	}
	if _, err := tryParseRegex("[unclosed"); err == nil {
		t.Errorf("expected error for bad pattern")
	}
	re2, err := tryParseRegex("^plain")
	if err != nil || !re2.MatchString("plain start") {
		t.Errorf("bare pattern failed: %v", err)
	}
}

func TestBuildCopyCommand(t *testing.T) {
	base := Task{ID: 1, Type: typeSend, Cron: "0 0 2 * * *", Chat: "@durov", Remark: "daily"}
	if got := buildCopyCommand(".", base); got != ".acron send 0 0 2 * * * @durov daily" {
		t.Errorf("send: %q", got)
	}
	base.ReplyTo = 42
	if got := buildCopyCommand(".", base); got != ".acron send 0 0 2 * * * @durov|42 daily" {
		t.Errorf("send+reply: %q", got)
	}
	cmd := Task{ID: 2, Type: typeCmd, Cron: "0 0 2 * * *", Chat: "me", Message: ".bf"}
	if got := buildCopyCommand("", cmd); got != "acron cmd 0 0 2 * * * me\n.bf" {
		t.Errorf("cmd: %q", got)
	}
	pin := Task{ID: 3, Type: typePin, Cron: "0 0 2 * * *", Chat: "-100123", MsgID: 55, Notify: true, PmOneSide: false}
	if got := buildCopyCommand(".", pin); got != ".acron pin 0 0 2 * * * -100123 55 1 0" {
		t.Errorf("pin: %q", got)
	}
	delRe := Task{ID: 4, Type: typeDelRe, Cron: "0 0 2 * * *", Chat: "-100123", Limit: 100, Regex: "^test.*", Remark: "cleanup"}
	if got := buildCopyCommand(".", delRe); got != ".acron del_re 0 0 2 * * * -100123 100 ^test.* cleanup" {
		t.Errorf("del_re: %q", got)
	}
	// chat id fallback when Chat is empty
	numeric := Task{ID: 5, Type: typeDel, Cron: "0 0 2 * * *", ChatID: -100123, MsgID: 9}
	if got := buildCopyCommand(".", numeric); got != ".acron del 0 0 2 * * * -100123 9" {
		t.Errorf("del numeric: %q", got)
	}
}

func TestRemarkFromLine(t *testing.T) {
	if r := remarkFromLine(".acron send 0 0 2 * * * @durov 备注 文本", 8); r != "备注 文本" {
		t.Errorf("remark: %q", r)
	}
	if r := remarkFromLine(".acron send 0 0 2 * * * @durov", 8); r != "" {
		t.Errorf("empty remark: %q", r)
	}
	if r := remarkFromLine(".acron del 0 0 2 * * * @durov 55 cleanup", 9); r != "cleanup" {
		t.Errorf("del remark: %q", r)
	}
}

func TestChunkLines(t *testing.T) {
	lines := []string{"a", "bb", "ccc"}
	chunks := chunkLines(lines, 5)
	if len(chunks) != 2 {
		t.Fatalf("chunkLines: got %d chunks %v", len(chunks), chunks)
	}
	for _, c := range chunks {
		if len([]rune(strings.TrimRight(c, "\n"))) > 5 {
			t.Errorf("chunk too long: %q", c)
		}
	}
}

func TestCronNextUsesLocation(t *testing.T) {
	sched, err := parseCronExpr("0 30 8 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	now := time.Date(2026, 3, 1, 23, 0, 0, 0, time.UTC) // 07:00 next day in Shanghai
	next := sched.Next(now.In(shanghai))
	if next.In(shanghai).Hour() != 8 || next.In(shanghai).Minute() != 30 {
		t.Errorf("next=%v want 08:30 Shanghai", next.In(shanghai))
	}
}

func TestTaskTypeLabel(t *testing.T) {
	zh := func(zh, _ string) string { return zh }
	if taskTypeLabel(zh, typeSend) != "定时发送" {
		t.Errorf("zh label")
	}
	en := func(_, en string) string { return en }
	if taskTypeLabel(en, typeDelRe) != "regex delete" {
		t.Errorf("en label")
	}
}

func TestParseListArgs(t *testing.T) {
	if all, tp := parseListArgs([]string{}); all || tp != "" {
		t.Errorf("empty: %v %q", all, tp)
	}
	if all, tp := parseListArgs([]string{"ALL"}); !all || tp != "" {
		t.Errorf("all: %v %q", all, tp)
	}
	if all, tp := parseListArgs([]string{"all", "del"}); !all || tp != "del" {
		t.Errorf("all del: %v %q", all, tp)
	}
	if all, tp := parseListArgs([]string{"del"}); all || tp != "del" {
		t.Errorf("del: %v %q", all, tp)
	}
	if all, tp := parseListArgs([]string{"send", "all"}); !all || tp != "send" {
		t.Errorf("send all: %v %q", all, tp)
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 10) != "short" {
		t.Errorf("short")
	}
	long := strings.Repeat("x", 300)
	if got := truncate(long, 200); len([]rune(got)) != 201 {
		t.Errorf("long: %d runes", len([]rune(got)))
	}
}

func TestEntityRoundTripUnused(t *testing.T) {
	// keep regexp import exercised
	_ = regexp.MustCompile("x")
}
