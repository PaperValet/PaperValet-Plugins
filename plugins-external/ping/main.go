package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
	"github.com/gotd/td/tg"
)

// Telegram data centers (same table as TeleBox / PagerMaid-Modify).
var dcs = map[int]string{
	1: "149.154.175.53",  // DC1 Miami
	2: "149.154.167.51",  // DC2 Amsterdam
	3: "149.154.175.100", // DC3 Miami
	4: "149.154.167.91",  // DC4 Amsterdam
	5: "91.108.56.130",   // DC5 Singapore
}

var dcLocations = map[int]string{1: "Miami", 2: "Amsterdam", 3: "Miami", 4: "Amsterdam", 5: "Singapore"}

type PingPlugin struct{}

func New() (plugin.Plugin, error) {
	return &PingPlugin{}, nil
}

var Metadata = &plugin.PluginMetadata{
	Name:        "ping",
	Description: "网络延迟测试工具",
	DescEN:      "Network latency tool",
	Version:     "1.1.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

func (p *PingPlugin) Name() string        { return "ping" }
func (p *PingPlugin) Description() string { return "网络延迟测试工具" }
func (p *PingPlugin) DescEN() string      { return "Network latency tool" }

func (p *PingPlugin) Start(ctx context.Context) error { return nil }
func (p *PingPlugin) Stop(ctx context.Context) error  { return nil }

func (p *PingPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "ping",
		Aliases:     []string{},
		Description: "网络延迟测试（Telegram API / 数据中心 / IP / 域名）",
		DescEN:      "Latency test (Telegram API / DCs / IP / domain)",
		Usage:       "ping [all|dc1-dc5|<IP/域名>|help]",
		UsageEN:     "ping [all|dc1-dc5|<IP/domain>|help]",
		Plugin:      p.Name(),
		Category:    "tools",
		Handler:     p.handlePing,
	})
}

func now() string { return time.Now().Format("2006-01-02 15:04:05") }

func (p *PingPlugin) handlePing(ctx *plugin.CommandContext) error {
	target := strings.ToLower(strings.TrimSpace(ctx.GetArg(0)))

	switch {
	case target == "":
		return p.pingTelegram(ctx)
	case target == "help" || target == "h":
		return ctx.Edit(helpText(ctx))
	case target == "all" || target == "dc":
		return p.pingAllDCs(ctx)
	}
	if n, ok := dcNumber(target); ok {
		return p.pingDC(ctx, n)
	}
	return p.pingTarget(ctx, target)
}

func helpText(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🏓 **Ping 工具**\n\n"+
			"**基础用法**\n"+
			"• `ping` — Telegram API / 消息延迟\n"+
			"• `ping all` — 所有数据中心延迟\n"+
			"• `ping dc1` — 指定数据中心（dc1-dc5）\n\n"+
			"**网络测试**\n"+
			"• `ping 8.8.8.8` — IP 延迟\n"+
			"• `ping google.com` — 域名延迟\n\n"+
			"**数据中心**\n"+
			"• DC1/DC3: Miami\n• DC2/DC4: Amsterdam\n• DC5: Singapore\n\n"+
			"💡 TCP 优先（443/80/22/53），失败回退 ICMP 再回退 HTTP；设置 ALL\\_PROXY/HTTPS\\_PROXY/HTTP\\_PROXY 时 TCP 测试经代理",
		"🏓 **Ping tool**\n\n"+
			"**Basic**\n"+
			"• `ping` — Telegram API / message latency\n"+
			"• `ping all` — all data centers\n"+
			"• `ping dc1` — one data center (dc1-dc5)\n\n"+
			"**Network**\n"+
			"• `ping 8.8.8.8` — IP latency\n"+
			"• `ping google.com` — domain latency\n\n"+
			"**Data centers**\n"+
			"• DC1/DC3: Miami\n• DC2/DC4: Amsterdam\n• DC5: Singapore\n\n"+
			"💡 TCP first (443/80/22/53), falls back to ICMP then HTTP; TCP goes through ALL\\_PROXY/HTTPS\\_PROXY/HTTP\\_PROXY when set")
}

