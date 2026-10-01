package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// unitConvert renders bytes (isBytes) or bytes/s as bits/s, base 1000,
// two decimals — same as the reference.
func unitConvert(v float64, isBytes bool) string {
	units := []string{"bps", "Kbps", "Mbps", "Gbps", "Tbps"}
	if isBytes {
		units = []string{"B", "KB", "MB", "GB", "TB"}
	} else {
		v *= 8
	}
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	return trimFloat(math.Round(v*100)/100) + units[i]
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

// flagEmoji converts an ISO country code into its regional-indicator flag.
func flagEmoji(cc string) string {
	cc = strings.ToUpper(strings.TrimSpace(cc))
	if len(cc) != 2 || cc[0] < 'A' || cc[0] > 'Z' || cc[1] < 'A' || cc[1] > 'Z' {
		return ""
	}
	return string(rune(0x1F1E6+int(cc[0]-'A'))) + string(rune(0x1F1E6+int(cc[1]-'A')))
}

// formatTimestamp turns 2024-01-02T03:04:05Z (or python's .ffffffZ) into
// "2024-01-02 03:04:05".
func formatTimestamp(ts string) string {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999Z", time.RFC3339} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.UTC().Format("2006-01-02 15:04:05")
		}
	}
	s := strings.Replace(ts, "T", " ", 1)
	s, _, _ = strings.Cut(s, ".")
	return strings.TrimSuffix(s, "Z")
}

type ipInfo struct {
	asInfo, ccCode, ccFlag string
}

type extraInfo struct {
	ip           ipInfo
	rx, tx       float64
	mtu          int
	haveTraffic  bool
	source       string
	uploadFailed bool
}

// formatResult builds the result card (Markdown).
func formatResult(lang string, r *Result, ex extraInfo) string {
	tl := func(zh, en string) string {
		if lang == "en-US" {
			return en
		}
		return zh
	}
	var b strings.Builder
	title := "⚡️ **SPEEDTEST by OOKLA**"
	if ex.ip.ccCode != "" {
		title += " " + plugin.Escape("@"+ex.ip.ccCode+ex.ip.ccFlag)
	}
	b.WriteString(title + "\n\n")

	name := plugin.Code(orDash(r.ISP))
	if ex.ip.asInfo != "" {
		name += " " + plugin.Escape(ex.ip.asInfo)
	}
	b.WriteString("Name  " + name + "\n")
	b.WriteString("Node  " + plugin.Code(fmt.Sprint(r.ServerID)) + " - " + plugin.Code(orDash(r.ServerName)) + " - " + plugin.Code(orDash(r.ServerLoc)) + "\n")

	ipv := "IPv4"
	if strings.Contains(r.ExternalIP, ":") {
		ipv = "IPv6"
	}
	conn := plugin.Code(ipv)
	if r.Interface != "" {
		conn += " - " + plugin.Code(r.Interface)
	}
	if ex.mtu > 0 {
		conn += " - MTU " + plugin.Code(fmt.Sprint(ex.mtu))
	}
	b.WriteString("Conn  " + conn + "\n")

	ping := plugin.Code("⇔" + trimFloat(r.Latency) + "ms")
	if r.Jitter > 0 {
		ping += " " + plugin.Code("±"+trimFloat(r.Jitter)+"ms")
	}
	b.WriteString("Ping  " + ping + "\n")
	if r.PacketLoss >= 0 {
		b.WriteString("Loss  " + plugin.Code(trimFloat(r.PacketLoss)+"%") + "\n")
	}

	up, upData := "FAILED", "FAILED"
	if !r.UploadFailed {
		up, upData = unitConvert(r.UpBps, false), unitConvert(r.UpBytes, true)
	}
	b.WriteString("Rate  " + plugin.Code("↓"+unitConvert(r.DownBps, false)) + " " + plugin.Code("↑"+up) + "\n")
	if r.DownBytes > 0 || r.UpBytes > 0 {
		b.WriteString("Data  " + plugin.Code("↓"+unitConvert(r.DownBytes, true)) + " " + plugin.Code("↑"+upData) + "\n")
	}
	if ex.haveTraffic {
		b.WriteString("Stat  " + plugin.Code("RX "+unitConvert(ex.rx, true)) + " " + plugin.Code("TX "+unitConvert(ex.tx, true)) + "\n")
	}
	if r.Timestamp != "" {
		b.WriteString("Time  " + plugin.Code(formatTimestamp(r.Timestamp)) + "\n")
	}
	if r.UploadFailed {
		b.WriteString("Note  " + plugin.Code(tl("上传测试失败，可能是网络环境限制", "Upload test failed, possibly a network restriction")) + "\n")
	}
	if r.ResultURL != "" {
		b.WriteString("Link  " + plugin.Link(tl("结果页面", "Result"), r.ResultURL) + "\n")
	}
	if ex.source == "system" {
		b.WriteString("\n💡 " + tl("使用系统 speedtest", "Using system speedtest"))
	}
	return strings.TrimRight(b.String(), "\n")
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
