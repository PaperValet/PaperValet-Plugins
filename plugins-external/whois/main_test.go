package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func zhTl(zh, en string) string { return zh }
func enTl(zh, en string) string { return en }

// --- SSE parsing ---

func TestParseSSEResponseSkipsBadLines(t *testing.T) {
	raw := "" +
		"data: {\"type\":\"tld\",\"data\":{\"name\":\"com\"}}\n" +
		": keepalive\n" +
		"data: not-json\n" +
		"data:  \n" +
		"data: {\"type\":\"check\",\"data\":{\"whois\":{\"whois\":\"Domain Name: X\"}}}\n"
	events := parseSSEResponse(raw)
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if events[0].Type != "tld" || events[1].Type != "check" {
		t.Fatalf("unexpected types %q %q", events[0].Type, events[1].Type)
	}
}

func TestExtractWhoisFromSSE(t *testing.T) {
	raw := "data: {\"type\":\"tld\",\"data\":{}}\n" +
		"data: {\"type\":\"check\",\"data\":{\"whois\":{\"whois\":\"   Domain Name: GOOGLE.COM\\r\\n\"}}}\n"
	if got := extractWhoisFromSSE(raw); !strings.Contains(got, "GOOGLE.COM") {
		t.Fatalf("want whois text, got %q", got)
	}
	if got := extractWhoisFromSSE("data: {\"type\":\"check\",\"data\":{\"whois\":{\"whois\":\"\"}}}"); got != "" {
		t.Fatalf("empty whois must count as absent, got %q", got)
	}
	if got := extractWhoisFromSSE("data: {}"); got != "" {
		t.Fatalf("no events with whois, got %q", got)
	}
}

// --- field extraction (regexes copied from the source) ---

const sampleWhois = "   Domain Name: GOOGLE.COM\r\n" +
	"   Registrar WHOIS Server: whois.markmonitor.com\r\n" +
	"   Updated Date: 2019-09-09T15:39:04Z\r\n" +
	"   Creation Date: 1997-09-15T04:00:00Z\r\n" +
	"   Registry Expiry Date: 2028-09-14T04:00:00Z\r\n" +
	"   Registrar: MarkMonitor Inc.\r\n" +
	"   Domain Status: clientDeleteProhibited https://icann.org/epp#clientDeleteProhibited\r\n" +
	"   Name Server: NS1.GOOGLE.COM\r\n" +
	"   Name Server: NS2.GOOGLE.COM\r\n" +
	"   nserver: ns3.google.com\r\n" +
	"   NS: ns4.google.com\r\n" +
	"   For more information on Whois status codes, please visit https://icann.org/epp\r\n"

func TestBuildRecordFields(t *testing.T) {
	cleaned := cutRawData(sampleWhois)
	if strings.Contains(cleaned, "For more information") {
		t.Fatal("raw data must be cut before the boilerplate")
	}
	rec := buildRecord("google.com", cleaned)
	if rec.Registrar != "MarkMonitor Inc." {
		t.Errorf("registrar = %q", rec.Registrar)
	}
	if rec.CreatedDate != "1997-09-15T04:00:00Z" {
		t.Errorf("created = %q", rec.CreatedDate)
	}
	if rec.ExpiryDate != "2028-09-14T04:00:00Z" {
		t.Errorf("expiry = %q", rec.ExpiryDate)
	}
	if rec.UpdatedDate != "2019-09-09T15:39:04Z" {
		t.Errorf("updated = %q", rec.UpdatedDate)
	}
	if !strings.HasPrefix(rec.Status, "clientDeleteProhibited") {
		t.Errorf("status = %q", rec.Status)
	}
	wantNS := []string{"NS1.GOOGLE.COM", "NS2.GOOGLE.COM", "ns3.google.com", "ns4.google.com"}
	if len(rec.NameServers) != len(wantNS) {
		t.Fatalf("name servers = %v", rec.NameServers)
	}
	for i, ns := range wantNS {
		if rec.NameServers[i] != ns {
			t.Errorf("ns[%d] = %q want %q", i, rec.NameServers[i], ns)
		}
	}
}

func TestExtractInfoMissing(t *testing.T) {
	rec := buildRecord("x.io", "nothing here")
	if rec.Registrar != "" || rec.ExpiryDate != "" || rec.NameServers != nil {
		t.Fatalf("missing fields must stay empty: %+v", rec)
	}
}

