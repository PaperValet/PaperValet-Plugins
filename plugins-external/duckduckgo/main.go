package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	defaultLimit  = 8
	maxLimit      = 15
	maxQueryRunes = 200

	ddgHTML   = "https://html.duckduckgo.com/html/"
	ddgLite   = "https://lite.duckduckgo.com/lite/"
	firecrawl = "https://api.firecrawl.dev/v2/search"

	// Telegram caps a message at 4096 UTF-16 units; leave headroom.
	pageSoftLimit = 3500
	pageHardLimit = 4000
)

var Metadata = &plugin.PluginMetadata{
	Name:        "duckduckgo",
	Description: "DuckDuckGo 网页搜索",
	DescEN:      "DuckDuckGo web search",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type DuckDuckGoPlugin struct {
	http *http.Client
}

func New() *DuckDuckGoPlugin {
	return &DuckDuckGoPlugin{http: &http.Client{
		Timeout: 25 * time.Second,
		// Never follow redirects to non-DDG hosts silently; DDG itself
		// answers directly.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}}
}

func (p *DuckDuckGoPlugin) Name() string        { return "duckduckgo" }
func (p *DuckDuckGoPlugin) Description() string { return Metadata.Description }
func (p *DuckDuckGoPlugin) DescEN() string      { return Metadata.DescEN }

func (p *DuckDuckGoPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "duckduckgo",
		Aliases:     []string{"ddg"},
		Description: "DuckDuckGo 网页搜索（HTML 版 + Lite 版，Firecrawl 免 Key 回退）",
		DescEN:      "DuckDuckGo web search (HTML + Lite endpoints, keyless Firecrawl fallback)",
		Usage:       "duckduckgo <关键词> [-n 条数 1-15] | duckduckgo help",
		UsageEN:     "duckduckgo <query> [-n count 1-15] | duckduckgo help",
		Plugin:      p.Name(),
		Category:    "tools",
		Handler:     p.handleSearch,
	})
}

func (p *DuckDuckGoPlugin) Start(ctx context.Context) error { return nil }
func (p *DuckDuckGoPlugin) Stop(ctx context.Context) error  { return nil }

// ---------------------------------------------------------------- types

type result struct {
	Title, URL, Snippet, Display, Source string
}

type bundle struct {
	results []result
	sources []string
	notes   []string
}

// ---------------------------------------------------------------- args

func parseLimitAndQuery(args []string) (string, int) {
	limit := defaultLimit
	var parts []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-n" || a == "--num" || a == "-l" || a == "--limit") && i+1 < len(args):
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				limit = clamp(n)
				i++
				continue
			}
		case strings.HasPrefix(a, "-n") && len(a) > 2:
			if n, err := strconv.Atoi(a[2:]); err == nil {
				limit = clamp(n)
				continue
			}
		}
		parts = append(parts, a)
	}
	return strings.TrimSpace(strings.Join(parts, " ")), limit
}

func clamp(n int) int {
	return min(maxLimit, max(1, n))
}

// ---------------------------------------------------------------- handler

func (p *DuckDuckGoPlugin) help(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🔍 **DuckDuckGo 搜索**\n\n"+
			"**用法**\n"+
			plugin.Code("duckduckgo <关键词>")+"\n"+
			plugin.Code("ddg <关键词>")+" 简写\n"+
			plugin.Code("duckduckgo <关键词> -n 5")+" 条数 1–15（默认 8）\n\n"+
			"**数据源（自动）**\n"+
			"1\\. DuckDuckGo HTML 版（浏览器请求头）\n"+
			"2\\. DuckDuckGo Lite 版（HTML 版被拦截时）\n"+
			"3\\. Firecrawl 免 Key 搜索（结果不足时补齐）\n\n"+
			"💡 结果较多时自动分多条消息发送",
		"🔍 **DuckDuckGo search**\n\n"+
			"**Usage**\n"+
			plugin.Code("duckduckgo <query>")+"\n"+
			plugin.Code("ddg <query>")+" short form\n"+
			plugin.Code("duckduckgo <query> -n 5")+" result count 1–15 (default 8)\n\n"+
			"**Sources (automatic)**\n"+
			"1\\. DuckDuckGo HTML (browser-like headers)\n"+
			"2\\. DuckDuckGo Lite (when HTML is blocked)\n"+
			"3\\. Firecrawl keyless search (fills up missing results)\n\n"+
			"💡 Long result lists are split across several messages")
}

