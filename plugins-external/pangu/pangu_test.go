package main

// pangu_test.go — spacing algorithm tests. The expected outputs were
// verified against the original TypeScript implementation (TeleBox
// pangu.ts PanguSpacer.spacing) run on Node, so they lock in the source's
// exact behavior, quirks included.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSpacingBasics(t *testing.T) {
	cases := []struct{ in, want string }{
		{"你好World123abc", "你好 World123abc"},
		{"中文abc", "中文 abc"},
		{"123中文", "123 中文"},
		{"中文English混合text", "中文 English 混合 text"},
		{"已有 空格 的text情况", "已有 空格 的 text 情况"},
		{"中文与English之间的空格已经有了", "中文与 English 之间的空格已经有了"},
		{"数字123456测试", "数字 123456 测试"},
		{"1中文", "1 中文"},
		{"中a文b数字2", "中 a 文 b 数字 2"},
		{"设备1台", "设备 1 台"},
		{"第1章第2节", "第 1 章第 2 节"},
		{"Vue3+TypeScript开发的项目", "Vue3+TypeScript 开发的项目"},
		{"iPhone15ProMax价格9999元", "iPhone15ProMax 价格 9999 元"},
		{"English中文ABC", "English 中文 ABC"},
		{"中文ABC", "中文 ABC"},
	}
	for _, c := range cases {
		if got := spacing(c.in); got != c.want {
			t.Errorf("spacing(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSpacingShortAndNoCJK(t *testing.T) {
	cases := []string{"", "a", "中", "ab", "plain english text", "123 456", "!!!", "hello world"}
	for _, c := range cases {
		if got := spacing(c); got != c {
			t.Errorf("spacing(%q) = %q, want unchanged", c, got)
		}
	}
}

func TestSpacingPunctuation(t *testing.T) {
	cases := []struct{ in, want string }{
		// ':' and '：' are NOT in the ANS class → no space (source quirk).
		{"中文:English", "中文:English"},
		{"中文：English", "中文：English"},
		{"中文: 中文", "中文: 中文"},
		// full-width punctuation untouched
		{"中文（全角）abc", "中文（全角）abc"},
		{"中文。English", "中文。English"},
		{"标题：内容", "标题：内容"},
		{"「你好」world", "「你好」world"},
		{"《书名》abc", "《书名》abc"},
		{"中文……abc", "中文……abc"},
		// half-width punctuation in the ANS class gets spaced before, not after
		{"中文;English", "中文 ;English"},
		{"中文,English", "中文 ,English"},
		{"中文.English", "中文 .English"},
		{"中文/English", "中文 /English"},
		{"中文\\English", "中文 \\English"},
		{"中文~English", "中文 ~English"},
		{"中文^abc", "中文 ^abc"},
		{"中文|English", "中文 |English"},
		{"中文!!English", "中文 !!English"},
		{"啊!!!看", "啊 !!! 看"},
		// '@' and '_' and '%' are NOT in the ANS class
		{"中文@abc", "中文@abc"},
		{"snake_case中文", "snake_case 中文"},
		{"中文_snake", "中文_snake"},
		{"百分比50%中文", "百分比 50%中文"},
		// runs keep inner rhythm: 50%, 3m/s, a=b, a::b
		{"今天气温28度，湿度60%，风速3m/s，AQI是45，天气good", "今天气温 28 度，湿度 60%，风速 3m/s，AQI 是 45，天气 good"},
		{"1+1=2中文", "1+1=2 中文"},
		{"中文a=b", "中文 a=b"},
		{"中文a::b中文", "中文 a::b 中文"},
		{"IPv6地址::1中文", "IPv6 地址::1 中文"},
		{"时间12:30中文", "时间 12:30 中文"},
	}
	for _, c := range cases {
		if got := spacing(c.in); got != c.want {
			t.Errorf("spacing(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSpacingQuotesAndBrackets(t *testing.T) {
	cases := []struct{ in, want string }{
		// quotes: CJK+quote and quote+CJK get spaces, then inner spaces are
		// collapsed back by the fix-quote rule
		{"他说\"你好\"然后走了", "他说 \"你好\" 然后走了"},
		{"中文\"引号\"test", "中文 \"引号\"test"},
		{"中文'单引号'abc", "中文 '单引号'abc"},
		{"他说\"hi\"然后", "他说 \"hi\" 然后"},
		{"数字\"123\"中文", "数字 \"123\" 中文"},
		// brackets: space before opening bracket only (closing side untouched
		// unless a digit/letter pair matches)
		{"再见(拜拜)", "再见 (拜拜)"},
		{"中文(括号)test", "中文 (括号)test"},
		{"中文[数组]abc", "中文 [数组]abc"},
		{"中文{花括号}abc", "中文 {花括号}abc"},
		{"中文<尖括号>abc", "中文 <尖括号>abc"},
		{"中文“智能引号”abc", "中文 “智能引号”abc"},
		{"中文(English)中文", "中文 (English) 中文"},
		// inner spaces inside brackets collapse; closing bracket + CJK then
		// gets its space from the ANSI rule
		{"( a )中文", "(a) 中文"},
		{"(  多空格  )中文", "(多空格) 中文"},
		{"HTTPS://EXAMPLE.COM中文", "HTTPS://EXAMPLE.COM 中文"}, // no / after scheme → not a URL
	}
	for _, c := range cases {
		if got := spacing(c.in); got != c.want {
			t.Errorf("spacing(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSpacingHash(t *testing.T) {
	cases := []struct{ in, want string }{
		{"中文#标签 内容", "中文 #标签 内容"},
		{"标签#中文abc", "标签 #中文 abc"},
		{"话题#中文#标签", "话题 #中文# 标签"},
		{"中文#a#b", "中文 #a#b"},
		{"中文#", "中文#"},
		{"中文a#tag", "中文 a#tag"},
		{"a#中文", "a# 中文"},
	}
	for _, c := range cases {
		if got := spacing(c.in); got != c.want {
			t.Errorf("spacing(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSpacingURLs(t *testing.T) {
	cases := []struct{ in, want string }{
		// http(s) URLs are protected verbatim, including the CJK tail some
		// regex engines disagree on
		{"请看https://example.com/中文测试这个链接", "请看https://example.com/中文测试这个链接"},
		{"链接https://example.com/page后面是中文", "链接https://example.com/page后面是中文"},
		{"中文https://example.com中文", "中文https://example.com中文"},
		{"test网址: https://a.b.c/测试 中文", "test 网址: https://a.b.c/测试 中文"},
	}
	for _, c := range cases {
		if got := spacing(c.in); got != c.want {
			t.Errorf("spacing(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// non-http schemes are not protected
	if got := spacing("链接ftp://files.example.com中文"); got != "链接 ftp://files.example.com 中文" {
		t.Errorf("ftp url: got %q", got)
	}
	if got := spacing("地址是www.example.com中文"); got != "地址是 www.example.com 中文" {
		t.Errorf("bare domain: got %q", got)
	}
	if got := spacing("e-mail地址abc@163.com中文"); got != "e-mail 地址 abc@163.com 中文" {
		t.Errorf("email: got %q", got)
	}
}

func TestSpacingEmoji(t *testing.T) {
	cases := []struct{ in, want string }{
		// emoji are neither CJK nor ANS: no space inserted around them
		{"你好😀emoji世界", "你好😀emoji 世界"},
		{"😀你 😀好 😀", "😀你 😀好 😀"},
		{"emoji😀中文", "emoji😀中文"},
		{"中文😀😀abc", "中文😀😀abc"},
	}
	for _, c := range cases {
		if got := spacing(c.in); got != c.want {
			t.Errorf("spacing(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSpacingMarkdownAndMultiline(t *testing.T) {
	cases := []struct{ in, want string }{
		{"MD **bold** 中文abc", "MD **bold** 中文 abc"},
		{"代码`code`中文", "代码 `code` 中文"},
		{"多行\n中文English\n下一行abc", "多行\n中文 English\n下一行 abc"},
		{"中文	English", "中文	English"}, // 	-adjacent pairs are untouched
	}
	for _, c := range cases {
		if got := spacing(c.in); got != c.want {
			t.Errorf("spacing(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Backtick around CJK (oracle-verified quirk: backtick is in the ANS
	// class, so it gets spaced on both sides).
	if got := spacing("中文`代码`中文"); got != "中文 ` 代码 ` 中文" {
		t.Errorf("backtick cjk: got %q", got)
	}
	// tabs count as \s in the fix rules but tab-adjacent pairs are untouched
	if got := spacing("中文\tEnglish"); got != "中文\tEnglish" {
		t.Errorf("tab: got %q", got)
	}
}

func TestSpacingIdempotent(t *testing.T) {
	inputs := []string{
		"中文English混合text", "他说\"你好\"然后走了", "中文(括号)test", "话题#中文#标签",
		"今天气温28度，湿度60%，风速3m/s", "再见(拜拜)", "啊!!!看", "中文`代码`abc",
	}
	for _, in := range inputs {
		once := spacing(in)
		twice := spacing(once)
		if once != twice {
			t.Errorf("not idempotent: %q → %q → %q", in, once, twice)
		}
	}
}

func TestSpacingOnlyAddsSpaces(t *testing.T) {
	inputs := []string{
		"你好World123abc", "中文English混合text", "他说\"你好\"然后走了",
		"话题#中文#标签", "啊!!!看", "中文`代码`abc", "再见(拜拜)",
		"今天气温28度，湿度60%，风速3m/s，AQI是45",
	}
	strip := func(s string) string {
		var b []rune
		for _, r := range s {
			switch r {
			case ' ', '\t', '\n', '\r':
			default:
				b = append(b, r)
			}
		}
		return string(b)
	}
	for _, in := range inputs {
		if got := spacing(in); strip(got) != strip(in) {
			t.Errorf("content changed: %q → %q", in, got)
		}
	}
}

func TestSpacingLongRealistic(t *testing.T) {
	cases := []struct{ in, want string }{
		{"你可以对比一下:中文和English混排的效果", "你可以对比一下:中文和 English 混排的效果"},
		{"当你凝视bug时bug也在凝视你", "当你凝视 bug 时 bug 也在凝视你"},
	}
	for _, c := range cases {
		if got := spacing(c.in); got != c.want {
			t.Errorf("spacing(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- store

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// fresh state
	if on, why := s.effective(100); on || why != "global" {
		t.Fatalf("fresh effective: %v %v", on, why)
	}
	// chat on
	if err := s.setChatMode(100, true); err != nil {
		t.Fatal(err)
	}
	if on, why := s.effective(100); !on || why != "chat" {
		t.Fatalf("chat on: %v %v", on, why)
	}
	// chat off overrides global
	if err := s.setGlobalMode(true); err != nil {
		t.Fatal(err)
	}
	if err := s.setChatMode(100, false); err != nil {
		t.Fatal(err)
	}
	if on, why := s.effective(100); on || why != "chat" {
		t.Fatalf("chat off vs global: %v %v", on, why)
	}
	// blacklist beats chat/global
	if _, err := s.listAdd("black", 100); err != nil {
		t.Fatal(err)
	}
	if on, why := s.effective(100); on || why != "black" {
		t.Fatalf("black: %v %v", on, why)
	}
	// whitelist beats blacklist
	if _, err := s.listAdd("white", 100); err != nil {
		t.Fatal(err)
	}
	if on, why := s.effective(100); !on || why != "white" {
		t.Fatalf("white: %v %v", on, why)
	}
	// reset chat
	ok, err := s.resetChat(100)
	if err != nil || !ok {
		t.Fatalf("reset: %v %v", ok, err)
	}
	if ok, _ = s.resetChat(100); ok {
		t.Fatal("second reset should be a no-op")
	}
	// stats
	_, w, b, custom := s.statsSnapshot()
	if w != 1 || b != 1 || custom != 0 {
		t.Fatalf("snapshot: %d %d %d", w, b, custom)
	}
	s.recordFormatted()
	if st := mustSnapshot(t, s); st.FormattedMessages != 1 || st.LastFormatted == 0 {
		t.Fatalf("stats after record: %+v", st)
	}

	// persistence: reload from disk
	s2, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.globalMode() != true {
		t.Fatal("globalMode not persisted")
	}
	if len(s2.list("white")) != 1 || len(s2.list("black")) != 1 {
		t.Fatalf("lists not persisted: %v %v", s2.list("white"), s2.list("black"))
	}
	if st := mustSnapshot(t, s2); st.FormattedMessages != 1 || st.EnabledChats != 0 {
		t.Fatalf("stats not persisted: %+v", st)
	}

	// list remove
	if ok, _ := s2.listRemove("white", 100); !ok {
		t.Fatal("listRemove white")
	}
	if ok, _ := s2.listRemove("white", 100); ok {
		t.Fatal("listRemove twice")
	}
}

func mustSnapshot(t *testing.T, s *store) panguStats {
	t.Helper()
	st, _, _, _ := s.statsSnapshot()
	return st
}

func TestStoreRoundTripKeepsFileMode(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.setChatMode(-1001234567890, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSpacingOracleCorpus(t *testing.T) {
	// 120 input/expected pairs sampled from a 4000-case differential fuzz
	// run against the original TypeScript implementation (Node). Each pair
	// was produced by executing the source regex cascade verbatim.
	cases := []struct{ in, want string }{
		{"<你https://example.com/a》 中好#《}、》", "<你https://example.com/a》 中好 #《}、》"},
		{"空白）https://中文.io/路径+？(|,", "空白）https:// 中文 .io/ 路径 +？(|,"},
		{"[汉→(…! 空%、%www.example.com。古https://a.co/x#tag…\"中： 「–文？）；世#古界～(好”）http://t.me/x?y=1&z=2好“古https://中文.io/路径文¥(’试https://中文.io/路径之试%", "[汉→(…! 空%、%www.example.com。古https://a.co/x#tag…\" 中： 「–文？）；世 #古界～(好”）http://t.me/x?y=1&z=2好 “古 https:// 中文 .io/ 路径文¥(’试 https:// 中文 .io/ 路径之试%"},
		{"你中界空格之试%www.example.com汉白'—测」www.example.comhttp://t.me/x?y=1&z=2？格http://t.me/x?y=1&z=2😀？9格字2>中~Z测”’测」", "你中界空格之试%www.example.com 汉白 '—测」www.example.comhttp://t.me/x?y=1&z=2？格http://t.me/x?y=1&z=2😀？9 格字 2> 中 ~Z 测”’测」"},
		{"6]", "6]"},
		{"古字？www.example.com「0汉—www.example.com古你好《7https://a.co/x#tag%…/€；X？“>盘之古「[66https://a.co/x#tag「~文测’https://example.com/a{", "古字？www.example.com「0 汉—www.example.com 古你好《7https://a.co/x#tag%…/€；X？“> 盘之古「[66https://a.co/x#tag「~ 文测’https://example.com/a{"},
		{"]www.example.com9）你|https://example.com/a中「字界～9」https://example.com/a（*¥ 白空X—-！文！盘a～字>好\n#好汉中👍、：", "]www.example.com9）你 |https://example.com/a中「字界～9」https://example.com/a（*¥ 白空 X—-！文！盘 a～字 > 好\n#好汉中👍、："},
		{"你^www.example.com[!^：盘b’http://t.me/x?y=1&z=2好?&之", "你 ^www.example.com[!^：盘 b’http://t.me/x?y=1&z=2好 ?& 之"},
		{"》www.example.com汉、;」", "》www.example.com 汉、;」"},
		{"¥！世$#界https://example.com/a）Zhttps://中文.io/路径a中.：→白https://a.co/x#tag{！测、…～…*空", "¥！世 $# 界https://example.com/a）Zhttps:// 中文 .io/ 路径 a 中 .：→白https://a.co/x#tag{！测、…～…* 空"},
		{"」‘2#[*》：好！。Y-…；www.example.com测。」", "」‘2#[*》：好！。Y-…；www.example.com 测。」"},
		{"；文,–（;之好、、c#-字…「😀^！测文!空你；…@4：好’", "；文 ,–（; 之好、、c#- 字…「😀^！测文 ! 空你；…@4：好’"},
		{"。世》:2€测好之字你“；）;？盘；字€空https://example.com/a&之中", "。世》:2€测好之字你 “；）;？盘；字€空https://example.com/a&之中"},
		{"；中盘）$文< 格）空~。@。–`·http://t.me/x?y=1&z=2?好@你²、/文<0http://t.me/x?y=1&z=2～$(。之字《Z白世古-=文盘～测", "；中盘）$ 文 < 格）空 ~。@。–`·http://t.me/x?y=1&z=2?好@你²、/ 文 <0http://t.me/x?y=1&z=2～$(。之字《Z 白世古 -= 文盘～测"},
		{"https://中文.io/路径「、、", "https:// 中文 .io/ 路径「、、"},
		{"£c\\；～0https://中文.io/路径格测·格 …https://a.co/x#tag(；盘界《《字…测b））你5（：；.、测」www.example.comYhttps://a.co/x#tag空。<", "£c\\；～0https:// 中文 .io/ 路径格测·格 …https://a.co/x#tag(；盘界《《字…测 b））你 5（：；.、测」www.example.comYhttps://a.co/x#tag空。<"},
		{"*{5https://中文.io/路径0好8²试0格·3世³·」https://a.co/x#taghttps://example.com/a试、～测’c《》”》「字」格https://a.co/x#tag：·好www.example.com《#https://a.co/x#tag之白测https://a.co/x#taghttps://中文.io/路径%", "*{5https:// 中文 .io/ 路径 0 好 8²试 0 格·3 世³·」https://a.co/x#taghttps://example.com/a 试、～测’c《》”》「字」格https://a.co/x#tag：·好 www.example.com《#https://a.co/x#tag之白测https://a.co/x#taghttps://中文 .io/ 路径%"},
		{"之你～》～\\<古、之中。试^]‘测*）「Y…白·《「$白!²》→£你中《`$字好http://t.me/x?y=1&z=2；界€", "之你～》～\\<古、之中。试 ^]‘测 *）「Y…白·《「$ 白 !²》→£你中《`$ 字好http://t.me/x?y=1&z=2；界€"},
		{"；）@€`、6*、https://中文.io/路径」https://example.com/a/6世\\https://example.com/ahttps://example.com/a世中“汉！汉「古& `https://中文.io/路径（中（”https://example.com/ahttps://a.co/x#tag测", "；）@€`、6*、https:// 中文 .io/ 路径」https://example.com/a/6世 \\https://example.com/ahttps://example.com/a 世中 “汉！汉「古 & `https:// 中文 .io/ 路径（中（”https://example.com/ahttps://a.co/x#tag 测"},
		{"测、字古<", "测、字古 <"},
		{"世空！https://example.com/a、http://t.me/x?y=1&z=2白试汉试0https://a.co/x#tag文；", "世空！https://example.com/a、http://t.me/x?y=1&z=2白试汉试 0https://a.co/x#tag文；"},
		{"（'7", "（'7"},
		{"3空中；http://t.me/x?y=1&z=2’Z？0～好字」\t？https://a.co/x#tag界https://a.co/x#tag)。…。～…4http://t.me/x?y=1&z=2《汉（字\"@：试,", "3 空中；http://t.me/x?y=1&z=2’Z？0～好字」\t？https://a.co/x#tag界https://a.co/x#tag)。…。～…4http://t.me/x?y=1&z=2《汉（字 \"@：试 ,"},
		{"界！。👍,…空世盘中Y-白你汉）古》；你https://example.com/aa字之～>之字文中&空½你」》!http://t.me/x?y=1&z=2", "界！。👍,…空世盘中 Y- 白你汉）古》；你https://example.com/aa字之～> 之字文中 & 空½你」》!http://t.me/x?y=1&z=2"},
		{"中(古X¥好汉格；试：€|http://t.me/x?y=1&z=2文/；？格（0之 《；www.example.com{@？字~：盘-http://t.me/x?y=1&z=2😀", "中 (古 X¥好汉格；试：€|http://t.me/x?y=1&z=2文 /；？格（0 之 《；www.example.com{@？字 ~：盘 -http://t.me/x?y=1&z=2😀"},
		{"http://t.me/x?y=1&z=2–http://t.me/x?y=1&z=23？“7^:汉*www.example.com你.·测https://a.co/x#tag\"www.example.com「", "http://t.me/x?y=1&z=2–http://t.me/x?y=1&z=23？“7^:汉 *www.example.com 你 .·测https://a.co/x#tag\"www.example.com「"},
		{"😀→古€’》测白试·X%。。世,文试好”格界）→“{试）—²*\"试～古{～", "😀→古€’》测白试·X%。。世 , 文试好” 格界）→“{试）—²*\" 试～古 {～"},
		{"👍Zhttp://t.me/x?y=1&z=2\t=格（4》好世³测「空字a https://中文.io/路径`。Z？空9\"¥;」《中“'😀X中古:→：", "👍Zhttp://t.me/x?y=1&z=2\t= 格（4》好世³测「空字 a https:// 中文 .io/ 路径 `。Z？空 9\"¥;」《中 “'😀X 中古:→："},
		{"；」5汉£https://中文.io/路径《～https://中文.io/路径格", "；」5 汉£https:// 中文 .io/ 路径《～https:// 中文 .io/ 路径格"},
		{"https://中文.io/路径文🎉盘6#…文\t\n#。·…界！（🎉https://中文.io/路径界）字：7界好www.example.com_…（文《世", "https:// 中文 .io/ 路径文🎉盘 6#…文\t\n#。·…界！（🎉https:// 中文 .io/ 路径界）字：7 界好 www.example.com_…（文《世"},
		{"「https://example.com/a<3\n汉c界）https://a.co/x#tag:}》http://t.me/x?y=1&z=2；：$^字。Y\t你http://t.me/x?y=1&z=2https://example.com/ahttps://中文.io/路径https://a.co/x#tag好¥}https://example.com/a、", "「https://example.com/a<3\n汉 c 界）https://a.co/x#tag:}》http://t.me/x?y=1&z=2；：$^ 字。Y\t你http://t.me/x?y=1&z=2https://example.com/ahttps:// 中文 .io/ 路径https://a.co/x#tag好¥}https://example.com/a、"},
		{"https://a.co/x#taghttps://a.co/x#tag（～\\’(https://中文.io/路径；a好\n》4€https://a.co/x#tag中：盘」b=你www.example.com（https://example.com/a)9$「）/…～中—http://t.me/x?y=1&z=2bX：试古", "https://a.co/x#taghttps://a.co/x#tag（～\\’(https:// 中文 .io/ 路径；a 好\n》4€https://a.co/x#tag中：盘」b= 你 www.example.com（https://example.com/a)9$「）/…～中—http://t.me/x?y=1&z=2bX：试古"},
		{"<「", "<「"},
		{"空”chttps://a.co/x#tag字。、https://a.co/x#tag测👍古好/https://example.com/a古汉6>～「试界", "空”chttps://a.co/x#tag字。、https://a.co/x#tag测👍古好 /https://example.com/a古汉 6>～「试界"},
		{"：汉75、（9https://中文.io/路径https://a.co/x#tagZ「。", "：汉 75、（9https:// 中文 .io/ 路径https://a.co/x#tagZ「。"},
		{"—&盘/世‘、|）空`、界}？。8www.example.com–https://example.com/a–‘空b", "—& 盘 / 世‘、|）空 `、界}？。8www.example.com–https://example.com/a–‘空 b"},
		{"空盘https://example.com/ahttp://t.me/x?y=1&z=2之字中Y““", "空盘https://example.com/ahttp://t.me/x?y=1&z=2 之字中 Y““"},
		{"=http://t.me/x?y=1&z=2", "=http://t.me/x?y=1&z=2"},
		{"。～文」https://中文.io/路径c测=a", "。～文」https:// 中文 .io/ 路径 c 测 =a"},
		{"世(～. Z」'8！你6古¥www.example.com格试https://a.co/x#tag（https://example.com/a；]Y?https://中文.io/路径>²格：你!<…白」;8》https://中文.io/路径}字世=～#+？", "世 (～. Z」'8！你 6 古¥www.example.com 格试https://a.co/x#tag（https://example.com/a；]Y?https:// 中文 .io/ 路径 >²格：你 !<…白」;8》https:// 中文 .io/ 路径} 字世 =～#+？"},
		{"$7白。¥a{试https://中文.io/路径盘：之；「£😀！👍：）界你`www.example.com‘8》：>€—”～白字www.example.com试（好https://a.co/x#tag…；", "$7 白。¥a{试 https:// 中文 .io/ 路径盘：之；「£😀！👍：）界你 `www.example.com‘8》：>€—”～白字 www.example.com 试（好https://a.co/x#tag…；"},
		{"/https://a.co/x#tag²。？?「😀$《https://中文.io/路径好~盘《", "/https://a.co/x#tag²。？?「😀$《https:// 中文 .io/ 路径好 ~ 盘《"},
		{"盘:www.example.comhttp://t.me/x?y=1&z=2》!¥字界http://t.me/x?y=1&z=2$（…好—格http://t.me/x?y=1&z=2！`~https://a.co/x#tag8£…1；·😀你字c！：¥盘《", "盘:www.example.comhttp://t.me/x?y=1&z=2》!¥字界http://t.me/x?y=1&z=2$（…好—格http://t.me/x?y=1&z=2！`~https://a.co/x#tag8£…1；·😀你字 c！：¥盘《"},
		{"古～*「！(白/https://a.co/x#tag4!！白…) 4：-《€《http://t.me/x?y=1&z=2£c→–5～世之£文aZ😀https://a.co/x#tag古", "古～*「！(白 /https://a.co/x#tag4!！白…) 4：-《€《http://t.me/x?y=1&z=2£c→–5～世之£文 aZ😀https://a.co/x#tag古"},
		{"https://example.com/a汉98好]5汉http://t.me/x?y=1&z=2白中古”》「！|格：之？试）《3🎉%2", "https://example.com/a汉 98 好]5 汉http://t.me/x?y=1&z=2白中古”》「！| 格：之？试）《3🎉%2"},
		{"！].’👍", "！].’👍"},
		{"。https://a.co/x#tag3\"", "。https://a.co/x#tag3\""},
		{"www.example.com{你字试「白字/]汉：https://a.co/x#tag\\。（汉www.example.com|」https://中文.io/路径http://t.me/x?y=1&z=2～世格试a:.测？*²」”之《古https://example.com/a世中～https://a.co/x#tag文汉https://中文.io/路径\"£空文Y", "www.example.com{你字试「白字 /] 汉：https://a.co/x#tag\\。（汉 www.example.com|」https:// 中文 .io/ 路径http://t.me/x?y=1&z=2～世格试 a:. 测？*²」” 之《古https://example.com/a世中～https://a.co/x#tag文汉 https:// 中文 .io/ 路径 \"£空文 Y"},
		{"：%》*³*好½&9>^8格。之#文格测盘4…[界", "：%》*³* 好½&9>^8 格。之 #文格测盘 4…[界"},
		{"」`文/https://a.co/x#tag~）好=（《；%<€测中}？www.example.com→试", "」` 文 /https://a.co/x#tag~）好 =（《；%<€测中}？www.example.com→试"},
		{"–}^试*？！👍白…:格Z(「/汉c界€；2好）7：？试:", "–}^ 试 *？！👍白…:格 Z(「/ 汉 c 界€；2 好）7：？试:"},
		{"\n世试！」\n汉～）http://t.me/x?y=1&z=2测～”2b6》盘; 空🎉》Y‘>6好https://中文.io/路径https://example.com/a€Zhttps://a.co/x#tag;www.example.com!(世(·盘https://中文.io/路径€-测a…", "\n世试！」\n汉～）http://t.me/x?y=1&z=2测～”2b6》盘 ; 空🎉》Y‘>6 好 https:// 中文 .io/ 路径https://example.com/a€Zhttps://a.co/x#tag;www.example.com!(世 (·盘 https:// 中文 .io/ 路径€- 测 a…"},
		{"1https://example.com/a好古*,界¥。之<》(🎉盘http://t.me/x?y=1&z=2？格好（)「", "1https://example.com/a好古 *, 界¥。之 <》(🎉盘http://t.me/x?y=1&z=2？格好（)「"},
		{"中³#好‘%>汉$_文8Z「_）https://a.co/x#tag", "中³# 好‘%> 汉 $_文 8Z「_）https://a.co/x#tag"},
		{"https://example.com/a6€)\"\"¥测|》好测1|¥～\"", "https://example.com/a6€)\"\"¥测 |》好测 1|¥～\""},
		{"「*；空[？¥¥；’>1试汉6中a\t空 /http://t.me/x?y=1&z=2好https://中文.io/路径好www.example.com!", "「*；空 [？¥¥；’>1 试汉 6 中 a\t空 /http://t.me/x?y=1&z=2好 https:// 中文 .io/ 路径好 www.example.com!"},
		{"\\字之格空白http://t.me/x?y=1&z=2好））bwww.example.com…5www.example.com,‘～ahttps://example.com/a¥-$你_汉https://a.co/x#tag²&£、X…", "\\ 字之格空白http://t.me/x?y=1&z=2好））bwww.example.com…5www.example.com,‘～ahttps://example.com/a¥-$ 你_汉https://a.co/x#tag²&£、X…"},
		{"·）³）\n《界好https://中文.io/路径？「", "·）³）\n《界好 https:// 中文 .io/ 路径？「"},
		{"世。·之》&,_。你》好》·汉|「/#？界、字古盘之》}(https://中文.io/路径试「》3!!2「中盘\n’、～白（世", "世。·之》&,_。你》好》·汉 |「/#？界、字古盘之》}(https:// 中文 .io/ 路径试「》3!!2「中盘\n’、～白（世"},
		{"格空https://中文.io/路径盘$#>「）https://example.com/aa？；🎉", "格空 https:// 中文 .io/ 路径盘 $#>「）https://example.com/aa？；🎉"},
		{"Z。（Y7'www.example.com盘@https://中文.io/路径‘世6€$！’\t4；³空4白～～#…中！–", "Z。（Y7'www.example.com 盘@https:// 中文 .io/ 路径‘世 6€$！’\t4；³空 4 白～～#…中！–"},
		{"《{「*→>Z「{_～汉·、<0c–", "《{「*→>Z「{_～汉·、<0c–"},
		{"；\\世？文）之！b\nawww.example.com白}Z’测之汉试'界¥http://t.me/x?y=1&z=2测「《>^‘格`空「Y《Z界：字", "；\\ 世？文）之！b\nawww.example.com 白}Z’测之汉试 ' 界¥http://t.me/x?y=1&z=2测「《>^‘格 ` 空「Y《Z 界：字"},
		{"http://t.me/x?y=1&z=2～6《。》€！你", "http://t.me/x?y=1&z=2～6《。》€！你"},
		{"}试中之$%3https://中文.io/路径？）}中‘https://a.co/x#tag格之/‘Z<https://a.co/x#tag）/‘盘6https://example.com/a测界你)http://t.me/x?y=1&z=2~www.example.com~http://t.me/x?y=1&z=2；）", "} 试中之 $%3https:// 中文 .io/ 路径？）} 中‘https://a.co/x#tag格之 /‘Z<https://a.co/x#tag）/‘盘 6https://example.com/a测界你)http://t.me/x?y=1&z=2~www.example.com~http://t.me/x?y=1&z=2；）"},
		{"https://a.co/x#tag」中,。https://example.com/a世)；%]²…https://example.com/a世、盘空：！)#）~:古汉9白（你.你www.example.com汉盘字）格", "https://a.co/x#tag」中 ,。https://example.com/a世)；%]²…https://example.com/a世、盘空：！)#）~:古汉 9 白（你 . 你 www.example.com 汉盘字）格"},
		{"字之]》→*「<～古试？6👍？", "字之]》→*「<～古试？6👍？"},
		{"测界汉@X”5白…www.example.com测好「好_@）》、古https://example.com/ahttps://中文.io/路径b8]。？3世–`～", "测界汉@X”5 白…www.example.com 测好「好_@）》、古https://example.com/ahttps://中文 .io/ 路径 b8]。？3 世–`～"},
		{"[、」试「,中试汉盘[、'Zhttps://中文.io/路径Z£`http://t.me/x?y=1&z=22www.example.comwww.example.comwww.example.com½Z中文}：文之空www.example.com*好*》\n格6~？中（‘空", "[、」试「, 中试汉盘 [、'Zhttps:// 中文 .io/ 路径 Z£`http://t.me/x?y=1&z=22www.example.comwww.example.comwww.example.com½Z 中文}：文之空 www.example.com* 好 *》\n格 6~？中（‘空"},
		{"（）字？”^X古@你）¥》古:$chttps://a.co/x#tag…！Y\"`！文}#试>9\n½,汉（！–:中b你https://a.co/x#tag汉", "（）字？”^X 古@你）¥》古:$chttps://a.co/x#tag…！Y\"`！文}# 试 >9\n½, 汉（！–:中 b 你https://a.co/x#tag汉"},
		{"！;\\www.example.com（2\t；《https://example.com/a、盘、c你「2格^\t>试好「字https://example.com/ahttps://example.com/ahttps://中文.io/路径盘", "！;\\www.example.com（2\t；《https://example.com/a、盘、c 你「2 格 ^\t> 试好「字https://example.com/ahttps://example.com/ahttps:// 中文 .io/ 路径盘"},
		{"界字之：「`、世9\"c之0文www.example.com…http://t.me/x?y=1&z=2文", "界字之：「`、世 9\"c 之 0 文 www.example.com…http://t.me/x?y=1&z=2文"},
		{"http://t.me/x?y=1&z=2？?好（9):文3」；€（=界{你5测古中、汉@)。·字–https://example.com/a=", "http://t.me/x?y=1&z=2？? 好（9):文 3」；€（= 界 {你 5 测古中、汉@)。·字–https://example.com/a="},
		{"https://a.co/x#tag盘!#之‘界格字《`《…([）→½…9你 《\"；*。", "https://a.co/x#tag盘 !# 之‘界格字《`《…([）→½…9 你 《\"；*。"},
		{"之<《 」€盘:…之中9你/.、试试：：》好7¥`「：古格5", "之 <《 」€盘:…之中 9 你 /.、试试：：》好 7¥`「：古格 5"},
		{"试?http://t.me/x?y=1&z=2你…a1!字…;、\n0汉。https://中文.io/路径好@试 www.example.com格空 #=\"<试）～！？0古；～‘。:中½", "试 ?http://t.me/x?y=1&z=2你…a1! 字…;、\n0 汉。https:// 中文 .io/ 路径好@试 www.example.com 格空 #=\"<试）～！？0 古；～‘。:中½"},
		{"4文\\/》好“5!！文盘你,7…字测", "4 文 \\/》好 “5!！文盘你 ,7…字测"},
		{"$(）汉白测、_空http://t.me/x?y=1&z=2、?中#6`测\\！空www.example.com&汉?https://a.co/x#tag³：[https://a.co/x#tag%（+。空%<之中、文盘古（—https://example.com/a6_测http://t.me/x?y=1&z=2文古", "$(）汉白测、_空http://t.me/x?y=1&z=2、? 中 #6` 测 \\！空 www.example.com& 汉 ?https://a.co/x#tag³：[https://a.co/x#tag%（+。空%<之中、文盘古（—https://example.com/a6_测http://t.me/x?y=1&z=2文古"},
		{"世之2'https://中文.io/路径https://a.co/x#tag!）https://example.com/a。古”8.空空古6中https://a.co/x#tag¥你、、界'", "世之 2'https:// 中文 .io/ 路径https://a.co/x#tag!）https://example.com/a。古”8. 空空古 6 中https://a.co/x#tag¥你、、界'"},
		{".b\t ；[http://t.me/x?y=1&z=2格0", ".b\t ；[http://t.me/x?y=1&z=2格 0"},
		{"界？https://a.co/x#tag盘」你好中汉https://a.co/x#taga测（世+）界www.example.comb", "界？https://a.co/x#tag盘」你好中汉https://a.co/x#taga测（世 +）界 www.example.comb"},
		{";、～ https://example.com/a2https://a.co/x#tag€Y《（、<b好测6=’试https://example.com/ahttps://a.co/x#tag)https://中文.io/路径古《²^", ";、～ https://example.com/a2https://a.co/x#tag€Y《（、<b 好测 6=’试https://example.com/ahttps://a.co/x#tag)https:// 中文 .io/ 路径古《²^"},
		{"9’字》好：中格你🎉你》X½=#,https://a.co/x#tag0古白！你》！http://t.me/x?y=1&z=2\n？空{～,之～中盘4空!好之https://a.co/x#tag：https://example.com/a", "9’字》好：中格你🎉你》X½=#,https://a.co/x#tag0古白！你》！http://t.me/x?y=1&z=2\n？空 {～, 之～中盘 4 空 ! 好之https://a.co/x#tag：https://example.com/a"},
		{")白」&.（\n!9～5\t,–中_|「格/盘~空'文白https://中文.io/路径2测http://t.me/x?y=1&z=2https://a.co/x#tagY古界%：]：！c之'~！_", ") 白」&.（\n!9～5\t,–中_|「格 / 盘 ~ 空 '文白 https:// 中文 .io/ 路径 2 测http://t.me/x?y=1&z=2https://a.co/x#tagY 古界%：]：！c 之'~！_"},
		{"→。）空」7< 63界？http://t.me/x?y=1&z=2Z、*&文？http://t.me/x?y=1&z=2", "→。）空」7< 63 界？http://t.me/x?y=1&z=2Z、*& 文？http://t.me/x?y=1&z=2"},
		{"；世http://t.me/x?y=1&z=2https://example.com/a；5]试·《–～格’(https://a.co/x#tag中", "；世http://t.me/x?y=1&z=2https://example.com/a；5] 试·《–～格’(https://a.co/x#tag中"},
		{"b你测<·1https://中文.io/路径《：}～中\\「", "b 你测 <·1https:// 中文 .io/ 路径《：}～中 \\「"},
		{"…、www.example.com…！《4汉白」\\$「古?好\nwww.example.com…（ _中http://t.me/x?y=1&z=2", "…、www.example.com…！《4 汉白」\\$「古 ? 好\nwww.example.com…（ _中http://t.me/x?y=1&z=2"},
		{"🎉³\"£试%www.example.com空“🎉空https://a.co/x#tag’白6？“》《+字`¥", "🎉³\"£试%www.example.com 空 “🎉空https://a.co/x#tag’白 6？“》《+ 字 `¥"},
		{"汉\n、中²…盘你！界·世.bhttps://example.com/a：^？」汉界界）1{!9£>", "汉\n、中²…盘你！界·世 .bhttps://example.com/a：^？」汉界界）1{!9£>"},
		{"€..|？；；中=(&{³格中5!Z格", "€..|？；；中 =(&{³格中 5!Z 格"},
		{"、\nwww.example.com_：www.example.com>!😀(https://example.com/a7·=|你https://中文.io/路径白字4「>https://example.com/a好《格³8；^你字+£", "、\nwww.example.com_：www.example.com>!😀(https://example.com/a7·=| 你 https:// 中文 .io/ 路径白字 4「>https://example.com/a好《格³8；^ 你字 +£"},
		{"）b", "）b"},
		{"¥https://example.com/a之！’'～）》之http://t.me/x?y=1&z=21。盘格」\t'\"界：空；3格盘·。[www.example.com字文「0汉8）www.example.com", "¥https://example.com/a之！’'～）》之http://t.me/x?y=1&z=21。盘格」'\" 界：空；3 格盘·。[www.example.com 字文「0 汉 8）www.example.com"},
		{"试<空、世b–\\*》中}古5？http://t.me/x?y=1&z=2？", "试 <空、世 b–\\*》中} 古 5？http://t.me/x?y=1&z=2？"},
		{"？}界7b～）》之www.example.com–」", "？} 界 7b～）》之 www.example.com–」"},
		{"；字\\\"‘,界格www.example.com—。)。‘www.example.com古www.example.com！？Z”www.example.com汉😀文‘盘盘0《:「½https://example.com/a4https://中文.io/路径测(；字https://中文.io/路径—³https://a.co/x#tag", "；字 \\\"‘, 界格 www.example.com—。)。‘www.example.com 古 www.example.com！？Z”www.example.com 汉😀文‘盘盘 0《:「½https://example.com/a4https://中文 .io/ 路径测 (；字 https:// 中文 .io/ 路径—³https://a.co/x#tag"},
		{"好试！http://t.me/x?y=1&z=2", "好试！http://t.me/x?y=1&z=2"},
		{"¥》：7好/空。→^」之「`www.example.com界（（」Y、你文白界、}白…“！「0Y格!https://中文.io/路径：\"空测🎉8：《", "¥》：7 好 / 空。→^」之「`www.example.com 界（（」Y、你文白界、} 白…“！「0Y 格 !https:// 中文 .io/ 路径：\" 空测🎉8：《"},
		{"→https://中文.io/路径～😀汉?…\\白》…～；https://example.com/a字好好www.example.com试「", "→https:// 中文 .io/ 路径～😀汉 ?…\\ 白》…～；https://example.com/a字好好 www.example.com 试「"},
		{"https://中文.io/路径https://example.com/a🎉；https://a.co/x#tagwww.example.com\\+～http://t.me/x?y=1&z=2’中https://中文.io/路径汉>🎉？：—文。https://a.co/x#tag、’8%@\"。-+www.example.com盘。Z」试！》https://a.co/x#tag!", "https:// 中文 .io/ 路径https://example.com/a🎉；https://a.co/x#tagwww.example.com\\+～http://t.me/x?y=1&z=2’中 https:// 中文 .io/ 路径汉 >🎉？：—文。https://a.co/x#tag、’8%@\"。-+www.example.com 盘。Z」试！》https://a.co/x#tag!"},
		{"5)*-", "5)*-"},
		{"之白", "之白"},
		{"’https://example.com/a2", "’https://example.com/a2"},
		{"～、！“<.！&文", "～、！“<.！& 文"},
		{"https://example.com/awww.example.com🎉5^¥文！`、测X；www.example.com", "https://example.com/awww.example.com🎉5^¥文！`、测 X；www.example.com"},
		{"’试…@界4格：…之[+世测界古？7中\"-/https://a.co/x#tag你～", "’试…@界 4 格：…之 [+ 世测界古？7 中 \"-/https://a.co/x#tag你～"},
		{"–界古–好https://中文.io/路径)🎉古8格白…€c」试]」、<。：www.example.comhttps://example.com/awww.example.com你古好http://t.me/x?y=1&z=2¥白|\\’白?", "–界古–好 https:// 中文 .io/ 路径)🎉古 8 格白…€c」试]」、<。：www.example.comhttps://example.com/awww.example.com 你古好http://t.me/x?y=1&z=2¥白 |\\’白 ?"},
		{"格_「…https://中文.io/路径）古https://中文.io/路径@》空😀https://中文.io/路径~汉《\"https://a.co/x#tag格https://example.com/a～试?试→4之!%Za7、古<https://a.co/x#tag》白：", "格_「…https:// 中文 .io/ 路径）古 https:// 中文 .io/ 路径@》空😀https:// 中文 .io/ 路径 ~ 汉《\"https://a.co/x#tag格https://example.com/a～试 ? 试→4 之 !%Za7、古 <https://a.co/x#tag》白："},
		{"测、「》https://a.co/x#taghttps://a.co/x#tag😀|\"`格盘‘https://a.co/x#tag\n白文[Z。Y测~》@\n(…+→🎉🎉、6=http://t.me/x?y=1&z=2：-《", "测、「》https://a.co/x#taghttps://a.co/x#tag😀|\"` 格盘‘https://a.co/x#tag\n白文 [Z。Y 测 ~》@\n(…+→🎉🎉、6=http://t.me/x?y=1&z=2：-《"},
		{"中*盘{https://example.com/a.。测“c你;http://t.me/x?y=1&z=2\\@」：之>9中https://example.com/a", "中 * 盘 {https://example.com/a.。测 “c 你 ;http://t.me/x?y=1&z=2\\@」：之>9 中https://example.com/a"},
		{"'’http://t.me/x?y=1&z=2盘测字你盘", "'’http://t.me/x?y=1&z=2盘测字你盘"},
		{"#http://t.me/x?y=1&z=2？中1白格‘)‘www.example.com。https://中文.io/路径'。古[;www.example.com½>字字文", "#http://t.me/x?y=1&z=2？中 1 白格‘)‘www.example.com。https:// 中文 .io/ 路径 '。古 [;www.example.com½> 字字文"},
		{",;'https://a.co/x#tagX=「www.example.com汉0&3", ",;'https://a.co/x#tagX=「www.example.com 汉 0&3"},
		{"🎉：.字空、世中https://中文.io/路径#世）=试0试」。=「～", "🎉：. 字空、世中 https:// 中文 .io/ 路径 #世）= 试 0 试」。=「～"},
		{"👍、¥界https://example.com/a$白世、中4格。～,³3]空#》[6*《、」–)https://a.co/x#tag》你界→<界http://t.me/x?y=1&z=2 http://t.me/x?y=1&z=2《+白好🎉!字空格", "👍、¥界https://example.com/a$ 白世、中 4 格。～,³3] 空 #》[6*《、」–)https://a.co/x#tag》你界→<界http://t.me/x?y=1&z=2 http://t.me/x?y=1&z=2《+ 白好🎉! 字空格"},
		{"好(—界", "好 (—界"},
		{"”7：」好好（\\https://example.com/a'盘", "”7：」好好（\\https://example.com/a' 盘"},
		{"好。…8）格–白2」～-;https://example.com/a空界盘·中！《字世试|www.example.com汉]", "好。…8）格–白 2」～-;https://example.com/a空界盘·中！《字世试 |www.example.com 汉]"},
		{"https://a.co/x#tag-https://a.co/x#tag’、古", "https://a.co/x#tag-https://a.co/x#tag’、古"},
	}
	for _, c := range cases {
		if got := spacing(c.in); got != c.want {
			t.Errorf("spacing(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
