package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "speedtest",
	Description: "网络速度测试（Speedtest by Ookla）",
	DescEN:      "Network speed test (Speedtest by Ookla)",
	Version:     "1.1.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type SpeedtestPlugin struct {
	mu      sync.Mutex // guards cfg file
	running sync.Mutex // one speed test at a time
	ctx     context.Context
	cancel  context.CancelFunc
}

func New() *SpeedtestPlugin {
	c, cancel := context.WithCancel(context.Background())
	return &SpeedtestPlugin{ctx: c, cancel: cancel}
}

func (p *SpeedtestPlugin) Name() string        { return "speedtest" }
func (p *SpeedtestPlugin) Description() string { return "网络速度测试（Speedtest by Ookla）" }
func (p *SpeedtestPlugin) DescEN() string      { return "Network speed test (Speedtest by Ookla)" }

func (p *SpeedtestPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "speedtest",
		Aliases:     []string{"st"},
		Description: "网络速度测试（Ookla CLI，自动下载）",
		DescEN:      "Network speed test (Ookla CLI, auto-downloaded)",
		Usage:       "speedtest [服务器ID|list|best|test <ID>|set <ID>|clear|type <photo|sticker|file|txt>|config|check|diagnose|fix|update|help] [--system|-s]",
		UsageEN:     "speedtest [serverID|list|best|test <ID>|set <ID>|clear|type <photo|sticker|file|txt>|config|check|diagnose|fix|update|help] [--system|-s]",
		Plugin:      p.Name(),
		Category:    "tools",
		Handler:     p.handle,
	})
}

func (p *SpeedtestPlugin) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.ctx.Err() != nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	p.mu.Unlock()
	return nil
}

// Stop aborts any running speed test / download.
func (p *SpeedtestPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	p.cancel()
	p.mu.Unlock()
	return nil
}

// opCtx merges the command context with the plugin lifetime.
func (p *SpeedtestPlugin) opCtx(ctx *plugin.CommandContext) (context.Context, context.CancelFunc) {
	p.mu.Lock()
	life := p.ctx
	p.mu.Unlock()
	c, cancel := context.WithCancel(ctx.Context())
	stop := context.AfterFunc(life, cancel)
	return c, func() { stop(); cancel() }
}

// ---------------------------------------------------------------- config

type msgType string

var defaultOrder = []msgType{"photo", "sticker", "file", "txt"}

type config struct {
	DefaultServerID *int    `json:"default_server_id,omitempty"`
	PreferredType   msgType `json:"preferred_type,omitempty"`
}

func configPath() string { return filepath.Join(dataDir, "config.json") }

func (p *SpeedtestPlugin) loadConfig() config {
	p.mu.Lock()
	defer p.mu.Unlock()
	var c config
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func (p *SpeedtestPlugin) updateConfig(fn func(*config)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var c config
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	fn(&c)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := configPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, configPath())
}

func messageOrder(pref msgType) []msgType {
	if pref == "" {
		return append([]msgType(nil), defaultOrder...)
	}
	out := []msgType{pref}
	for _, t := range defaultOrder {
		if t != pref {
			out = append(out, t)
		}
	}
	return out
}

func orderString(o []msgType) string {
	s := make([]string, len(o))
	for i, t := range o {
		s[i] = string(t)
	}
	return strings.Join(s, " → ")
}

// ---------------------------------------------------------------- handler

const header = "⚡️ **SPEEDTEST by OOKLA**\n\n"

