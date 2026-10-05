package main

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	historyPage    = 100             // messages.getHistory page size
	defaultMax     = 20000           // messages scanned per report
	minMax         = 100             // floor for the max_messages setting
	maxFloodWait   = time.Minute     // flood waits longer than this abort
	progressEvery  = 3 * time.Second // progress edit throttle
	cacheLimit     = 24              // cached reports kept per plugin
	cacheFile      = "reports.json"  // under data/annualreport/
	stateFile      = "stats.json"    // first-run time + report counter
	hitokotoURL    = "https://v1.hitokoto.cn/"
	reportMaxRunes = 3500 // telegram message budget per chunk
)

var Metadata = &plugin.PluginMetadata{
	Name:        "annualreport",
	Description: "群/时段消息统计报告与账号年度报告",
	DescEN:      "Chat stats report by period, plus your account year in review",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// AnnualReportPlugin walks chat history (or dialogs) and renders stats.
type AnnualReportPlugin struct {
	mu     sync.Mutex
	dir    string
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // in-flight command scans, joined by Stop
	host   plugin.Host
	set    plugin.Settings
	http   *http.Client
}

func New() *AnnualReportPlugin {
	return &AnnualReportPlugin{http: &http.Client{Timeout: 10 * time.Second}}
}

func (p *AnnualReportPlugin) Name() string        { return "annualreport" }
func (p *AnnualReportPlugin) Description() string { return Metadata.Description }
func (p *AnnualReportPlugin) DescEN() string      { return Metadata.DescEN }

func (p *AnnualReportPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	set, err := mgr.Host().Settings(&plugin.SettingsSpec{
		Plugin:  p.Name(),
		Title:   "📊 年度报告",
		TitleEN: "📊 Annual Report",
		Settings: []plugin.Setting{{
			Key: "max_messages", Label: "单次扫描上限", LabelEN: "Scan limit",
			Hint:   "每次报告最多回溯的消息条数，防止大群扫太久",
			HintEN: "Maximum messages walked per report, so huge groups do not take forever",
			Kind:   plugin.SettingNumber, Default: defaultMax, Min: minMax, Max: 200000,
		}},
	})
	if err != nil {
		return err
	}
	p.set = set
	if dir, err := mgr.Host().DataDir(p.Name()); err == nil {
		p.mu.Lock()
		p.dir = dir
		p.mu.Unlock()
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "annualreport",
		Description: "统计群消息：总量、Top 发言、时段/星期分布、媒体构成；me 为账号年度报告",
		DescEN:      "Chat stats: totals, top talkers, hour/weekday bins, media mix; me gives your account year in review",
		Usage:       "annualreport [年份|YYYY-MM|YYYY-MM-DD|all] [refresh] · annualreport me · annualreport help",
		UsageEN:     "annualreport [year|YYYY-MM|YYYY-MM-DD|all] [refresh] · annualreport me · annualreport help",
		Plugin:      p.Name(),
		Category:    "info",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *AnnualReportPlugin) Start(_ context.Context) error {
	p.mu.Lock()
	if p.cancel == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	p.mu.Unlock()
	return nil
}

// Stop cancels scans and waits for them to finish (bounded by ctx).
func (p *AnnualReportPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if ctx == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// lifetime is the context scans run on, cancelled by Stop.
func (p *AnnualReportPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	return p.ctx
}

// max returns the scan limit from the settings panel.
func (p *AnnualReportPlugin) max() int {
	if p.set == nil {
		return defaultMax
	}
	if n := p.set.Int("max_messages"); n >= minMax {
		return n
	}
	return defaultMax
}

func (p *AnnualReportPlugin) dataDir() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dir == "" {
		return filepath.Join("data", p.Name())
	}
	return p.dir
}

// bumpCount increments the generated-report counter (source stats.json).
func (p *AnnualReportPlugin) bumpCount() {
	path := filepath.Join(p.dataDir(), stateFile)
	s := loadState(path)
	s.ReportCount++
	_ = writeJSON(path, s)
}

// ---------------------------------------------------------------- help

func help(tl func(string, string) string) string {
	return tl(
		"📊 **年度报告**\n\n"+
			"**用法**\n"+
			plugin.Code("annualreport")+" 当前所在会话的年度统计（默认去年/今年）\n"+
			plugin.Code("annualreport 2025")+" 指定年份\n"+
			plugin.Code("annualreport 2025-06")+" 指定月份\n"+
			plugin.Code("annualreport 2025-06-01")+" 指定某天\n"+
			plugin.Code("annualreport all")+" 全部历史（受扫描上限约束）\n"+
			plugin.Code("annualreport refresh")+" 忽略缓存重新统计\n"+
			plugin.Code("annualreport me")+" 账号年度报告：会话/黑名单/会员/一言\n"+
			plugin.Code("annualreport help")+" 本帮助\n\n"+
			"**统计内容**\n"+
			"消息总量、发言 Top 10、24 小时活跃分布、星期分布、媒体构成\n"+
			"结果缓存在 data/annualreport/，refresh 强制刷新\n\n"+
			"💡 扫描上限在机器人面板设置",
		"📊 **Annual report**\n\n"+
			"**Usage**\n"+
			plugin.Code("annualreport")+" this chat's stats for the year in review\n"+
			plugin.Code("annualreport 2025")+" a given year\n"+
			plugin.Code("annualreport 2025-06")+" a given month\n"+
			plugin.Code("annualreport 2025-06-01")+" a given day\n"+
			plugin.Code("annualreport all")+" all history (bounded by the scan limit)\n"+
			plugin.Code("annualreport refresh")+" rescan, ignoring the cache\n"+
			plugin.Code("annualreport me")+" your account year in review: chats, blocklist, premium, a quote\n"+
			plugin.Code("annualreport help")+" this help\n\n"+
			"**Stats**\n"+
			"Message totals, top 10 talkers, activity by hour, weekdays, media mix\n"+
			"Reports are cached under data/annualreport/; refresh forces a rescan\n\n"+
			"💡 The scan limit is set in the bot panel")
}

func (p *AnnualReportPlugin) handle(ctx *plugin.CommandContext) error {
	if a := strings.ToLower(ctx.GetArg(0)); len(ctx.Args) == 1 && (a == "help" || a == "h") {
		return ctx.Edit(help(ctx.Tlocal))
	}
	refresh := false
	args := stripRefresh(ctx.Args, &refresh)
	if len(args) > 0 && strings.EqualFold(args[0], "me") {
		return p.handleMe(ctx)
	}
	return p.handleChat(ctx, args, refresh)
}

// stripRefresh removes a refresh token found at any position, reporting
// whether one was seen. "annualreport 2025 refresh" is the same as
// "annualreport refresh 2025".
func stripRefresh(args []string, refresh *bool) []string {
	if refresh == nil {
		return args
	}
	out := args[:0]
	for _, a := range args {
		if strings.EqualFold(strings.TrimSpace(a), "refresh") {
			*refresh = true
			continue
		}
		out = append(out, a)
	}
	return out
}

func (p *AnnualReportPlugin) handleChat(ctx *plugin.CommandContext, args []string, refresh bool) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()))
	}
	chatID := plugin.ChatIDOfInput(peer)
	per, ok := parsePeriod(strings.ToLower(strings.Join(args, " ")), time.Now())
	if !ok {
		return ctx.Edit("❌ " + ctx.Tlocal("无法识别的时间段：", "Unrecognized period: ") +
			plugin.Code(strings.Join(args, " ")) + "\n\n" + help(ctx.Tlocal))
	}
	key := cacheKey(chatID, per.Label, ctx.Lang)
	if !refresh {
		if e, hit := loadCacheEntry(p.dataDir(), key); hit && e.Report != nil {
			p.bumpCount()
			return p.sendReport(ctx, renderChat(ctx.Tlocal, e.Report, cacheNote(ctx.Tlocal, e.GeneratedAt)))
		}
	}

	run := *ctx
	run.Ctx = p.lifetime()
	report, err := p.scanTracked(&run, peer, chatID, per)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("统计失败：", "Scan failed: ") + errText(ctx.Tlocal, err))
	}
	p.bumpCount()
	saveCacheEntry(p.dataDir(), key, &cacheEntry{GeneratedAt: time.Now().Unix(), Report: report})
	return p.sendReport(ctx, renderChat(ctx.Tlocal, report, ""))
}

