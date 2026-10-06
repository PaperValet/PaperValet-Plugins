package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// formatSize renders a byte count with binary units.
func formatSize(size float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	if size < 0 {
		size = 0
	}
	level := 0
	for size >= 1024 && level < len(units)-1 {
		size /= 1024
		level++
	}
	return fmt.Sprintf("%.2f %s", size, units[level])
}

// formatBytes is formatSize for int64 inputs.
func formatBytes(n int64) string { return formatSize(float64(n)) }

// formatRemaining renders seconds as 02d天HH小时MM分SS秒 / 02dd HHh MMm SSs.
func formatRemaining(tl func(zh, en string) string, seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	d := seconds / 86400
	h := (seconds % 86400) / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	if tlIsEn(tl) {
		return fmt.Sprintf("%02dd %02dh %02dm %02ds", d, h, m, s)
	}
	return fmt.Sprintf("%02d天%02d小时%02d分%02d秒", d, h, m, s)
}

// tlIsEn reports whether the locale function resolves English. Tlocal picks
// by ctx.Lang; tests emulate it with a stub.
func tlIsEn(tl func(zh, en string) string) bool {
	return tl("zh", "en") == "en"
}

// progressBar renders a 20-block bar with the usage percent and a health
// emoji (🟢<30 🟡<70 🟠<90 🔴 otherwise), mirroring the source.
func progressBar(percent float64, tl func(zh, en string) string) string {
	filled := int(math.Round(percent / 5))
	if filled > 20 {
		filled = 20
	}
	if filled < 0 {
		filled = 0
	}
	emoji := "🟢"
	switch {
	case percent >= 90:
		emoji = "🔴"
	case percent >= 70:
		emoji = "🟠"
	case percent >= 30:
		emoji = "🟡"
	}
	word := map[string][2]string{
		"🟢": {"良好", "good"}, "🟡": {"正常", "normal"}, "🟠": {"偏高", "high"}, "🔴": {"警告", "critical"},
	}[emoji]
	w := word[0]
	if tlIsEn(tl) {
		w = word[1]
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)
	return fmt.Sprintf("%s %s %.2f%% %s", bar, emoji, percent, w)
}

// subCycle classifies the subscription cycle from the expiry timestamp.
type subCycle struct {
	isLongTerm  bool
	isSingle    bool
	resetInfo   string // zh; en built in render
	daysToReset int64
}

// getSubCycle ports the TS getSubType: expire far beyond 3 years counts as
// long-term; under 45 days counts as a single (non-renewing) subscription;
// otherwise the quota resets monthly on the expiry day-of-month.
func getSubCycle(expireTs int64, now time.Time) subCycle {
	if expireTs == 0 {
		return subCycle{isSingle: true, resetInfo: "未知或永久"}
	}
	secsToExpire := expireTs - now.Unix()
	daysToExpire := secsToExpire / 86400
	if secsToExpire < 0 {
		daysToExpire = 0
	}
	isLongTerm := secsToExpire > 3*365*86400
	if daysToExpire < 45 && !isLongTerm {
		return subCycle{isSingle: true, daysToReset: daysToExpire}
	}
	resetDay := time.Unix(expireTs, 0).Day()
	// next reset: this month's resetDay at 00:00, else next month's
	nextReset := time.Date(now.Year(), now.Month(), resetDay, 0, 0, 0, 0, now.Location())
	if !nextReset.After(now) {
		nextReset = time.Date(now.Year(), now.Month()+1, resetDay, 0, 0, 0, 0, now.Location())
	}
	days := (nextReset.Unix() - now.Unix()) / 86400
	if days < 1 {
		days = 1
	}
	return subCycle{
		isLongTerm:  isLongTerm,
		resetInfo:   fmt.Sprintf("每月%d日", resetDay),
		daysToReset: days,
	}
}

// splitLongMessage cuts text into chunks of at most max runes on line
// boundaries (fallback: hard cut).
func splitLongMessage(text string, maxRunes int) []string {
	if len([]rune(text)) <= maxRunes {
		return []string{text}
	}
	var parts []string
	var cur []rune
	for _, line := range strings.Split(text, "\n") {
		lr := []rune(line)
		need := len(lr) + 1
		if len(cur) > 0 && len(cur)+need > maxRunes {
			parts = append(parts, string(cur))
			cur = lr
		} else {
			if len(cur) > 0 {
				cur = append(cur, '\n')
			}
			cur = append(cur, lr...)
		}
		if len(cur) > maxRunes { // single line longer than max: hard split
			parts = append(parts, string(cur[:maxRunes]))
			cur = cur[maxRunes:]
		}
	}
	if len(cur) > 0 {
		parts = append(parts, string(cur))
	}
	return parts
}

