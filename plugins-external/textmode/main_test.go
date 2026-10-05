package main

// main_test.go — parsing, UTF-16 length, entity building, list priority and
// the skip-decision matrix. Pure logic only, no network.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"
)

// ---------------------------------------------------------------- mode parsing

func TestParseMode(t *testing.T) {
	for _, m := range allModes {
		if got, ok := parseMode(string(m)); !ok || got != m {
			t.Errorf("parseMode(%q) = %v, %v", m, got, ok)
		}
	}
	for _, bad := range []string{"", "OFF", "Del", "spoiler", "strike", "underline ", "all4", "plain", "x"} {
		if _, ok := parseMode(bad); ok {
			t.Errorf("parseMode(%q) should fail", bad)
		}
	}
}

func TestModeLabel(t *testing.T) {
	zh := func(zh, _ string) string { return zh }
	en := func(_, en string) string { return en }
	if got := modeLabel(zh, modeOff); got != "off" {
		t.Errorf("off label = %q", got)
	}
	if got := modeLabel(zh, modeBold); got != "bold （粗体）" {
		t.Errorf("bold zh label = %q", got)
	}
	if got := modeLabel(en, modeMask); got != "mask (spoiler)" {
		t.Errorf("mask en label = %q", got)
	}
	if got := modeLabel(en, modeAll); got != "all (all)" {
		t.Errorf("all en label = %q", got)
	}
}

// ---------------------------------------------------------------- UTF-16

