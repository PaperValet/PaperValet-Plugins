package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// ============================================================
// Help and argument errors
// ============================================================

func help(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🌐 **BGP 路由查询**\n\n"+
			"**用法**\n"+
			plugin.Code("bgp 1.1.1.1")+" IP：起源 AS、宣告前缀、注册信息\n"+
			plugin.Code("bgp 8.8.8.0/24")+" 前缀：宣告状态、起源 AS、常见路径\n"+
			plugin.Code("bgp AS15169")+" AS：名称、上下游邻居、宣告前缀数\n"+
			plugin.Code("bgp dns 1.1.1.1")+" 该 IP 的正反向 DNS 记录\n"+
			plugin.Code("bgp")+" 回复一条含 IP / 前缀 / AS 的消息\n\n"+
			"**说明**\n"+
			"路由图来自 bgp.tools，现需登录，尽力抓取\n"+
			"数据来自 Team Cymru 与 RIPEstat",
		"🌐 **BGP lookup**\n\n"+
			"**Usage**\n"+
			plugin.Code("bgp 1.1.1.1")+" IP: origin AS, covering prefix, registry data\n"+
			plugin.Code("bgp 8.8.8.0/24")+" prefix: announced state, origin AS, common paths\n"+
			plugin.Code("bgp AS15169")+" ASN: name, upstream/downstream neighbours, prefix count\n"+
			plugin.Code("bgp dns 1.1.1.1")+" forward/reverse DNS for the IP\n"+
			plugin.Code("bgp")+" reply to a message with an IP / prefix / ASN\n\n"+
			"**Notes**\n"+
			"Graph comes from bgp.tools, now login-walled; fetched best-effort\n"+
			"Data from Team Cymru and RIPEstat")
}

func badArgs(ctx *plugin.CommandContext, dns bool) string {
	if dns {
		return "❌ " + ctx.Tlocal("请提供有效的 IP 地址，例如 ", "Provide a valid IP, e.g. ") +
			plugin.Code("bgp dns 1.1.1.1") + "\n\n" + help(ctx)
	}
	return "❌ " + ctx.Tlocal("没认出 IP、前缀或 AS 号", "No IP, prefix or ASN found") + "\n\n" + help(ctx)
}

// ============================================================
// bgp <ip> and bgp <prefix>
// ============================================================

// runNet handles both an IP and an explicit prefix: the data sources and
// output are the same, only the “covering prefix” line differs.
func (p *BgpPlugin) runNet(ctx *plugin.CommandContext, query, hostIP string, explicitPrefix bool) error {
	_ = ctx.Edit("🔍 " + ctx.Tlocal("正在查询 BGP 路由信息", "Looking up BGP routing info") + " " + plugin.Code(query))

	qctx, cancel := context.WithTimeout(ctx.Context(), 90*time.Second)
	defer cancel()

	// Cymru gives registry data and, for an IP, its covering prefix.
	// The Cymru dial and the RIPEstat network-info call run in parallel —
	// together they dominated the serial latency of the whole query.
	var cy cymruRecord
	var cyErr error
	cyDone := make(chan struct{})
	go func() {
		defer close(cyDone)
		switch {
		case hostIP != "":
			cy, cyErr = cymruLookup(qctx, hostIP)
		default:
			if _, ipnet, err := net.ParseCIDR(query); err == nil {
				cy, cyErr = cymruLookup(qctx, ipnet.String())
			} else {
				cyErr = errors.New("unreachable")
			}
		}
	}()

	// RIPEstat: covering prefix + origins, then routing state and paths.
	var ni networkInfo
	niErr := p.http.ripeGet(qctx, "network-info", orStr(hostIP, query), &ni)
	<-cyDone

	prefix := ni.Prefix
	if prefix == "" {
		prefix = cy.Prefix
	}
	if prefix == "" && hostIP != "" {
		if ip := net.ParseIP(hostIP); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				prefix = cidr24(ip4)
			} else {
				prefix = cidr48(ip)
			}
		}
	}

	var ov prefixOverview
	var ovErr error
	if prefix != "" {
		ovErr = p.http.ripeGet(qctx, "prefix-overview", prefix, &ov)
	}
	var lg lookingGlass
	var lgErr error
	if prefix != "" && (ov.Announced || ovErr != nil) {
		lgErr = p.http.ripeGet(qctx, "looking-glass", prefix, &lg)
	}

	pages := renderNet(ctx, query, hostIP, explicitPrefix, cy, cyErr, ni, niErr, ov, ovErr, lg, lgErr)
	if err := deliver(ctx, pages); err != nil {
		return err
	}

	// Best-effort bgp.tools graph on top of the text answer.
	p.tryGraph(ctx, hostIP, query)
	return nil
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func cidr24(ip4 net.IP) string { return ip4.Mask(net.CIDRMask(24, 32)).String() + "/24" }
func cidr48(ip net.IP) string  { return ip.Mask(net.CIDRMask(48, 128)).String() + "/48" }

