package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "whois",
	Description: "域名 WHOIS 注册信息查询",
	DescEN:      "Domain WHOIS registration lookup",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

const (
	defaultTimeout = 15 * time.Second
	minTimeout     = 5
	maxTimeout     = 60
	maxBatch       = 10
	batchGap       = 500 * time.Millisecond
	dataFile       = "whois_data.json"
)

// WhoisPlugin answers the whois command backed by the namebeta check API.
type WhoisPlugin struct {
	set   plugin.Settings
	store *store
}

func New() *WhoisPlugin { return &WhoisPlugin{} }

func (p *WhoisPlugin) Name() string        { return "whois" }
func (p *WhoisPlugin) Description() string { return Metadata.Description }
func (p *WhoisPlugin) DescEN() string      { return Metadata.DescEN }

func (p *WhoisPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	set, err := mgr.Host().Settings(&plugin.SettingsSpec{
		Plugin:  p.Name(),
		Title:   "🔎 WHOIS 查询",
		TitleEN: "🔎 WHOIS Lookup",
		Settings: []plugin.Setting{{
			Key: "timeout", Label: "超时（秒）", LabelEN: "Timeout (seconds)",
			Hint:    "域名查询超时时间",
			HintEN:  "Timeout for domain queries",
			Kind:    plugin.SettingNumber,
			Default: 15, Min: minTimeout, Max: maxTimeout,
		}},
	})
	if err != nil {
		return err
	}
	p.set = set

	dir, err := mgr.Host().DataDir(p.Name())
	if err != nil {
		return err
	}
	p.store, err = newStore(filepath.Join(dir, dataFile))
	if err != nil {
		return err
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "whois",
		Description: "查询域名 WHOIS 注册信息（注册商/日期/DNS），支持批量与历史",
		DescEN:      "Look up domain WHOIS data (registrar/dates/DNS), with batch and history",
		Usage:       "whois <域名> · whois batch <域名...> · whois history · whois clear · 回复含域名的消息: whois",
		UsageEN:     "whois <domain> · whois batch <domain...> · whois history · whois clear · reply to a domain message: whois",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *WhoisPlugin) Start(context.Context) error { return nil }
func (p *WhoisPlugin) Stop(context.Context) error  { return nil }

// timeout reads the panel timeout setting, falling back to 15s.
func (p *WhoisPlugin) timeout() time.Duration {
	if p.set == nil {
		return defaultTimeout
	}
	sec := p.set.Int("timeout")
	if sec < minTimeout {
		return defaultTimeout
	}
	return time.Duration(sec) * time.Second
}

func (p *WhoisPlugin) handle(c *plugin.CommandContext) error {
	if c.Message == nil || c.Message.Message == nil || c.API == nil {
		return plugin.ErrNoMessage
	}
	tl := c.Tlocal
	args := c.Args
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}

	switch {
	case sub == "help" || sub == "h":
		return c.Edit(helpText(tl))
	case sub == "batch":
		return p.handleBatch(c, args[1:])
	case sub == "history":
		history, cacheCount, cacheHours := p.store.snapshot()
		return c.Edit(renderHistory(tl, history, cacheCount, cacheHours))
	case sub == "clear":
		return p.handleClear(c)
	}

	domain := ""
	// A replied message's first domain wins, like the source.
	if c.Message.IsReply {
		if r, err := c.ReplyMessage(); err == nil && r != nil {
			domain = extractDomain(r.Message)
		} else if err != nil && c.Logger != nil {
			c.Logger.Warn("whois: fetch replied message failed", "error", err)
		}
	}
	if domain == "" && sub != "" {
		domain = cleanDomain(sub)
	}
	if domain == "" {
		return c.Edit(helpText(tl))
	}
	if !validDomain(domain) {
		return c.Edit("❌ " + tl("域名格式无效", "Invalid domain format") + "\n\n" +
			tl("输入的域名：", "Domain entered: ") + plugin.Code(domain) + "\n\n" +
			"💡 " + tl("请输入有效的域名，例如：", "Please enter a valid domain, e.g.:") + "\n" +
			"• example.com\n• google.com\n• github.io")
	}

	// Cache first, then a live query.
	if rec, ok := p.store.cached(domain); ok {
		return c.Edit(renderResult(tl, rec, true))
	}
	_ = c.Edit("🔍 " + tl("正在查询域名信息…", "Querying domain info…") + "\n\n" +
		tl("域名：", "Domain: ") + plugin.Code(domain))

	rec, err := p.query(c.Context(), domain)
	if err != nil {
		return c.Edit(renderQueryError(tl, domain, err))
	}
	if err := p.store.save(rec); err != nil && c.Logger != nil {
		c.Logger.Warn("whois: persist record failed", "error", err)
	}
	return c.Edit(renderResult(tl, rec, false))
}

