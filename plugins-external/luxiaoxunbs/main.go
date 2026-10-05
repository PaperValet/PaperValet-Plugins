package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "luxiaoxunbs",
	Description: "鲁小迅整点报时（贴纸）",
	DescEN:      "Lu Xiaoxun hourly sticker clock",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type LuxiaoxunbsPlugin struct {
	mu       sync.Mutex
	dir      string
	state    *state
	loaded   bool
	docs     []*tg.Document
	setTitle string
	api      *tg.Client
	resolver plugin.PeerResolver
	logger   plugin.Logger

	cancel context.CancelFunc
	done   chan struct{}
}

func New() *LuxiaoxunbsPlugin { return &LuxiaoxunbsPlugin{} }

func (p *LuxiaoxunbsPlugin) Name() string        { return "luxiaoxunbs" }
func (p *LuxiaoxunbsPlugin) Description() string { return Metadata.Description }
func (p *LuxiaoxunbsPlugin) DescEN() string      { return Metadata.DescEN }

func (p *LuxiaoxunbsPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	host := mgr.Host()
	if host != nil {
		p.api, p.resolver = host.API(), host.PeerResolver()
		p.logger = host.Logger(p.Name())
		dir, err := host.DataDir(p.Name())
		if err != nil {
			return err
		}
		p.dir = dir
	} else {
		p.dir = "data/luxiaoxunbs"
	}
	st, err := load(filepath.Join(p.dir, stateFile))
	if err != nil {
		return fmt.Errorf("luxiaoxunbs: load state: %w", err)
	}
	p.state = st

	if host != nil {
		_ = host.Bot(p.Name()).SetPage(&plugin.Page{
			Title: "订阅列表", TitleEN: "Subscriptions",
			Handle: p.page,
		})
	}

	return mgr.RegisterCommand(&plugin.Command{
		Name:        "luxiaoxunbs",
		Description: "整点报时：每小时发送鲁小迅小时钟贴纸",
		DescEN:      "Hourly Lu Xiaoxun clock sticker",
		Usage:       "luxiaoxunbs sub|unsub|list|reload|help",
		UsageEN:     "luxiaoxunbs sub|unsub|list|reload|help",
		Plugin:      p.Name(),
		Category:    "fun",
		Handler:     p.handle,
	})
}

func (p *LuxiaoxunbsPlugin) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	done := p.done
	p.mu.Unlock()
	go p.loop(runCtx, done)
	return nil
}

func (p *LuxiaoxunbsPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// subCommand maps the first argument to an action, mirroring the source's
// subcommand set (with the Chinese spellings kept, as TeleBox did).
func subCommand(arg string) string {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "", "help", "帮助":
		return "help"
	case "sub", "订阅":
		return "sub"
	case "unsub", "退订":
		return "unsub"
	case "list", "列表":
		return "list"
	case "reload", "重载":
		return "reload"
	}
	return "help"
}

func (p *LuxiaoxunbsPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	p.capture(ctx)
	sub := subCommand(ctx.GetArg(0))
	tl := ctx.Tlocal

	switch sub {
	case "help":
		return ctx.Edit(helpText(ctx))
	case "list":
		return p.cmdList(ctx)
	case "reload":
		return p.cmdReload(ctx)
	case "sub", "unsub":
		ok, err := p.checkPermission(ctx)
		if err != nil {
			return ctx.Edit("❌ " + tl("权限检查失败: ", "Permission check failed: ") + plugin.Escape(err.Error()))
		}
		if !ok {
			return ctx.Edit("❌ " + tl("权限不足，无法操作整点报时", "You don't have permission to manage the hourly report"))
		}
		if sub == "sub" {
			return p.cmdSub(ctx)
		}
		return p.cmdUnsub(ctx)
	}
	return ctx.Edit(helpText(ctx))
}

