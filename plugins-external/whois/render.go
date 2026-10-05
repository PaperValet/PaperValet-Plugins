package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// WhoisRecord is one domain lookup result, persisted in history and cache.
type WhoisRecord struct {
	Domain      string   `json:"domain"`
	Registrar   string   `json:"registrar,omitempty"`
	CreatedDate string   `json:"createdDate,omitempty"`
	ExpiryDate  string   `json:"expiryDate,omitempty"`
	UpdatedDate string   `json:"updatedDate,omitempty"`
	Status      string   `json:"status,omitempty"`
	NameServers []string `json:"nameServers,omitempty"`
	RawData     string   `json:"rawData,omitempty"`
	QueryTime   string   `json:"queryTime"`
}

const (
	timeMinute = time.Minute
	timeHour   = time.Hour
)

// tsTime aliases time.Time for the renderer helpers' signatures.
type tsTime = time.Time

// quoteBlock renders s as a Telegram quote block (the source used an
// expandable blockquote; PaperValet's markdown has none, so a plain `>`
// quote carries the raw data).
func quoteBlock(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// relativeTime renders a coarse "x minutes ago"-style suffix like the
// source's dayjs.fromNow() cache stamp.
func relativeTime(tl func(zh, en string) string, from, now tsTime) string {
	d := now.Sub(from)
	switch {
	case d < timeMinute:
		return tl("刚刚", "just now")
	case d < timeHour:
		return fmt.Sprintf(tl("%d 分钟前", "%d minutes ago"), int(d/timeMinute))
	case d < 24*timeHour:
		return fmt.Sprintf(tl("%d 小时前", "%d hours ago"), int(d/timeHour))
	case d < 30*24*timeHour:
		return fmt.Sprintf(tl("%d 天前", "%d days ago"), int(d/(24*timeHour)))
	case d < 365*24*timeHour:
		return fmt.Sprintf(tl("%d 个月前", "%d months ago"), int(d/(30*24*timeHour)))
	default:
		return fmt.Sprintf(tl("%d 年前", "%d years ago"), int(d/(365*24*timeHour)))
	}
}

// expiryNote renders the source's expiry grading after the expiry date:
// expired ⚠️ / <30 days ⚠️ N days / <90 days italic note; "" when none.
func expiryNote(tl func(zh, en string) string, days int) string {
	switch {
	case days < 0:
		return " " + plugin.Bold("⚠️ "+tl("已过期", "expired"))
	case days < 30:
		return fmt.Sprintf(" "+plugin.Bold("⚠️ "+tl("%d 天后过期", "expires in %d days")), days)
	case days < 90:
		return fmt.Sprintf(" "+plugin.Italic(tl("（%d 天后过期）", "(expires in %d days)")), days)
	}
	return ""
}

// expiryDays computes the days-until-expiry grade for a record; ok is
// false when the date cannot be parsed (the source skips the note then).
func expiryDays(rec WhoisRecord, now tsTime) (int, bool) {
	if rec.ExpiryDate == "" {
		return 0, false
	}
	t, ok := parseWhoisDate(rec.ExpiryDate)
	if !ok {
		return 0, false
	}
	return int(t.Sub(now) / (24 * timeHour)), true
}

// renderResult renders the ✅ result card. fromCache adds the "(cache:
// x ago)" stamp like the source.
func renderResult(tl func(zh, en string) string, rec WhoisRecord, fromCache bool) string {
	var b strings.Builder
	b.WriteString("✅ " + plugin.Bold(tl("WHOIS 查询结果", "WHOIS lookup result")))
	if fromCache {
		if qt, ok := parseQueryTime(rec.QueryTime); ok {
			b.WriteString(" " + plugin.Italic("("+tl("缓存: ", "cache: ")+relativeTime(tl, qt, nowFunc())+")"))
		}
	}
	b.WriteString("\n\n🌐 " + plugin.Bold(tl("域名", "Domain")+": ") + plugin.Code(rec.Domain) + "\n\n")
	if rec.Registrar != "" {
		b.WriteString("📋 " + plugin.Bold(tl("注册商", "Registrar")+": ") + plugin.Escape(rec.Registrar) + "\n")
	}
	if rec.CreatedDate != "" {
		b.WriteString("📅 " + plugin.Bold(tl("注册日期", "Created")+": ") + plugin.Escape(rec.CreatedDate) + "\n")
	}
	if rec.ExpiryDate != "" {
		b.WriteString("⏰ " + plugin.Bold(tl("过期日期", "Expiry")+": ") + plugin.Escape(rec.ExpiryDate))
		if days, ok := expiryDays(rec, nowFunc()); ok {
			b.WriteString(expiryNote(tl, days))
		}
		b.WriteString("\n")
	}
	if rec.UpdatedDate != "" {
		b.WriteString("🔄 " + plugin.Bold(tl("更新日期", "Updated")+": ") + plugin.Escape(rec.UpdatedDate) + "\n")
	}
	if rec.Status != "" {
		b.WriteString("📊 " + plugin.Bold(tl("域名状态", "Status")+": ") + plugin.Escape(rec.Status) + "\n")
	}
	if len(rec.NameServers) > 0 {
		b.WriteString("\n🖥️ " + plugin.Bold(tl("DNS 服务器", "Name servers")) + "\n")
		for _, ns := range rec.NameServers[:min(5, len(rec.NameServers))] {
			b.WriteString("• " + plugin.Code(ns) + "\n")
		}
	}
	if rec.RawData != "" {
		b.WriteString("\n📄 " + plugin.Bold(tl("原始 WHOIS 数据", "Raw WHOIS data")) + "\n")
		raw := truncateRunes(rec.RawData, rawMaxRunes)
		b.WriteString(quoteBlock(plugin.Escape(raw)))
		if runeLen(rec.RawData) > rawMaxRunes {
			b.WriteString("\n\n" + plugin.Italic(tl("（数据已截断，仅显示前 3000 字符）", "(truncated, first 3000 characters shown)")))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// batchLine is one row of a batch summary.
type batchLine struct {
	domain string
	ok     bool
	cached bool
}

// renderBatchDone renders the batch summary: per-domain lines plus the
// success/failure tally.
func renderBatchDone(tl func(zh, en string) string, results []batchLine, success, fail int) string {
	var b strings.Builder
	b.WriteString("📊 " + plugin.Bold(tl("批量查询完成", "Batch query finished")) + "\n\n")
	fmt.Fprintf(&b, "%s %d\n", plugin.Bold(tl("成功", "Succeeded")), success)
	fmt.Fprintf(&b, "%s %d\n\n", plugin.Bold(tl("失败", "Failed")), fail)
	b.WriteString(plugin.Bold(tl("查询结果", "Results")) + "\n")
	for _, r := range results {
		line := "✅ " + plugin.Code(r.domain)
		if r.cached {
			line += " - " + plugin.Italic(tl("缓存", "cached"))
		} else if !r.ok {
			line = "❌ " + plugin.Code(r.domain) + " - " + tl("查询失败", "query failed")
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n💡 " + tl("使用 ", "Use ") + plugin.Code("whois history") + tl(" 查看详细信息", " for details"))
	return strings.TrimRight(b.String(), "\n")
}

// renderHistory renders the last 20 lookups plus the stats block.
func renderHistory(tl func(zh, en string) string, history []WhoisRecord, cacheCount, cacheHours int) string {
	if len(history) == 0 {
		return "📭 " + plugin.Bold(tl("暂无查询历史", "No query history")) + "\n\n💡 " +
			tl("使用 ", "Use ") + plugin.Code("whois <"+tl("域名", "domain")+">") + tl(" 开始查询", " to start")
	}
	var b strings.Builder
	n := min(20, len(history))
	fmt.Fprintf(&b, "📜 %s %s\n\n", plugin.Bold(tl("查询历史", "Query history")),
		plugin.Italic(tl(fmt.Sprintf("（最近 %d 条）", n), fmt.Sprintf("(last %d)", n))))
	for i, rec := range history[:n] {
		stamp := rec.QueryTime
		if qt, ok := parseQueryTime(rec.QueryTime); ok {
			stamp = fmt.Sprintf("%s (%s)", qt.Local().Format("01-02 15:04"), relativeTime(tl, qt, nowFunc()))
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, plugin.Code(rec.Domain), plugin.Italic(stamp))
		if days, ok := expiryDays(rec, nowFunc()); ok {
			switch {
			case days < 0:
				fmt.Fprintf(&b, "   ⚠️ %s\n", plugin.Bold(tl("已过期", "expired")))
			case days < 30:
				fmt.Fprintf(&b, "   ⚠️ %s\n", plugin.Bold(fmt.Sprintf(tl("%d 天后过期", "expires in %d days"), days)))
			}
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%s\n", plugin.Bold(tl("统计信息", "Stats")))
	fmt.Fprintf(&b, "• %s %d\n", tl("总查询次数:", "Total queries:"), len(history))
	fmt.Fprintf(&b, "• %s %d\n", tl("缓存域名数:", "Cached domains:"), cacheCount)
	fmt.Fprintf(&b, "• %s %d %s\n\n", tl("缓存时长:", "Cache duration:"), cacheHours, tl("小时", "hours"))
	b.WriteString("💡 " + tl("使用 ", "Use ") + plugin.Code("whois clear") + tl(" 清除历史记录", " to clear history"))
	return strings.TrimRight(b.String(), "\n")
}

// renderCleared renders the clear confirmation.
func renderCleared(tl func(zh, en string) string, historyCount, cacheCount int) string {
	return "🗑️ " + plugin.Bold(tl("清除完成", "Cleared")) + "\n\n" +
		fmt.Sprintf("• %s %d %s\n", tl("清除历史记录:", "History cleared:"), historyCount, tl("条", "entries")) +
		fmt.Sprintf("• %s %d %s", tl("清除缓存:", "Cache cleared:"), cacheCount, tl("个域名", "domains"))
}

// renderEmptyClear renders the "nothing to clear" reply.
func renderEmptyClear(tl func(zh, en string) string) string {
	return "📭 " + plugin.Bold(tl("没有需要清除的记录", "Nothing to clear"))
}

// truncateRunes cuts s to at most max runes.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// runeLen counts runes in s.
func runeLen(s string) int { return len([]rune(s)) }