func helpText(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		header+
			"**用法**\n"+
			"• `speedtest` — 开始速度测试\n"+
			"• `speedtest [服务器ID]` — 使用指定服务器测试\n"+
			"• `speedtest list` — 显示附近服务器列表\n"+
			"• `speedtest best` — 查找推荐服务器（按延迟）\n"+
			"• `speedtest test [ID]` — 测试指定服务器可用性\n"+
			"• `speedtest set [ID]` — 设置默认服务器\n"+
			"• `speedtest clear` — 清除默认服务器\n"+
			"• `speedtest type photo/sticker/file/txt` — 设置优先消息类型\n"+
			"• `speedtest config` — 显示配置\n"+
			"• `speedtest check` — 检查网络连接\n"+
			"• `speedtest diagnose` — 诊断 CLI 可执行文件\n"+
			"• `speedtest fix` — 自动修复 CLI 安装\n"+
			"• `speedtest update` — 重新下载 Speedtest CLI\n\n"+
			"**系统 speedtest**\n"+
			"添加 `--system` 或 `-s` 使用系统已安装的 speedtest（失败回退内置 CLI），例: `speedtest -s 12345`\n\n"+
			"💡 CLI 自动下载到 data/speedtest/",
		header+
			"**Usage**\n"+
			"• `speedtest` — run a speed test\n"+
			"• `speedtest [serverID]` — test against a server\n"+
			"• `speedtest list` — nearby servers\n"+
			"• `speedtest best` — recommended servers (by latency)\n"+
			"• `speedtest test [ID]` — check a server\n"+
			"• `speedtest set [ID]` — set default server\n"+
			"• `speedtest clear` — clear default server\n"+
			"• `speedtest type photo/sticker/file/txt` — preferred result type\n"+
			"• `speedtest config` — show config\n"+
			"• `speedtest check` — check connectivity\n"+
			"• `speedtest diagnose` — diagnose the CLI binary\n"+
			"• `speedtest fix` — repair the CLI install\n"+
			"• `speedtest update` — re-download Speedtest CLI\n\n"+
			"**System speedtest**\n"+
			"Add `--system` or `-s` to use an installed speedtest (falls back to the bundled CLI), e.g. `speedtest -s 12345`\n\n"+
			"💡 The CLI is downloaded into data/speedtest/")
}