// capture picks up the live client/resolver/logger from a command context.
func (p *LuxiaoxunbsPlugin) capture(ctx *plugin.CommandContext) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.API != nil {
		p.api = ctx.API
	}
	if ctx.PeerResolver != nil {
		p.resolver = ctx.PeerResolver
	}
	if ctx.Logger != nil {
		p.logger = ctx.Logger
	}
}

// checkPermission mirrors the source: private chats always pass; in groups
// and channels the sender must be an admin (creator or admin rights).
func (p *LuxiaoxunbsPlugin) checkPermission(ctx *plugin.CommandContext) (bool, error) {
	p.mu.Lock()
	api := p.api
	p.mu.Unlock()
	if api == nil {
		return false, fmt.Errorf("no api client")
	}
	sender := ctx.Message.UserID
	if sender == 0 {
		sender = ctx.SelfID
	}
	if sender == 0 {
		return false, fmt.Errorf("cannot determine sender")
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return false, err
	}
	if u, ok := peer.(*tg.InputPeerUser); ok {
		_ = u
		return true, nil
	}
	if _, ok := peer.(*tg.InputPeerSelf); ok {
		return true, nil
	}
	if ch, ok := peer.(*tg.InputPeerChannel); ok {
		return canAdminChannel(ctx.Context(), api, ch, sender)
	}
	if _, ok := peer.(*tg.InputPeerChat); ok {
		// basic group: MessagesGetFullChat, like the ban plugin
		return canAdminBasicGroup(ctx.Context(), api, -ctx.Message.ChatID, sender)
	}
	return false, fmt.Errorf("unknown peer type %T", peer)
}

func (p *LuxiaoxunbsPlugin) cmdSub(ctx *plugin.CommandContext) error {
	key := strconv.FormatInt(ctx.Message.ChatID, 10)
	tl := ctx.Tlocal
	p.mu.Lock()
	if _, ok := p.state.Subs[key]; ok {
		p.mu.Unlock()
		return ctx.Edit("❌ " + tl("你已经订阅了整点报时", "This chat already subscribes to the hourly report"))
	}
	p.state.Subs[key] = time.Now().Unix()
	err := save(filepath.Join(p.dir, stateFile), p.state)
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + tl("你已经成功订阅了整点报时", "Subscribed to the hourly report"))
}

func (p *LuxiaoxunbsPlugin) cmdUnsub(ctx *plugin.CommandContext) error {
	key := strconv.FormatInt(ctx.Message.ChatID, 10)
	tl := ctx.Tlocal
	p.mu.Lock()
	if _, ok := p.state.Subs[key]; !ok {
		p.mu.Unlock()
		return ctx.Edit("❌ " + tl("你还没有订阅整点报时", "This chat does not subscribe to the hourly report"))
	}
	delete(p.state.Subs, key)
	delete(p.state.Last, key)
	err := save(filepath.Join(p.dir, stateFile), p.state)
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + tl("你已经成功退订了整点报时", "Unsubscribed from the hourly report"))
}

func (p *LuxiaoxunbsPlugin) cmdList(ctx *plugin.CommandContext) error {
	key := strconv.FormatInt(ctx.Message.ChatID, 10)
	p.mu.Lock()
	subs := len(p.state.Subs)
	_, isSub := p.state.Subs[key]
	p.mu.Unlock()
	tl := ctx.Tlocal
	var b strings.Builder
	b.WriteString("📊 **" + tl("订阅状态", "Subscription status") + "**\n\n")
	if isSub {
		b.WriteString(tl("当前聊天  ✅ 已订阅", "This chat  ✅ subscribed"))
	} else {
		b.WriteString(tl("当前聊天  ❌ 未订阅", "This chat  ❌ not subscribed"))
	}
	b.WriteString("\n")
	b.WriteString(tl("总订阅数  ", "Total subscribed chats  ") + plugin.Code(subs) + "\n\n")
	if isSub {
		b.WriteString("💡 " + tl("使用 `luxiaoxunbs unsub` 退订", "Use `luxiaoxunbs unsub` to unsubscribe"))
	} else {
		b.WriteString("💡 " + tl("使用 `luxiaoxunbs sub` 订阅", "Use `luxiaoxunbs sub` to subscribe"))
	}
	return ctx.Edit(b.String())
}

