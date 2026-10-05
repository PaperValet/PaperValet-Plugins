package main

import (
	"testing"
)

func TestParseSetArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		src     string
		dst     string
		opts    []string
		topic   int
		wantErr bool
	}{
		{name: "single arg means here->target", args: []string{"@dest"}, src: "here", dst: "@dest"},
		{name: "source and target", args: []string{"@src", "@dest"}, src: "@src", dst: "@dest"},
		{name: "full options", args: []string{"@src", "@dest", "photo", "video", "silent", "handle_edited"},
			src: "@src", dst: "@dest", opts: []string{"photo", "video", "silent", "handle_edited"}},
		{name: "all keyword", args: []string{"123", "-100456", "all"}, src: "123", dst: "-100456", opts: []string{"all"}},
		{name: "replyTo topic", args: []string{"@src", "@dest", "replyTo:42"}, src: "@src", dst: "@dest", topic: 42},
		{name: "unknown option", args: []string{"@src", "@dest", "nope"}, wantErr: true},
		{name: "bad replyTo", args: []string{"@src", "@dest", "replyTo:x"}, wantErr: true},
		{name: "empty", args: nil, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst, opts, topic, err := parseSetArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got src=%q dst=%q", src, dst)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if src != tc.src || dst != tc.dst || topic != tc.topic {
				t.Fatalf("got src=%q dst=%q topic=%d, want %q %q %d", src, dst, topic, tc.src, tc.dst, tc.topic)
			}
			if len(opts) != len(tc.opts) {
				t.Fatalf("opts = %v, want %v", opts, tc.opts)
			}
			for i := range opts {
				if opts[i] != tc.opts[i] {
					t.Fatalf("opts = %v, want %v", opts, tc.opts)
				}
			}
		})
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in    string
		peer  string
		topic int
		has   bool
	}{
		{"@chan", "@chan", 0, false},
		{"@chan|12", "@chan", 12, true},
		{"@chan｜34", "@chan", 34, true},
		{"@chan|bad", "@chan", 0, false},
		{"-100123", "-100123", 0, false},
	}
	for _, tc := range cases {
		peer, topic, has := parseTarget(tc.in)
		if peer != tc.peer || topic != tc.topic || has != tc.has {
			t.Errorf("parseTarget(%q) = %q,%d,%v want %q,%d,%v", tc.in, peer, topic, has, tc.peer, tc.topic, tc.has)
		}
	}
}

