package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const apiBase = "http://ip-api.com/json/"

var Metadata = &plugin.PluginMetadata{
	Name:        "ip",
	Description: "查询 IP 或域名的归属地",
	DescEN:      "Look up where an IP or domain lives",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type IPPlugin struct{ http *http.Client }

func New() *IPPlugin { return &IPPlugin{http: &http.Client{Timeout: 15 * time.Second}} }

func (p *IPPlugin) Name() string        { return "ip" }
func (p *IPPlugin) Description() string { return Metadata.Description }
func (p *IPPlugin) DescEN() string      { return Metadata.DescEN }

func (p *IPPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "ip",
		Description: "查询 IP 或域名的地理位置、ISP 和 AS，也可回复含 IP 的消息",
		DescEN:      "Show location, ISP and AS of an IP or domain, or of one found in the replied message",
		Usage:       "ip <IP|域名|链接> · 回复消息: ip",
		UsageEN:     "ip <IP|domain|URL> · reply: ip",
		Plugin:      p.Name(),
		Category:    "tools",
		Handler:     p.handle,
	})
}

func (p *IPPlugin) Start(context.Context) error { return nil }
func (p *IPPlugin) Stop(context.Context) error  { return nil }

func help(tl func(string, string) string) string {
	return tl(
		"📍 **IP 查询**\n\n"+plugin.Code("ip 8.8.8.8")+"\n"+plugin.Code("ip github.com")+"\n"+plugin.Code("ip 2001:4860:4860::8888")+"\n回复含 IP、域名或链接的消息发 "+plugin.Code("ip"),
		"📍 **IP lookup**\n\n"+plugin.Code("ip 8.8.8.8")+"\n"+plugin.Code("ip github.com")+"\n"+plugin.Code("ip 2001:4860:4860::8888")+"\nOr reply "+plugin.Code("ip")+" to a message with an IP, domain or link")
}

func (p *IPPlugin) handle(ctx *plugin.CommandContext) error {
	if a := strings.ToLower(ctx.RawArgs); a == "help" || a == "h" {
		return ctx.Edit(help(ctx.Tlocal))
	}
	query := extractTarget(ctx.RawArgs)
	if query == "" && ctx.RawArgs == "" && ctx.Message.IsReply {
		if r, err := ctx.ReplyMessage(); err == nil && r != nil {
			query = extractTarget(r.Message)
		}
	}
	if query == "" {
		if ctx.RawArgs != "" || ctx.Message.IsReply {
			return ctx.Edit("❌ " + ctx.Tlocal("没认出 IP 或域名", "No IP or domain found") + "\n\n" + help(ctx.Tlocal))
		}
		return ctx.Edit(help(ctx.Tlocal))
	}
	_ = ctx.Edit("🔍 " + ctx.Tlocal("正在查询 ", "Looking up ") + plugin.Code(query))
	lang := "en"
	if ctx.Lang != "en-US" {
		lang = "zh-CN"
	}
	info, err := p.lookup(ctx.Context(), query, lang)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("查询失败", "Lookup failed") + " · " + plugin.Code(query) + "\n> " + failReason(ctx.Tlocal, err))
	}
	return editNoPreview(ctx, render(ctx.Tlocal, query, info))
}

type ipInfo struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Country  string `json:"country"`
	Region   string `json:"regionName"`
	City     string `json:"city"`
	ISP      string `json:"isp"`
	Org      string `json:"org"`
	AS       string `json:"as"`
	Query    string `json:"query"`
	Timezone string `json:"timezone"`
	Reverse  string `json:"reverse"`
	Proxy    bool   `json:"proxy"`
	Hosting  bool   `json:"hosting"`
	Mobile   bool   `json:"mobile"`
}

type apiError struct{ msg string }

func (e *apiError) Error() string { return e.msg }

func (p *IPPlugin) lookup(ctx context.Context, query, lang string) (*ipInfo, error) {
	u := apiBase + url.PathEscape(query) + "?lang=" + lang +
		"&fields=status,message,country,regionName,city,isp,org,as,query,timezone,reverse,proxy,hosting,mobile"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "PaperValet-IP/1.0")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &apiError{"rate limited"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var info ipInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return nil, err
	}
	if info.Status != "success" {
		return nil, &apiError{info.Message}
	}
	return &info, nil
}

