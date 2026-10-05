package main

// pangu plugin — 盘古之白: insert spaces between CJK and Latin/digit text.
// Ported from TeleBox pangu.ts. The `pangu` command tests/controls the
// formatter; a listener auto-spaces the account's own outgoing messages
// (auto mode per the bot-panel setting).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "pangu",
	Description: "中英文之间自动加空格（盘古之白）",
	DescEN:      "Space CJK and Latin text (pangu)",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type PanguPlugin struct {
	host  plugin.Host
	log   plugin.Logger
	store *store
	set   plugin.Settings

	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	stopListen func()
}

func New() *PanguPlugin { return &PanguPlugin{} }

func (p *PanguPlugin) Name() string        { return "pangu" }
func (p *PanguPlugin) Description() string { return Metadata.Description }
func (p *PanguPlugin) DescEN() string      { return Metadata.DescEN }

func (p *PanguPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.log = p.host.Logger(p.Name())
	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return err
	}
	if p.store, err = newStore(dir); err != nil {
		return fmt.Errorf("pangu: load config: %w", err)
	}
	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin: p.Name(), Title: "📝 盘古之白", TitleEN: "📝 Pangu",
		Settings: []plugin.Setting{
			{Key: "auto", Label: "自动模式", LabelEN: "Auto mode",
				Hint:   "开启后自动给发出的普通消息加空格（按各会话开关、白/黑名单和全局模式判断）",
				HintEN: "When on, your own plain messages are spaced automatically (per-chat switches, white/blacklist and global mode apply)",
				Kind:   plugin.SettingToggle, Default: false},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "pangu",
		Description: "在中英文、数字之间加空格（盘古之白），可测试或开关自动格式化",
		DescEN:      "Space CJK and Latin/digits (pangu); test the formatter or toggle auto spacing",
		Usage:       "pangu <文本> · pangu on/off/reset · pangu whitelist add/remove/list · pangu blacklist add/remove/list · pangu stats · pangu help",
		UsageEN:     "pangu <text> · pangu on/off/reset · pangu whitelist add/remove/list · pangu blacklist add/remove/list · pangu stats · pangu help",
		Plugin:      p.Name(),
		Category:    "tools",
		Handler:     p.handle,
	})
}

func (p *PanguPlugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return nil
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.stopListen = p.host.Listen(p.Name(), p.onMessage)
	return nil
}

