package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestParseCategory(t *testing.T) {
	// bare setu uses the configured default
	if c, arg, ok := parseCategory(nil, "rand"); !ok || arg != "" || c == nil || c.Key != "rand" || c.Command != "rand" {
		t.Fatalf("default: %+v %q %v", c, arg, ok)
	}
	// default falls back to rand when unknown
	if c, _, ok := parseCategory(nil, "bogus"); !ok || c.Key != "rand" {
		t.Fatal("bad default should fall back to rand")
	}
	// every category is reachable by key and maps to its bot command
	for _, want := range categories {
		c, arg, ok := parseCategory([]string{want.Key}, "")
		if !ok || arg != "" || c == nil || c.Key != want.Key || c.Command != want.Command || c.Text != want.Text {
			t.Errorf("%s: got %+v arg %q ok %v", want.Key, c, arg, ok)
		}
	}
	// case-insensitive and @-prefixed
	if c, _, ok := parseCategory([]string{"COSER"}, ""); !ok || c.Command != "cos" {
		t.Fatal("case")
	}
	if c, _, ok := parseCategory([]string{"qd"}, ""); !ok || !c.Text {
		t.Fatal("qd must be the text check-in")
	}
	// help
	if _, arg, ok := parseCategory([]string{"help"}, ""); ok || arg != "help" {
		t.Fatal("help")
	}
	if _, arg, ok := parseCategory([]string{"h"}, ""); ok || arg != "help" {
		t.Fatal("h")
	}
	// unknown
	if _, arg, ok := parseCategory([]string{"xxx"}, ""); ok || arg != "xxx" {
		t.Fatal("unknown")
	}
}

func TestCategoryTable(t *testing.T) {
	// every key has a bilingual label and the qd special case is right
	for _, c := range categories {
		n, ok := categoryNames[c.Key]
		if !ok || n[0] == "" || n[1] == "" {
			t.Errorf("no label for %s", c.Key)
		}
		if c.Key == "qd" != c.Text {
			t.Errorf("%s text flag wrong", c.Key)
		}
	}
	if len(categoryChoices()) != len(categories) {
		t.Fatal("choices must cover all categories")
	}
}

func TestIsBotError(t *testing.T) {
	for _, s := range []string{
		"没有找到相关图片",
		"发生错误，请稍后再试",
		"搜索失败",
		"该内容不存在",
		"Internal server error",
		"Image not found",
		"Invalid command",
	} {
		if !isBotError(s) {
			t.Errorf("should be error: %q", s)
		}
	}
	for _, s := range []string{
		"正在搜索...",
		"请稍候",
		"签到成功，连续签到 3 天",
		"Loading image",
	} {
		if isBotError(s) {
			t.Errorf("should not be error: %q", s)
		}
	}
}

func TestClipText(t *testing.T) {
	if got := clipText("abc", 5); got != "abc" {
		t.Fatal(got)
	}
	if got := clipText("abcdefg", 5); got != "abcd…" {
		t.Fatal(got)
	}
	// multibyte runes count as one
	if got := clipText(strings.Repeat("图", 6), 4); got != "图图图…" {
		t.Fatal(got)
	}
}

func TestSentID(t *testing.T) {
	if got := sentID(&tg.UpdateShortSentMessage{ID: 42}); got != 42 {
		t.Fatal(got)
	}
	ups := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateMessageID{ID: 7},
	}}
	if got := sentID(ups); got != 7 {
		t.Fatal(got)
	}
	if got := sentID(&tg.Updates{}); got != 0 {
		t.Fatal(got)
	}
}

// replyWait drives waitReply without a host: it feeds messages and checks
// classification and the after filter.
func TestWaitReply(t *testing.T) {
	p := &SetuPlugin{}
	events := make(chan *tg.Message, 8)

	// stale messages are skipped, media wins
	go func() {
		events <- &tg.Message{ID: 5, Message: "old"}
		events <- &tg.Message{ID: 7, Message: "搜索中..."}
		events <- &tg.Message{ID: 8, Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 1}}}
	}()
	res, err := p.waitReply(context.Background(), events, 6, false)
	if err != nil || res == nil || res.Media == nil {
		t.Fatalf("media: %+v %v", res, err)
	}

	// error keyword text ends the wait as a text reply
	events = make(chan *tg.Message, 8)
	go func() {
		events <- &tg.Message{ID: 3, Message: "没有找到图片"}
	}()
	res, err = p.waitReply(context.Background(), events, 1, false)
	if err != nil || res == nil || res.Media != nil || res.Text != "没有找到图片" {
		t.Fatalf("error text: %+v %v", res, err)
	}

	// image mode: progress text followed by timeout is NOT a success — the
	// progress text ("searching…") must not be reported as the bot's answer
	events = make(chan *tg.Message, 8)
	go func() {
		events <- &tg.Message{ID: 3, Message: "正在搜索..."}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	res, err = p.waitReply(ctx, events, 1, false)
	if err == nil || res != nil {
		t.Fatalf("progress text on timeout must fail, got %+v %v", res, err)
	}
	if !errors.Is(err, errNoReply) {
		t.Fatalf("want errNoReply, got %v", err)
	}

	// check-in: progress text followed by timeout keeps the last text as
	// the result (any reply is the answer for the check-in)
	events = make(chan *tg.Message, 8)
	go func() {
		events <- &tg.Message{ID: 3, Message: "签到处理中..."}
	}()
	ctx3, cancel3 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel3()
	res, err = p.waitReply(ctx3, events, 1, true)
	if err != nil || res == nil || res.Text != "签到处理中..." {
		t.Fatalf("checkin last text: %+v %v", res, err)
	}

	// timeout without any text is an error
	events = make(chan *tg.Message, 8)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	if _, err := p.waitReply(ctx2, events, 1, false); err == nil {
		t.Fatal("want timeout error")
	}

	// check-in: any text is the answer, even without error keywords
	events = make(chan *tg.Message, 8)
	go func() {
		events <- &tg.Message{ID: 4, Message: "签到成功 +10"}
	}()
	res, err = p.waitReply(context.Background(), events, 1, true)
	if err != nil || res == nil || res.Text != "签到成功 +10" {
		t.Fatalf("checkin: %+v %v", res, err)
	}
}

func TestHelpRender(t *testing.T) {
	p := &SetuPlugin{}
	out := p.help(func(zh, en string) string { return en })
	if !strings.Contains(out, "setu rand") || !strings.Contains(out, "setu qd") || !strings.Contains(out, botUsername) {
		t.Fatal(out)
	}
	zh := p.help(func(zh, en string) string { return zh })
	if !strings.Contains(zh, "签到") {
		t.Fatal(zh)
	}
}
