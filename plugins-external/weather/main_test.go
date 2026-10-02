package main

import (
	"strings"
	"testing"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func TestWindIndex(t *testing.T) {
	cases := map[float64]string{0: "北", 359: "北", 90: "东", 180: "南", 225: "西南", 270: "西", -90: "西"}
	for deg, want := range cases {
		if got := windDirsZh[windIndex(deg)]; got != want {
			t.Errorf("%v: got %s want %s", deg, got, want)
		}
	}
}

func TestHelpers(t *testing.T) {
	if hhmm("2026-10-01T06:12") != "06:12" {
		t.Fatal("hhmm")
	}
	if n := locationName(&geoResult{Name: "北京", Admin1: "北京", Country: "中国"}); n != "北京, 中国" {
		t.Fatal(n)
	}
	ctx := &plugin.CommandContext{}
	w := warnings(ctx, 36, 50, 12, 95)
	if len(w) != 4 || !strings.Contains(w[3], "雷暴") {
		t.Fatalf("%v", w)
	}
	if !hasHan("北京") || hasHan("Beijing") {
		t.Fatal("hasHan")
	}
}

func TestExactMatch(t *testing.T) {
	res := []geoResult{{ID: 1, Name: "York"}, {ID: 3, Name: "New York", Population: 5}, {ID: 2, Name: "New York", Population: 9000}}
	if r := exactMatch(res, "new york"); r == nil || r.ID != 2 {
		t.Fatal("exact")
	}
	if exactMatch(res, "Paris") != nil {
		t.Fatal("nomatch")
	}
}

func TestValidCity(t *testing.T) {
	if got, err := validCity("  New   York "); err != nil || got != "New York" {
		t.Fatalf("got %q %v", got, err)
	}
	if got, err := validCity(""); err != nil || got != "" {
		t.Fatal("empty must clear")
	}
	if _, err := validCity(strings.Repeat("长", 65)); err == nil {
		t.Fatal("too long accepted")
	}
}