// pingTelegram measures a raw API round trip (users.getUsers self) and the
// latency of editing the command message.
func (p *PingPlugin) pingTelegram(ctx *plugin.CommandContext) error {
	apiMs := int64(-1)
	if ctx.API != nil {
		c, cancel := context.WithTimeout(ctx.Context(), 15*time.Second)
		start := time.Now()
		_, err := ctx.API.UsersGetUsers(c, []tg.InputUserClass{&tg.InputUserSelf{}})
		cancel()
		if err == nil {
			apiMs = time.Since(start).Milliseconds()
		}
	}

	start := time.Now()
	if err := ctx.Edit("🏓 Pong!"); err != nil {
		return err
	}
	msgMs := time.Since(start).Milliseconds()

	api := ctx.Tlocal("失败", "failed")
	if apiMs >= 0 {
		api = fmt.Sprintf("%dms", apiMs)
	}
	return ctx.Edit("🏓 **Pong!**\n\n" +
		ctx.Tlocal("📡 API延迟  ", "📡 API latency  ") + plugin.Code(api) + "\n" +
		ctx.Tlocal("✏️ 消息延迟  ", "✏️ Edit latency  ") + plugin.Code(fmt.Sprintf("%dms", msgMs)) + "\n\n" +
		"⏰ " + plugin.Italic(now()))
}

func dcNumber(s string) (int, bool) {
	s = strings.ToLower(s)
	if len(s) != 3 || !strings.HasPrefix(s, "dc") {
		return 0, false
	}
	n, err := strconv.Atoi(s[2:])
	if err != nil || n < 1 || n > 5 {
		return 0, false
	}
	return n, true
}

// dcLatency probes one DC: TCP 443/80 (2 samples), then a single ICMP ping.
func dcLatency(ctx context.Context, dc int) (int, bool) {
	ip := dcs[dc]
	if r := tcpingProbe(ctx, ip, []int{443, 80}, 2, 3*time.Second); r != nil {
		return r.avg, true
	}
	if ms, ok := icmpOnce(ctx, ip); ok {
		return ms, true
	}
	return 0, false
}

func dcLine(ctx *plugin.CommandContext, dc, ms int, ok bool) string {
	label := fmt.Sprintf("🌐 DC%d (%s)  ", dc, dcLocations[dc])
	if !ok {
		return label + plugin.Code(ctx.Tlocal("超时", "timeout"))
	}
	return label + plugin.Code(fmtMs(ms))
}

func (p *PingPlugin) pingAllDCs(ctx *plugin.CommandContext) error {
	_ = ctx.Edit(ctx.Tlocal("🔍 正在测试所有数据中心延迟...", "🔍 Testing all data centers..."))

	type res struct {
		ms int
		ok bool
	}
	results := make([]res, 6)
	var wg sync.WaitGroup
	for dc := 1; dc <= 5; dc++ {
		wg.Add(1)
		go func(dc int) {
			defer wg.Done()
			ms, ok := dcLatency(ctx.Context(), dc)
			results[dc] = res{ms, ok}
		}(dc)
	}
	wg.Wait()

	lines := make([]string, 0, 5)
	for dc := 1; dc <= 5; dc++ {
		lines = append(lines, dcLine(ctx, dc, results[dc].ms, results[dc].ok))
	}
	return ctx.Edit(ctx.Tlocal("🌐 **Telegram 数据中心延迟**", "🌐 **Telegram data center latency**") +
		"\n\n" + strings.Join(lines, "\n") + proxyNote(ctx) + "\n\n⏰ " + plugin.Italic(now()))
}

func (p *PingPlugin) pingDC(ctx *plugin.CommandContext, dc int) error {
	_ = ctx.Edit(fmt.Sprintf(ctx.Tlocal("🔍 正在测试 DC%d (%s)...", "🔍 Testing DC%d (%s)..."), dc, dcLocations[dc]))
	ms, ok := dcLatency(ctx.Context(), dc)
	return ctx.Edit(ctx.Tlocal("🎯 **数据中心延迟测试**", "🎯 **Data center latency**") + "\n" +
		plugin.Code(fmt.Sprintf("dc%d", dc)) + " → " + plugin.Code(dcs[dc]) + "\n\n" +
		dcLine(ctx, dc, ms, ok) + proxyNote(ctx) + "\n\n⏰ " + plugin.Italic(now()))
}

