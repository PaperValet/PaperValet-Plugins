package main

import (
	"context"
	"fmt"
	"html"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// httpClient wraps a shared http.Client with the timeouts the source used.
type httpClient struct {
	client *http.Client
}

func newHTTPClient() *httpClient {
	return &httpClient{client: &http.Client{Timeout: 20 * time.Second}}
}

// errKind values for subResult (localized at render time).
const (
	errUnreachable = "unreachable" // non-200 HTTP status
	errNoUserInfo  = "no-userinfo" // 200 but no subscription-userinfo header
	errOther       = "other"       // transport error, details in errDetail
)

var baseURLRe = regexp.MustCompile(`https?://[^/]+`)
var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// isPrivateHost reports whether the URL points at a private or loopback
// address. Reply-mode candidates come from arbitrary users' messages, so the
// plugin must not be tricked into probing internal networks (weak SSRF).
func isPrivateHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	// A hostname resolving into private space is harder to check without a
	// lookup; block the obvious local names and numeric-looking hosts.
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return true
	}
	return net.ParseIP(host) != nil
}

// mappingCache caches the remote key=name provider mapping for 10 minutes.
// The fetch runs outside the lock (singleflight): queryAll's parallel
// goroutines would otherwise all block on one slow remote fetch.
type mappingCache struct {
	mu       sync.Mutex
	mappings map[string]string
	fetched  time.Time
	fetching chan struct{} // closed when the in-flight fetch finished
}

const mappingTTL = 10 * time.Minute

func (c *mappingCache) get(ctx context.Context, h *httpClient) map[string]string {
	c.mu.Lock()
	if len(c.mappings) > 0 && time.Since(c.fetched) < mappingTTL {
		c.mu.Unlock()
		return c.mappings
	}
	// Join the in-flight fetch instead of starting another.
	if c.fetching != nil {
		done := c.fetching
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		c.mu.Lock()
		m := c.mappings
		c.mu.Unlock()
		return m
	}
	done := make(chan struct{})
	c.fetching = done
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.fetching == done {
			c.fetching = nil
		}
		c.mu.Unlock()
		close(done)
	}()
	return c.fetchAndStore(ctx, h)
}

// fetchAndStore performs the remote fetch (caller holds the fetch slot) and
// updates the cache, keeping stale data on failure.
func (c *mappingCache) fetchAndStore(ctx context.Context, h *httpClient) map[string]string {
	m, err := fetchRemoteMappings(ctx, h.client)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && len(m) > 0 {
		c.mappings = m
		c.fetched = time.Now()
	}
	if len(c.mappings) > 0 {
		return c.mappings // keep stale rather than nothing
	}
	return nil
}

// fetchRemoteMappings downloads the community provider-name mapping file.
func fetchRemoteMappings(ctx context.Context, client *http.Client) (map[string]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, remoteMappingsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseMappingLines(string(body)), nil
}

// parseMappingLines turns "key=name" lines (comments with #) into a map.
func parseMappingLines(content string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, "=")
		if i <= 0 {
			continue
		}
		m[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
	}
	return m
}

// mappingName looks up the provider name whose key appears in the URL.
// Keys are walked in sorted order so the result is deterministic when
// several keys match the same URL.
func mappingName(mappings map[string]string, subURL string) string {
	name := ""
	for _, key := range sortedKeys(mappings) {
		if strings.Contains(subURL, key) {
			return mappings[key]
		}
	}
	return name
}

// sortedKeys lists a map's keys in sorted order (deterministic iteration).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// get fetches a URL with the given UA and returns status, headers and body.
func (h *httpClient) get(ctx context.Context, url, ua string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp, nil, err
	}
	return resp, body, nil
}

// websiteInfo describes the provider's main site.
type websiteInfo struct {
	website string // scheme://host, empty when unknown
	name    string // page title, or a failure description
}

// getWebsiteInfo resolves the provider's site and title from a subscription
// URL: it tries /auth/login first, then the bare host.
func (p *SubinfoPlugin) getWebsiteInfo(ctx context.Context, subURL string) websiteInfo {
	base := baseURLRe.FindString(subURL)
	if base == "" {
		return websiteInfo{}
	}
	if isPrivateHost(base) {
		return websiteInfo{}
	}
	for _, path := range []string{base + "/auth/login", base} {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, body, err := p.http.get(cctx, path, browserUserAgent)
		cancel()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		title := ""
		if m := titleRe.FindStringSubmatch(string(body)); m != nil {
			title = strings.TrimSpace(html.UnescapeString(m[1]))
		}
		title = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(title, "登录 — ", ""), " | 登录", ""))
		switch {
		case strings.Contains(title, "Cloudflare") || strings.Contains(title, "Just a moment"):
			return websiteInfo{base, "Cloudflare 防御"}
		case strings.Contains(title, "Access denied") || strings.Contains(title, "404 Not Found"):
			return websiteInfo{base, "非机场面板域名"}
		}
		return websiteInfo{base, title}
	}
	return websiteInfo{base, "连接失败"}
}