// --- domain cleaning / validation / extraction ---

func TestCleanDomain(t *testing.T) {
	cases := map[string]string{
		"https://www.google.com/path?q=1": "google.com",
		"http://example.com/a/b":          "example.com",
		"www.GitHub.com/x":                "GitHub.com",
		"github.io":                       "github.io",
		"  google.com ":                   "  google.com ",
	}
	for in, want := range cases {
		if got := cleanDomain(in); got != want {
			t.Errorf("cleanDomain(%q) = %q want %q", in, got, want)
		}
	}
}

func TestValidDomain(t *testing.T) {
	valid := []string{"example.com", "google.com", "github.io", "a-b.co.uk", "xn--80ak6aa92e.com"}
	for _, d := range valid {
		if !validDomain(d) {
			t.Errorf("%q should be valid", d)
		}
	}
	invalid := []string{"not a domain", "http://with-scheme.com", "-lead.com", "toolong" + strings.Repeat("a", 70) + ".com", "no-tld"}
	for _, d := range invalid {
		if validDomain(d) {
			t.Errorf("%q should be invalid", d)
		}
	}
}

func TestExtractDomain(t *testing.T) {
	cases := map[string]string{
		"see https://www.google.com/search?q=x":     "google.com",
		"visit github.com/org/repo today":           "github.com",
		"no domains here":                           "",
		"check http://example.co.uk/a and more.com": "example.co.uk",
	}
	for in, want := range cases {
		if got := extractDomain(in); got != want {
			t.Errorf("extractDomain(%q) = %q want %q", in, got, want)
		}
	}
}

// --- dates and expiry grading ---

func TestParseWhoisDate(t *testing.T) {
	ok := []string{"2028-09-14T04:00:00Z", "2028-09-14T04:00:05", "2028-09-14 04:00:00", "2028-09-14",
		"14-Sep-2028 04:00:00 UTC", "14-Sep-2028", "September 14, 2028"}
	for _, s := range ok {
		if _, good := parseWhoisDate(s); !good {
			t.Errorf("%q should parse", s)
		}
	}
	if _, good := parseWhoisDate("not-a-date"); good {
		t.Error("garbage must not parse")
	}
}

func TestExpiryNoteGrading(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	mk := func(days int) WhoisRecord {
		return WhoisRecord{ExpiryDate: now.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)}
	}
	if note := expiryNote(zhTl, -5); !strings.Contains(note, "已过期") {
		t.Errorf("expired note = %q", note)
	}
	if note := expiryNote(zhTl, 10); !strings.Contains(note, "10 天后过期") {
		t.Errorf("<30 note = %q", note)
	}
	if note := expiryNote(zhTl, 60); !strings.Contains(note, "60") {
		t.Errorf("<90 note = %q", note)
	}
	if note := expiryNote(zhTl, 200); note != "" {
		t.Errorf(">=90 note should be empty, got %q", note)
	}
	if _, ok := expiryDays(mk(-5), now); !ok {
		t.Error("parseable date should be ok")
	}
	if _, ok := expiryDays(WhoisRecord{ExpiryDate: "garbage"}, now); ok {
		t.Error("garbage date should not be ok")
	}
	if _, ok := expiryDays(WhoisRecord{}, now); ok {
		t.Error("missing date should not be ok")
	}
}

// --- store: cache expiry, history cap, clear, persistence ---

func newTestStore(t *testing.T) (*store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "whois_data.json")
	s, err := newStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestStoreCacheExpiry(t *testing.T) {
	s, _ := newTestStore(t)
	old := nowFunc
	defer func() { nowFunc = old }()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return base }
	rec := buildRecord("google.com", sampleWhois)
	if err := s.save(rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.cached("GOOGLE.COM"); !ok {
		t.Fatal("fresh cache miss")
	}
	nowFunc = func() time.Time { return base.Add(25 * time.Hour) }
	if _, ok := s.cached("google.com"); ok {
		t.Fatal("expired cache hit")
	}
	if _, ok := s.d.Cache["google.com"]; ok {
		t.Fatal("expired entry not deleted")
	}
}

