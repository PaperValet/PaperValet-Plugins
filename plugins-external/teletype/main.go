package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "teletype",
	Description: "打字机效果",
	DescEN:      "Typewriter effect",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type TeletypePlugin struct {
	host plugin.Host
	set  plugin.Settings
	log  plugin.Logger

	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	stopListen func()
}

func New() *TeletypePlugin { return &TeletypePlugin{} }

func (p *TeletypePlugin) Name() string        { return "teletype" }
func (p *TeletypePlugin) Description() string { return Metadata.Description }
func (p *TeletypePlugin) DescEN() string      { return Metadata.DescEN }

func (p *TeletypePlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.log = p.host.Logger(p.Name())
	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin: p.Name(), Title: "⌨️ 打字机", TitleEN: "⌨️ Teletype",
		Settings: []plugin.Setting{
			{Key: "auto", Label: "自动模式", LabelEN: "Auto mode",
				Hint: "开启后自动给发出的普通消息加打字机效果", HintEN: "When on, your own plain messages get the typewriter effect",
				Kind: plugin.SettingToggle, Default: false},
			{Key: "speed", Label: "打字速度", LabelEN: "Typing speed",
				Hint:   "每字停顿（毫秒），默认 50",
				HintEN: "Pause per character (ms), default 50",
				Kind:   plugin.SettingChoice, Default: "50",
				Choices: []plugin.Choice{
					{Value: "25", Label: "快 (25ms)", LabelEN: "Fast (25ms)"},
					{Value: "50", Label: "标准 (50ms)", LabelEN: "Normal (50ms)"},
					{Value: "100", Label: "慢 (100ms)", LabelEN: "Slow (100ms)"},
					{Value: "200", Label: "很慢 (200ms)", LabelEN: "Very slow (200ms)"},
				}},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "teletype",
		Description: "把文本用打字机效果重新打出来",
		DescEN:      "Retype text with a typewriter effect",
		Usage:       "teletype <文本> · teletype help",
		UsageEN:     "teletype <text> · teletype help",
		Plugin:      p.Name(),
		Category:    "fun",
		Handler:     p.handle,
	})
}

func (p *TeletypePlugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return nil
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.stopListen = p.host.Listen(p.Name(), p.onMessage)
	return nil
}

func (p *TeletypePlugin) Stop(context.Context) error {
	p.mu.Lock()
	stop, cancel := p.stopListen, p.cancel
	p.stopListen, p.cancel = nil, nil
	p.mu.Unlock()
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel()
	}
	p.wg.Wait()
	return nil
}

func (p *TeletypePlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

func (p *TeletypePlugin) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancel != nil
}

func (p *TeletypePlugin) help(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"⌨️ **打字机效果**\n\n"+
			"**用法**\n"+
			plugin.Code("teletype <文本>")+" 把这条命令消息逐字打出来（带 █ 光标）\n"+
			plugin.Code("teletype <N> <文本>")+" 以每字 N 毫秒的速度打（覆盖面板设置）\n"+
			plugin.Code("teletype help")+" 显示本帮助\n\n"+
			"**设置**（机器人面板）\n"+
			"自动模式：给自己发出的普通消息自动加效果\n"+
			"打字速度：每字停顿，默认 50ms\n\n"+
			"💡 文本最长 200 字符",
		"⌨️ **Typewriter**\n\n"+
			"**Usage**\n"+
			plugin.Code("teletype <text>")+" retype this command message character by character (with a █ cursor)\n"+
			plugin.Code("teletype <N> <text>")+" type at N ms per character (overrides the panel setting)\n"+
			plugin.Code("teletype help")+" show this help\n\n"+
			"**Settings** (bot panel)\n"+
			"Auto mode: apply the effect to your own plain messages\n"+
			"Typing speed: pause per character, 50ms by default\n\n"+
			"💡 Text is capped at 200 characters")
}

