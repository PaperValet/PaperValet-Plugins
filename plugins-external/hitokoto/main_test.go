package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func TestParseTypes(t *testing.T) {
	cases := []struct {
		in      []string
		types   []string
		invalid []string
	}{
		{nil, nil, nil},
		{[]string{"a"}, []string{"a"}, nil},
		{[]string{"A", "c", "a"}, []string{"a", "c"}, nil},
		{[]string{"a,c"}, []string{"a", "c"}, nil},
		{[]string{"ach"}, []string{"a", "c", "h"}, nil},
		{[]string{"hh"}, []string{"h"}, nil},
		{[]string{"z", "b"}, []string{"b"}, []string{"z"}},
		{[]string{"xyz"}, nil, []string{"xyz"}},
	}
	for _, c := range cases {
		ty, inv := parseTypes(c.in)
		if !reflect.DeepEqual(ty, c.types) || !reflect.DeepEqual(inv, c.invalid) {
			t.Errorf("%v: got %v %v", c.in, ty, inv)
		}
	}
}

func TestFormat(t *testing.T) {
	who := "冈崎朋也"
	s := format(&plugin.CommandContext{}, &hitokotoResp{Hitokoto: "a_b", From: "CLANNAD", FromWho: &who, Type: "a"})
	if !strings.Contains(s, `a\_b`) || !strings.Contains(s, "《CLANNAD》（动画） - 冈崎朋也") {
		t.Fatal(s)
	}
}

func TestDefaultTypesFromPanel(t *testing.T) {
	if ts, inv := parseTypes(strings.Fields("a c")); len(ts) != 2 || len(inv) != 0 {
		t.Fatalf("%v %v", ts, inv)
	}
	if ts, _ := parseTypes(strings.Fields("")); len(ts) != 0 {
		t.Fatal("empty panel value must mean no filter")
	}
	keys := sortedTypeKeys()
	if len(keys) != len(typeNames) {
		t.Fatal("sortedTypeKeys mismatch")
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			t.Fatal("not sorted")
		}
	}
}

// panelDefaultTypes is the command path's fallback: with no args (and no
// invalid ones) the panel default is parsed into types.
func panelDefaultTypes(args []string, panel string) []string {
	types, invalid := parseTypes(args)
	if len(types) == 0 && len(invalid) == 0 {
		types, _ = parseTypes(strings.Fields(panel))
	}
	return types
}

func TestPanelDefaultFallback(t *testing.T) {
	if ts := panelDefaultTypes(nil, "a"); len(ts) != 1 || ts[0] != "a" {
		t.Fatalf("single default: %v", ts)
	}
	if ts := panelDefaultTypes(nil, ""); ts != nil {
		t.Fatalf("empty default must stay empty: %v", ts)
	}
	// explicit args win over the panel default
	if ts := panelDefaultTypes([]string{"b"}, "a,c"); len(ts) != 1 || ts[0] != "b" {
		t.Fatalf("explicit wins: %v", ts)
	}
	// invalid args keep the error path (panel default not consulted)
	if ts := panelDefaultTypes([]string{"zz"}, "a"); ts != nil {
		t.Fatalf("invalid must not fall back: %v", ts)
	}
	// multi-pick panel value ("a c" as stored by the choices UI)
	if ts := panelDefaultTypes(nil, "a c"); len(ts) != 2 || ts[0] != "a" || ts[1] != "c" {
		t.Fatalf("multi default: %v", ts)
	}
}

func TestRetryable(t *testing.T) {
	for _, code := range []int{429, 500, 502, 503} {
		if !retryable(&statusCodeError{code: code}) {
			t.Errorf("HTTP %d should retry", code)
		}
	}
	for _, code := range []int{400, 403, 404, 301} {
		if retryable(&statusCodeError{code: code}) {
			t.Errorf("HTTP %d must fail fast", code)
		}
	}
	if !retryable(errors.New("connection reset")) {
		t.Error("transport errors should retry")
	}
}