func (p *SpeedtestPlugin) handle(ctx *plugin.CommandContext) error {
	var args []string
	useSystem := false
	for _, a := range ctx.Args {
		if strings.HasPrefix(a, "-") {
			if a == "--system" || a == "-s" {
				useSystem = true
			}
			continue
		}
		args = append(args, a)
	}
	cmd := ""
	if len(args) > 0 {
		cmd = strings.ToLower(args[0])
	}
	arg1 := ""
	if len(args) > 1 {
		arg1 = args[1]
	}

	c, cancel := p.opCtx(ctx)
	defer cancel()

	switch cmd {
	case "help", "h":
		return ctx.Edit(helpText(ctx))
	case "list", "ls":
		return p.cmdList(c, ctx)
	case "set":
		return p.cmdSet(ctx, arg1)
	case "clear":
		if err := p.updateConfig(func(cf *config) { cf.DefaultServerID = nil }); err != nil {
			return ctx.Edit("❌ " + plugin.Escape(err.Error()))
		}
		return ctx.Edit(header + "✅ " + ctx.Tlocal("默认服务器已清除", "Default server cleared"))
	case "config", "cfg":
		return p.cmdConfig(c, ctx)
	case "type":
		return p.cmdType(ctx, arg1)
	case "check":
		_ = ctx.Edit(ctx.Tlocal("🔍 正在检查网络连接...", "🔍 Checking connectivity..."))
		ok, msg := checkNetwork(c, ctx)
		icon := "✅"
		if !ok {
			icon = "❌"
		}
		return ctx.Edit(header + icon + " " + ctx.Tlocal("网络状态  ", "Network  ") + plugin.Code(msg) + "\n\n" +
			ctx.Tlocal("💡 若连接异常，请检查网络设置 / DNS / 防火墙", "💡 If it fails, check network settings / DNS / firewall"))
	case "test":
		return p.cmdTest(c, ctx, arg1)
	case "best":
		return p.cmdBest(c, ctx)
	case "diagnose", "diag":
		return p.cmdDiagnose(c, ctx)
	case "fix":
		_ = ctx.Edit(ctx.Tlocal("🔧 正在自动修复 speedtest 安装...", "🔧 Repairing speedtest install..."))
		if err := autoFix(c); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("自动修复失败: ", "Auto-fix failed: ") + plugin.Escape(err.Error()) + "\n\n💡 " +
				ctx.Tlocal("检查网络、磁盘空间与文件权限", "Check network, disk space and permissions"))
		}
		return ctx.Edit(header + "✅ " + ctx.Tlocal("自动修复完成", "Repaired") + "\n" +
			ctx.Tlocal("平台  ", "Platform  ") + plugin.Code(runtime.GOOS+"/"+runtime.GOARCH) + "\n" +
			ctx.Tlocal("路径  ", "Path  ") + plugin.Code(cliPath()))
	case "update", "upgrade":
		_ = ctx.Edit(ctx.Tlocal("🔄 正在更新 Speedtest CLI...", "🔄 Updating Speedtest CLI..."))
		removeCLI()
		if err := downloadCLI(c, true); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("更新失败: ", "Update failed: ") + plugin.Escape(err.Error()) + "\n\n💡 " +
				ctx.Tlocal("检查网络、磁盘空间与文件权限", "Check network, disk space and permissions"))
		}
		d := diagnose(c)
		ver := d.version
		if ver == "" {
			ver = cliVersion
		}
		return ctx.Edit(header + "✅ " + ctx.Tlocal("Speedtest® CLI 已更新", "Speedtest® CLI updated") + "\n" +
			ctx.Tlocal("版本  ", "Version  ") + plugin.Code(ver) + "\n" +
			ctx.Tlocal("平台  ", "Platform  ") + plugin.Code(runtime.GOOS+"/"+runtime.GOARCH) + "\n" +
			ctx.Tlocal("路径  ", "Path  ") + plugin.Code(cliPath()))
	}

	if cmd == "" {
		return p.cmdRun(c, ctx, 0, useSystem)
	}
	if id, err := strconv.Atoi(cmd); err == nil && id > 0 {
		return p.cmdRun(c, ctx, id, useSystem)
	}
	return ctx.Edit("❌ " + ctx.Tlocal("参数错误", "Invalid arguments") + "\n\n" + helpText(ctx))
}

func serverLine(s Server) string {
	return plugin.Code(fmt.Sprint(s.ID)) + " - " + plugin.Code(orDash(s.Name)) + " - " + plugin.Code(orDash(s.Location))
}

func (p *SpeedtestPlugin) cmdList(c context.Context, ctx *plugin.CommandContext) error {
	_ = ctx.Edit(ctx.Tlocal("🔍 正在获取服务器列表...", "🔍 Fetching server list..."))
	servers, err := listServers(c)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("获取服务器列表失败: ", "Failed to list servers: ") + plugin.Escape(err.Error()))
	}
	if len(servers) > 20 {
		servers = servers[:20]
	}
	lines := make([]string, len(servers))
	for i, s := range servers {
		lines[i] = serverLine(s)
	}
	return ctx.Edit(header + strings.Join(lines, "\n") + "\n\n💡 " +
		ctx.Tlocal("使用 `speedtest set [ID]` 设为默认服务器", "Use `speedtest set [ID]` to make one the default"))
}

func (p *SpeedtestPlugin) cmdSet(ctx *plugin.CommandContext, arg string) error {
	id, err := strconv.Atoi(arg)
	if err != nil || id <= 0 {
		return ctx.Edit("❌ " + ctx.Tlocal("请指定有效的服务器ID，例: ", "Give a valid server ID, e.g. ") + plugin.Code("speedtest set 12345"))
	}
	if err := p.updateConfig(func(cf *config) { cf.DefaultServerID = &id }); err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()))
	}
	return ctx.Edit(header + "✅ " + ctx.Tlocal("默认服务器已设置为 ", "Default server set to ") + plugin.Code(fmt.Sprint(id)))
}