func (p *TeletypePlugin) handle(ctx *plugin.CommandContext) error {
	if a := strings.ToLower(ctx.GetArg(0)); a == "help" || a == "h" {
		return ctx.Edit(p.help(ctx))
	}
	text := strings.TrimSpace(ctx.GetArgs())
	if text == "" {
		return ctx.Edit("❌ " + ctx.Tlocal("缺少文本。用法：", "Missing text. Usage: ") +
			plugin.Code("teletype <文本> <text>") + "\n\n" + p.help(ctx))
	}
	// Optional leading number overrides the panel speed for this run.
	interval := intervalMs(p.set.String("speed"))
	if fields := strings.Fields(text); len(fields) > 1 {
		if n, err := strconv.Atoi(fields[0]); err == nil && n > 0 {
			interval = intervalMs(fields[0])
			text = strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
		}
	}
	if tooLong(text) {
		return ctx.Edit("❌ " + ctx.Tlocal("文本过长，最多 200 字符。", "Text too long, 200 characters max."))
	}
	if !p.running() {
		return ctx.Edit("❌ " + ctx.Tlocal("插件未运行。", "Plugin is not running."))
	}

	// Handlers run on the update path: animate in the background, keep the
	// message id and peer so the edits survive the handler's context.
	ev := ctx.Message
	api := ctx.API
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return fmt.Errorf("teletype: resolve peer: %w", err)
	}
	msgID := ev.Message.ID
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if err := p.animate(p.lifetime(), api, peer, msgID, text, interval); err != nil {
			p.log.Warn("teletype: animation failed", "error", err)
		}
	}()
	return nil
}

// animate replays the message through the frame sequence. Edits happen
// strictly in order; MESSAGE_NOT_MODIFIED is not an error (the source
// ignores it too), everything else aborts the run.
func (p *TeletypePlugin) animate(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, msgID int, text string, interval time.Duration) error {
	fr := steps(text)
	last := ""
	for i, f := range fr {
		md := frameMD(f)
		if md == last {
			continue // Telegram rejects no-op edits
		}
		if err := editMessage(ctx, api, peer, msgID, md); err != nil {
			if tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
				continue
			}
			return err
		}
		last = md
		if i < len(fr)-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
	}
	return nil
}

// editMessage replaces a message's text, parsing Markdown so the cursor
// block and escaped user text survive.
func editMessage(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, msgID int, md string) error {
	plain, entities := plugin.ParseMarkdown(md, func(int64) (tg.InputUserClass, error) {
		return nil, fmt.Errorf("no resolver")
	})
	req := &tg.MessagesEditMessageRequest{
		Peer:    peer,
		ID:      msgID,
		Message: plain,
	}
	if len(entities) > 0 {
		req.SetEntities(entities)
	}
	_, err := api.MessagesEditMessage(ctx, req)
	return err
}

// onMessage implements auto mode on the update path: filter fast, animate
// in a goroutine.
func (p *TeletypePlugin) onMessage(_ context.Context, ev *plugin.MessageEvent, edited bool) {
	if ev == nil || ev.Message == nil {
		return
	}
	if p.set == nil || !p.set.Bool("auto") {
		return
	}
	sig := autoSignal{
		edited: edited,
		out:    ev.IsOut,
		media:  ev.Media != nil,
		text:   ev.Text,
	}
	if !autoPick(sig, p.host.Prefixes()) {
		return
	}
	if tooLong(ev.Text) || !p.running() {
		return
	}
	peer, err := p.host.PeerResolver().ResolveFromChatID(context.Background(), ev.ChatID)
	if err != nil {
		p.log.Debug("teletype: resolve failed", "chat", ev.ChatID, "error", err)
		return
	}
	api := p.host.API()
	interval := intervalMs(p.set.String("speed"))
	msgID := ev.Message.ID
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if err := p.animate(p.lifetime(), api, peer, msgID, ev.Text, interval); err != nil {
			p.log.Debug("teletype: auto animation failed", "error", err)
		}
	}()
}