// query runs one live lookup, mapping transport failures to the source's
// error branches.
func (p *WhoisPlugin) query(ctx context.Context, domain string) (WhoisRecord, error) {
	whois, err := fetchWhoisCheck(ctx, p.timeout(), domain)
	if err != nil {
		return WhoisRecord{}, err
	}
	if whois == "" {
		return WhoisRecord{}, errNoWhois{}
	}
	return buildRecord(domain, cutRawData(whois)), nil
}

// errNoWhois marks a 200 answer without whois data.
type errNoWhois struct{}

func (errNoWhois) Error() string { return "no whois data" }

// renderQueryError renders the source's failure card: lookup failure,
// timeout, rate limit, denied, other HTTP, or transport.
func renderQueryError(tl func(zh, en string) string, domain string, err error) string {
	var b strings.Builder
	if _, ok := err.(errNoWhois); ok {
		b.WriteString("❌ " + tl("查询失败", "Lookup failed") + "\n\n" +
			tl("域名：", "Domain: ") + plugin.Code(domain) + "\n\n" +
			"💡 " + tl("可能的原因：", "Possible reasons:") + "\n" +
			"• " + tl("域名不存在或未注册", "Domain does not exist or is unregistered") + "\n" +
			"• " + tl("域名格式不正确", "Malformed domain") + "\n" +
			"• " + tl("WHOIS 信息不可用", "WHOIS data unavailable") + "\n\n" +
			"📖 " + tl("请检查域名拼写是否正确", "Please check the domain spelling"))
		return b.String()
	}
	b.WriteString("❌ " + tl("查询失败", "Lookup failed") + "\n\n" +
		tl("域名：", "Domain: ") + plugin.Code(domain) + "\n\n")
	var he *httpStatusError
	switch {
	case isTimeoutErr(err):
		b.WriteString(tl("错误：请求超时", "Error: request timed out") + "\n\n💡 " + tl("请检查网络连接后重试", "Check the network and retry"))
	case asHTTPStatus(err, &he) && he.code == 429:
		b.WriteString(tl("错误：请求过于频繁", "Error: too many requests") + "\n\n💡 " + tl("请稍后再试", "Try again later"))
	case asHTTPStatus(err, &he) && he.code == 403:
		b.WriteString(tl("错误：API 访问被拒绝", "Error: API access denied") + "\n\n💡 " + tl("可能需要更换 API 服务", "The API service may need replacing"))
	case asHTTPStatus(err, &he):
		fmt.Fprintf(&b, "%s %d\n%s %s\n\n💡 %s",
			tl("错误代码：", "Error code:"), he.code,
			tl("错误信息：", "Error message:"), plugin.Escape(err.Error()),
			tl("请稍后重试", "Try again later"))
	default:
		msg := err.Error()
		if msg == "" {
			msg = tl("未知错误", "unknown error")
		}
		b.WriteString(tl("错误信息：", "Error message: ") + plugin.Escape(msg) + "\n\n💡 " + tl("请检查网络连接后重试", "Check the network and retry"))
	}
	return b.String()
}

// asHTTPStatus extracts an *httpStatusError from err.
func asHTTPStatus(err error, target **httpStatusError) bool {
	if he, ok := err.(*httpStatusError); ok {
		*target = he
		return true
	}
	return false
}

