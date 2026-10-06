package main

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// ============================================================
// Page packing
// ============================================================

// pageLimit mirrors duckduckgo: the first page can run to the hard limit,
// continuations stay comfortably under Telegram's 4096-char cap.
const (
	pageSoftLimit = 3500
	pageHardLimit = 4000
)

// packPages packs whole lines into pages; page 0 carries the header.
// A single line longer than a page (a giant Pre block of PTR records,
// say) is split at whitespace so no page can exceed Telegram's cap and
// fail the edit outright.
func packPages(header string, lines []string) []string {
	var pages []string
	split := make([]string, 0, len(lines))
	for _, ln := range lines {
		if len(ln) <= pageHardLimit {
			split = append(split, ln)
			continue
		}
		for len(ln) > pageSoftLimit {
			cut := strings.LastIndexByte(ln[:pageSoftLimit], ' ')
			if cut < pageSoftLimit/2 {
				cut = pageSoftLimit
			}
			split = append(split, strings.TrimRight(ln[:cut], " "))
			ln = strings.TrimLeft(ln[cut:], " ")
		}
		if ln != "" {
			split = append(split, ln)
		}
	}
	lines = split
	cur := header
	flush := func() {
		if cur != "" {
			pages = append(pages, cur)
		}
		cur = ""
	}
	for _, ln := range lines {
		limit := pageSoftLimit
		if len(pages) == 0 {
			limit = pageHardLimit
		}
		if cur == "" || len(cur)+len(ln)+1 <= limit {
			if cur != "" {
				cur += "\n"
			}
			cur += ln
			continue
		}
		flush()
		cur = ln
	}
	flush()
	if n := len(pages); n > 1 {
		for i := range pages {
			marker := fmt.Sprintf("📄 (%d/%d)", i+1, n)
			if len(pages[i])+len(marker)+2 <= pageHardLimit {
				pages[i] += "\n\n" + marker
			}
		}
	}
	return pages
}

// ============================================================
// Small helpers
// ============================================================

func orDash(s string) string {
	if strings.TrimSpace(s) == "" || s == "NA" {
		return "—"
	}
	return s
}

// row renders one "> icon label: value" line.
func row(icon, label, val string) string { return "> " + icon + " " + label + ": " + val }

func bgpToolsLink(resource string) string {
	return plugin.Link("bgp.tools/"+resource, "https://bgp.tools/"+resource)
}

// holderLabel trims the registry slug and renders "AS123 · Name".
func holderLabel(asn int, holder string) string {
	label := "AS" + strconv.Itoa(asn)
	if h := asHolderName(holder); h != "" && h != "NA" {
		label += " · " + h
	}
	return label
}

// ============================================================
// bgp <ip|prefix>
// ============================================================