func proxyNote(ctx *plugin.CommandContext) string {
	if px := resolveProxy(); px != nil {
		return "\n\n💡 " + ctx.Tlocal("TCP 测试经代理 ", "TCP via proxy ") + plugin.Code(px.display())
	}
	return ""
}

func fmtMs(ms int) string {
	if ms <= 0 {
		return "<1ms"
	}
	return fmt.Sprintf("%dms", ms)
}

func (p *PingPlugin) pingTarget(ctx *plugin.CommandContext, target string) error {
	parsed := parseTarget(target)
	if parsed.typ == "invalid" {
		return ctx.Edit("❌ " + ctx.Tlocal("无效的目标: ", "Invalid target: ") + plugin.Code(target))
	}
	_ = ctx.Edit(ctx.Tlocal("🔍 正在测试 ", "🔍 Testing ") + plugin.Code(target) + "...")

	base := ctx.Context()
	host := parsed.value // hostname kept for HTTP(S) (Host/SNI)
	addr := host         // resolved address for TCP / ICMP

	var lines []string

	// DNS
	if parsed.typ == "domain" {
		c, cancel := context.WithTimeout(base, 5*time.Second)
		start := time.Now()
		ips, err := net.DefaultResolver.LookupIPAddr(c, host)
		cancel()
		dnsMs := int(time.Since(start).Milliseconds())
		if err != nil || len(ips) == 0 {
			lines = append(lines, ctx.Tlocal("🔍 DNS解析  ", "🔍 DNS  ")+plugin.Code(ctx.Tlocal("失败", "failed")))
		} else {
			ip := pickIP(ips)
			addr = ip
			lines = append(lines, ctx.Tlocal("🔍 DNS解析  ", "🔍 DNS  ")+plugin.Code(fmtMs(dnsMs))+" → "+plugin.Code(ip))
		}
	}

	// Run the independent probes concurrently.
	var (
		wg       sync.WaitGroup
		probe    *tcpProbeResult
		icmp     icmpResult
		icmpErr  error
		tcp80    = -1
		tcp443   = -1
		httpMs   = -1
		httpsMs  = -1
		icmpDone bool
	)
	wg.Add(6)
	go func() { defer wg.Done(); icmp, icmpErr = systemPing(base, addr, 3) }()
	go func() { defer wg.Done(); probe = tcpingProbe(base, addr, []int{443, 80, 22, 53}, 3, 3*time.Second) }()
	go func() { defer wg.Done(); tcp80 = tcpPing(base, addr, 80, 5*time.Second) }()
	go func() { defer wg.Done(); tcp443 = tcpPing(base, addr, 443, 5*time.Second) }()
	go func() { defer wg.Done(); httpMs = httpPing(base, host, false) }()
	go func() { defer wg.Done(); httpsMs = httpPing(base, host, true) }()
	wg.Wait()
	icmpDone = icmpErr == nil && icmp.avg >= 0 && icmp.loss < 100

	// Headline latency: TCP first, then ICMP, then HTTP (reference order).
	latLabel := ctx.Tlocal("🏓 延迟  ", "🏓 Latency  ")
	lossWord := ctx.Tlocal("丢包", "loss")
	switch {
	case probe != nil:
		extra := fmt.Sprintf("(%s: %d%%, TCP:%d", lossWord, probe.loss, probe.port)
		if resolveProxy() != nil {
			extra += ", via proxy"
		}
		lines = append(lines, latLabel+plugin.Code(fmtMs(probe.avg))+" "+plugin.Escape(extra+")"))
	case icmpDone:
		lines = append(lines, latLabel+plugin.Code(fmtMs(icmp.avg))+" "+plugin.Escape(fmt.Sprintf("(%s: %d%%, ICMP)", lossWord, icmp.loss)))
	case httpMs > 0:
		lines = append(lines, latLabel+plugin.Code(fmtMs(httpMs))+" "+plugin.Escape("(HTTP)"))
	default:
		lines = append(lines, ctx.Tlocal("🏓 连通性  ", "🏓 Reachability  ")+plugin.Code(ctx.Tlocal("不可达", "unreachable")))
	}
	if httpsMs > 0 {
		lines = append(lines, ctx.Tlocal("📡 HTTPS请求  ", "📡 HTTPS request  ")+plugin.Code(fmtMs(httpsMs)))
	}

	// Detailed breakdown (kept from the previous Go implementation).
	var detail []string
	if tcp80 >= 0 {
		detail = append(detail, "TCP 80  "+plugin.Code(fmtMs(tcp80)))
	}
	if tcp443 >= 0 {
		detail = append(detail, "TCP 443  "+plugin.Code(fmtMs(tcp443)))
	}
	if httpMs >= 0 {
		detail = append(detail, "HTTP  "+plugin.Code(fmtMs(httpMs)))
	}
	if httpsMs >= 0 {
		detail = append(detail, "HTTPS  "+plugin.Code(fmtMs(httpsMs)))
	}
	if icmpDone {
		detail = append(detail, "ICMP  "+plugin.Code(fmtMs(icmp.avg))+" "+plugin.Escape(fmt.Sprintf("(%s %d%%)", lossWord, icmp.loss)))
	} else if icmpErr != nil {
		detail = append(detail, "ICMP  "+plugin.Code(ctx.Tlocal("不可用", "n/a")))
	} else {
		detail = append(detail, "ICMP  "+plugin.Code(ctx.Tlocal("超时", "timeout")))
	}

	typeName := map[string][2]string{
		"ip":     {"IP地址", "IP"},
		"domain": {"域名", "Domain"},
	}[parsed.typ]
	head := "🎯 **" + ctx.Tlocal(typeName[0]+"延迟测试", typeName[1]+" latency") + "**\n"
	if addr == target {
		head += plugin.Code(target)
	} else {
		head += plugin.Code(target) + " → " + plugin.Code(addr)
	}

	out := head + "\n\n" + strings.Join(lines, "\n")
	if len(detail) > 0 {
		out += "\n\n**" + ctx.Tlocal("详细", "Details") + "**\n" + strings.Join(detail, "\n")
	}
	if probe == nil && !icmpDone && httpMs < 0 && httpsMs < 0 {
		out += "\n\n❌ " + ctx.Tlocal("所有测试均失败，目标可能不可达", "All probes failed, target may be unreachable")
	}
	out += "\n\n⏰ " + plugin.Italic(now())
	return ctx.Edit(out)
}