func TestParseIndices(t *testing.T) {
	cases := []struct {
		in      string
		total   int
		want    []int
		invalid int
	}{
		{"1", 3, []int{0}, 0},
		{"1,3", 3, []int{0, 2}, 0},
		{"1-3", 3, []int{0, 1, 2}, 0},
		{"2-2", 3, []int{1}, 0},
		{"1-9", 3, []int{0, 1, 2}, 0}, // range clipped to total
		{"4", 3, nil, 1},
		{"0", 3, nil, 1},
		{"x", 3, nil, 1},
		{"3-1", 3, nil, 1}, // reversed range is invalid
		{"", 3, nil, 0},
		{"1,,2", 3, []int{0, 1}, 0},
	}
	for _, tc := range cases {
		got, invalid := parseIndices(tc.in, tc.total)
		if len(invalid) != tc.invalid {
			t.Errorf("parseIndices(%q) invalid = %v, want %d entries", tc.in, invalid, tc.invalid)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseIndices(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseIndices(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestParseBackupArgs(t *testing.T) {
	if _, _, c, err := parseBackupArgs([]string{"@a", "@b"}); err != nil || c != backupDefault {
		t.Errorf("default count: got %d err=%v", c, err)
	}
	if _, _, c, err := parseBackupArgs([]string{"@a", "@b", "250"}); err != nil || c != 250 {
		t.Errorf("explicit count: got %d err=%v", c, err)
	}
	if _, _, c, err := parseBackupArgs([]string{"@a", "@b", "999999"}); err != nil || c != backupMax {
		t.Errorf("count must be capped at %d: got %d err=%v", backupMax, c, err)
	}
	if _, _, _, err := parseBackupArgs([]string{"@a", "@b", "-5"}); err == nil {
		t.Error("negative count must fail")
	}
	if _, _, _, err := parseBackupArgs([]string{"@a"}); err == nil {
		t.Error("missing target must fail")
	}
}

func TestRuleFiltering(t *testing.T) {
	r := &Rule{Filters: []string{"广告", "spam"}}
	if !r.isFiltered("新版广告上线") {
		t.Error("keyword hit must filter")
	}
	if !r.isFiltered("SPAM here") {
		t.Error("keyword match must be case-insensitive")
	}
	if r.isFiltered("正常消息") {
		t.Error("clean message must pass blacklist")
	}

	r2 := &Rule{WhitelistMode: true, Whitelist: []string{"^\\d+$", "github\\.com"}}
	if !r2.isFiltered("no match at all") {
		t.Error("whitelist miss must filter")
	}
	if r2.isFiltered("see github.com/foo") {
		t.Error("whitelist hit must pass")
	}
	if r2.isFiltered("12345") {
		t.Error("regex anchor hit must pass")
	}

	r3 := &Rule{WhitelistMode: true}
	if !r3.isFiltered("anything") {
		t.Error("empty whitelist filters everything")
	}
}

func TestRuleTypes(t *testing.T) {
	r := &Rule{Options: []string{"photo", "video", "silent", "handle_edited"}}
	types := r.types()
	if len(types) != 2 || types[0] != "photo" || types[1] != "video" {
		t.Errorf("types = %v", types)
	}
	if !r.has("silent") || !r.has("handle_edited") {
		t.Error("has() must find flags")
	}
	if !r.has("photo") {
		t.Error("has() must find type options too")
	}
}

func TestRuleHasFindsTypes(t *testing.T) {
	r := &Rule{Options: []string{"photo"}}
	if !r.has("photo") {
		t.Error("has must match any stored option")
	}
}

func TestParseIndicesBounded(t *testing.T) {
	idx, invalid := parseIndices("1-5", 3)
	if len(idx) != 3 || len(invalid) != 0 {
		t.Errorf("clipped range: idx=%v invalid=%v", idx, invalid)
	}
}

func TestAppendUniqueRemoveMatch(t *testing.T) {
	l := appendUnique(nil, "a")
	l = appendUnique(l, "a")
	l = appendUnique(l, "b")
	if len(l) != 2 {
		t.Fatalf("appendUnique: %v", l)
	}
	l = removeMatch(l, "a")
	if len(l) != 1 || l[0] != "b" {
		t.Fatalf("removeMatch: %v", l)
	}
}

func TestStatsAddAndTotals(t *testing.T) {
	s := &Stats{}
	s.add(100)
	s.add(100)
	s.add(200)
	totals := s.totals()
	if totals[100] != 2 || totals[200] != 1 {
		t.Errorf("totals = %v", totals)
	}
	rec := s.recent(100, 7)
	if len(rec) != 1 || rec[0].Count != 2 {
		t.Errorf("recent = %v", rec)
	}
}

func TestContainsAny(t *testing.T) {
	if !containsAny("rpc error: CHAT_FORWARDS_RESTRICTED", "CHAT_FORWARDS_RESTRICTED") {
		t.Error("must detect restriction")
	}
	if containsAny("some other error", "CHAT_FORWARDS_RESTRICTED") {
		t.Error("must not detect false positive")
	}
}

func TestIsCommandText(t *testing.T) {
	for _, s := range []string{".shift list", "!ping", "/menu", "#help"} {
		if !isCommandText(s) {
			t.Errorf("%q must be a command", s)
		}
	}
	for _, s := range []string{"hello", "a.b", "", "plain text"} {
		if isCommandText(s) {
			t.Errorf("%q must not be a command", s)
		}
	}
}
