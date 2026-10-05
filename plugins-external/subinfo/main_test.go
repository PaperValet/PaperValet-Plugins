package main

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func zhOnly(zh, _ string) string { return zh }
func enOnly(_, en string) string { return en }

func TestParseUserinfo(t *testing.T) {
	up, down, total, expire, start := parseUserinfo("upload=1048576; download=2097152; total=10737418240; expire=1735689600; starttime=1704067200")
	if up != 1048576 || down != 2097152 || total != 10737418240 || expire != 1735689600 || start != 1704067200 {
		t.Fatalf("got up=%d down=%d total=%d expire=%d start=%d", up, down, total, expire, start)
	}
	// missing fields stay zero, order-independent, spaces tolerated
	up, down, total, expire, start = parseUserinfo("download=5; upload=3")
	if up != 3 || down != 5 || total != 0 || expire != 0 || start != 0 {
		t.Fatalf("partial: got %d %d %d %d %d", up, down, total, expire, start)
	}
	// garbage values ignored
	_, _, total, _, _ = parseUserinfo("total=abc; expire=")
	if total != 0 {
		t.Fatalf("garbage total = %d", total)
	}
}

func TestContentDispositionName(t *testing.T) {
	cases := map[string]string{
		`attachment; filename="config.yml"`:                    "config.yml",
		`attachment; filename*=UTF-8''%E9%A3%9E%E9%B8%9F.yaml`: "飞鸟.yaml",
		`attachment; filename="sub %E8%AE%A2%E9%98%85.txt"`:    "sub 订阅.txt",
		"":                            "",
		`inline; filename='raw name'`: "raw name",
	}
	for in, want := range cases {
		if got := contentDispositionName(in); got != want {
			t.Errorf("contentDispositionName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMappingLines(t *testing.T) {
	m := parseMappingLines("# comment\na.com=机场A\n  b.com = B  \nnoequals\n=c\n")
	if len(m) != 2 {
		t.Fatalf("len = %d, want 2: %v", len(m), m)
	}
	if m["a.com"] != "机场A" || m["b.com"] != "B" {
		t.Fatalf("m = %v", m)
	}
}

func TestExtractURLs(t *testing.T) {
	got := extractURLs("看 https://a.com/sub?x=1 和 https://a.com/sub?x=1 还有 http://b.com，重复")
	want := []string{"https://a.com/sub?x=1", "http://b.com，重复"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestRegionOf(t *testing.T) {
	cases := map[string]string{
		"🇭🇰 HK IEPL 01":  "香港",
		"Japan Tokyo 03": "日本",
		"US Los Angeles": "美国",
		"新加坡 SG01":       "新加坡",
		"火星节点":           "",
	}
	for in, want := range cases {
		if got := regionOf(in); got != want {
			t.Errorf("regionOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCountRegions(t *testing.T) {
	regions := countRegions([]string{"香港 01", "HK 02", "日本 01", "Moon"})
	if len(regions) != 3 {
		t.Fatalf("regions = %v", regions)
	}
	// 香港 merged (2), 日本 (1), 其他 (1)
	byName := map[string]int{}
	for _, r := range regions {
		byName[r.k] = r.v
	}
	if byName["香港"] != 2 || byName["日本"] != 1 || byName["其他"] != 1 {
		t.Fatalf("byName = %v", byName)
	}
}

const clashYAML = `port: 7890
proxies:
  - name: "香港 IEPL 01"
    type: ss
    server: a.com
  - name: "Japan Tokyo 02"
    type: vmess
    server: b.com
  - {name: US-01, type: trojan, server: c.com}
`

func TestParseYAMLNodes(t *testing.T) {
	n := parseNodes([]byte(clashYAML))
	if n == nil {
		t.Fatal("nil")
	}
	if n.count != 3 {
		t.Fatalf("count = %d", n.count)
	}
	types := map[string]int{}
	for _, kv := range n.types {
		types[kv.k] = kv.v
	}
	if types["ss"] != 1 || types["vmess"] != 1 || types["trojan"] != 1 {
		t.Fatalf("types = %v", types)
	}
	if len(n.names) != 3 || n.names[0] != "香港 IEPL 01" {
		t.Fatalf("names = %v", n.names)
	}
	regions := map[string]int{}
	for _, kv := range n.regions {
		regions[kv.k] = kv.v
	}
	if regions["香港"] != 1 || regions["日本"] != 1 || regions["美国"] != 1 {
		t.Fatalf("regions = %v", regions)
	}
}

func TestParseBase64Nodes(t *testing.T) {
	links := []string{
		"trojan://pass@hk.example.com:443?security=tls#%E9%A6%99%E6%B8%AF01",
		"ss://YWVzLTI1Ni1nY206cGFzcw==@jp.example.com:8388#Japan-Tokyo",
		"vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"ps":"美国节点","add":"us.com","port":443,"id":"x"}`)),
		"random junk line",
		"",
	}
	body := base64.StdEncoding.EncodeToString([]byte(strings.Join(links, "\n")))
	n := parseNodes([]byte(body))
	if n.count != 3 {
		t.Fatalf("count = %d, names = %v", n.count, n.names)
	}
	want := []string{"香港01", "Japan-Tokyo", "美国节点"}
	for i, w := range want {
		if n.names[i] != w {
			t.Errorf("names[%d] = %q, want %q", i, n.names[i], w)
		}
	}
}

func TestParseBase64NodesPlain(t *testing.T) {
	// plain (non-base64) list should still scan
	body := "ss://x@y:1#Name\nignore me\n"
	n := parseNodes([]byte(body))
	if n.count != 1 || n.names[0] != "Name" {
		t.Fatalf("count=%d names=%v", n.count, n.names)
	}
}

func TestParseNodesGarbage(t *testing.T) {
	n := parseNodes([]byte("totally not a subscription"))
	if n == nil || n.count != 0 {
		t.Fatalf("n = %+v", n)
	}
}

func TestDecodeBase64Loose(t *testing.T) {
	withWS := "dHJvamFuOi8vYUBiOjEj\neA=="
	if got := decodeBase64Loose(withWS); string(got) != "trojan://a@b:1#x" {
		t.Fatalf("loose decode = %q", got)
	}
	if got := decodeBase64Loose(""); got != nil {
		t.Fatalf("empty = %q", got)
	}
}

func TestExtractNodeNameURL(t *testing.T) {
	cases := map[string]string{
		"trojan://p@hk.com:443#%E9%A6%99%E6%B8%AF": "香港",
		"ss://x@jp.com:1#Tokyo":                    "Tokyo",
		"vless://u@us.com:443?x=1":                 "us.com:443", // fallback: host after @
	}
	for in, want := range cases {
		if got := extractNodeNameURL(in); got != want {
			t.Errorf("extractNodeNameURL(%q) = %q, want %q", in, got, want)
		}
	}
	// vmess ps field
	vm := "vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"ps":"名称","add":"a","port":1}`))
	if got := extractNodeNameURL(vm); got != "名称" {
		t.Errorf("vmess ps = %q", got)
	}
	// long garbage truncated
	long := strings.Repeat("x", 100)
	if got := extractNodeNameURL(long); len([]rune(got)) != 60 {
		t.Errorf("truncation: len = %d", len([]rune(got)))
	}
}

func TestGetSubCycle(t *testing.T) {
	now := time.Unix(1_700_000_000, 0) // 2023-11-14 UTC
	// single: expiring in 20 days
	c := getSubCycle(now.Unix()+20*86400, now)
	if !c.isSingle || c.isLongTerm {
		t.Fatalf("20d: %+v", c)
	}
	// long-term: > 3 years out
	c = getSubCycle(now.Unix()+4*365*86400, now)
	if !c.isLongTerm {
		t.Fatalf("4y: %+v", c)
	}
	// monthly: expiring on day 10 in ~10 months; reset day = 10
	c = getSubCycle(time.Date(now.Year()+1, 8, 10, 0, 0, 0, 0, time.UTC).Unix(), now)
	if c.isSingle || c.isLongTerm {
		t.Fatalf("monthly: %+v", c)
	}
	if !strings.Contains(c.resetInfo, "10") {
		t.Fatalf("resetInfo = %q", c.resetInfo)
	}
	if c.daysToReset <= 0 || c.daysToReset > 31 {
		t.Fatalf("daysToReset = %d", c.daysToReset)
	}
	// no expiry
	c = getSubCycle(0, now)
	if !c.isSingle || c.resetInfo != "未知或永久" {
		t.Fatalf("zero: %+v", c)
	}
	// day-31 overflow normalization: next month has fewer days
	c = getSubCycle(time.Date(now.Year()+1, 1, 31, 0, 0, 0, 0, time.UTC).Unix(), time.Unix(time.Date(now.Year(), 2, 1, 0, 0, 0, 0, time.UTC).Unix(), 0))
	if c.isSingle || c.isLongTerm {
		t.Fatalf("overflow: %+v", c)
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[int64]string{
		0:          "0.00 B",
		512:        "512.00 B",
		1073741824: "1.00 GB",
		-5:         "0.00 B",
		1536:       "1.50 KB",
	}
	for in, want := range cases {
		if got := formatBytes(in); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatRemaining(t *testing.T) {
	sec := int64(3*86400 + 2*3600 + 5*60 + 7)
	if got := formatRemaining(zhOnly, sec); got != "03天02小时05分07秒" {
		t.Errorf("zh = %q", got)
	}
	if got := formatRemaining(enOnly, sec); got != "03d 02h 05m 07s" {
		t.Errorf("en = %q", got)
	}
	if got := formatRemaining(zhOnly, -5); got != "00天00小时00分00秒" {
		t.Errorf("negative = %q", got)
	}
}

func TestProgressBar(t *testing.T) {
	if got := progressBar(50, zhOnly); !strings.Contains(got, "🟡") || !strings.Contains(got, "50.00%") {
		t.Errorf("50%% = %q", got)
	}
	if got := progressBar(95, zhOnly); !strings.Contains(got, "🔴") {
		t.Errorf("95%% = %q", got)
	}
	if got := progressBar(10, enOnly); !strings.Contains(got, "good") {
		t.Errorf("10%% en = %q", got)
	}
	blocks := strings.Count(progressBar(50, zhOnly), "█")
	if blocks != 10 {
		t.Errorf("filled blocks = %d", blocks)
	}
}

func TestSplitLongMessage(t *testing.T) {
	// short stays whole
	if got := splitLongMessage("hello", 10); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("short = %v", got)
	}
	// splits on line boundaries
	lines := []string{}
	for i := 0; i < 30; i++ {
		lines = append(lines, strings.Repeat("l", 10))
	}
	parts := splitLongMessage(strings.Join(lines, "\n"), 100)
	if len(parts) < 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	for i, p := range parts {
		if len([]rune(p)) > 100 {
			t.Errorf("part %d len %d > 100", i, len([]rune(p)))
		}
	}
	// reassembled content preserved
	rejoined := strings.Join(parts, "\n")
	if len([]rune(rejoined)) != len([]rune(strings.Join(lines, "\n"))) {
		t.Errorf("content lost: %d vs %d", len([]rune(rejoined)), len([]rune(strings.Join(lines, "\n"))))
	}
}

func TestRenderReportDetailed(t *testing.T) {
	now := time.Unix(1_735_689_600, 0) // 2025-01-01
	old := queryTime
	queryTime = func() time.Time { return now }
	defer func() { queryTime = old }()

	r := &subResult{
		success: true, configName: "测试机场", status: "有效",
		upload: 1 << 30, download: 2 << 30, total: 100 << 30,
		expireTs: now.Add(200 * 24 * time.Hour).Unix(), startTs: now.Add(-30 * 24 * time.Hour).Unix(),
		website: websiteInfo{website: "https://panel.example.com", name: "测试机场"},
		nodes:   &nodeInfo{count: 2, types: []kv{{"ss", 2}}, regions: []kv{{"香港", 2}}, names: []string{"香港 01", "HK 02"}},
	}
	r.used = r.upload + r.download
	r.remain = r.total - r.used
	r.percent = float64(r.used) / float64(r.total) * 100

	out := renderReport(zhOnly, []string{"https://sub.example.com/x"}, []*subResult{r}, false, false)
	for _, want := range []string{"测试机场", "有效", "流量信息", "时间信息", "节点信息", "香港:2", "已用", "剩余", "周期", "历史日均", "进度", "https://panel.example.com", "每月"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "统计") {
		t.Error("single link should not have stats line")
	}

	// en labels
	outEn := renderReport(enOnly, []string{"https://sub.example.com/x"}, []*subResult{r}, false, false)
	for _, want := range []string{"Provider", "Traffic", "Nodes", "active", "Expires"} {
		if !strings.Contains(outEn, want) {
			t.Errorf("en missing %q in:\n%s", want, outEn)
		}
	}
}

func TestRenderReportBriefAndFailed(t *testing.T) {
	now := time.Unix(1_735_689_600, 0)
	old := queryTime
	queryTime = func() time.Time { return now }
	defer func() { queryTime = old }()

	ok := &subResult{success: true, configName: "A", status: "有效", total: 100, used: 40, remain: 60,
		expireTs: now.Add(10 * 24 * time.Hour).Unix()}
	bad := &subResult{url: "https://bad.example.com/x", configName: "未知", status: "失败", errKind: errUnreachable, errDetail: "503"}

	out := renderReport(zhOnly, []string{"https://a.example.com/s", "https://bad.example.com/x"}, []*subResult{ok, bad}, true, false)
	for _, want := range []string{"A", "总流量", "到期时间", "查询失败", "503"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// brief mode has no trailing stats line (matches the source's cha)
	if strings.Contains(out, "统计") {
		t.Errorf("brief mode should not have stats line:\n%s", out)
	}
	// detailed mode with multiple links does get one
	out = renderReport(zhOnly, []string{"https://a.example.com/s", "https://bad.example.com/x"}, []*subResult{ok, bad}, false, false)
	if !strings.Contains(out, "统计") {
		t.Errorf("detailed multi-link missing stats line:\n%s", out)
	}

	// no-userinfo failure renders its own label
	noInfo := &subResult{url: "https://b.com", configName: "B", status: "失败", errKind: errNoUserInfo}
	out = renderReport(zhOnly, []string{"https://b.com"}, []*subResult{noInfo}, false, false)
	if !strings.Contains(out, "无流量统计信息") {
		t.Errorf("missing label in:\n%s", out)
	}
}

func TestStatsLine(t *testing.T) {
	s := statsLine(zhOnly, 2, 1, 1, 0)
	for _, want := range []string{"✅有效:2", "⚠️耗尽:1", "❌过期:1"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %q", want, s)
		}
	}
}

func TestQuote(t *testing.T) {
	q := quote("a\nb")
	if q != "> a\n> b" {
		t.Fatalf("quote = %q", q)
	}
}

func TestNodeStatsBlock(t *testing.T) {
	n := &nodeInfo{count: 3, types: []kv{{"ss", 2}, {"trojan", 1}}, regions: []kv{{"香港", 2}, {"其他", 1}}, names: nil}
	b := nodeStatsBlock(zhOnly, n)
	for _, want := range []string{"数量: 3", "ss:2", "trojan:1", "香港:2", "主要: 香港"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q in:\n%s", want, b)
		}
	}
}