func (p *LuxiaoxunbsPlugin) cmdReload(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	api := p.api
	p.mu.Unlock()
	if api == nil {
		return ctx.Edit("❌ " + tl("没有可用的客户端", "No client available"))
	}
	docs, title, err := loadStickerSet(ctx.Context(), api)
	if err != nil {
		return ctx.Edit("❌ " + tl("贴纸包加载失败: ", "Failed to load the sticker set: ") + plugin.Escape(err.Error()))
	}
	p.mu.Lock()
	p.docs, p.setTitle, p.loaded = docs, title, true
	p.mu.Unlock()
	return ctx.Edit("✅ " + tl("贴纸包重新加载成功", "Sticker set reloaded") + "  " + plugin.Code(title) + "\n" +
		tl("共 ", "Stickers: ") + plugin.Code(len(docs)))
}

// loop fires the hourly report at each full hour, like the source cron
// "0 * * * *".
func (p *LuxiaoxunbsPlugin) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		now := time.Now()
		next := nextHourly(now)
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := p.sendAll(ctx); err != nil {
			p.warn("hourly report failed", err)
		}
	}
}

// sendAll sends the hour sticker to every subscribed chat, deleting the
// previous message first and pruning dead chats.
func (p *LuxiaoxunbsPlugin) sendAll(ctx context.Context) error {
	p.mu.Lock()
	if !p.loaded || len(p.docs) == 0 {
		// Lazy-load on first tick, like the source load-on-demand.
		api := p.api
		p.mu.Unlock()
		if api == nil {
			return fmt.Errorf("no api client")
		}
		docs, title, err := loadStickerSet(ctx, api)
		if err != nil {
			return fmt.Errorf("load sticker set: %w", err)
		}
		p.mu.Lock()
		p.docs, p.setTitle, p.loaded = docs, title, true
		p.mu.Unlock()
	}
	now := time.Now()
	idx := hourIndex(now, len(p.docs))
	doc := pickSticker(p.docs, idx)
	if doc == nil {
		return fmt.Errorf("no sticker for hour %d", idx)
	}
	subs := make(map[string]int64, len(p.state.Subs))
	for k, v := range p.state.Subs {
		subs[k] = v
	}
	api := p.api
	p.mu.Unlock()
	if api == nil {
		return fmt.Errorf("no api client")
	}
	if len(subs) == 0 {
		return nil
	}

	var remove []string
	for key := range subs {
		chatID, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			remove = append(remove, key)
			continue
		}
		peer, err := p.resolve(ctx, chatID)
		if err != nil {
			// Unresolvable at send time: keep the sub but record nothing.
			p.warn("resolve chat", fmt.Errorf("%s: %w", key, err))
			continue
		}
		p.mu.Lock()
		lastID := p.state.Last[key]
		p.mu.Unlock()

		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		newID, err := sendStickerToChat(sctx, api, peer, doc, lastID)
		cancel()
		if err != nil {
			if d, ok := floodWait(err); ok {
				p.warn("flood wait", fmt.Errorf("%s: retry in %v", key, d+time.Second))
				continue
			}
			p.warn("send failed", fmt.Errorf("%s: %w", key, err))
			if fatalSendError(err.Error()) || tgerr.Is(err, "CHAT_WRITE_FORBIDDEN", "CHAT_NOT_FOUND", "USER_IS_BLOCKED", "PEER_ID_INVALID", "CHANNEL_PRIVATE", "CHAT_ID_INVALID") {
				remove = append(remove, key)
			}
			continue
		}
		p.mu.Lock()
		p.state.Last[key] = newID
		if err := save(filepath.Join(p.dir, stateFile), p.state); err != nil {
			p.warn("save last message id", err)
		}
		p.mu.Unlock()
	}
	if len(remove) > 0 {
		p.mu.Lock()
		changed := false
		for _, k := range remove {
			if _, ok := p.state.Subs[k]; ok {
				delete(p.state.Subs, k)
				delete(p.state.Last, k)
				changed = true
			}
		}
		if changed {
			if err := save(filepath.Join(p.dir, stateFile), p.state); err != nil {
				p.warn("save after pruning", err)
			}
		}
		p.mu.Unlock()
	}
	return nil
}

