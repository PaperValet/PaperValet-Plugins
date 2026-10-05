// Package main implements the bgp plugin: routing data for an IP, prefix or
// ASN from Team Cymru whois and RIPEstat, plus a best-effort BGP path graph
// from bgp.tools rendered to PNG with ffmpeg.
package main

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "bgp",
	Description: "查询 IP、前缀或 AS 的 BGP 路由信息",
	DescEN:      "BGP routing info for an IP, prefix or ASN",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type BgpPlugin struct {
	http *client
}

func New() *BgpPlugin {
	return &BgpPlugin{http: newClient(20 * time.Second)}
}

func (p *BgpPlugin) Name() string        { return "bgp" }
func (p *BgpPlugin) Description() string { return Metadata.Description }
func (p *BgpPlugin) DescEN() string      { return Metadata.DescEN }

func (p *BgpPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "bgp",
		Description: "查询 IP、前缀或 AS 的 BGP 路由信息（起源、路径、DNS、路由图）",
		DescEN:      "BGP routing info for an IP, prefix or ASN (origin, paths, DNS, graph)",
		Usage:       "bgp <IP|前缀|AS> · bgp dns <IP> · 回复消息: bgp | bgp help",
		UsageEN:     "bgp <IP|prefix|AS> · bgp dns <IP> · reply: bgp | bgp help",
		Plugin:      p.Name(),
		Category:    "tools",
		RateLimit:   5,
		Handler:     p.handle,
	})
}

func (p *BgpPlugin) Start(context.Context) error { return nil }
func (p *BgpPlugin) Stop(context.Context) error  { return nil }

func (p *BgpPlugin) handle(ctx *plugin.CommandContext) error {
	if a := strings.ToLower(strings.TrimSpace(ctx.RawArgs)); a == "help" || a == "h" {
		return ctx.Edit(help(ctx))
	}

	args := ctx.Args
	dnsMode := false
	if len(args) > 0 && strings.EqualFold(args[0], "dns") {
		dnsMode = true
		args = args[1:]
	}

	// Resolve the target: arguments first, then the replied message, like
	// the TeleBox source (args → trigger text → reply text).
	text := strings.Join(args, " ")
	replyText := ""
	if ctx.Message != nil && ctx.Message.IsReply {
		if r, err := ctx.ReplyMessage(); err == nil && r != nil {
			replyText = r.Message
		}
	}

	if dnsMode {
		ip := extractIP(text)
		if ip == "" {
			ip = extractIP(replyText)
		}
		if ip == "" {
			return ctx.Edit(badArgs(ctx, true))
		}
		return p.runDNS(ctx, ip)
	}

	// A bare CIDR prefix is a prefix query; otherwise the first IP wins.
	if _, ipnet, err := net.ParseCIDR(text); err == nil {
		return p.runNet(ctx, ipnet.String(), extractIP(text), true)
	}

	ip := extractIP(text)
	if ip == "" {
		ip = extractIP(replyText)
	}
	if ip != "" {
		return p.runNet(ctx, ip, ip, false)
	}

	if asn, ok := parseASN(text); ok {
		return p.runASN(ctx, asn)
	}
	if asn, ok := parseASN(replyText); ok {
		return p.runASN(ctx, asn)
	}

	if strings.TrimSpace(ctx.RawArgs) == "" && replyText == "" {
		return ctx.Edit(help(ctx))
	}
	return ctx.Edit(badArgs(ctx, false))
}