// handleBatch runs the bounded batch loop with progress edits and a
// success/failure tally, caching each result.
func (p *WhoisPlugin) handleBatch(c *plugin.CommandContext, domains []string) error {
	tl := c.Tlocal
	if len(domains) == 0 {
		return c.Edit("❌ " + tl("请提供要查询的域名", "Provide domains to query") + "\n\n💡 " +
			tl("使用示例：", "Example:") + " " + plugin.Code("whois batch google.com github.com"))
	}
	if len(domains) > maxBatch {
		return c.Edit("❌ " + tl("批量查询限制", "Batch query limit") + "\n\n" +
			fmt.Sprintf(tl("每次最多查询 %d 个域名，您提供了 %d 个", "At most %d domains per batch, you gave %d"), maxBatch, len(domains)))
	}
	_ = c.Edit("🔍 " + tl("批量查询中…", "Batch query in progress…") + "\n\n" +
		fmt.Sprintf(tl("域名数量：%d", "Domains: %d"), len(domains)))

	var lines []batchLine
	success, fail := 0, 0
	for i, raw := range domains {
		domain := cleanDomain(raw)
		_ = c.Edit("🔍 " + tl("批量查询中…", "Batch query in progress…") + "\n" +
			fmt.Sprintf(tl("进度：%d/%d", "Progress: %d/%d"), i+1, len(domains)) + "\n" +
			tl("当前域名：", "Current: ") + plugin.Code(domain))
		if _, ok := p.store.cached(domain); ok {
			lines = append(lines, batchLine{domain: domain, ok: true, cached: true})
			success++
			continue
		}
		if _, err := p.query(c.Context(), domain); err != nil {
			lines = append(lines, batchLine{domain: domain})
			fail++
		} else {
			lines = append(lines, batchLine{domain: domain, ok: true})
			success++
		}
		if i < len(domains)-1 {
			select {
			case <-time.After(batchGap):
			case <-c.Context().Done():
				return c.Edit(renderBatchDone(tl, lines, success, fail))
			}
		}
	}
	return c.Edit(renderBatchDone(tl, lines, success, fail))
}

// handleClear wipes history and cache.
func (p *WhoisPlugin) handleClear(c *plugin.CommandContext) error {
	tl := c.Tlocal
	historyCount, cacheCount, err := p.store.clear()
	if err != nil {
		return c.Edit("❌ " + tl("清除失败", "Clear failed") + ": " + plugin.Escape(err.Error()))
	}
	if historyCount == 0 && cacheCount == 0 {
		return c.Edit(renderEmptyClear(tl))
	}
	return c.Edit(renderCleared(tl, historyCount, cacheCount))
}

// helpText renders the usage card (the source's promised expiry reminder
// never existed in code, so it is not advertised).
func helpText(tl func(zh, en string) string) string {
	return tl(
		"🔍 **WHOIS 域名查询**\n\n"+
			"**功能**\n"+
			"• 查询域名注册信息和状态\n"+
			"• 显示注册/过期/更新日期\n"+
			"• 查看DNS服务器和注册商\n"+
			"• 批量查询多个域名\n"+
			"• 查询历史记录缓存\n"+
			"• 域名到期分级提示\n\n"+
			"**用法**\n"+
			plugin.Code("whois <域名>")+" 查询指定域名\n"+
			plugin.Code("whois")+" 回复包含域名的消息\n"+
			plugin.Code("whois batch <域名1> <域名2>...")+" 批量查询（最多 10 个）\n"+
			plugin.Code("whois history")+" 查看查询历史\n"+
			plugin.Code("whois clear")+" 清除历史记录\n\n"+
			"**示例**\n"+
			plugin.Code("whois google.com")+"\n"+
			plugin.Code("whois batch google.com github.com")+"\n\n"+
			"**说明**\n"+
			"• 支持自动提取 URL 中的域名\n"+
			"• 查询结果自动缓存 24 小时",
		"🔍 **WHOIS Domain Lookup**\n\n"+
			"**Features**\n"+
			"• Domain registration info and status\n"+
			"• Created/expiry/updated dates\n"+
			"• Name servers and registrar\n"+
			"• Batch lookups\n"+
			"• History with cache\n"+
			"• Expiry grading\n\n"+
			"**Usage**\n"+
			plugin.Code("whois <domain>")+" look up a domain\n"+
			plugin.Code("whois")+" reply to a message containing a domain\n"+
			plugin.Code("whois batch <d1> <d2>...")+" batch lookup (max 10)\n"+
			plugin.Code("whois history")+" show query history\n"+
			plugin.Code("whois clear")+" clear history\n\n"+
			"**Examples**\n"+
			plugin.Code("whois google.com")+"\n"+
			plugin.Code("whois batch google.com github.com")+"\n\n"+
			"**Notes**\n"+
			"• Extracts domains from URLs automatically\n"+
			"• Results cached for 24 hours")
}