func (p *SpeedtestPlugin) cmdType(ctx *plugin.CommandContext, arg string) error {
	t := msgType(strings.ToLower(arg))
	if t == "text" {
		t = "txt"
	}
	valid := false
	for _, v := range defaultOrder {
		if v == t {
			valid = true
		}
	}
	if !valid {
		return ctx.Edit("❌ " + ctx.Tlocal("参数错误，用法: ", "Invalid type, usage: ") + plugin.Code("speedtest type photo/sticker/file/txt"))
	}
	if err := p.updateConfig(func(cf *config) { cf.PreferredType = t }); err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()))
	}
	return ctx.Edit(header +
		ctx.Tlocal("优先类型  ", "Preferred type  ") + plugin.Code(string(t)) + "\n" +
		ctx.Tlocal("当前顺序  ", "Order  ") + plugin.Code(orderString(messageOrder(t))))
}

func (p *SpeedtestPlugin) cmdConfig(c context.Context, ctx *plugin.CommandContext) error {
	cf := p.loadConfig()
	def := "Auto"
	if cf.DefaultServerID != nil {
		def = fmt.Sprint(*cf.DefaultServerID)
	}
	typ := ctx.Tlocal("默认", "default") + " (" + orderString(defaultOrder) + ")"
	if cf.PreferredType != "" {
		typ = string(cf.PreferredType)
	}
	ver := cliVersion + ctx.Tlocal("（未安装）", " (not installed)")
	if _, err := os.Stat(cliPath()); err == nil {
		if d := diagnose(c); d.version != "" {
			ver = d.version
		}
	}
	sys := ctx.Tlocal("无", "none")
	if bin, fl, err := findSystemCLI(c); err == nil {
		sys = bin
		if fl == flavourPython {
			sys += " (python speedtest-cli)"
		}
	}
	return ctx.Edit(header + "**" + ctx.Tlocal("配置", "Config") + "**\n" +
		ctx.Tlocal("默认服务器  ", "Default server  ") + plugin.Code(def) + "\n" +
		ctx.Tlocal("优先类型  ", "Preferred type  ") + plugin.Code(typ) + "\n" +
		"Speedtest® CLI  " + plugin.Code(ver) + "\n" +
		ctx.Tlocal("系统 speedtest  ", "System speedtest  ") + plugin.Code(sys))
}

// tcpLatency measures a TCP connect to a speedtest server.
func tcpLatency(c context.Context, s Server) (time.Duration, error) {
	if s.Host == "" {
		return 0, errors.New("no host")
	}
	host, port := s.Host, strconv.Itoa(s.Port)
	if h, pt, err := net.SplitHostPort(s.Host); err == nil {
		host, port = h, pt
	}
	if port == "0" || port == "" {
		port = "8080"
	}
	cc, cancel := context.WithTimeout(c, 5*time.Second)
	defer cancel()
	var d net.Dialer
	start := time.Now()
	conn, err := d.DialContext(cc, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return 0, err
	}
	el := time.Since(start)
	conn.Close()
	return el, nil
}

func (p *SpeedtestPlugin) cmdTest(c context.Context, ctx *plugin.CommandContext, arg string) error {
	id, err := strconv.Atoi(arg)
	if err != nil || id <= 0 {
		return ctx.Edit("❌ " + ctx.Tlocal("请指定有效的服务器ID，例: ", "Give a valid server ID, e.g. ") + plugin.Code("speedtest test 12345"))
	}
	_ = ctx.Edit(fmt.Sprintf(ctx.Tlocal("🔍 正在测试服务器 %d 的可用性...", "🔍 Checking server %d..."), id))
	servers, err := listServers(c)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("测试失败: ", "Test failed: ") + plugin.Escape(err.Error()))
	}
	for _, s := range servers {
		if s.ID != id {
			continue
		}
		out := header + "✅ " + ctx.Tlocal("服务器 ", "Server ") + plugin.Code(fmt.Sprint(id)) + "  " + plugin.Code(ctx.Tlocal("可用", "available")) + "\n" + serverLine(s)
		if d, err := tcpLatency(c, s); err == nil {
			out += "\n" + ctx.Tlocal("延迟  ", "Latency  ") + plugin.Code(fmt.Sprintf("%dms", d.Milliseconds()))
		} else {
			out += "\n" + ctx.Tlocal("延迟  ", "Latency  ") + plugin.Code(ctx.Tlocal("连接失败", "connect failed"))
		}
		return ctx.Edit(out)
	}
	return ctx.Edit(header + "❌ " + ctx.Tlocal("服务器 ", "Server ") + plugin.Code(fmt.Sprint(id)) + " " +
		ctx.Tlocal("不在附近可用列表中", "is not in the nearby server list") + "\n\n💡 " +
		ctx.Tlocal("使用 `speedtest list` 查看可用服务器", "Use `speedtest list` to see available servers"))
}

