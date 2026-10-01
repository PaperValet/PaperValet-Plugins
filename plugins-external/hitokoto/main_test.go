package main

import (
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