func renderNet(ctx *plugin.CommandContext, query, hostIP string, explicitPrefix bool,
	cy cymruRecord, cyErr error, ni networkInfo, niErr error,
	ov prefixOverview, ovErr error, lg lookingGlass, lgErr error) []string {

	var b strings.Builder
	title := ctx.Tlocal("BGP 路由查询", "BGP routing")
	b.WriteString("🌐 **" + title + "** · " + plugin.Code(query) + "\n\n")

	prefix := ov.Resource
	if prefix == "" {
		prefix = ni.Prefix
	}
	if prefix == "" {
		prefix = cy.Prefix
	}
	if explicitPrefix {
		b.WriteString(row("🧱", ctx.Tlocal("前缀", "Prefix"), plugin.Code(query)) + "\n")
	} else if prefix != "" {
		b.WriteString(row("🧱", ctx.Tlocal("所属前缀", "Covering prefix"), plugin.Code(prefix)) + "\n")
	}

	// Origin AS: prefer prefix-overview, then network-info, then Cymru.
	type asEntry struct {
		asn    int
		holder string
	}
	var order []asEntry
	seen := map[int]bool{}
	add := func(asn int, holder string) {
		if asn <= 0 || seen[asn] {
			return
		}
		seen[asn] = true
		order = append(order, asEntry{asn, holder})
	}
	for _, a := range ov.ASNs {
		add(a.ASN, a.Holder)
	}
	for _, s := range ni.ASNs {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			add(n, cy.ASName)
		}
	}
	if len(order) == 0 && cy.AS != "" && cy.AS != "NA" {
		if n, err := strconv.Atoi(cy.AS); err == nil {
			add(n, cy.ASName)
		}
	}
	switch {
	case len(order) > 0:
		labels := make([]string, len(order))
		for i, e := range order {
			labels[i] = plugin.Link(holderLabel(e.asn, e.holder), "https://bgp.tools/as/"+strconv.Itoa(e.asn))
		}
		b.WriteString(row("🔢", ctx.Tlocal("起源 AS", "Origin AS"), strings.Join(labels, " · ")) + "\n")
	case ovErr == nil && niErr == nil && !ov.Announced && ov.Resource != "":
		// RIPEstat answered but sees no announcement: a genuinely routed-nowhere
		// prefix (or private/reserved space) — say so instead of an em dash.
		b.WriteString(row("🔢", ctx.Tlocal("起源 AS", "Origin AS"), ctx.Tlocal("未宣告", "not announced")) + "\n")
		b.WriteString(row("⚠️", ctx.Tlocal("状态", "Status"), ctx.Tlocal("该前缀当前未被宣告", "prefix is not currently announced")) + "\n")
	default:
		b.WriteString(row("🔢", ctx.Tlocal("起源 AS", "Origin AS"), orDash("")) + "\n")
	}

	if cy.ASName != "" && cy.ASName != "NA" {
		b.WriteString(row("🏷", ctx.Tlocal("AS 名称", "AS name"), plugin.Escape(cy.ASName)) + "\n")
	}
	if r := registryRow(ctx, cy.Registry, cy.CC, cy.Allocated); r != "" {
		b.WriteString(r + "\n")
	}
	// The IANA-level "block" (whole /8) is noise; show allocation blocks
	// smaller than a /4 only.
	if ov.Block.Resource != "" && !isIANABlock(ov.Block.Resource) {
		b.WriteString(row("📦", ctx.Tlocal("地址块", "Block"), plugin.Escape(ov.Block.Resource)) + "\n")
	}
	if len(ov.Related) > 0 {
		label := ctx.Tlocal("相关前缀", "Related prefixes")
		if !ov.Announced {
			label = ctx.Tlocal("上级前缀", "Less specific")
		}
		b.WriteString(row("🔗", label, plugin.Code(strings.Join(ov.Related, " "))) + "\n")
	}

	// Common transit paths from the RIS looking glass.
	var lines []string
	if len(order) > 0 {
		peers := collectPeers(lg.RRCs)
		if stats := topPaths(dedupePaths(peers, order[0].asn), 5); len(stats) > 0 {
			lines = append(lines, "")
			lines = append(lines, "🛤 **"+ctx.Tlocal("常见路径", "Common paths")+"** ("+ctx.Tlocal("观测对等点数", "observing peers")+")")
			for _, s := range stats {
				lines = append(lines, plugin.Code(s.Path)+" · ×"+strconv.Itoa(s.Count))
			}
		} else if lgErr == nil && len(peers) == 0 {
			lines = append(lines, "", "🛤 "+ctx.Tlocal("RIS 未见该前缀的路径数据", "No RIS path data for this prefix"))
		}
	}

	links := "🔗 " + bgpToolsLink("prefix/"+orStr(prefix, query))
	if !explicitPrefix && hostIP != "" {
		links += " · " + bgpToolsLink("ip/"+hostIP)
	}
	lines = append(lines, "", links)
	lines = append(lines, "🗺 "+ctx.Tlocal(
		"路由图尽力从 bgp.tools 抓取（现需登录，可能失败）",
		"Route graph fetched best-effort from bgp.tools (login-walled, may fail)"))

	return packPages(b.String(), compact(lines))
}

// compact drops empty lines at slice boundaries so header/extra sections
// never stack two blank lines.
func compact(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if ln == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, ln)
	}
	return out
}

