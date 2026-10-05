package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// renderChat turns a Report into chunked Markdown messages. note is the
// cache annotation line ("" when fresh).
func renderChat(tl func(string, string) string, r *Report, note string) []string {
	var b strings.Builder
	if r.Period == "all" {
		b.WriteString("📊 **" + tl("时段报告", "Chat report") + "** · " + plugin.Bold(r.Title) + "\n")
	} else {
		b.WriteString("📊 **" + tl("年度报告", "Year in review") + " " + plugin.Code(r.Period) + "** · " + plugin.Bold(r.Title) + "\n")
	}
	if r.Messages == 0 {
		b.WriteString("\n> " + tl("这个时间段里没有找到消息", "No messages found in this period"))
		if note != "" {
			b.WriteString("\n\n" + note)
		}
		return []string{b.String()}
	}
	avg := ""
	if r.Days > 0 {
		avg = " · " + fmt.Sprintf(tl("日均 %d 条", "%d/day"), r.Messages/r.Days)
	}
	b.WriteString("\n**" + tl("活跃度", "Activity") + "**\n")
	b.WriteString("> " + fmt.Sprintf(tl("共 %d 条消息 · %d 人发言", "%d messages from %d talkers"), r.Messages, r.Senders) + avg + "\n")
	b.WriteString("> " + fmt.Sprintf(tl("媒体 %d 条 · 文字 %d 条", "%d media · %d text"), r.Media, r.Messages-r.Media) + "\n")
	if r.First > 0 {
		b.WriteString("> " + tl("首条 ", "first ") + ts(r.First) + " · " + tl("最近 ", "last ") + ts(r.Last) + "\n")
	}

	if len(r.Top) > 0 {
		b.WriteString("\n**" + tl("发言排行", "Top talkers") + "**\n")
		for i, s := range r.Top {
			b.WriteString(fmt.Sprintf("%s %s · %s\n", medal(i+1), displayName(r, s.ID), plugin.Code(strconv.Itoa(s.N))))
		}
	}

	b.WriteString("\n**" + tl("活跃时段", "By hour") + "**\n")
	b.WriteString(hourHistogram(tl, r))
	b.WriteString("\n" + weekdayLine(tl, r))

	if len(r.MediaMix) > 0 {
		b.WriteString("\n**" + tl("媒体构成", "Media mix") + "**\n")
		mediaLines(tl, r, &b)
	}

	if note != "" {
		b.WriteString("\n" + note)
	}
	b.WriteString("\n" + tl("数据来自 messages.getHistory 回溯", "Data from walking chat history"))
	return splitMessage(b.String())
}

func mediaLines(tl func(string, string) string, r *Report, b *strings.Builder) {
	kindNames := map[string][2]string{
		"photo":    {"图片", "photos"},
		"video":    {"视频", "videos"},
		"gif":      {"GIF", "GIFs"},
		"sticker":  {"贴纸", "stickers"},
		"voice":    {"语音", "voice notes"},
		"audio":    {"音乐", "audio files"},
		"document": {"文件", "documents"},
		"other":    {"其他", "other"},
	}
	keys := make([]string, 0, len(r.MediaMix))
	for k := range r.MediaMix {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return r.MediaMix[keys[i]] > r.MediaMix[keys[j]] })
	total := 0
	for _, v := range r.MediaMix {
		total += v
	}
	for i, k := range keys {
		if i >= 6 {
			break
		}
		n := kindNames[k]
		if n[0] == "" {
			n = [2]string{k, k}
		}
		pct := 100
		if total > 0 {
			pct = r.MediaMix[k] * 100 / total
		}
		b.WriteString("> " + tl(n[0], n[1]) + " " + plugin.Code(strconv.Itoa(r.MediaMix[k])) +
			" · " + strconv.Itoa(pct) + "%\n")
	}
}

// hourHistogram renders a compact 24-bucket histogram, 2 hours per line,
// each line showing both hours' counts.
func hourHistogram(tl func(string, string) string, r *Report) string {
	max := 0
	for _, n := range r.Hours {
		if n > max {
			max = n
		}
	}
	var b strings.Builder
	for h := 0; h < 24; h += 2 {
		n1, n2 := r.Hours[h], r.Hours[h+1]
		_ = n2
		b.WriteString("> " + fmt.Sprintf("%02d–%02d", h, h+2) + " " + hourBar(n1, max) + "\n")
	}
	peak := 0
	peakN := 0
	for h, n := range r.Hours {
		if n > peakN {
			peak, peakN = h, n
		}
	}
	if peakN > 0 {
		b.WriteString("> " + tl("高峰时段 ", "peak hour ") + plugin.Code(fmt.Sprintf("%02d:00", peak)) +
			" · " + fmt.Sprintf(tl("%d 条", "%d msgs"), peakN) + "\n")
	}
	return b.String()
}

// weekdayLine renders the busiest weekdays.
func weekdayLine(tl func(string, string) string, r *Report) string {
	wdZh := []string{"日", "一", "二", "三", "四", "五", "六"}
	wdEn := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	peak, peakN := 0, 0
	for d, n := range r.Weekdays {
		if n > peakN {
			peak, peakN = d, n
		}
	}
	if peakN == 0 {
		return ""
	}
	weekday := fmt.Sprintf(tl("周%s", "%s"), tl(wdZh[peak], wdEn[peak]))
	return "**" + tl("星期分布", "By weekday") + "**\n> " + tl("最活跃 ", "busiest ") + plugin.Bold(weekday) +
		" · " + fmt.Sprintf(tl("%d 条", "%d msgs"), peakN)
}

func activeDays(r *Report) int {
	return r.Days
}

// displayName finds a sender's name, falling back to an id.
func displayName(r *Report, id int64) string {
	if n, ok := r.Names[id]; ok && n != "" {
		return plugin.Escape(n)
	}
	return tlUnknown(id)
}

func tlUnknown(id int64) string {
	return "user " + plugin.Code(strconv.FormatInt(id, 10))
}

func medal(i int) string {
	switch i {
	case 1:
		return "🥇"
	case 2:
		return "🥈"
	case 3:
		return "🥉"
	default:
		return strconv.Itoa(i) + "."
	}
}

func ts(unix int64) string {
	// UTC date, ISO style — dates are illustrative, timezone of the host
	// (time.Local in aggregate) is what binned them.
	return time.Unix(unix, 0).Format("2006-01-02")
}

// cacheNote annotates a cached report with its generation time.
func cacheNote(tl func(string, string) string, generatedAt int64) string {
	if generatedAt <= 0 {
		return ""
	}
	return "♻️ " + tl("缓存于 ", "cached ") + ts(generatedAt) + " · " + tl("refresh 刷新", "refresh to rescan")
}

// splitMessage cuts a rendered report into telegram-sized chunks on line
// boundaries, with a (x/y) part marker when split.
func splitMessage(s string) []string {
	if len([]rune(s)) <= reportMaxRunes {
		return []string{s}
	}
	runes := []rune(s)
	var parts []string
	for len(runes) > reportMaxRunes {
		cut := -1
		for i := reportMaxRunes; i > reportMaxRunes/2; i-- {
			if runes[i] == '\n' {
				cut = i + 1
				break
			}
		}
		if cut <= 0 {
			cut = reportMaxRunes
		}
		parts = append(parts, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		parts = append(parts, string(runes))
	}
	for i, p := range parts {
		marker := fmt.Sprintf("\n(%d/%d)", i+1, len(parts))
		parts[i] = strings.TrimRight(p, "\n") + marker
	}
	return parts
}