func (p *LuxiaoxunbsPlugin) resolve(ctx context.Context, chatID int64) (tg.InputPeerClass, error) {
	p.mu.Lock()
	resolver := p.resolver
	p.mu.Unlock()
	if resolver == nil {
		return nil, fmt.Errorf("no peer resolver")
	}
	return resolver.ResolveFromChatID(ctx, chatID)
}

func (p *LuxiaoxunbsPlugin) warn(what string, err error) {
	p.mu.Lock()
	l := p.logger
	p.mu.Unlock()
	if l != nil {
		l.Warn("luxiaoxunbs: "+what, "error", err)
	}
}

// page renders the subscription list in the companion bot with a remove
// button per chat.
func (p *LuxiaoxunbsPlugin) page(c *plugin.BotContext) (*plugin.View, error) {
	tl := c.Tlocal
	if strings.HasPrefix(c.Data, "rm:") {
		key := strings.TrimPrefix(c.Data, "rm:")
		p.mu.Lock()
		delete(p.state.Subs, key)
		delete(p.state.Last, key)
		_ = save(filepath.Join(p.dir, stateFile), p.state)
		p.mu.Unlock()
		c.Toast(tl("已移除", "Removed"))
	}
	p.mu.Lock()
	keys := make([]string, 0, len(p.state.Subs))
	for k := range p.state.Subs {
		keys = append(keys, k)
	}
	p.mu.Unlock()

	v := &plugin.View{Text: "🕒 **" + tl("整点报时订阅", "Hourly report subscriptions") + "**  " + plugin.Code(len(keys))}
	if len(keys) == 0 {
		v.Text += "\n\n" + tl("还没有订阅；在任意聊天里发 `luxiaoxunbs sub` 开启", "No subscriptions yet; send `luxiaoxunbs sub` in any chat")
		return v, nil
	}
	for _, k := range keys {
		v.Text += "\n" + plugin.Code(k)
		v.Buttons = append(v.Buttons, plugin.Row(plugin.Btn("🗑 "+k, "rm:"+k).Danger()))
	}
	return v, nil
}

func helpText(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("🕒 **" + tl("鲁小迅整点报时", "Lu Xiaoxun hourly report") + "**\n\n")
	b.WriteString("**" + tl("功能", "What it does") + "**\n")
	b.WriteString(tl("• 每小时整点自动发送鲁小迅贴纸报时\n", "• sends a Lu Xiaoxun clock sticker on the hour\n"))
	b.WriteString(tl("• 自动删除上一条报时消息（1小时后）\n", "• the previous sticker is deleted on the next report\n"))
	b.WriteString(tl("• 支持群组和私聊订阅\n", "• works in private chats and groups\n"))
	b.WriteString("\n**" + tl("可用命令", "Commands") + "**\n")
	b.WriteString(line("luxiaoxunbs sub", "订阅整点报时", "subscribe"))
	b.WriteString(line("luxiaoxunbs unsub", "退订整点报时", "unsubscribe"))
	b.WriteString(line("luxiaoxunbs list", "查看订阅状态", "show status"))
	b.WriteString(line("luxiaoxunbs reload", "重新加载贴纸包", "reload the sticker set"))
	b.WriteString("\n**" + tl("注意", "Notes") + "**\n")
	b.WriteString(tl("• 群组订阅需要管理员权限\n", "• group subscriptions need admin rights\n"))
	b.WriteString(tl("• 请先添加贴纸包 ", "• install the sticker set first: ") + plugin.Code("https://t.me/addstickers/luxiaoxunbs"))
	return b.String()
}