func (p *SpeedtestPlugin) cmdBest(c context.Context, ctx *plugin.CommandContext) error {
	_ = ctx.Edit(ctx.Tlocal("🎯 正在查找推荐服务器...", "🎯 Finding recommended servers..."))
	servers, err := listServers(c)
	if err != nil || len(servers) == 0 {
		msg := ctx.Tlocal("无法获取服务器列表", "Could not fetch server list")
		if err != nil {
			msg += ": " + err.Error()
		}
		return ctx.Edit("❌ " + plugin.Escape(msg) + "\n\n💡 " + ctx.Tlocal("检查网络连接后重试", "Check connectivity and retry"))
	}
	if len(servers) > 10 {
		servers = servers[:10]
	}
	type scored struct {
		s  Server
		ms int64
	}
	res := make([]scored, len(servers))
	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Add(1)
		go func(i int, s Server) {
			defer wg.Done()
			res[i] = scored{s, -1}
			if d, err := tcpLatency(c, s); err == nil {
				res[i].ms = d.Milliseconds()
			}
		}(i, s)
	}
	wg.Wait()
	sort.SliceStable(res, func(i, j int) bool {
		a, b := res[i].ms, res[j].ms
		if a < 0 {
			return false
		}
		if b < 0 {
			return true
		}
		return a < b
	})
	var lines []string
	for i, r := range res {
		if i >= 3 {
			break
		}
		lat := ctx.Tlocal("超时", "timeout")
		if r.ms >= 0 {
			lat = fmt.Sprintf("%dms", r.ms)
		}
		lines = append(lines, fmt.Sprintf("%d. ", i+1)+serverLine(r.s)+" "+plugin.Code(lat))
	}
	return ctx.Edit(header + "🎯 **" + ctx.Tlocal("推荐服务器（按延迟）", "Recommended servers (by latency)") + "**\n" +
		strings.Join(lines, "\n") + "\n\n💡 " +
		ctx.Tlocal("`speedtest set [ID]` 设为默认，`speedtest [ID]` 直接测试", "`speedtest set [ID]` to make default, `speedtest [ID]` to test"))
}

func (p *SpeedtestPlugin) cmdDiagnose(c context.Context, ctx *plugin.CommandContext) error {
	_ = ctx.Edit(ctx.Tlocal("🔍 正在诊断 speedtest 可执行文件...", "🔍 Diagnosing speedtest binary..."))
	d := diagnose(c)
	icon, status := "✅", ctx.Tlocal("正常", "OK")
	if !d.canRun {
		icon, status = "❌", ctx.Tlocal("异常", "broken")
	}
	exists := ctx.Tlocal("否", "no")
	if d.exists {
		exists = ctx.Tlocal("是", "yes")
	}
	out := header + icon + " " + ctx.Tlocal("可执行文件  ", "Executable  ") + plugin.Code(status) + "\n"
	if d.problem != "" {
		out += ctx.Tlocal("问题  ", "Problem  ") + plugin.Code(d.problem) + "\n"
	}
	if d.version != "" {
		out += ctx.Tlocal("版本  ", "Version  ") + plugin.Code(d.version) + "\n"
	}
	out += ctx.Tlocal("平台  ", "Platform  ") + plugin.Code(runtime.GOOS) + "\n" +
		ctx.Tlocal("架构  ", "Arch  ") + plugin.Code(runtime.GOARCH) + "\n" +
		ctx.Tlocal("路径  ", "Path  ") + plugin.Code(cliPath()) + "\n" +
		ctx.Tlocal("存在  ", "Exists  ") + plugin.Code(exists)
	if !d.canRun {
		out += "\n\n💡 " + ctx.Tlocal("使用 `speedtest fix` 自动修复", "Run `speedtest fix` to repair")
	}
	return ctx.Edit(out)
}