func TestUTF16Len(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"你好", 2},                   // CJK are BMP: 1 unit each
		{"a😀b", 4},                  // emoji is astral: 2 units
		{"😀😀", 4},                   // two emoji
		{"𝕏", 2},                    // astral letter
		{"你好😀world", 9},             // 2 + 2 + 5
		{"\U0001F1E8\U0001F1F3", 4}, // regional indicators (flag)
		{"e\u0301", 2},              // combining accent: two BMP units
	}
	for _, c := range cases {
		if got := utf16Len(c.in); got != c.want {
			t.Errorf("utf16Len(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- entities

func entityKind(e tg.MessageEntityClass) string {
	switch e.(type) {
	case *tg.MessageEntityStrike:
		return "strike"
	case *tg.MessageEntityBold:
		return "bold"
	case *tg.MessageEntityItalic:
		return "italic"
	case *tg.MessageEntityUnderline:
		return "underline"
	case *tg.MessageEntitySpoiler:
		return "spoiler"
	case *tg.MessageEntityCode:
		return "code"
	}
	return "?"
}

func TestModeEntities(t *testing.T) {
	cases := map[mode][]string{
		modeDel:       {"strike"},
		modeBold:      {"bold"},
		modeItalic:    {"italic"},
		modeUnderline: {"underline"},
		modeMask:      {"spoiler"},
		modeAll:       {"strike", "bold", "italic", "underline"},
	}
	for m, want := range cases {
		ctors := modeEntities(m)
		if len(ctors) != len(want) {
			t.Fatalf("mode %v: %d ctors, want %d", m, len(ctors), len(want))
		}
		got := buildEntities(nil, "hello", ctors)
		if len(got) != len(want) {
			t.Fatalf("mode %v: %d entities, want %d", m, len(got), len(want))
		}
		for i, kind := range want {
			if entityKind(got[i]) != kind {
				t.Errorf("mode %v: entity %d = %s, want %s", m, i, entityKind(got[i]), kind)
			}
		}
	}
	if ctors := modeEntities(modeOff); ctors != nil {
		t.Errorf("modeOff should have no entities, got %d", len(ctors))
	}
}

func TestBuildEntities(t *testing.T) {
	existing := []tg.MessageEntityClass{
		&tg.MessageEntityCode{Offset: 0, Length: 3},
	}
	got := buildEntities(existing, "abc😀x", modeEntities(modeBold))
	if len(got) != 2 {
		t.Fatalf("got %d entities, want 2", len(got))
	}
	// existing preserved first, untouched
	c := got[0].(*tg.MessageEntityCode)
	if c.Offset != 0 || c.Length != 3 {
		t.Errorf("existing entity mutated: %+v", c)
	}
	// new bold covers the whole text in UTF-16 units: 3 + 2 + 1 = 6
	b := got[1].(*tg.MessageEntityBold)
	if b.Offset != 0 || b.Length != 6 {
		t.Errorf("bold = %+v, want offset 0 length 6", b)
	}
	// all mode stacks four overlapping entities
	got = buildEntities(nil, "hi", modeEntities(modeAll))
	if len(got) != 4 {
		t.Fatalf("all: got %d entities, want 4", len(got))
	}
	for _, e := range got {
		if e.GetOffset() != 0 || e.GetLength() != 2 {
			t.Errorf("all entity = off %d len %d, want 0/2", e.GetOffset(), e.GetLength())
		}
	}
}

// ---------------------------------------------------------------- command skip

func TestIsCommand(t *testing.T) {
	prefixes := []string{".", "!", "。"}
	yes := []string{"/start", ".textmode", "!ping", "。帮助"}
	no := []string{"hello", "a.b", "text .x", "你好。世界"}
	for _, s := range yes {
		if !isCommand(s, prefixes) {
			t.Errorf("isCommand(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isCommand(s, prefixes) {
			t.Errorf("isCommand(%q) = true, want false", s)
		}
	}
	// empty prefix must not make everything a command
	if isCommand("hello", []string{""}) {
		t.Error("empty prefix matched")
	}
	if isCommand("hello", nil) {
		t.Error("nil prefixes should not match non-slash text")
	}
}

// ---------------------------------------------------------------- store

func TestStoreModesAndLists(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.chatMode(1); ok {
		t.Fatal("fresh store should have no chat mode")
	}
	if err := s.setChatMode(-1001234567890, modeMask); err != nil {
		t.Fatal(err)
	}
	if m, ok := s.chatMode(-1001234567890); !ok || m != modeMask {
		t.Fatalf("chatMode = %v %v", m, ok)
	}
	ok, err := s.resetChat(-1001234567890)
	if err != nil || !ok {
		t.Fatalf("reset: %v %v", ok, err)
	}
	if ok, _ = s.resetChat(-1001234567890); ok {
		t.Fatal("second reset should be a no-op")
	}

	// list add/remove, including duplicates
	if added, _ := s.listAdd(listWhite, 5); !added {
		t.Fatal("listAdd white")
	}
	if added, _ := s.listAdd(listWhite, 5); added {
		t.Fatal("duplicate listAdd should report false")
	}
	if added, _ := s.listAdd(listBlack, 5); !added {
		t.Fatal("same id can be in both lists")
	}
	if !s.inList(listWhite, 5) || !s.inList(listBlack, 5) {
		t.Fatal("inList")
	}
	if removed, _ := s.listRemove(listBlack, 5); !removed {
		t.Fatal("listRemove black")
	}
	if removed, _ := s.listRemove(listBlack, 5); removed {
		t.Fatal("second listRemove should report false")
	}

	// persistence round-trip
	s2, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.inList(listWhite, 5) || s2.inList(listBlack, 5) {
		t.Fatal("lists not persisted")
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["chats"]; !ok {
		t.Fatal("config.json missing chats")
	}
	if _, ok := raw["whitelist"]; !ok {
		t.Fatal("config.json missing whitelist")
	}
	if _, ok := raw["blacklist"]; !ok {
		t.Fatal("config.json missing blacklist")
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode = %v, want 0600", info.Mode().Perm())
	}
}

// ---------------------------------------------------------------- priority matrix

func TestShouldProcessMatrix(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const (
		plain  = 1 // never whitelisted
		moded  = 2 // per-chat bold
		offed  = 3 // per-chat off
		black  = 4 // blacklisted, chat mode set
		white1 = 5 // whitelisted
		other  = 6 // whitelisted, no per-chat setting
		both   = 7 // in whitelist AND blacklist
	)
	if err := s.setChatMode(moded, modeBold); err != nil {
		t.Fatal(err)
	}
	if err := s.setChatMode(offed, modeOff); err != nil {
		t.Fatal(err)
	}
	if err := s.setChatMode(black, modeItalic); err != nil {
		t.Fatal(err)
	}
	if err := s.setChatMode(both, modeDel); err != nil {
		t.Fatal(err)
	}
	if _, err := s.listAdd(listBlack, black); err != nil {
		t.Fatal(err)
	}
	if _, err := s.listAdd(listBlack, both); err != nil {
		t.Fatal(err)
	}
	// Whitelist every chat under test except `plain`, so the whitelist gate
	// passes and the later rules are what the cases actually exercise.
	for _, id := range []int64{moded, offed, black, white1, other, both} {
		if _, err := s.listAdd(listWhite, id); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name   string
		chat   int64
		global mode
		want   mode
	}{
		{"unset chat + off global", other, modeOff, modeOff},
		{"unset chat falls back to global", other, modeMask, modeMask},
		{"chat mode beats global", moded, modeMask, modeBold},
		{"chat off falls back to global (source semantics)", offed, modeDel, modeDel},
		{"blacklist skips even with a chat mode", black, modeOff, modeOff},
		{"blacklist skips even with a global mode", black, modeAll, modeOff},
		{"non-empty whitelist excludes unlisted chats", plain, modeMask, modeOff},
		{"non-empty whitelist admits listed chats", white1, modeOff, modeOff},
		{"non-empty whitelist admits listed chats + global", white1, modeItalic, modeItalic},
		{"blacklisted chat also in whitelist: still skipped (source checks white gate, then black)", both, modeOff, modeOff},
	}
	for _, c := range cases {
		if got := s.shouldProcess(c.chat, c.global); got != c.want {
			t.Errorf("%s: shouldProcess(%d, %v) = %v, want %v", c.name, c.chat, c.global, got, c.want)
		}
	}
}

func TestEffectiveForStatus(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.effective(1, modeOff); got != modeOff {
		t.Errorf("unset chat, off global = %v", got)
	}
	if got := s.effective(1, modeBold); got != modeBold {
		t.Errorf("unset chat falls to global = %v", got)
	}
	if err := s.setChatMode(1, modeUnderline); err != nil {
		t.Fatal(err)
	}
	if got := s.effective(1, modeBold); got != modeUnderline {
		t.Errorf("chat mode = %v", got)
	}
	if err := s.setChatMode(1, modeOff); err != nil {
		t.Fatal(err)
	}
	if got := s.effective(1, modeBold); got != modeBold {
		t.Errorf("chat off falls to global = %v", got)
	}
}