func (p *DuckDuckGoPlugin) handleSearch(ctx *plugin.CommandContext) error {
	if len(ctx.Args) == 0 {
		return ctx.Edit(p.help(ctx))
	}
	if a := strings.ToLower(ctx.Args[0]); len(ctx.Args) == 1 && (a == "help" || a == "h") {
		return ctx.Edit(p.help(ctx))
	}
	query, limit := parseLimitAndQuery(ctx.Args)
	if query == "" {
		return ctx.Edit(p.help(ctx))
	}
	if len([]rune(query)) > maxQueryRunes {
		return ctx.Edit("❌ " + ctx.Tlocal("关键词过长（最多 200 字符）", "Query too long (max 200 characters)"))
	}

	_ = ctx.Edit("⏳ " + ctx.Tlocal("正在搜索 ", "Searching ") + plugin.Code(query) + "…")
	started := time.Now()
	b := p.searchAll(ctx.Context(), query, limit)
	if ctx.Context().Err() != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("搜索已取消或超时", "Search cancelled or timed out"))
	}
	pages := packPages(ctx, query, b, time.Since(started))
	return p.deliver(ctx, pages)
}

// ---------------------------------------------------------------- search

func (p *DuckDuckGoPlugin) searchAll(ctx context.Context, query string, limit int) bundle {
	var b bundle
	var collected []result

	res, err := p.fetchDDG(ctx, query, limit)
	if len(res) > 0 {
		b.sources = append(b.sources, "DuckDuckGo")
		collected = append(collected, res...)
	} else if err != nil {
		b.notes = append(b.notes, err.Error())
	}

	if len(collected) < limit && ctx.Err() == nil {
		fc, err := p.fetchFirecrawl(ctx, query, limit)
		if len(fc) > 0 {
			b.sources = append(b.sources, "Firecrawl")
			collected = append(collected, fc...)
		} else if len(collected) == 0 {
			if err != nil {
				b.notes = append(b.notes, "Firecrawl: "+err.Error())
			} else {
				b.notes = append(b.notes, "Firecrawl: no results")
			}
		}
	}
	b.results = dedupe(collected, limit)
	return b
}

var browserUAs = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:125.0) Gecko/20100101 Firefox/125.0",
}

// browserHeaders imitates a Chrome form submission. Go's TLS fingerprint
// cannot be disguised with the stdlib, so instead of curl_cffi's
// impersonation we rely on the form-POST flow that DDG serves to browsers
// (GET is far more likely to hit the 202 anomaly page).
func browserHeaders(req *http.Request, origin string) {
	ua := browserUAs[rand.IntN(len(browserUAs))]
	h := req.Header
	h.Set("User-Agent", ua)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Origin", origin)
	h.Set("Referer", origin+"/")
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("Upgrade-Insecure-Requests", "1")
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("Sec-Fetch-User", "?1")
	h.Set("Cache-Control", "max-age=0")
	if strings.Contains(ua, "Chrome/") {
		v := ua[strings.Index(ua, "Chrome/")+7:]
		v = v[:strings.Index(v, ".")]
		h.Set("Sec-Ch-Ua", `"Chromium";v="`+v+`", "Google Chrome";v="`+v+`", "Not-A.Brand";v="99"`)
		h.Set("Sec-Ch-Ua-Mobile", "?0")
		platform := `"Windows"`
		switch {
		case strings.Contains(ua, "Macintosh"):
			platform = `"macOS"`
		case strings.Contains(ua, "Linux"):
			platform = `"Linux"`
		}
		h.Set("Sec-Ch-Ua-Platform", platform)
	}
}