// sendReport edits the first chunk and posts the rest as new messages.
func (p *AnnualReportPlugin) sendReport(ctx *plugin.CommandContext, chunks []string) error {
	if len(chunks) == 0 {
		return nil
	}
	if err := ctx.Edit(chunks[0]); err != nil {
		return err
	}
	for _, c := range chunks[1:] {
		if _, err := p.host.Send(ctx.Context(), ctx.Message.ChatID, c, 0); err != nil {
			if ctx.Logger != nil {
				ctx.Logger.Warn("annualreport: send chunk failed", "error", err)
			}
			break
		}
	}
	return nil
}

// scanTracked runs scanChat as a waitgroup-tracked unit so Stop can join it.
func (p *AnnualReportPlugin) scanTracked(ctx *plugin.CommandContext, peer tg.InputPeerClass, chatID int64, per period) (*Report, error) {
	p.wg.Add(1)
	defer p.wg.Done()
	return p.scanChat(ctx, peer, chatID, per)
}

// scanChat walks the history of peer and aggregates one period.
func (p *AnnualReportPlugin) scanChat(ctx *plugin.CommandContext, peer tg.InputPeerClass, chatID int64, per period) (*Report, error) {
	_ = ctx.Edit("🔄 " + ctx.Tlocal("正在统计消息，大群可能需要几分钟…", "Scanning messages, this can take a while in big chats…"))
	last := time.Now()
	msgs, names, err := walkHistory(ctx.Context(), ctx.API, peer, p.max(), per.From, func(n int) {
		if time.Since(last) >= progressEvery {
			last = time.Now()
			_ = ctx.Edit(fmt.Sprintf(ctx.Tlocal("🔄 已读取 %d 条消息…", "🔄 %d messages read…"), n))
		}
	})
	if err != nil {
		return nil, err
	}
	report := aggregate(chatID, per.Label, toSamples(msgs, names), per.From, per.To)
	report.Names = names
	report.Chars = countChars(msgs)
	report.Title = chatTitle(ctx.Context(), ctx.API, peer, names, p.selfID())
	return report, nil
}

// countChars sums the text length of the scanned messages.
func countChars(msgs []*tg.Message) int {
	n := 0
	for _, m := range msgs {
		n += len([]rune(m.Message))
	}
	return n
}

func (p *AnnualReportPlugin) selfID() int64 {
	if p.host != nil {
		return p.host.SelfID()
	}
	return 0
}