// checkNetwork mirrors the reference pre-flight check.
func checkNetwork(c context.Context, ctx *plugin.CommandContext) (bool, string) {
	cc, cancel := context.WithTimeout(c, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cc, http.MethodGet, "https://www.speedtest.net", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 PaperValet-Speedtest/1.1")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		return true, ctx.Tlocal("网络连接正常", "Connection OK")
	}
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.As(err, &dnsErr):
		return false, ctx.Tlocal("DNS解析失败，请检查DNS设置", "DNS lookup failed, check DNS settings")
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return false, ctx.Tlocal("连接超时，网络可能较慢或不稳定", "Timed out, network slow or unstable")
	case errors.As(err, &opErr) && strings.Contains(opErr.Error(), "refused"):
		return false, ctx.Tlocal("连接被拒绝，可能存在防火墙阻止", "Connection refused, maybe a firewall")
	}
	return false, ctx.Tlocal("网络连接异常: ", "Network error: ") + err.Error()
}

// ---------------------------------------------------------------- run

func (p *SpeedtestPlugin) cmdRun(c context.Context, ctx *plugin.CommandContext, serverID int, useSystem bool) error {
	if !p.running.TryLock() {
		return ctx.Edit("❌ " + ctx.Tlocal("已有测速正在进行，请稍后再试", "A speed test is already running, try again later"))
	}
	defer p.running.Unlock()

	_ = ctx.Edit(ctx.Tlocal("🔍 正在检查网络连接...", "🔍 Checking connectivity..."))
	if ok, msg := checkNetwork(c, ctx); !ok {
		return ctx.Edit("❌ " + ctx.Tlocal("网络连接异常，无法进行速度测试", "Network problem, cannot run speed test") + "\n\n" +
			ctx.Tlocal("检测结果  ", "Result  ") + plugin.Code(msg) + "\n\n💡 " +
			ctx.Tlocal("检查网络 / DNS / 防火墙，或使用 `speedtest check` 重新检查", "Check network / DNS / firewall, or run `speedtest check`"))
	}

	cf := p.loadConfig()
	if serverID == 0 && cf.DefaultServerID != nil {
		serverID = *cf.DefaultServerID
	}
	if _, err := os.Stat(cliPath()); err != nil && !useSystem {
		_ = ctx.Edit(ctx.Tlocal("📥 正在下载 Speedtest CLI...", "📥 Downloading Speedtest CLI..."))
		if err := downloadCLI(c, false); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("下载 Speedtest CLI 失败: ", "Failed to download Speedtest CLI: ") + plugin.Escape(err.Error()) +
				"\n\n💡 " + ctx.Tlocal("可使用 `speedtest --system` 改用系统 speedtest", "Try `speedtest --system` to use an installed speedtest"))
		}
	}
	_ = ctx.Edit(ctx.Tlocal("⚡️ 网络连接正常，正在进行速度测试...", "⚡️ Connection OK, running speed test..."))

	res, source, err := runSpeedtest(c, serverID, useSystem)
	if err != nil {
		msg := err.Error()
		hint := ""
		var re *runError
		if errors.As(err, &re) && re.network {
			hint = "\n\n💡 " + ctx.Tlocal(
				"检查网络连接；`speedtest list` 查看服务器，`speedtest set [ID]` 选择其他服务器",
				"Check connectivity; `speedtest list` shows servers, `speedtest set [ID]` picks another one")
		}
		return ctx.Edit("❌ " + ctx.Tlocal("速度测试失败: ", "Speed test failed: ") + plugin.Escape(msg) + hint)
	}

	ex := extraInfo{source: source}
	if res.ExternalIP != "" {
		ex.ip = lookupIP(c, res.ExternalIP)
	}
	if rx, tx, mtu, ok := interfaceTraffic(res.Interface); ok {
		ex.rx, ex.tx, ex.mtu, ex.haveTraffic = rx, tx, mtu, true
	}
	text := formatResult(ctx.Lang, res, ex)

	for _, t := range messageOrder(cf.PreferredType) {
		if p.trySend(c, ctx, t, res, text) {
			return nil
		}
	}
	return ctx.Edit(text)
}