func pickIP(ips []net.IPAddr) string {
	for _, ip := range ips {
		if ip.IP.To4() != nil {
			return ip.IP.String()
		}
	}
	return ips[0].IP.String()
}

type parsedTarget struct {
	typ   string // ip | domain | dc | invalid
	value string
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9._:\-]+$`)

func parseTarget(input string) parsedTarget {
	input = strings.TrimSpace(input)
	if n, ok := dcNumber(input); ok {
		return parsedTarget{typ: "dc", value: dcs[n]}
	}
	// Accept URLs like https://example.com/path for convenience.
	if strings.Contains(input, "://") {
		if u, err := url.Parse(input); err == nil && u.Hostname() != "" {
			input = u.Hostname()
		}
	}
	input = strings.TrimSuffix(strings.TrimPrefix(input, "["), "]")
	if ip := net.ParseIP(input); ip != nil {
		return parsedTarget{typ: "ip", value: ip.String()}
	}
	if input == "" || strings.HasPrefix(input, "-") || !hostRe.MatchString(input) || strings.Contains(input, ":") {
		return parsedTarget{typ: "invalid", value: input}
	}
	return parsedTarget{typ: "domain", value: input}
}

// ---------------------------------------------------------------- TCP

type tcpProbeResult struct {
	avg, best, port, loss int
}

// tcpingProbe tries ports in order for each sample; the first that connects
// counts. Returns nil when every sample failed.
func tcpingProbe(ctx context.Context, host string, ports []int, samples int, timeout time.Duration) *tcpProbeResult {
	type hit struct{ ms, port int }
	var ok []hit
	for i := 0; i < samples; i++ {
		if ctx.Err() != nil {
			break
		}
		for _, port := range ports {
			if ms := tcpPing(ctx, host, port, timeout); ms >= 0 {
				ok = append(ok, hit{ms, port})
				break
			}
		}
	}
	if len(ok) == 0 {
		return nil
	}
	sum, best := 0, ok[0].ms
	count := map[int]int{}
	for _, h := range ok {
		sum += h.ms
		if h.ms < best {
			best = h.ms
		}
		count[h.port]++
	}
	port, maxC := ok[0].port, 0
	keys := make([]int, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		if count[k] > maxC {
			maxC, port = count[k], k
		}
	}
	avg := int(float64(sum)/float64(len(ok)) + 0.5)
	loss := int(float64(samples-len(ok))/float64(samples)*100 + 0.5)
	return &tcpProbeResult{avg: avg, best: best, port: port, loss: loss}
}

// tcpPing measures a TCP connect, through the configured proxy if any.
func tcpPing(ctx context.Context, host string, port int, timeout time.Duration) int {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	var (
		conn net.Conn
		err  error
	)
	if px := resolveProxy(); px != nil {
		conn, err = px.dial(c, host, port)
	} else {
		var d net.Dialer
		conn, err = d.DialContext(c, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	}
	elapsed := time.Since(start)
	if err != nil {
		return -1
	}
	conn.Close()
	return int(elapsed.Milliseconds())
}

// ---------------------------------------------------------------- HTTP

// httpPing times the first response to HEAD / (redirects not followed).
// Certificates are not verified: only latency matters and IP targets would
// otherwise always fail on HTTPS.
func httpPing(ctx context.Context, host string, https bool) int {
	scheme := "http"
	if https {
		scheme = "https"
	}
	u := scheme + "://" + hostForURL(host) + "/"
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodHead, u, nil)
	if err != nil {
		return -1
	}
	req.Header.Set("User-Agent", "PaperValet-Ping/1.1")
	tr := &http.Transport{
		Proxy:             nil,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // latency probe only
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return -1
	}
	resp.Body.Close()
	return int(elapsed.Milliseconds())
}

func hostForURL(host string) string {
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "[" + host + "]"
	}
	return host
}

// ---------------------------------------------------------------- ICMP

type icmpResult struct {
	avg, loss int
}

var (
	avgRe  = regexp.MustCompile(`(?:min/avg/max[^=]*=\s*[0-9.]+/)([0-9.]+)`)
	lossRe = regexp.MustCompile(`([0-9.]+)% packet loss`)
	timeRe = regexp.MustCompile(`time[=<]([0-9.]+)`)
)

func parsePingOutput(out string) icmpResult {
	r := icmpResult{avg: -1, loss: 100}
	if m := avgRe.FindStringSubmatch(out); m != nil {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			r.avg = int(f + 0.5)
		}
	}
	if m := lossRe.FindStringSubmatch(out); m != nil {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			r.loss = int(f + 0.5)
		}
	}
	return r
}

func validPingTarget(t string) bool {
	return t != "" && !strings.HasPrefix(t, "-") && hostRe.MatchString(t)
}

// systemPing runs the system ping binary (no shell). Error means ping is
// missing / unusable; unreachable hosts return loss=100.
func systemPing(ctx context.Context, target string, count int) (icmpResult, error) {
	if !validPingTarget(target) {
		return icmpResult{avg: -1, loss: 100}, errors.New("invalid target")
	}
	if count < 1 || count > 10 {
		count = 3
	}
	bin, err := exec.LookPath("ping")
	if err != nil {
		return icmpResult{avg: -1, loss: 100}, err
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, bin, "-c", strconv.Itoa(count), "-W", "3", "-i", "0.5", target).CombinedOutput()
	r := parsePingOutput(string(out))
	if err != nil && r.avg < 0 && !strings.Contains(string(out), "packet loss") {
		// e.g. permission denied / unknown option: retry without -i.
		out, err = exec.CommandContext(c, bin, "-c", strconv.Itoa(count), "-W", "3", target).CombinedOutput()
		r = parsePingOutput(string(out))
		if err != nil && r.avg < 0 && !strings.Contains(string(out), "packet loss") {
			return r, fmt.Errorf("ping: %v", err)
		}
	}
	return r, nil
}

func icmpOnce(ctx context.Context, target string) (int, bool) {
	if !validPingTarget(target) {
		return 0, false
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "ping", "-c", "1", "-W", "3", target).Output()
	if err != nil {
		return 0, false
	}
	if m := timeRe.FindStringSubmatch(string(out)); m != nil {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			return int(f + 0.5), true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------- proxy

type pingProxy struct {
	kind     string // socks5 | socks4 | http
	host     string
	port     int
	user     string
	password string
}

func (p *pingProxy) display() string {
	return p.kind + "://" + net.JoinHostPort(p.host, strconv.Itoa(p.port))
}

// resolveProxy reads ALL_PROXY / HTTPS_PROXY / HTTP_PROXY (either case),
// mirroring the reference's environment fallback.
func resolveProxy() *pingProxy {
	for _, k := range []string{"ALL_PROXY", "all_proxy", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return parseProxyURL(v)
		}
	}
	return nil
}

func parseProxyURL(raw string) *pingProxy {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	proto := strings.ToLower(u.Scheme)
	px := &pingProxy{host: u.Hostname()}
	switch proto {
	case "socks5", "socks5h", "socks":
		px.kind = "socks5"
	case "socks4", "socks4a":
		px.kind = "socks4"
	case "http", "https":
		px.kind = "http"
	default:
		return nil
	}
	if ps := u.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n <= 0 || n > 65535 {
			return nil
		}
		px.port = n
	} else if px.kind == "http" {
		px.port = 8080
	} else {
		px.port = 1080
	}
	if u.User != nil {
		px.user = u.User.Username()
		px.password, _ = u.User.Password()
	}
	return px
}

func (p *pingProxy) dial(ctx context.Context, host string, port int) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(p.host, strconv.Itoa(p.port)))
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	switch p.kind {
	case "socks5":
		err = socks5Handshake(conn, p, host, port)
	case "socks4":
		err = socks4Handshake(conn, p, host, port)
	default:
		err = httpConnectHandshake(conn, p, host, port)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func socks5Handshake(conn net.Conn, p *pingProxy, host string, port int) error {
	if p.user != "" {
		if _, err := conn.Write([]byte{5, 2, 0, 2}); err != nil {
			return err
		}
	} else if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return err
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if buf[0] != 5 {
		return errors.New("bad socks5 version")
	}
	switch buf[1] {
	case 0:
	case 2:
		if len(p.user) > 255 || len(p.password) > 255 {
			return errors.New("socks5 credentials too long")
		}
		auth := []byte{1, byte(len(p.user))}
		auth = append(auth, p.user...)
		auth = append(auth, byte(len(p.password)))
		auth = append(auth, p.password...)
		if _, err := conn.Write(auth); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			return err
		}
		if buf[1] != 0 {
			return errors.New("socks5 auth failed")
		}
	default:
		return fmt.Errorf("socks5 auth method %d", buf[1])
	}

	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 1)
			req = append(req, v4...)
		} else {
			req = append(req, 4)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("host too long")
		}
		req = append(req, 3, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	if head[0] != 5 {
		return errors.New("bad socks5 reply")
	}
	if head[1] != 0 {
		return fmt.Errorf("socks5 connect status %d", head[1])
	}
	var skip int
	switch head[3] {
	case 1:
		skip = 4 + 2
	case 4:
		skip = 16 + 2
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return err
		}
		skip = int(l[0]) + 2
	default:
		return fmt.Errorf("socks5 atyp %d", head[3])
	}
	_, err := io.ReadFull(conn, make([]byte, skip))
	return err
}

func socks4Handshake(conn net.Conn, p *pingProxy, host string, port int) error {
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return errors.New("socks4 needs IPv4")
	}
	req := []byte{4, 1}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	req = append(req, ip...)
	req = append(req, p.user...)
	req = append(req, 0)
	if _, err := conn.Write(req); err != nil {
		return err
	}
	resp := make([]byte, 8)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}
	if resp[1] != 0x5a {
		return fmt.Errorf("socks4 status %d", resp[1])
	}
	return nil
}

func httpConnectHandshake(conn net.Conn, p *pingProxy, host string, port int) error {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if p.user != "" {
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(p.user+":"+p.password)) + "\r\n"
	}
	req += "Proxy-Connection: keep-alive\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http proxy CONNECT %d", resp.StatusCode)
	}
	return nil
}