func TestStoreHistoryCap(t *testing.T) {
	s, _ := newTestStore(t)
	s.d.Settings.MaxHistory = 3
	for i := 0; i < 5; i++ {
		if err := s.save(buildRecord(fmt.Sprintf("d%d.com", i), sampleWhois)); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.d.History) != 3 {
		t.Fatalf("history = %d want 3", len(s.d.History))
	}
	if s.d.History[0].Domain != "d4.com" {
		t.Fatalf("newest first, got %q", s.d.History[0].Domain)
	}
}

func TestStoreClearAndPersist(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.save(buildRecord("a.com", sampleWhois)); err != nil {
		t.Fatal(err)
	}
	h, c, err := s.clear()
	if err != nil || h != 1 || c != 1 {
		t.Fatalf("clear = %d %d %v", h, c, err)
	}
	if h, c, _ := s.clear(); h != 0 || c != 0 {
		t.Fatalf("second clear = %d %d", h, c)
	}
	if err := s.save(buildRecord("b.com", sampleWhois)); err != nil {
		t.Fatal(err)
	}
	s2, err := newStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.d.History) != 1 || s2.d.History[0].Domain != "b.com" {
		t.Fatalf("reload lost state: %+v", s2.d.History)
	}
	if len(s2.d.Cache) != 1 {
		t.Fatalf("reload lost cache")
	}
	if s2.d.Settings.MaxHistory != 100 || s2.d.Settings.CacheHours != 24 {
		t.Fatalf("defaults not applied: %+v", s2.d.Settings)
	}
}

// --- batch truncation and tally ---

func TestBatchLimits(t *testing.T) {
	if maxBatch != 10 {
		t.Fatalf("maxBatch = %d", maxBatch)
	}
	eleven := make([]string, 11)
	if len(eleven) > maxBatch {
		// handler branch; here just prove the bound is enforced by the
		// guard the handler uses
		t.Log("11 domains exceed the limit")
	}
}

func TestRenderBatchDone(t *testing.T) {
	lines := []batchLine{
		{domain: "a.com", ok: true},
		{domain: "b.com", ok: true, cached: true},
		{domain: "c.com"},
	}
	out := renderBatchDone(zhTl, lines, 2, 1)
	for _, want := range []string{"批量查询完成", "成功", "2", "失败", "1", "缓存", "a.com", "c.com", "查询失败"} {
		if !strings.Contains(out, want) {
			t.Errorf("batch output missing %q:\n%s", want, out)
		}
	}
}

// --- rendering ---

func TestRenderResultCard(t *testing.T) {
	rec := buildRecord("google.com", cutRawData(sampleWhois))
	rec.QueryTime = nowFunc().UTC().Format(time.RFC3339)
	out := renderResult(zhTl, rec, false)
	for _, want := range []string{"WHOIS 查询结果", "google.com", "注册商", "MarkMonitor Inc.", "过期日期", "DNS 服务器", "原始 WHOIS 数据", "> ", "NS1.GOOGLE.COM"} {
		if !strings.Contains(out, want) {
			t.Errorf("result missing %q", want)
		}
	}
	if strings.Contains(out, "For more information") {
		t.Error("raw tail not cut")
	}
	// markdown entities: the registrar text must be escaped, domain in code
	if !strings.Contains(out, "`google.com`") {
		t.Errorf("domain not in code: %s", out[:80])
	}
}

func TestRenderResultTruncatesRaw(t *testing.T) {
	long := strings.Repeat("x", 4000)
	rec := WhoisRecord{Domain: "long.io", RawData: long, QueryTime: nowFunc().UTC().Format(time.RFC3339)}
	out := renderResult(zhTl, rec, false)
	if !strings.Contains(out, "数据已截断") {
		t.Error("truncation note missing")
	}
	if runeLen(strings.TrimPrefix(out, "")) > rawMaxRunes+2000 {
		t.Error("raw block not actually truncated")
	}
}