// lookupIP queries ip-api.com for AS / country (reference behaviour).
func lookupIP(c context.Context, ip string) ipInfo {
	cc, cancel := context.WithTimeout(c, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cc, http.MethodGet, "http://ip-api.com/json/"+ip+"?fields=status,as,country,countryCode", nil)
	if err != nil {
		return ipInfo{}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ipInfo{}
	}
	defer resp.Body.Close()
	var d struct {
		Status      string `json:"status"`
		AS          string `json:"as"`
		CountryCode string `json:"countryCode"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&d) != nil || d.Status == "fail" {
		return ipInfo{}
	}
	as, _, _ := strings.Cut(d.AS, " ")
	return ipInfo{asInfo: as, ccCode: d.CountryCode, ccFlag: flagEmoji(d.CountryCode)}
}

var ifaceRe = regexp.MustCompile(`^[A-Za-z0-9_.@:\-]{1,32}$`)

// interfaceTraffic reads RX/TX counters and MTU from sysfs (Linux only).
func interfaceTraffic(name string) (rx, tx float64, mtu int, ok bool) {
	if runtime.GOOS != "linux" || !ifaceRe.MatchString(name) || strings.Contains(name, "..") {
		return
	}
	read := func(f string) (float64, bool) {
		b, err := os.ReadFile(filepath.Join("/sys/class/net", name, f))
		if err != nil {
			return 0, false
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		return v, err == nil
	}
	var ok1, ok2, ok3 bool
	rx, ok1 = read("statistics/rx_bytes")
	tx, ok2 = read("statistics/tx_bytes")
	m, ok3 := read("mtu")
	if ok3 {
		mtu = int(m)
	}
	ok = ok1 && ok2
	return
}

// ---------------------------------------------------------------- sending

// trySend delivers the result as type t; false means "try the next type".
func (p *SpeedtestPlugin) trySend(c context.Context, ctx *plugin.CommandContext, t msgType, res *Result, text string) bool {
	if t == "txt" {
		return ctx.Edit(text) == nil
	}
	if res.ImageURL == "" || ctx.API == nil {
		return false
	}
	tmp := filepath.Join(dataDir, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return false
	}
	img, err := fetchImage(c, res.ImageURL, tmp)
	if err != nil {
		if ctx.Logger != nil {
			ctx.Logger.Warn("speedtest: fetch result image failed", "err", err)
		}
		return false
	}
	defer os.Remove(img)

	caption, ents := plugin.ParseMarkdown(text, nil)
	if len([]rune(caption)) > 1024 {
		caption, ents = "", nil
	}
	switch t {
	case "photo":
		if err := sendUploaded(c, ctx, img, "photo", caption, ents); err != nil {
			return false
		}
		if caption == "" {
			return ctx.Edit(text) == nil
		}
		_ = ctx.Delete()
		return true
	case "file":
		if err := sendUploaded(c, ctx, img, "file", caption, ents); err != nil {
			return false
		}
		if caption == "" {
			return ctx.Edit(text) == nil
		}
		_ = ctx.Delete()
		return true
	case "sticker":
		st, err := toStickerWebp(c, img, tmp)
		if err != nil {
			return false
		}
		defer os.Remove(st)
		if err := sendUploaded(c, ctx, st, "sticker", "", nil); err != nil {
			return false
		}
		_ = ctx.Edit(text)
		return true
	}
	return false
}

func uniqueName(prefix, ext string) string {
	return fmt.Sprintf("%s_%d_%06d%s", prefix, time.Now().UnixNano(), rand.IntN(1000000), ext)
}

// fetchImage downloads the result PNG and fills its rounded corners with
// the card background (like the reference's sharp step).
func fetchImage(c context.Context, url, dir string) (string, error) {
	cc, cancel := context.WithTimeout(c, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cc, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 PaperValet-Speedtest/1.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, uniqueName("speedtest", ".png"))
	if src, err := png.Decode(bytes.NewReader(data)); err == nil {
		if f, err := os.Create(path); err == nil {
			err = png.Encode(f, fillCorners(src, color.RGBA{0x21, 0x23, 0x38, 0xff}, 14))
			f.Close()
			if err == nil {
				return path, nil
			}
		}
	}
	// Not a PNG we can process: send it as-is.
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// fillCorners paints a border of inset px with bg and keeps the centre,
// hiding the transparent rounded corners of Ookla's share image.
func fillCorners(src image.Image, bg color.Color, inset int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	maxInset := (min(w, h) - 1) / 2
	inset = max(0, min(inset, maxInset))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)
	inner := image.Rect(inset, inset, w-inset, h-inset)
	draw.Draw(dst, inner, src, b.Min.Add(image.Pt(inset, inset)), draw.Over)
	return dst
}

// toStickerWebp converts the image to a 512px WebP using ffmpeg.
func toStickerWebp(c context.Context, src, dir string) (string, error) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, uniqueName("speedtest_sticker", ".webp"))
	vf := "scale=512:512:force_original_aspect_ratio=decrease,format=rgba,pad=512:512:(ow-iw)/2:(oh-ih)/2:color=black@0"
	for _, q := range []string{"85", "65"} {
		cc, cancel := context.WithTimeout(c, 30*time.Second)
		cmdOut, err := exec.CommandContext(cc, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", src,
			"-vf", vf, "-c:v", "libwebp", "-q:v", q, "-frames:v", "1", out).CombinedOutput()
		cancel()
		if err != nil {
			return "", fmt.Errorf("ffmpeg: %v %s", err, strings.TrimSpace(string(cmdOut)))
		}
		if st, err := os.Stat(out); err == nil && st.Size() <= 512*1024 {
			return out, nil
		}
	}
	return out, nil
}

func sendUploaded(c context.Context, ctx *plugin.CommandContext, path, kind, caption string, ents []tg.MessageEntityClass) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	uc, cancel := context.WithTimeout(c, 2*time.Minute)
	defer cancel()
	file, err := uploader.NewUploader(ctx.API).FromPath(uc, path)
	if err != nil {
		return err
	}
	var media tg.InputMediaClass
	switch kind {
	case "photo":
		media = &tg.InputMediaUploadedPhoto{File: file}
	case "file":
		media = &tg.InputMediaUploadedDocument{
			File: file, MimeType: "image/png", ForceFile: true,
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "speedtest.png"}},
		}
	case "sticker":
		media = &tg.InputMediaUploadedDocument{
			File: file, MimeType: "image/webp",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: "speedtest.webp"},
				&tg.DocumentAttributeSticker{Alt: "⚡", Stickerset: &tg.InputStickerSetEmpty{}},
			},
		}
	}
	req := &tg.MessagesSendMediaRequest{Peer: peer, Media: media, Message: caption, RandomID: rand.Int64()}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	if ctx.Message != nil && ctx.Message.ReplyToID > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: ctx.Message.ReplyToID})
	}
	_, err = ctx.API.MessagesSendMedia(uc, req)
	return err
}