func registryRow(ctx *plugin.CommandContext, registry, cc, date string) string {
	var parts []string
	for _, s := range []string{registry, cc, date} {
		if s != "" && s != "NA" {
			parts = append(parts, plugin.Escape(s))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return row("🗓", ctx.Tlocal("注册信息", "Registry"), strings.Join(parts, " · "))
}

// isIANABlock reports whether the prefix is a giant IANA allocation (/4
// and shorter for v4, /12 and shorter for v6) not worth showing.
func isIANABlock(prefix string) bool {
	_, ipnet, err := net.ParseCIDR(prefix)
	if err != nil {
		return false
	}
	ones, _ := ipnet.Mask.Size()
	if ipnet.IP.To4() != nil {
		return ones <= 8
	}
	return ones <= 12
}

// ============================================================
// bgp <asn>
// ============================================================

func renderASN(ctx *plugin.CommandContext, asn int, ov asOverview, ovErr error,
	nb neighboursData, nbErr error, prefixCount int, apErr error,
	cy cymruRecord, cyErr error) []string {

	asnStr := "AS" + strconv.Itoa(asn)
	var b strings.Builder
	b.WriteString("🌐 **" + asnStr + "**\n\n")

	holder := ov.Holder
	if holder == "" {
		holder = cy.ASName
	}
	if holder != "" && holder != "NA" {
		b.WriteString(row("🏷", ctx.Tlocal("名称", "Name"), plugin.Escape(holder)) + "\n")
	}
	if ov.Block.Resource != "" {
		b.WriteString(row("📦", ctx.Tlocal("AS 块", "Block"), plugin.Escape(ov.Block.Resource)) + "\n")
	}
	if r := registryRow(ctx, cy.Registry, cy.CC, cy.Allocated); r != "" {
		b.WriteString(r + "\n")
	}
	var lines []string
	if nbErr == nil && nb.Counts.Unique > 0 {
		lines = append(lines, row("🤝", ctx.Tlocal("邻居", "Neighbours"), fmt.Sprintf("%d %s · %d %s · %d %s",
			nb.Counts.Left, ctx.Tlocal("上游", "upstream"),
			nb.Counts.Right, ctx.Tlocal("下游", "downstream"),
			nb.Counts.Unique, ctx.Tlocal("合计", "total"))))
	}
	if apErr == nil && prefixCount > 0 {
		lines = append(lines, row("🧱", ctx.Tlocal("宣告前缀", "Announced prefixes"), "**"+strconv.Itoa(prefixCount)+"**"))
	}
	lines = append(lines, "", "🔗 "+bgpToolsLink(asnStr)+" · "+plugin.Link("bgp.he.net/"+asnStr, "https://bgp.he.net/"+asnStr))

	var errs []string
	for _, e := range []struct {
		name string
		err  error
	}{
		{ctx.Tlocal("AS 概览", "AS overview"), ovErr},
		{ctx.Tlocal("邻居", "neighbours"), nbErr},
		{ctx.Tlocal("前缀列表", "prefix list"), apErr},
		{ctx.Tlocal("注册信息", "registry"), cyErr},
	} {
		if e.err != nil {
			errs = append(errs, "> ❌ "+e.name+": "+plugin.Escape(e.err.Error()))
		}
	}
	if len(errs) > 0 {
		lines = append(lines, "", strings.Join(errs, "\n"))
	}
	return packPages(b.String(), lines)
}

// ============================================================
// bgp dns <ip>
// ============================================================

func renderDNS(ctx *plugin.CommandContext, ipStr string, chain dnsChain) []string {
	var names []string
	names = append(names, chain.Reverse[ipStr]...)
	if len(names) == 0 {
		// No PTR: use forward nodes that resolve to this IP.
		var keys []string
		for name := range chain.Forward {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			for _, a := range chain.Forward[name] {
				if a == ipStr {
					names = append(names, name)
					break
				}
			}
		}
	}
	if len(names) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString("🌐 **" + ctx.Tlocal("DNS 解析记录", "DNS records") + "** · " + plugin.Code(ipStr) + "\n")

	var rows []string
	for _, n := range names {
		rows = append(rows, ipStr+"\t"+n)
	}
	seenIP := map[string]bool{ipStr: true}
	for _, n := range names {
		for _, a := range chain.Forward[n] {
			if seenIP[a] {
				continue
			}
			seenIP[a] = true
			rows = append(rows, a+"\t"+n)
		}
	}
	var lines []string
	lines = append(lines, plugin.Pre(strings.Join(rows, "\n")))
	if len(chain.AuthNS) > 0 {
		lines = append(lines, row("🖥", ctx.Tlocal("权威 NS", "Authoritative NS"), plugin.Code(strings.Join(chain.AuthNS, " "))))
	}
	return packPages(b.String(), lines)
}