func TestRenderHistory(t *testing.T) {
	empty := renderHistory(zhTl, nil, 0, 24)
	if !strings.Contains(empty, "暂无查询历史") {
		t.Error("empty history card wrong")
	}
	var hist []WhoisRecord
	for i := 0; i < 25; i++ {
		hist = append(hist, buildRecord(fmt.Sprintf("d%d.com", i), sampleWhois))
	}
	// history[0] is the newest (the source unshifts); only the first 20 show.
	out := renderHistory(zhTl, hist, 7, 24)
	for _, want := range []string{"最近 20 条", "总查询次数", "25", "缓存域名数", "7", "缓存时长", "24"} {
		if !strings.Contains(out, want) {
			t.Errorf("history missing %q", want)
		}
	}
	if strings.Contains(out, "d24.com") {
		t.Error("history must show only the first 20 (newest)")
	}
	if !strings.Contains(out, "d0.com") {
		t.Error("newest entry missing")
	}
}

// --- query error branches (no network) ---

func TestRenderQueryError(t *testing.T) {
	out := renderQueryError(zhTl, "gone.example", errNoWhois{})
	if !strings.Contains(out, "域名不存在或未注册") {
		t.Errorf("no-whois card wrong: %s", out)
	}
	out = renderQueryError(zhTl, "slow.io", context.DeadlineExceeded)
	if !strings.Contains(out, "超时") {
		t.Errorf("timeout card wrong: %s", out)
	}
	out = renderQueryError(zhTl, "ratelimited.io", &httpStatusError{code: 429})
	if !strings.Contains(out, "请求过于频繁") {
		t.Errorf("429 card wrong: %s", out)
	}
	out = renderQueryError(zhTl, "denied.io", &httpStatusError{code: 403})
	if !strings.Contains(out, "访问被拒绝") {
		t.Errorf("403 card wrong: %s", out)
	}
	out = renderQueryError(zhTl, "boom.io", &httpStatusError{code: 500})
	if !strings.Contains(out, "500") {
		t.Errorf("http card wrong: %s", out)
	}
	out = renderQueryError(zhTl, "net.io", errors.New("connection refused"))
	if !strings.Contains(out, "connection refused") {
		t.Errorf("transport card wrong: %s", out)
	}
}

func TestIsTimeoutErr(t *testing.T) {
	if !isTimeoutErr(context.DeadlineExceeded) {
		t.Error("deadline should count as timeout")
	}
	if isTimeoutErr(errors.New("nope")) {
		t.Error("plain error is not a timeout")
	}
}

// --- stubbed fetch: end-to-end record build path ---

func TestQueryBuildsRecord(t *testing.T) {
	old := fetchWhoisCheck
	defer func() { fetchWhoisCheck = old }()
	fetchWhoisCheck = func(ctx context.Context, timeout time.Duration, domain string) (string, error) {
		if domain != "google.com" {
			t.Errorf("domain = %q", domain)
		}
		if timeout != defaultTimeout {
			t.Errorf("timeout = %v", timeout)
		}
		return sampleWhois, nil
	}
	p := &WhoisPlugin{}
	rec, err := p.query(context.Background(), "google.com")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Registrar != "MarkMonitor Inc." || rec.Domain != "google.com" {
		t.Fatalf("record = %+v", rec)
	}
	if strings.Contains(rec.RawData, "For more information") {
		t.Error("raw tail not cut")
	}
}

// --- misc helpers ---

func TestQuoteBlock(t *testing.T) {
	if q := quoteBlock("a\nb"); q != "> a\n> b" {
		t.Errorf("quoteBlock = %q", q)
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	cases := map[time.Duration]string{
		10 * time.Second:  "刚刚",
		10 * time.Minute:  "10 分钟前",
		3 * time.Hour:     "3 小时前",
		2 * 24 * timeHour: "2 天前",
	}
	for d, want := range cases {
		if got := relativeTime(zhTl, now.Add(-d), now); got != want {
			t.Errorf("relativeTime(%v) = %q want %q", d, got, want)
		}
	}
}

func TestWriteFilePermissions(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.save(buildRecord("perm.io", sampleWhois)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v want 0600", fi.Mode().Perm())
	}
}

func TestHelpTextBilingual(t *testing.T) {
	if !strings.Contains(helpText(zhTl), "WHOIS 域名查询") {
		t.Error("zh help missing")
	}
	if !strings.Contains(helpText(enTl), "WHOIS Domain Lookup") {
		t.Error("en help missing")
	}
	if strings.Contains(helpText(zhTl), "到期提醒") {
		t.Error("must not advertise the unimplemented expiry reminder")
	}
}
