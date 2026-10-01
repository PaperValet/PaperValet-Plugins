package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gotd/td/tg"
)

func TestReverseText(t *testing.T) {
	cases := map[string]string{
		"Hello World":    "dlroW olleH",
		"你好世界":           "界世好你",
		"ab\ncd":         "ba\ndc",
		"a👍🏽b":           "b👍🏽a",
		"x🇨🇳🇺🇸y":         "y🇺🇸🇨🇳x",
		"1👨‍👩‍👧2":        "2👨‍👩‍👧1",
		"e\u0301f":       "fe\u0301",
		"#\uFE0F\u20E3!": "!#\uFE0F\u20E3",
	}
	for in, want := range cases {
		if got := reverseText(in); got != want {
			t.Errorf("reverseText(%q)=%q want %q", in, got, want)
		}
	}
}

func TestReverseEntities(t *testing.T) {
	// "ab cd": bold "ab" -> reversed "dc ba", bold at 3..5
	ents := []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 2}}
	got := reverseEntities("ab cd", ents)
	if len(got) != 1 || got[0].GetOffset() != 3 || got[0].GetLength() != 2 {
		t.Fatalf("got %+v", got)
	}
	// original must be untouched
	if ents[0].GetOffset() != 0 {
		t.Fatal("mutated input")
	}
	// multi-line spanning entity "ab\ncd", italic 1..4 ("b\nc") -> "ba\ndc": b at 0, c at 4
	got = reverseEntities("ab\ncd", []tg.MessageEntityClass{&tg.MessageEntityItalic{Offset: 1, Length: 3}})
	if len(got) != 2 || got[0].GetOffset() != 0 || got[0].GetLength() != 1 || got[1].GetOffset() != 4 || got[1].GetLength() != 1 {
		t.Fatalf("got %+v", got)
	}
	// UTF-16: "😀x" bold on x (offset 2 len 1) -> "x😀": offset 0
	got = reverseEntities("😀x", []tg.MessageEntityClass{&tg.MessageEntityTextURL{Offset: 2, Length: 1, URL: "u"}})
	if len(got) != 1 || got[0].GetOffset() != 0 || got[0].(*tg.MessageEntityTextURL).URL != "u" {
		t.Fatalf("got %+v", got)
	}
}

func TestExtractMediaOptions(t *testing.T) {
	cases := []struct {
		in   []string
		want mediaOptions
	}{
		{nil, mediaOptions{flip: "h", remaining: []string{}}},
		{[]string{"v"}, mediaOptions{flip: "v", remaining: []string{}}},
		{[]string{"c"}, mediaOptions{flip: "", invert: true, remaining: []string{}}},
		{[]string{"H", "c"}, mediaOptions{flip: "h", invert: true, remaining: []string{}}},
		{[]string{"c", "hi"}, mediaOptions{flip: "h", invert: true, remaining: []string{"hi"}}},
		{[]string{"hello", "v"}, mediaOptions{flip: "h", remaining: []string{"hello", "v"}}},
	}
	for _, c := range cases {
		got := extractMediaOptions(c.in)
		if got.remaining == nil {
			got.remaining = []string{}
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: got %+v want %+v", c.in, got, c.want)
		}
	}
}

func TestRawRemainder(t *testing.T) {
	full := ".rev c  hello\n  world  "
	sub, base, ok := rawRemainder(full, []string{"hello", "world"})
	if !ok || sub != "hello\n  world" || base != 8 {
		t.Fatalf("got %q %d %v", sub, base, ok)
	}
	if _, _, ok := rawRemainder(".x foo", []string{"bar"}); ok {
		t.Fatal("mismatch accepted")
	}
	// entity shifting: bold on "world" in full text
	ents := shiftEntities([]tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 16, Length: 5}}, base, utf16Len(sub))
	if len(ents) != 1 || ents[0].GetOffset() != 8 {
		t.Fatalf("got %+v", ents)
	}
}

func TestTrimWithEntities(t *testing.T) {
	s, e := trimWithEntities("  ab ", []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 2, Length: 3}})
	if s != "ab" || len(e) != 1 || e[0].GetOffset() != 0 || e[0].GetLength() != 2 {
		t.Fatalf("%q %+v", s, e)
	}
}

// TestFfmpegTransforms runs every pipeline on generated samples.
func TestFfmpegTransforms(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	gen := map[mediaKind][]string{
		kindPhoto:     {"-f", "lavfi", "-i", "testsrc=size=64x48", "-frames:v", "1", "src.jpg"},
		kindImage:     {"-f", "lavfi", "-i", "testsrc=size=64x48", "-frames:v", "1", "src.png"},
		kindSticker:   {"-f", "lavfi", "-i", "testsrc=size=64x64", "-frames:v", "1", "-c:v", "libwebp", "src.webp"},
		kindGif:       {"-f", "lavfi", "-i", "testsrc=size=64x48:duration=0.5", "src.gif"},
		kindAnimation: {"-f", "lavfi", "-i", "testsrc=size=64x48:duration=0.5", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-f", "mp4", "src.gif.mp4"},
		kindWebm:      {"-f", "lavfi", "-i", "testsrc=size=64x64:duration=0.5", "-c:v", "libvpx-vp9", "-pix_fmt", "yuva420p", "src.webm"},
	}
	ext := map[mediaKind]string{kindPhoto: ".jpg", kindImage: ".png", kindSticker: ".webp", kindGif: ".gif", kindAnimation: ".gif.mp4", kindWebm: ".webm"}
	for kind, args := range gen {
		src := filepath.Join(dir, args[len(args)-1])
		args = append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args[:len(args)-1]...)
		if out, err := exec.Command("ffmpeg", append(args, src)...).CombinedOutput(); err != nil {
			t.Fatalf("gen %v: %v %s", kind, err, out)
		}
		for _, o := range []mediaOptions{{flip: "h"}, {flip: "v", invert: true}, {invert: true}} {
			dst := filepath.Join(dir, "out"+ext[kind])
			if err := runFfmpeg(context.Background(), buildFfmpegArgs(src, dst, kind, buildFilters(o))); err != nil {
				t.Fatalf("kind %v opts %+v: %v", kind, o, err)
			}
			if st, err := os.Stat(dst); err != nil || st.Size() == 0 {
				t.Fatalf("kind %v: no output", kind)
			}
			os.Remove(dst)
		}
	}
}