// ============================================================
// bgp <asn>
// ============================================================

func (p *BgpPlugin) runASN(ctx *plugin.CommandContext, asn int) error {
	asnStr := fmt.Sprintf("AS%d", asn)
	_ = ctx.Edit("🔍 " + ctx.Tlocal("正在查询", "Looking up") + " " + plugin.Code(asnStr) + " …")

	qctx, cancel := context.WithTimeout(ctx.Context(), 60*time.Second)
	defer cancel()

	// All four sources are independent; fetching them in parallel halves
	// the worst-case latency under the shared qctx deadline.
	var (
		ov    asOverview
		nb    neighboursData
		ap    announcedPrefixes
		cy    cymruRecord
		ovErr error
		nbErr error
		apErr error
		cyErr error
		wg    sync.WaitGroup
	)
	wg.Add(4)
	go func() { defer wg.Done(); ovErr = p.http.ripeGet(qctx, "as-overview", asnStr, &ov) }()
	go func() { defer wg.Done(); nbErr = p.http.ripeGet(qctx, "asn-neighbours", asnStr, &nb) }()
	go func() { defer wg.Done(); apErr = p.http.ripeGet(qctx, "announced-prefixes", asnStr, &ap) }()
	go func() { defer wg.Done(); cy, cyErr = cymruLookup(qctx, asnStr) }()
	wg.Wait()

	return deliver(ctx, renderASN(ctx, asn, ov, ovErr, nb, nbErr, len(ap.Prefixes), apErr, cy, cyErr))
}

// ============================================================
// bgp dns <ip>
// ============================================================

func (p *BgpPlugin) runDNS(ctx *plugin.CommandContext, ipStr string) error {
	_ = ctx.Edit("🔍 " + ctx.Tlocal("正在查询 DNS 解析记录", "Looking up DNS records") + " " + plugin.Code(ipStr))
	qctx, cancel := context.WithTimeout(ctx.Context(), 40*time.Second)
	defer cancel()

	var chain dnsChain
	if err := p.http.ripeGet(qctx, "dns-chain", ipStr, &chain); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("DNS 查询失败", "DNS lookup failed") + "\n> " + plugin.Escape(err.Error()))
	}
	pages := renderDNS(ctx, ipStr, chain)
	if len(pages) == 0 {
		return ctx.Edit("❌ " + ctx.Tlocal("未找到 DNS 解析记录", "No DNS records found") + "\n\n" +
			"🔗 " + plugin.Link("bgp.tools/ip/"+ipStr, "https://bgp.tools/ip/"+ipStr))
	}
	return deliver(ctx, pages)
}

// ============================================================
// bgp.tools route graph (best effort)
// ============================================================

// prefixCandidates lists the prefixes to try for the graph, in order
// (the TeleBox source tried /24 then /23).
func prefixCandidates(ip net.IP) []string {
	if ip4 := ip.To4(); ip4 != nil {
		return []string{
			ip4.Mask(net.CIDRMask(24, 32)).String() + "/24",
			ip4.Mask(net.CIDRMask(23, 32)).String() + "/23",
		}
	}
	return []string{ip.Mask(net.CIDRMask(48, 128)).String() + "/48"}
}

// fetchGraphSVG downloads the bgp.tools path image for the first prefix
// that has one. Returns the SVG bytes and the prefix used.
func (p *BgpPlugin) fetchGraphSVG(ctx context.Context, ip net.IP) ([]byte, string, error) {
	for _, prefix := range prefixCandidates(ip) {
		u := "https://bgp.tools/pathimg/rt-" + strings.ReplaceAll(prefix, "/", "_") +
			"?4c1db184-e649-4491-8b7f-06177bcb4f25&loggedin"
		data, err := p.http.getRaw(ctx, u, map[string]string{
			"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.6261.112 Safari/537.36",
			"Accept":          "image/svg+xml,image/avif,image/webp,*/*;q=0.8",
			"Referer":         "https://bgp.tools/",
			"Accept-Language": "en-US,en;q=0.9",
		}, 4<<20)
		if err != nil || len(data) == 0 {
			continue // 307 → login wall; skipped
		}
		head := data
		if len(head) > 512 {
			head = head[:512]
		}
		if !strings.Contains(strings.ToLower(string(head)), "<svg") {
			continue
		}
		return data, prefix, nil
	}
	return nil, "", errors.New("graph unavailable")
}

