package main

import (
	"testing"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		args         []string
		target, text string
	}{
		{[]string{"hello", "world"}, "", "hello world"},
		{[]string{"en", "你好"}, "en", "你好"},
		{[]string{"ja", "hello"}, "ja", "hello"},
		{[]string{"zh", "hello"}, "zh-CN", "hello"},
		{[]string{"tw", "hello"}, "zh-TW", "hello"},
		{[]string{"it", "is", "fine"}, "", "it is fine"},
		{[]string{"-t", "it", "hello"}, "it", "hello"},
		{[]string{"to:no", "hello"}, "no", "hello"},
		{[]string{"en"}, "en", ""},
		{[]string{"it"}, "it", ""},
	}
	for _, c := range cases {
		tg, tx, _ := parseArgs(c.args)
		if tg != c.target || tx != c.text {
			t.Errorf("%v: got (%q,%q) want (%q,%q)", c.args, tg, tx, c.target, c.text)
		}
	}
}

func TestParsers(t *testing.T) {
	r, err := parseGtx([]byte(`[[["你好","hello",null,null,1],["世界","world",null,null,1]],null,"en"]`))
	if err != nil || r.text != "你好世界" || r.detected != "en" {
		t.Fatalf("gtx %+v %v", r, err)
	}
	r, err = parseDict([]byte(`[["Hello World.\nsecond line","zh-CN"]]`))
	if err != nil || r.text != "Hello World.\nsecond line" || r.detected != "zh-CN" {
		t.Fatalf("dict %+v %v", r, err)
	}
	body := ")]}'\n\n541\n" + `[["wrb.fr","MkEWBc","[[null,null,\"zh-CN\",[],null,null,[\"x\",\"auto\",\"en\",true]],[[[null,null,null,null,null,[[\"Hello World.\",null,null,null,null,null,\"你好，世界。\",1],[\"The weather is nice today.\",null,true,null,null,null,\"今天天气很好。\",1]],null,null,null,[]]],\"en\",1,\"zh-CN\",[\"x\",\"auto\",\"en\",true]],\"zh-CN\",null,null,null,null,[[[0]]]]",null,null,null,"generic"]]` + "\n25\n"
	r, err = parseBatch([]byte(body))
	if err != nil || r.text != "Hello World. The weather is nice today." || r.detected != "zh-CN" {
		t.Fatalf("batch %+v %v", r, err)
	}
}

func TestHelpers(t *testing.T) {
	if !mostlyChinese("你好，世界") || mostlyChinese("hello 世") || mostlyChinese("こんにちは世界") {
		t.Fatal("mostlyChinese")
	}
	parts := splitRunes("aaaa\nbbbb\ncccc", 6)
	if len(parts) != 3 || parts[0] != "aaaa\n" {
		t.Fatalf("%q", parts)
	}
}

func TestTargetHelpers(t *testing.T) {
	if !sameLang("zh-TW", "zh-CN") || !sameLang("zh", "zh-CN") || sameLang("", "en") || sameLang("ja", "en") {
		t.Fatal("sameLang")
	}
	if flipTarget("zh-CN") != "en" || flipTarget("en") != "zh-CN" || flipTarget("ja") != "en" {
		t.Fatal("flipTarget")
	}
	for _, c := range targetChoices {
		if _, ok := langNames[c]; !ok {
			t.Fatalf("choice %s has no name", c)
		}
	}
	p := New()
	if def, flip := p.defaults(); def != "zh-CN" || !flip {
		t.Fatal("defaults without settings")
	}
}
