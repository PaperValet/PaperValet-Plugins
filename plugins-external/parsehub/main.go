// Package parsehub relays links to the @ParseHubot Telegram bot and forwards
// the parsed media/text results back to the chat that asked for them.
//
// The port keeps the behaviour of the TeleBox parsehub plugin: extract links
// from the command text (or the replied-to message), submit each one to the
// bot, poll the bot chat until the progress messages settle into final
// results, then forward them without author attribution.
package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	// botUsername is the parsing bot the plugin depends on.
	botUsername = "ParseHubot"
	// pollInterval is how often the bot chat is polled for new messages.
	pollInterval = 2 * time.Second
	// maxWait is the initial wait for a result; large uploads take a while.
	maxWait = 10 * time.Minute
	// resultIdle ends polling once results are in and activity stops.
	resultIdle = 5 * time.Second
	// progressExtend keeps waiting while the bot still reports progress.
	progressExtend = 2 * time.Minute
	// hardCap bounds the total wait even with progress extensions.
	hardCap = 30 * time.Minute
	// fetchLimit is the bot history window polled each round.
	fetchLimit = 50
	// linkGap separates two submissions to the bot.
	linkGap = 600 * time.Millisecond
	// forwardChunk is Telegram's per-forward limit.
	forwardChunk = 100
	// maxFetchFails is how many consecutive bot-chat fetches may fail
	// (FLOOD_WAIT on the 2s poll included) before the link is given up.
	maxFetchFails = 3
)

var Metadata = &plugin.PluginMetadata{
	Name:        "parsehub",
	Description: "解析链接并转发结果",
	DescEN:      "Parse links and forward the results",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type ParseHubPlugin struct {
	mu      sync.Mutex
	dir     string
	rel     *relay
	logger  plugin.Logger
	set     plugin.Settings
	wg      sync.WaitGroup
	stopped bool

	ctx    context.Context
	cancel context.CancelFunc
}

func New() *ParseHubPlugin {
	return &ParseHubPlugin{dir: "data/parsehub"}
}

func (p *ParseHubPlugin) Name() string        { return "parsehub" }
func (p *ParseHubPlugin) Description() string { return Metadata.Description }
func (p *ParseHubPlugin) DescEN() string      { return Metadata.DescEN }

// lifetime returns the plugin-scoped context background relays run on; it is
// cancelled in Stop so in-flight relays end with the plugin.
func (p *ParseHubPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	return p.ctx
}

func (p *ParseHubPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	host := mgr.Host()
	if host != nil {
		p.mu.Lock()
		p.logger = host.Logger(p.Name())
		p.mu.Unlock()
		set, err := host.Settings(&plugin.SettingsSpec{
			Plugin:  p.Name(),
			Title:   "🔗 链接解析",
			TitleEN: "🔗 Link parsing",
			Settings: []plugin.Setting{
				{
					Key:     "delete_cmd",
					Label:   "完成后删除命令消息",
					LabelEN: "Delete the command when done",
					Hint:    "关闭后在原处保留 parsehub 命令消息",
					HintEN:  "Keep the parsehub command message in the chat",
					Kind:    plugin.SettingToggle,
					Default: true,
				},
				{
					Key:     "muted",
					Label:   "静音机器人会话",
					LabelEN: "Mute the bot chat",
					Hint:    "解除对 @ParseHubot 的屏蔽并永久静音，避免通知打扰",
					HintEN:  "Unblock and permanently mute @ParseHubot to avoid notifications",
					Kind:    plugin.SettingToggle,
					Default: true,
				},
			},
		})
		if err != nil {
			return err
		}
		p.set = set
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "parsehub",
		Description: "解析抖音/小红书/推特等 40+ 平台链接，通过 @ParseHubot 返回视频图文",
		DescEN:      "Parse links from Douyin, XHS, Twitter and more via @ParseHubot",
		Usage:       "parsehub <链接> | 回复含链接的消息 parsehub | parsehub help",
		UsageEN:     "parsehub <link> | reply to a link with parsehub | parsehub help",
		Plugin:      p.Name(),
		Category:    "media",
		Handler:     p.handle,
	})
}

// Start restores the ignore-baseline so a restart never forwards the bot's
// history or its welcome message.
func (p *ParseHubPlugin) Start(_ context.Context) error {
	if err := p.load(); err != nil && p.logger != nil {
		p.logger.Warn("parsehub: load state", "error", err)
	}
	p.mu.Lock()
	p.stopped = false
	p.mu.Unlock()
	p.lifetime()
	return nil
}

func (p *ParseHubPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	p.stopped = true
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

func (p *ParseHubPlugin) handle(ctx *plugin.CommandContext) error {
	if len(ctx.Args) > 0 && strings.EqualFold(ctx.Args[0], "help") {
		return ctx.Edit(p.help(ctx))
	}
	if len(ctx.Args) == 0 && !ctx.Message.IsReply {
		return ctx.Edit(p.help(ctx))
	}

	links := extractLinks(ctx.GetArgs())
	if ctx.Message.IsReply && ctx.Message.ReplyToID > 0 {
		if reply, err := ctx.ReplyMessage(); err == nil {
			links = mergeLinks(links, extractLinks(reply.Message))
		} else if p.logger != nil {
			p.logger.Warn("parsehub: reply fetch failed", "error", err)
		}
	}
	if len(links) == 0 {
		return ctx.Edit("❌ " + ctx.Tlocal(
			"没有找到链接，请直接附带链接或回复一条含链接的消息",
			"No link found; append one or reply to a message that has one"))
	}

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return ctx.Edit("❌ " + ctx.Tlocal("插件正在停止，请稍后再试", "The plugin is stopping, try again later"))
	}
	p.wg.Add(1)
	p.mu.Unlock()
	run := *ctx
	run.Ctx = p.lifetime() // plugin-scoped: cancelled in Stop
	go func() {
		defer p.wg.Done()
		p.runLinks(&run, links)
	}()
	return nil
}