// tryGraph renders the bgp.tools SVG to a PNG with ffmpeg (librsvg) and
// sends it as a photo. Silent on failure: the text answer already notes
// the graph is best-effort.
func (p *BgpPlugin) tryGraph(ctx *plugin.CommandContext, hostIP, query string) {
	if hostIP == "" {
		return // explicit prefixes skip the graph, like the source
	}
	ip := net.ParseIP(hostIP)
	if ip == nil {
		return
	}
	gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx.Context()), 60*time.Second)
	defer cancel()

	svg, prefix, err := p.fetchGraphSVG(gctx, ip)
	if err != nil {
		// Not fatal — the text answer already says the graph is
		// best-effort — but a silent login-wall would look like the
		// feature vanished, so leave a trace.
		if p.log != nil {
			p.log.Debug("bgp: route graph unavailable", "ip", hostIP, "error", err)
		}
		return
	}
	dir, err := os.MkdirTemp("", "bgp-")
	if err != nil {
		return
	}
	defer os.RemoveAll(dir)
	svgPath := filepath.Join(dir, "graph.svg")
	pngPath := filepath.Join(dir, "graph.png")
	if err := os.WriteFile(svgPath, svg, 0o600); err != nil {
		return
	}
	cmd := exec.CommandContext(gctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-y", "-i", svgPath, "-frames:v", "1", "-update", "1",
		"-vf", "scale=2400:1800:force_original_aspect_ratio=decrease", pngPath)
	if err := cmd.Run(); err != nil {
		return
	}
	if st, err := os.Stat(pngPath); err != nil || st.Size() == 0 {
		return
	}
	caption := "🌐 " + plugin.Bold(ctx.Tlocal("BGP 路由图", "BGP route paths")) + "\n" +
		plugin.Code(hostIP) + " · " + ctx.Tlocal("前缀", "prefix") + " " + plugin.Code(prefix) + "\n" +
		"🔗 " + plugin.Link("bgp.tools/prefix/"+prefix, "https://bgp.tools/prefix/"+prefix)
	_ = ctx.ReplyMedia(pngPath, caption)
}

// ============================================================
// Delivery
// ============================================================

// deliver edits the command message with the first page and sends the rest
// as follow-ups, all without link previews (duckduckgo style).
func deliver(ctx *plugin.CommandContext, pages []string) error {
	if len(pages) == 0 {
		return ctx.Edit("❌")
	}
	peer, err := ctx.ResolvePeer()
	if err == nil && ctx.API != nil && ctx.Message != nil && ctx.Message.Message != nil {
		if err := editNoPreview(ctx, peer, pages[0]); err == nil {
			return sendRest(ctx, peer, pages[1:])
		}
	}
	if err := ctx.Edit(pages[0]); err != nil {
		return err
	}
	for _, pg := range pages[1:] {
		if err := ctx.Reply(pg); err != nil {
			return err
		}
	}
	return nil
}

func editNoPreview(ctx *plugin.CommandContext, peer tg.InputPeerClass, text string) error {
	plain, ents := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesEditMessageRequest{Peer: peer, ID: ctx.Message.Message.ID, Message: plain, NoWebpage: true}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	_, err := ctx.API.MessagesEditMessage(ctx.Context(), req)
	return err
}

func sendRest(ctx *plugin.CommandContext, peer tg.InputPeerClass, pages []string) error {
	for _, pg := range pages {
		plain, ents := plugin.ParseMarkdown(pg, nil)
		req := &tg.MessagesSendMessageRequest{Peer: peer, Message: plain, NoWebpage: true, RandomID: time.Now().UnixNano()}
		if len(ents) > 0 {
			req.SetEntities(ents)
		}
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: ctx.Message.Message.ID})
		if _, err := ctx.API.MessagesSendMessage(ctx.Context(), req); err != nil {
			return ctx.Reply(pg)
		}
	}
	return nil
}