func (p *PanguPlugin) Stop(context.Context) error {
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

func (p *PanguPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

func (p *PanguPlugin) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancel != nil
}

// ---------------------------------------------------------------- command

var subcommands = map[string]bool{
	"on": true, "off": true, "global": true,
	"whitelist": true, "wl": true, "blacklist": true, "bl": true,
	"stats": true, "stat": true, "reset": true,
}

func (p *PanguPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	if a := strings.ToLower(ctx.GetArg(0)); a == "help" || a == "h" {
		return ctx.Edit(p.help(ctx))
	}

	first := strings.ToLower(ctx.GetArg(0))
	if first != "" && !subcommands[first] {
		// Not a subcommand: treat the whole argument text as test input.
		return p.handleTest(ctx)
	}

	switch first {
	case "":
		return p.showStatus(ctx)
	case "on":
		if err := p.store.setChatMode(ctx.Message.ChatID, true); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		return ctx.Edit("✅ " + ctx.Tlocal("已在当前会话开启 pangu 格式化", "Pangu enabled in this chat"))
	case "off":
		if err := p.store.setChatMode(ctx.Message.ChatID, false); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		return ctx.Edit("❌ " + ctx.Tlocal("已在当前会话关闭 pangu 格式化", "Pangu disabled in this chat"))
	case "reset":
		changed, err := p.store.resetChat(ctx.Message.ChatID)
		if err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		if !changed {
			return ctx.Edit("ℹ️ " + ctx.Tlocal("当前会话未进行特殊设置", "No per-chat setting for this chat"))
		}
		return ctx.Edit("🔄 " + ctx.Tlocal("已重置当前会话设置", "Chat setting reset"))
	case "global":
		return p.handleGlobalMode(ctx)
	case "whitelist", "wl":
		return p.handleList(ctx, "white")
	case "blacklist", "bl":
		return p.handleList(ctx, "black")
	case "stats", "stat":
		return p.showStats(ctx)
	}
	return ctx.Edit(p.help(ctx))
}

func (p *PanguPlugin) handleGlobalMode(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	switch strings.ToLower(ctx.GetArg(1)) {
	case "":
		on := p.store.globalMode()
		return ctx.Edit("🌐 **" + tl("全局模式", "Global mode") + "**  " + onOff(tl, on))
	case "on", "enable", "true":
		if err := p.store.setGlobalMode(true); err != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		return ctx.Edit("✅ " + tl("全局模式已开启", "Global mode enabled"))
	case "off", "disable", "false":
		if err := p.store.setGlobalMode(false); err != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		return ctx.Edit("❌ " + tl("全局模式已关闭", "Global mode disabled"))
	}
	return ctx.Edit("❌ " + tl("无效的参数", "Invalid argument") + "\n\n" +
		tl("使用：", "Usage: ") + plugin.Code("pangu global on/off"))
}

func (p *PanguPlugin) handleList(ctx *plugin.CommandContext, kind string) error {
	tl := ctx.Tlocal
	name, nameEN := "白名单", "Whitelist"
	if kind == "black" {
		name, nameEN = "黑名单", "Blacklist"
	}
	switch strings.ToLower(ctx.GetArg(1)) {
	case "add":
		added, err := p.store.listAdd(kind, ctx.Message.ChatID)
		if err != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		if !added {
			return ctx.Edit("ℹ️ " + tl("当前会话已在"+name+"中", "This chat is already in the "+nameEN))
		}
		return ctx.Edit("✅ " + tl("已将当前会话加入"+name, "Added this chat to the "+nameEN))
	case "remove", "rm", "del":
		removed, err := p.store.listRemove(kind, ctx.Message.ChatID)
		if err != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		if !removed {
			return ctx.Edit("ℹ️ " + tl("当前会话不在"+name+"中", "This chat is not in the "+nameEN))
		}
		return ctx.Edit("✅ " + tl("已将当前会话移出"+name, "Removed this chat from the "+nameEN))
	case "list", "ls", "":
		items := p.store.list(kind)
		if len(items) == 0 {
			return ctx.Edit("📝 " + tl(name+"列表为空", nameEN+" is empty"))
		}
		var b strings.Builder
		fmt.Fprintf(&b, "📝 **%s (%d)**\n\n", tl(name, nameEN), len(items))
		for i, id := range items {
			fmt.Fprintf(&b, "%d. %s\n", i+1, plugin.Code(id))
		}
		return ctx.Edit(strings.TrimRight(b.String(), "\n"))
	}
	return ctx.Edit(p.help(ctx))
}

func (p *PanguPlugin) handleTest(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	text := strings.TrimSpace(ctx.GetArgs())
	if text == "" {
		return ctx.Edit("❌ " + tl("请提供测试文本", "Provide some text") + "\n\n" +
			tl("使用：", "Usage: ") + plugin.Code("pangu 你好World123测试"))
	}
	formatted := spacing(text)
	state := tl("无需调整", "no change needed")
	if formatted != text {
		state = tl("已优化", "adjusted")
	}
	return ctx.Edit("🔤 **" + tl("盘古之白格式化", "Pangu formatting") + "**\n\n" +
		tl("原始文本", "Original") + "\n" + plugin.Pre(text) + "\n\n" +
		tl("格式化后", "Formatted") + "\n" + plugin.Pre(formatted) + "\n\n" +
		tl("状态", "Status") + ": " + state)
}

func onOff(tl func(string, string) string, on bool) string {
	if on {
		return "✅ " + tl("开启", "on")
	}
	return "❌ " + tl("关闭", "off")
}

func (p *PanguPlugin) showStatus(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	chatID := ctx.Message.ChatID
	on, why := p.store.effective(chatID)
	var status string
	switch why {
	case "white":
		status = onOff(tl, on) + tl("（白名单强制）", " (whitelist forced)")
	case "black":
		status = onOff(tl, on) + tl("（黑名单强制）", " (blacklist forced)")
	case "chat":
		status = onOff(tl, on)
	default:
		status = onOff(tl, on) + tl("（全局）", " (global)")
	}
	chatMode, chatSet := p.store.chatMode(chatID)
	chatText := tl("未设置", "not set")
	if chatSet {
		chatText = onOff(tl, chatMode)
	}
	stats, white, black, custom := p.store.statsSnapshot()
	last := tl("从未", "never")
	if stats.LastFormatted > 0 {
		last = time.UnixMilli(stats.LastFormatted).Format("2006-01-02 15:04:05")
	}

	var b strings.Builder
	b.WriteString("📊 **" + tl("盘古之白状态", "Pangu status") + "**\n\n")
	fmt.Fprintf(&b, "%s  %s\n", tl("当前会话", "Chat"), plugin.Code(fmt.Sprint(chatID)))
	fmt.Fprintf(&b, "%s  %s\n", tl("生效状态", "Effective"), status)
	fmt.Fprintf(&b, "%s  %s · %s  %s\n",
		tl("白名单", "Whitelist"), yesNo(tl, p.store.inList(p.store.list("white"), chatID)),
		tl("黑名单", "Blacklist"), yesNo(tl, p.store.inList(p.store.list("black"), chatID)))
	fmt.Fprintf(&b, "%s  %s\n", tl("会话设置", "Chat setting"), chatText)
	fmt.Fprintf(&b, "%s  %s\n", tl("全局模式", "Global mode"), onOff(tl, p.store.globalMode()))
	b.WriteString("\n📈 **" + tl("统计", "Stats") + "**\n")
	fmt.Fprintf(&b, "%s %d · %s %d\n", tl("已格式化消息", "Formatted messages"), stats.FormattedMessages, tl("启用会话", "Enabled chats"), stats.EnabledChats)
	fmt.Fprintf(&b, "%s %s · %s %d · %s %d\n", tl("最后格式化", "Last formatted"), last, tl("白名单", "Whitelist"), white, tl("黑名单", "Blacklist"), black)
	fmt.Fprintf(&b, "%s %d · %s %s\n", tl("自定义会话", "Custom chats"), custom, tl("自动模式", "Auto mode"), onOff(tl, p.autoEnabled()))
	return ctx.Edit(strings.TrimRight(b.String(), "\n"))
}

func (p *PanguPlugin) showStats(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	stats, white, black, custom := p.store.statsSnapshot()
	last := tl("从未", "never")
	if stats.LastFormatted > 0 {
		last = time.UnixMilli(stats.LastFormatted).Format("2006-01-02 15:04:05")
	}
	var b strings.Builder
	b.WriteString("📈 **" + tl("盘古之白统计", "Pangu stats") + "**\n\n")
	fmt.Fprintf(&b, "• %s %d\n", tl("已格式化消息", "Formatted messages"), stats.FormattedMessages)
	fmt.Fprintf(&b, "• %s %d\n", tl("启用会话数", "Enabled chats"), stats.EnabledChats)
	fmt.Fprintf(&b, "• %s %s\n", tl("最后格式化", "Last formatted"), last)
	fmt.Fprintf(&b, "• %s %d · %s %d\n", tl("白名单数量", "Whitelist"), white, tl("黑名单数量", "Blacklist"), black)
	fmt.Fprintf(&b, "• %s %d\n", tl("自定义设置会话数", "Custom chats"), custom)
	return ctx.Edit(strings.TrimRight(b.String(), "\n"))
}

func yesNo(tl func(string, string) string, v bool) string {
	if v {
		return "✅ " + tl("是", "yes")
	}
	return "❌ " + tl("否", "no")
}

func (p *PanguPlugin) help(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("📝 **" + tl("盘古之白 Pangu", "Pangu") + "**\n\n")
	b.WriteString("**" + tl("用法", "Usage") + "**\n")
	b.WriteString(line("pangu <"+tl("文本", "text")+">", "试一试格式化效果", "preview the spacing"))
	b.WriteString(line("pangu on / off", "在当前会话开启/关闭", "enable/disable in this chat"))
	b.WriteString(line("pangu reset", "清除当前会话的设置", "clear this chat's setting"))
	b.WriteString(line("pangu global on / off", "全局模式开关", "global mode switch"))
	b.WriteString(line("pangu whitelist add / remove / list", "管理白名单（当前会话）", "manage the whitelist (this chat)"))
	b.WriteString(line("pangu blacklist add / remove / list", "管理黑名单（当前会话）", "manage the blacklist (this chat)"))
	b.WriteString(line("pangu stats", "查看统计信息", "show statistics"))
	b.WriteString(line("pangu", "查看当前会话状态", "show this chat's status"))
	b.WriteString("\n**" + tl("优先级", "Priority") + "**\n")
	b.WriteString(tl("⚪ 白名单 > ⚫ 黑名单 > 💬 会话设置 > 🌐 全局模式", "⚪ whitelist > ⚫ blacklist > 💬 per-chat > 🌐 global"))
	b.WriteString("\n\n💡 " + tl("自动模式（自动给自己发出的消息加空格）在机器人面板设置", "Auto mode (spacing your own outgoing messages) is set in the bot panel"))
	return b.String()
}

// ---------------------------------------------------------------- listener

func (p *PanguPlugin) autoEnabled() bool {
	return p.set != nil && p.set.Bool("auto")
}

// onMessage runs on the update path: filter fast, edit in a goroutine.
// Mirrors the source listener: only the account's own messages (or Saved
// Messages), never commands, never edits (our own edits come back).
func (p *PanguPlugin) onMessage(_ context.Context, ev *plugin.MessageEvent, edited bool) {
	if edited || ev == nil || ev.Message == nil {
		return
	}
	if !p.autoEnabled() || !ev.IsOut {
		return
	}
	text := ev.Text
	if strings.TrimSpace(text) == "" {
		return
	}
	for _, pref := range p.host.Prefixes() {
		if strings.HasPrefix(text, pref) {
			return
		}
	}
	if strings.HasPrefix(text, "/") {
		return
	}
	on, _ := p.store.effective(ev.ChatID)
	if !on {
		return
	}
	formatted := spacing(text)
	if formatted == text {
		return
	}
	// Copy what the edit needs, then run it off the update path.
	chatID, msgID := ev.ChatID, ev.Message.ID
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if err := p.editMessage(p.lifetime(), chatID, msgID, formatted); err != nil {
			if !tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
				p.log.Debug("pangu: edit failed", "chat", chatID, "msg", msgID, "error", err)
			}
		} else {
			p.store.recordFormatted()
		}
	}()
}

// editMessage replaces a message's text as-is (no Markdown parsing: the
// source sends the raw formatted text and so do we).
func (p *PanguPlugin) editMessage(ctx context.Context, chatID int64, msgID int, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	peer, err := p.host.PeerResolver().ResolveFromChatID(ctx, chatID)
	if err != nil {
		return err
	}
	_, err = p.host.API().MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
		Peer:    peer,
		ID:      msgID,
		Message: text,
	})
	return err
}