// subResult carries everything the renderer needs for one link.
type subResult struct {
	url        string
	success    bool
	configName string
	status     string // 有效/耗尽/过期 (失败 implies !success)
	errKind    string
	errDetail  string
	profileURL string
	upload     int64
	download   int64
	total      int64
	used       int64
	remain     int64
	percent    float64
	expireTs   int64
	startTs    int64
	website    websiteInfo
	nodes      *nodeInfo
}

// parseUserinfo parses a subscription-userinfo header value into its
// upload/download/total/expire/starttime fields (missing keys stay 0).
func parseUserinfo(header string) (up, down, total, expire, start int64) {
	for _, part := range strings.Split(header, ";") {
		i := strings.Index(part, "=")
		if i <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(part[:i]))
		val := strings.TrimSpace(part[i+1:])
		var dst *int64
		switch key {
		case "upload":
			dst = &up
		case "download":
			dst = &down
		case "total":
			dst = &total
		case "expire":
			dst = &expire
		case "starttime":
			dst = &start
		}
		if dst == nil {
			continue
		}
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			*dst = n
		}
	}
	return
}

// contentDispositionName extracts the config name from a
// content-disposition header, handling both filename= and RFC 5987
// filename*= forms.
func contentDispositionName(cd string) string {
	if cd == "" {
		return ""
	}
	for _, part := range strings.Split(cd, ";") {
		part = strings.TrimSpace(part)
		if after, ok := strings.CutPrefix(part, "filename*="); ok {
			// format: charset'lang'value — keep the value
			if i := strings.LastIndex(after, "''"); i >= 0 {
				after = after[i+2:]
			}
			if d, err := url.QueryUnescape(strings.Trim(after, `"' `)); err == nil {
				return d
			}
			return strings.Trim(after, `"' `)
		}
	}
	for _, part := range strings.Split(cd, ";") {
		part = strings.TrimSpace(part)
		if after, ok := strings.CutPrefix(part, "filename="); ok {
			after = strings.Trim(strings.TrimSpace(after), `"' `)
			if d, err := url.QueryUnescape(after); err == nil && d != "" {
				return d
			}
			return after
		}
	}
	return ""
}

// processSubscription queries one subscription URL end to end: provider
// website title (in parallel), the subscription itself, then node parsing.
func (p *SubinfoPlugin) processSubscription(ctx context.Context, subURL string) *subResult {
	res := &subResult{url: subURL, configName: "未知", status: "失败"}
	if isPrivateHost(subURL) {
		res.errKind, res.errDetail = errOther, "private address rejected"
		return res
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		res.website = p.getWebsiteInfo(ctx, subURL)
	}()

	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, body, err := p.http.get(cctx, subURL, subUserAgent)
	wg.Wait()

	switch {
	case err != nil:
		res.errKind, res.errDetail = errOther, err.Error()
		return res
	case resp.StatusCode != http.StatusOK:
		res.errKind, res.errDetail = errUnreachable, strconv.Itoa(resp.StatusCode)
		return res
	}

	// Quota name: remote mapping → content-disposition → site title.
	mappings := p.mu.get(ctx, p.http)
	switch {
	case mappingName(mappings, subURL) != "":
		res.configName = mappingName(mappings, subURL)
	case contentDispositionName(resp.Header.Get("content-disposition")) != "":
		res.configName = contentDispositionName(resp.Header.Get("content-disposition"))
	case res.website.name != "" && res.website.name != "连接失败":
		res.configName = res.website.name
	}
	res.profileURL = resp.Header.Get("profile-web-page-url")

	userinfo := resp.Header.Get("subscription-userinfo")
	if userinfo == "" {
		res.errKind = errNoUserInfo
		return res
	}

	up, down, total, expire, start := parseUserinfo(userinfo)
	res.upload, res.download, res.total, res.expireTs, res.startTs = up, down, total, expire, start
	res.used = up + down
	if total > res.used {
		res.remain = total - res.used
	}
	if total > 0 {
		res.percent = math.Round(float64(res.used)/float64(total)*10000) / 100
	}

	res.status = "有效"
	if total > 0 && res.remain <= 0 {
		res.status = "耗尽"
	}
	if expire > 0 && queryTime().Unix() > expire {
		res.status = "过期"
	}

	res.nodes = parseNodes(body)
	res.success = true
	return res
}