var errBlocked = errors.New("DDG anti-bot page (202)")

func (p *DuckDuckGoPlugin) postForm(ctx context.Context, endpoint, origin string, form url.Values) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	browserHeaders(req, origin)
	resp, err := p.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(body), resp.StatusCode, nil
}

var anomalyRe = regexp.MustCompile(`(?i)anomaly-modal|Unfortunately, bots use DuckDuckGo`)

func isBlocked(status int, body string) bool {
	return status == http.StatusAccepted || anomalyRe.MatchString(body)
}

// fetchDDG queries the HTML endpoint, then the Lite endpoint if blocked.
func (p *DuckDuckGoPlugin) fetchDDG(ctx context.Context, query string, limit int) ([]result, error) {
	var errs []string
	body, status, err := p.postForm(ctx, ddgHTML, "https://html.duckduckgo.com", url.Values{"q": {query}, "b": {""}, "kl": {""}})
	switch {
	case err != nil:
		errs = append(errs, "html: "+err.Error())
	case isBlocked(status, body):
		errs = append(errs, "html: "+errBlocked.Error())
	case status != http.StatusOK:
		errs = append(errs, fmt.Sprintf("html: HTTP %d", status))
	default:
		if r := parseHTMLResults(body, limit); len(r) > 0 {
			return r, nil
		}
		errs = append(errs, "html: no results")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	body, status, err = p.postForm(ctx, ddgLite, "https://lite.duckduckgo.com", url.Values{"q": {query}, "kl": {""}})
	switch {
	case err != nil:
		errs = append(errs, "lite: "+err.Error())
	case isBlocked(status, body):
		errs = append(errs, "lite: "+errBlocked.Error())
	case status != http.StatusOK:
		errs = append(errs, fmt.Sprintf("lite: HTTP %d", status))
	default:
		if r := parseLiteResults(body, limit); len(r) > 0 {
			return r, nil
		}
		errs = append(errs, "lite: no results")
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

// ---------------------------------------------------------------- parsing

var (
	tagRe        = regexp.MustCompile(`<[^>]+>`)
	spaceRe      = regexp.MustCompile(`\s+`)
	blockSplitRe = regexp.MustCompile(`class="result results_links`)
	titleRe1     = regexp.MustCompile(`(?i)class="result__a"\s+href="([^"]+)"[^>]*>([\s\S]*?)</a>`)
	titleRe2     = regexp.MustCompile(`(?i)href="([^"]+)"[^>]*class="result__a"[^>]*>([\s\S]*?)</a>`)
	snippetRe    = regexp.MustCompile(`(?i)class="result__snippet"[^>]*>([\s\S]*?)</(?:a|td|div)`)
	displayRe    = regexp.MustCompile(`(?i)class="result__url"[^>]*>([\s\S]*?)</`)
	ddgAdURLRe   = regexp.MustCompile(`(?i)^https?://duckduckgo\.com/(c/|y\.js)`)

	liteLinkRe    = regexp.MustCompile(`(?i)<a[^>]+href=["']([^"']+)["'][^>]*class=["']result-link["'][^>]*>([\s\S]*?)</a>|<a[^>]+class=["']result-link["'][^>]*href=["']([^"']+)["'][^>]*>([\s\S]*?)</a>`)
	liteSnippetRe = regexp.MustCompile(`(?i)class=["']result-snippet["'][^>]*>([\s\S]*?)</td>`)
	liteDisplayRe = regexp.MustCompile(`(?i)class=["']link-text["'][^>]*>([\s\S]*?)</span>`)
)

func stripHTML(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// decodeUDDG unwraps DDG redirect links (//duckduckgo.com/l/?uddg=…).
func decodeUDDG(href string) string {
	href = html.UnescapeString(href)
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if v := u.Query().Get("uddg"); v != "" {
		return v
	}
	return href
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

func validResultURL(u string) bool {
	return (strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) && !ddgAdURLRe.MatchString(u)
}

func parseHTMLResults(body string, limit int) []result {
	if anomalyRe.MatchString(body) {
		return nil
	}
	blocks := blockSplitRe.Split(body, -1)
	var out []result
	for _, block := range blocks[min(1, len(blocks)):] {
		if len(out) >= limit {
			break
		}
		if strings.Contains(block, "result--ad") || strings.Contains(block, "y.js?ad_") {
			continue
		}
		m := titleRe1.FindStringSubmatch(block)
		if m == nil {
			m = titleRe2.FindStringSubmatch(block)
		}
		if m == nil {
			continue
		}
		title, link := stripHTML(m[2]), decodeUDDG(m[1])
		if title == "" || !validResultURL(link) {
			continue
		}
		r := result{Title: title, URL: link, Source: "ddg-html"}
		if sn := snippetRe.FindStringSubmatch(block); sn != nil {
			r.Snippet = stripHTML(sn[1])
		}
		if d := displayRe.FindStringSubmatch(block); d != nil {
			r.Display = stripHTML(d[1])
		}
		if r.Display == "" {
			r.Display = hostOf(link)
		}
		out = append(out, r)
	}
	return out
}

func parseLiteResults(body string, limit int) []result {
	if anomalyRe.MatchString(body) {
		return nil
	}
	links := liteLinkRe.FindAllStringSubmatchIndex(body, -1)
	var out []result
	for i, loc := range links {
		if len(out) >= limit {
			break
		}
		href, title := "", ""
		if loc[2] >= 0 {
			href, title = body[loc[2]:loc[3]], body[loc[4]:loc[5]]
		} else {
			href, title = body[loc[6]:loc[7]], body[loc[8]:loc[9]]
		}
		end := len(body)
		if i+1 < len(links) {
			end = links[i+1][0]
		}
		seg := body[loc[1]:end]
		// Sponsored rows in Lite carry the result-sponsored class.
		rowStart := strings.LastIndex(body[:loc[0]], "<tr")
		if rowStart >= 0 && strings.Contains(body[rowStart:loc[0]], "result-sponsored") {
			continue
		}
		link := decodeUDDG(href)
		t := stripHTML(title)
		if t == "" || !validResultURL(link) {
			continue
		}
		r := result{Title: t, URL: link, Source: "ddg-lite"}
		if sn := liteSnippetRe.FindStringSubmatch(seg); sn != nil {
			r.Snippet = stripHTML(sn[1])
		}
		if d := liteDisplayRe.FindStringSubmatch(seg); d != nil {
			r.Display = stripHTML(d[1])
		}
		if r.Display == "" {
			r.Display = hostOf(link)
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------- firecrawl

type fcItem struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

func (p *DuckDuckGoPlugin) fetchFirecrawl(ctx context.Context, query string, limit int) ([]result, error) {
	payload, _ := json.Marshal(map[string]any{"query": query, "limit": min(limit, maxLimit)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, firecrawl, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "PaperValet-Search/1.1")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	items, err := parseFirecrawl(body)
	if err != nil {
		return nil, err
	}
	var out []result
	for _, it := range items {
		if len(out) >= limit {
			break
		}
		if !validResultURL(it.URL) {
			continue
		}
		t := strings.TrimSpace(it.Title)
		if t == "" {
			t = hostOf(it.URL)
		}
		out = append(out, result{
			Title: t, URL: it.URL, Snippet: cleanMarkdown(it.Description),
			Display: hostOf(it.URL), Source: "firecrawl",
		})
	}
	return out, nil
}

// parseFirecrawl accepts v2 {data:{web:[…]}}, v1 {data:[…]} and {web:[…]}.
func parseFirecrawl(body []byte) ([]fcItem, error) {
	var root struct {
		Data json.RawMessage `json:"data"`
		Web  []fcItem        `json:"web"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	if len(root.Web) > 0 {
		return root.Web, nil
	}
	var v2 struct {
		Web []fcItem `json:"web"`
	}
	if json.Unmarshal(root.Data, &v2) == nil && len(v2.Web) > 0 {
		return v2.Web, nil
	}
	var v1 []fcItem
	if json.Unmarshal(root.Data, &v1) == nil {
		return v1, nil
	}
	return nil, nil
}

var (
	mdImageRe = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	mdLinkRe  = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	mdNoiseRe = regexp.MustCompile("(?m)^\\s*(#+|[-*>|]+|```.*)\\s*")
)

// cleanMarkdown flattens Firecrawl's markdown-ish descriptions into prose.
func cleanMarkdown(s string) string {
	s = mdImageRe.ReplaceAllString(s, "")
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = mdNoiseRe.ReplaceAllString(s, "")
	s = strings.NewReplacer("|", " ", "**", "", "__", "", "`", "", "¶", "").Replace(s)
	s = html.UnescapeString(tagRe.ReplaceAllString(s, " "))
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

func dedupe(items []result, limit int) []result {
	seen := map[string]bool{}
	var out []result
	for _, it := range items {
		if it.Title == "" || it.URL == "" {
			continue
		}
		key := strings.ToLower(strings.TrimRight(it.URL, "/"))
		key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
		key = strings.TrimPrefix(key, "www.")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------- output

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func header(ctx *plugin.CommandContext, query string, total int, sources []string, elapsed time.Duration) string {
	s := "🔍 **DuckDuckGo**\n\n" +
		ctx.Tlocal("关键词", "Query") + "  " + plugin.Code(query) + "\n" +
		ctx.Tlocal("结果", "Results") + "  " + plugin.Code(fmt.Sprintf("%d · %dms", total, elapsed.Milliseconds()))
	if len(sources) > 0 {
		s += "\n" + ctx.Tlocal("来源", "Sources") + "  " + plugin.Code(strings.Join(sources, " + "))
	}
	return s
}

// resultBlock renders one result. Lines never start with list/heading
// markers so the Markdown parser keeps them as plain paragraph lines.
func resultBlock(r result, idx int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("**%d.** ", idx) + plugin.Link(truncRunes(r.Title, 120), r.URL) + "\n")
	meta := r.Display
	if r.Source == "firecrawl" {
		meta = strings.TrimSpace(meta + " · firecrawl")
	}
	if meta != "" {
		b.WriteString("🔗 " + plugin.Code(truncRunes(meta, 80)))
	}
	if r.Snippet != "" {
		b.WriteString("\n› " + plugin.Escape(truncRunes(r.Snippet, 280)))
	}
	return b.String()
}

func footer(ctx *plugin.CommandContext, query string) string {
	return "🌐 " + plugin.Link(ctx.Tlocal("在 DuckDuckGo 打开", "Open in DuckDuckGo"), "https://duckduckgo.com/?q="+url.QueryEscape(query))
}

// plainLen measures what Telegram will count: UTF-16 units of the rendered text.
func plainLen(md string) int {
	plain, _ := plugin.ParseMarkdown(md, nil)
	return len(utf16.Encode([]rune(plain)))
}

type page struct {
	text     string
	from, to int
}

// packPages packs whole result blocks into as many messages as needed,
// never splitting a block, then appends page markers.
func packPages(ctx *plugin.CommandContext, query string, b bundle, elapsed time.Duration) []string {
	total := len(b.results)
	if total == 0 {
		s := header(ctx, query, 0, b.sources, elapsed) + "\n\n❌ " +
			ctx.Tlocal("没有找到结果，可换关键词重试", "No results, try different keywords")
		if len(b.notes) > 0 {
			s += "\n\n**" + ctx.Tlocal("诊断", "Diagnostics") + "**\n" + plugin.Escape(truncRunes(strings.Join(b.notes, "\n"), 600))
		}
		return []string{s + "\n\n" + footer(ctx, query)}
	}

	var pages []page
	cur := page{text: header(ctx, query, total, b.sources, elapsed)}
	join := func(base, block string) string {
		if base == "" {
			return block
		}
		return base + "\n\n" + block
	}
	flush := func() {
		if cur.text != "" {
			pages = append(pages, cur)
		}
		cur = page{}
	}
	for i, r := range b.results {
		idx := i + 1
		block := resultBlock(r, idx)
		limit := pageSoftLimit
		if cur.from == 0 && len(pages) == 0 {
			limit = pageHardLimit
		}
		if next := join(cur.text, block); plainLen(next) <= limit {
			cur.text = next
		} else {
			flush()
			cur.text = "🔍 " + plugin.Code(query) + " · " + ctx.Tlocal(
				fmt.Sprintf("续 · 第 %d 条起 / 共 %d 条", idx, total),
				fmt.Sprintf("continued · from #%d of %d", idx, total)) + "\n\n" + block
		}
		if cur.from == 0 {
			cur.from = idx
		}
		cur.to = idx
	}
	if next := join(cur.text, footer(ctx, query)); plainLen(next) <= pageHardLimit {
		cur.text = next
		flush()
	} else {
		flush()
		pages = append(pages, page{text: footer(ctx, query)})
	}

	out := make([]string, len(pages))
	n := len(pages)
	for i, pg := range pages {
		out[i] = pg.text
		if n == 1 {
			continue
		}
		hint := fmt.Sprintf("📄 (%d/%d)", i+1, n)
		if pg.from > 0 {
			if pg.from == pg.to {
				hint += ctx.Tlocal(fmt.Sprintf(" · 第 %d 条", pg.from), fmt.Sprintf(" · #%d", pg.from))
			} else {
				hint += ctx.Tlocal(fmt.Sprintf(" · 第 %d–%d 条", pg.from, pg.to), fmt.Sprintf(" · #%d–%d", pg.from, pg.to))
			}
		}
		if i < n-1 {
			hint += ctx.Tlocal(" · 续见下一条", " · continued below")
		}
		if plainLen(out[i]+"\n\n"+hint) <= pageHardLimit {
			out[i] += "\n\n" + hint
		}
	}
	return out
}

// deliver edits the command message with the first page and sends the rest
// as follow-ups, all without link previews.
func (p *DuckDuckGoPlugin) deliver(ctx *plugin.CommandContext, pages []string) error {
	if ctx.API == nil || ctx.Message == nil || ctx.Message.Message == nil {
		return ctx.Edit(pages[0])
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit(pages[0])
	}
	plain, ents := plugin.ParseMarkdown(pages[0], nil)
	edit := &tg.MessagesEditMessageRequest{Peer: peer, ID: ctx.Message.Message.ID, Message: plain, NoWebpage: true}
	if len(ents) > 0 {
		edit.SetEntities(ents)
	}
	if _, err := ctx.API.MessagesEditMessage(ctx.Context(), edit); err != nil {
		if err := ctx.Edit(pages[0]); err != nil {
			return err
		}
	}
	for _, pg := range pages[1:] {
		plain, ents := plugin.ParseMarkdown(pg, nil)
		req := &tg.MessagesSendMessageRequest{
			Peer: peer, Message: plain, NoWebpage: true, RandomID: rand.Int64(),
		}
		if len(ents) > 0 {
			req.SetEntities(ents)
		}
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: ctx.Message.Message.ID})
		if _, err := ctx.API.MessagesSendMessage(ctx.Context(), req); err != nil {
			if err := ctx.Reply(pg); err != nil {
				return err
			}
		}
	}
	return nil
}