func failReason(tl func(string, string) string, err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.msg {
		case "private range":
			return tl("这是内网地址", "That is a private address")
		case "reserved range":
			return tl("这是保留地址", "That is a reserved address")
		case "invalid query":
			return tl("域名解析不到或格式不对", "The domain does not resolve or the query is malformed")
		case "rate limited":
			return tl("查询太频繁，等一分钟再试", "Too many lookups, try again in a minute")
		}
		return plugin.Escape(ae.msg)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return tl("请求超时，稍后再试", "Request timed out, try again later")
	}
	return plugin.Escape(err.Error())
}

func orNA(s string) string {
	if strings.TrimSpace(s) == "" {
		return "N/A"
	}
	return s
}

var asNum = regexp.MustCompile(`^AS(\d+)`)

func render(tl func(string, string) string, query string, d *ipInfo) string {
	var b strings.Builder
	b.WriteString("🌍 **" + tl("IP 查询", "IP lookup") + "** · " + plugin.Code(d.Query))
	if query != d.Query {
		b.WriteString(" ← " + plugin.Escape(query))
	}
	b.WriteString("\n\n")
	loc := []string{}
	for _, s := range []string{d.Country, d.Region, d.City} {
		if s != "" && (len(loc) == 0 || loc[len(loc)-1] != s) {
			loc = append(loc, s)
		}
	}
	line := func(icon, label, val string) {
		b.WriteString("> " + icon + " " + label + " " + val + "\n")
	}
	line("📍", tl("位置", "Location"), plugin.Escape(orNA(strings.Join(loc, " · "))))
	line("🏢", "ISP", plugin.Escape(orNA(d.ISP)))
	if d.Org != "" && d.Org != d.ISP {
		line("🏦", tl("组织", "Org"), plugin.Escape(d.Org))
	}
	as := plugin.Escape(orNA(d.AS))
	if m := asNum.FindStringSubmatch(d.AS); m != nil {
		as = plugin.Link(d.AS, "https://bgp.he.net/AS"+m[1])
	}
	line("🔢", "AS", as)
	if d.Timezone != "" {
		line("⏰", tl("时区", "Timezone"), plugin.Escape(d.Timezone))
	}
	if d.Reverse != "" {
		line("↩️", "rDNS", plugin.Code(d.Reverse))
	}
	var tags []string
	if d.Proxy {
		tags = append(tags, tl("代理/VPN", "proxy/VPN"))
	}
	if d.Hosting {
		tags = append(tags, tl("数据中心", "datacenter"))
	}
	if d.Mobile {
		tags = append(tags, tl("移动网络", "mobile"))
	}
	if len(tags) > 0 {
		b.WriteString("\n🏷 " + strings.Join(tags, " · "))
	}
	return strings.TrimRight(b.String(), "\n")
}

var (
	ipv4Re   = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`)
	ipv6Re   = regexp.MustCompile(`[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}`)
	domainRe = regexp.MustCompile(`\b(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}\b`)
)

// extractTarget finds the first IP, then URL host, then domain in text.
func extractTarget(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if ip := net.ParseIP(strings.Trim(text, "[]")); ip != nil {
		return ip.String()
	}
	if m := ipv4Re.FindString(text); m != "" {
		return m
	}
	for _, m := range ipv6Re.FindAllString(text, -1) {
		if ip := net.ParseIP(m); ip != nil && strings.Contains(m, ":") {
			return ip.String()
		}
	}
	for _, f := range strings.Fields(text) {
		if u, err := url.Parse(f); err == nil && u.Host != "" {
			return strings.ToLower(u.Hostname())
		}
	}
	return strings.ToLower(domainRe.FindString(text))
}

// editNoPreview edits the command message without a link preview, which
// ctx.Edit cannot turn off.
func editNoPreview(ctx *plugin.CommandContext, text string) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	plain, ents := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesEditMessageRequest{Peer: peer, ID: ctx.Message.Message.ID, Message: plain, NoWebpage: true}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	_, err = ctx.API.MessagesEditMessage(ctx.Context(), req)
	return err
}