// statsLine renders the trailing summary for multi-link queries.
func statsLine(tl func(zh, en string) string, valid, exhausted, expired, failed int) string {
	return fmt.Sprintf("📈 %s ✅%s:%d | ⚠️%s:%d | ❌%s:%d | ❓%s:%d",
		plugin.Bold(tl("统计", "Stats")),
		tl("有效", "valid"), valid,
		tl("耗尽", "exhausted"), exhausted,
		tl("过期", "expired"), expired,
		tl("失败", "failed"), failed)
}

// quote prefixes every line of s with "> " for a Telegram quote block.
func quote(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = "> " + strings.TrimLeft(l, "> ")
	}
	return strings.Join(lines, "\n")
}

// nodeStatsBlock renders the node count/types/regions summary lines.
func nodeStatsBlock(tl func(zh, en string) string, n *nodeInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d\n", tl("数量", "Count"), n.count)
	if len(n.types) > 0 {
		parts := make([]string, len(n.types))
		for i, t := range n.types {
			parts[i] = fmt.Sprintf("%s:%d", t.k, t.v)
		}
		fmt.Fprintf(&b, "%s: %s\n", tl("类型", "Types"), strings.Join(parts, ", "))
	}
	if len(n.regions) > 0 {
		parts := make([]string, len(n.regions))
		for i, r := range n.regions {
			parts[i] = fmt.Sprintf("%s:%d", r.k, r.v)
		}
		fmt.Fprintf(&b, "%s: %s\n", tl("地区分布", "Regions"), strings.Join(parts, ", "))
		// top region share
		top := n.regions[0]
		for _, r := range n.regions[1:] {
			if r.v > top.v {
				top = r
			}
		}
		if n.count > 0 {
			pct := math.Round(float64(top.v)/float64(n.count)*10000) / 100
			fmt.Fprintf(&b, "%s: %s (%.2f%%)\n", tl("主要", "Top"), top.k, pct)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderReport renders the full or brief report for all links.
func renderReport(tl func(zh, en string) string, urls []string, results []*subResult, brief, txt bool) string {
	var reports []string
	var valid, exhausted, expired, failed int
	now := queryTime()

	for i, u := range urls {
		r := results[i]
		if r == nil {
			continue
		}
		if !r.success {
			failed++
			reports = append(reports, renderFailed(tl, u, r, txt))
			continue
		}
		switch r.status {
		case "耗尽":
			exhausted++
		case "过期":
			expired++
		default:
			valid++
		}
		if brief {
			reports = append(reports, renderBrief(tl, u, r, txt, now))
		} else {
			reports = append(reports, renderDetailed(tl, u, r, txt, now))
		}
	}

	sep := "\n\n" + strings.Repeat("=", 30) + "\n\n"
	if txt {
		sep = "\n" + strings.Repeat("=", 40) + "\n"
		if brief {
			sep = "\n" + strings.Repeat("-", 30) + "\n"
		}
	} else if brief {
		sep = "\n\n" + strings.Repeat("=", 30) + "\n\n"
	}
	out := strings.Join(reports, sep)
	if !brief && len(urls) > 1 {
		out += sep + statsLine(tl, valid, exhausted, expired, failed)
	}
	return out
}

// renderFailed renders the block for a link that could not be queried.
func renderFailed(tl func(zh, en string) string, u string, r *subResult, txt bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🧾 %s: %s\n", plugin.Bold(tl("订阅链接", "Link")), codeOrPlain(u, txt))
	switch r.errKind {
	case errNoUserInfo:
		fmt.Fprintf(&b, "%s\n", plugin.Bold(tl("无流量统计信息", "No traffic info")))
	default:
		msg := r.errDetail
		if r.errKind == errUnreachable {
			msg = tl("无法访问", "unreachable") + " (HTTP " + r.errDetail + ")"
		}
		if msg == "" {
			msg = tl("未知错误", "unknown error")
		}
		fmt.Fprintf(&b, "%s %s\n", plugin.Bold(tl("查询失败", "Query failed")), codeOrPlain(msg, txt))
	}
	if r.website.website != "" {
		fmt.Fprintf(&b, "%s %s: %s\n", linkIcon, plugin.Bold(tl("官网链接", "Website")), rawOrLink(r.website.website, txt))
	}
	if r.configName != "" && r.configName != "未知" {
		fmt.Fprintf(&b, "🏢 %s: %s\n", plugin.Bold(tl("机场名称", "Provider")), codeOrPlain(r.configName, txt))
	}
	return strings.TrimRight(b.String(), "\n")
}

const linkIcon = "🔗"

// renderBrief is the compact cha format.
func renderBrief(tl func(zh, en string) string, u string, r *subResult, txt bool, now time.Time) string {
	var b strings.Builder
	statusIcon := map[string]string{"有效": "✅", "耗尽": "⚠️", "过期": "❌"}[r.status]
	fmt.Fprintf(&b, "%s %s: %s\n", statusIcon, plugin.Bold(tl("机场名称", "Provider")), codeOrPlain(r.configName, txt))
	if w := websiteOrProfile(r); w != "" {
		fmt.Fprintf(&b, "🔗 %s: %s\n", plugin.Bold(tl("官网链接", "Website")), rawOrLink(w, txt))
	}
	fmt.Fprintf(&b, "🏷️ %s: %s\n", plugin.Bold(tl("订阅链接", "Link")), codeOrPlain(u, txt))
	fmt.Fprintf(&b, "📊 %s\n", plugin.Bold(tl("流量信息", "Traffic")))
	fmt.Fprintf(&b, "  %s: %s\n", tl("总流量", "Total"), codeOrPlain(formatBytes(r.total), txt))
	fmt.Fprintf(&b, "  %s ↑: %s\n", tl("已用上行", "Upload"), codeOrPlain(formatBytes(r.upload), txt))
	fmt.Fprintf(&b, "  %s ↓: %s\n", tl("已用下行", "Download"), codeOrPlain(formatBytes(r.download), txt))
	fmt.Fprintf(&b, "  %s: %s\n", tl("已用总量", "Used"), codeOrPlain(formatBytes(r.used), txt))
	fmt.Fprintf(&b, "  %s: %s\n", tl("剩余流量", "Remaining"), codeOrPlain(formatBytes(r.remain), txt))
	if r.expireTs > 0 {
		fmt.Fprintf(&b, "⏰ %s: %s", plugin.Bold(tl("到期时间", "Expires")),
			codeOrPlain(time.Unix(r.expireTs, 0).Format("2006-01-02 15:04:05"), txt))
		if left := r.expireTs - now.Unix(); left > 0 {
			fmt.Fprintf(&b, "\n  %s: %s", tl("剩余时间", "Time left"), codeOrPlain(formatRemaining(tl, left), txt))
		} else {
			fmt.Fprintf(&b, " (%s)", tl("已过期", "expired"))
		}
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "⏰ %s: %s\n", plugin.Bold(tl("到期时间", "Expires")), codeOrPlain(tl("未知或永久", "unknown or never"), txt))
	}
	if r.nodes != nil && len(r.nodes.names) > 0 {
		names := escapeNames(r.nodes.names, txt)
		fmt.Fprintf(&b, "\n📋 %s (%d)\n%s", tl("节点列表", "Node list"), len(names), strings.Join(names, "\n"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderDetailed is the full subinfo format.
func renderDetailed(tl func(zh, en string) string, u string, r *subResult, txt bool, now time.Time) string {
	var b strings.Builder
	statusIcon := map[string]string{"有效": "✅", "耗尽": "⚠️", "过期": "❌"}[r.status]
	statusWord := r.status
	if tlIsEn(tl) {
		statusWord = map[string]string{"有效": "active", "耗尽": "exhausted", "过期": "expired"}[r.status]
	}

	fmt.Fprintf(&b, "📄 %s: %s\n", plugin.Bold(tl("机场名称", "Provider")), codeOrPlain(r.configName, txt))
	if w := websiteOrProfile(r); w != "" {
		fmt.Fprintf(&b, "🔗 %s: %s\n", plugin.Bold(tl("官网链接", "Website")), rawOrLink(w, txt))
	}
	fmt.Fprintf(&b, "🏷️ %s: %s\n", plugin.Bold(tl("订阅链接", "Link")), codeOrPlain(u, txt))
	fmt.Fprintf(&b, "⏱️ %s: %s\n", plugin.Bold(tl("查询时间", "Queried at")), codeOrPlain(now.Format("2006-01-02 15:04:05"), txt))
	fmt.Fprintf(&b, "%s %s: %s\n\n", statusIcon, plugin.Bold(tl("状态", "Status")), plugin.Bold(statusWord))

	// traffic block
	fmt.Fprintf(&b, "📊 %s\n", plugin.Bold(tl("流量信息", "Traffic")))
	var tb strings.Builder
	fmt.Fprintf(&tb, "%s: %s\n", tl("总计", "Total"), formatBytes(r.total))
	fmt.Fprintf(&tb, "%s: %s (↑%s ↓%s)\n", tl("已用", "Used"), formatBytes(r.used), formatBytes(r.upload), formatBytes(r.download))
	fmt.Fprintf(&tb, "%s: %s\n", tl("剩余", "Remaining"), formatBytes(r.remain))
	fmt.Fprintf(&tb, "%s: %s", tl("进度", "Progress"), progressBar(r.percent, tl))
	b.WriteString(quote(tb.String()))
	b.WriteString("\n\n")

	// time block
	if r.expireTs > 0 {
		fmt.Fprintf(&b, "⏰ %s\n", plugin.Bold(tl("时间信息", "Time")))
		var t strings.Builder
		fmt.Fprintf(&t, "%s: %s\n", tl("到期", "Expires"), time.Unix(r.expireTs, 0).Format("2006-01-02 15:04:05"))
		if left := r.expireTs - now.Unix(); left > 0 {
			fmt.Fprintf(&t, "%s: %s\n", tl("剩余", "Left"), formatRemaining(tl, left))
		} else {
			fmt.Fprintf(&t, "%s: %s\n", tl("状态", "Status"), tl("已过期", "expired"))
		}
		cycle := getSubCycle(r.expireTs, now)
		cycleWord := cycle.resetInfo
		if tlIsEn(tl) {
			switch {
			case cycle.isLongTerm:
				cycleWord = "long-term"
			case cycle.isSingle:
				cycleWord = "single period"
			default:
				cycleWord = fmt.Sprintf("resets on day %d monthly", time.Unix(r.expireTs, 0).Day())
			}
		} else {
			switch {
			case cycle.isLongTerm:
				cycleWord = "长期有效"
			case cycle.isSingle:
				cycleWord = "单次订阅"
			}
		}
		fmt.Fprintf(&t, "%s: %s\n", tl("周期", "Cycle"), cycleWord)
		if cycle.daysToReset > 0 && !cycle.isLongTerm {
			fmt.Fprintf(&t, "%s: %s\n", tl("下次重置/到期", "Next reset/expiry"), formatRemaining(tl, cycle.daysToReset*86400))
			if r.remain > 0 {
				fmt.Fprintf(&t, "%s: %s/%s\n", tl("建议日均用量", "Suggested daily"), formatBytes(int64(float64(r.remain)/float64(cycle.daysToReset))), tl("天", "day"))
			}
		}
		if r.startTs > 0 && now.Unix() > r.startTs {
			days := float64(now.Unix()-r.startTs) / 86400
			if days < 1 {
				days = 1
			}
			fmt.Fprintf(&t, "%s: %s/%s\n", tl("历史日均", "Historical daily"), formatSize(float64(r.used)/days), tl("天", "day"))
		}
		if r.used > 0 && r.remain > 0 {
			usedDays := float64(now.Unix()-r.startTs) / 86400
			if usedDays < 1 {
				usedDays = 1
			}
			daily := float64(r.used) / usedDays
			if daily > 0 {
				depleteDays := int64(math.Floor(float64(r.remain) / daily))
				fmt.Fprintf(&t, "%s: %s\n", tl("预计耗尽日期", "Est. depletion"), now.AddDate(0, 0, int(depleteDays)).Format("2006-01-02"))
			}
			fmt.Fprintf(&t, "%s: ↑%.2f%% ↓%.2f%%", tl("上下行比例", "Up/Down ratio"),
				float64(r.upload)/float64(r.used)*100, float64(r.download)/float64(r.used)*100)
		}
		b.WriteString(quote(strings.TrimRight(t.String(), "\n")))
		b.WriteString("\n\n")
	}

	// nodes block
	fmt.Fprintf(&b, "🌐 %s\n", plugin.Bold(tl("节点信息", "Nodes")))
	if r.nodes != nil {
		b.WriteString(quote(nodeStatsBlock(tl, r.nodes)))
		if len(r.nodes.names) > 0 {
			fmt.Fprintf(&b, "\n\n📋 %s (%d)\n%s", tl("节点列表", "Node list"), len(r.nodes.names),
				strings.Join(escapeNames(r.nodes.names, txt), "\n"))
		}
	} else {
		fmt.Fprintf(&b, "(%s)", tl("未能解析节点列表", "node list unavailable"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// websiteOrProfile prefers the panel's profile-web-page-url, falling back
// to the resolved site base.
func websiteOrProfile(r *subResult) string {
	if r.profileURL != "" {
		return r.profileURL
	}
	return r.website.website
}

// escapeNames escapes each node name for markdown output; txt output keeps
// them verbatim (the file is plain text).
func escapeNames(names []string, txt bool) []string {
	out := make([]string, len(names))
	for i, n := range names {
		if txt {
			out[i] = n
		} else {
			out[i] = plugin.Escape(n)
		}
	}
	return out
}

// codeOrPlain wraps in inline code for message output; txt keeps it bare.
func codeOrPlain(s string, txt bool) string {
	if txt {
		return s
	}
	return plugin.Code(s)
}

// rawOrLink renders a URL as a proper markdown link (message) or bare (txt).
func rawOrLink(u string, txt bool) string {
	if txt {
		return u
	}
	return plugin.Link(u, u)
}
