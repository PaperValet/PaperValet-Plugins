package main

// textmode plugin — auto-format the account's own plain text messages.
// Ported from TeleBox mode.ts: edit each outgoing plain message so its whole
// text gets an entity layer — del (strike), bold, italic, underline, mask
// (spoiler), or all four at once. Per-chat mode, a whitelist/blacklist of
// chats and a global default mode (bot-panel setting) pick what applies.
//
// The edit goes through the raw messages.editMessage API with explicit
// entities (PaperValet's Markdown layer has no underline), keeping the text
// content untouched and preserving any entities the message already had.

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "textmode",
	Description: "自动给自己发的消息加格式（删除线/粗体/斜体/下划线/遮罩）",
	DescEN:      "Auto-format your own messages (strike/bold/italic/underline/spoiler)",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type TextmodePlugin struct {
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

func New() *TextmodePlugin { return &TextmodePlugin{} }

func (p *TextmodePlugin) Name() string        { return "textmode" }
func (p *TextmodePlugin) Description() string { return Metadata.Description }
func (p *TextmodePlugin) DescEN() string      { return Metadata.DescEN }

func (p *TextmodePlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.log = p.host.Logger(p.Name())
	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return err
	}
	if p.store, err = newStore(dir); err != nil {
		return err
	}
	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin: p.Name(), Title: "🎨 消息格式", TitleEN: "🎨 Text mode",
		Settings: []plugin.Setting{
			{Key: "global_mode", Label: "全局默认模式", LabelEN: "Default mode",
				Hint:   "会话未单独设置模式时，给自己发出的普通消息套用的格式",
				HintEN: "Format applied to your own plain messages in chats without a per-chat mode",
				Kind:   plugin.SettingChoice, Default: string(modeOff),
				Choices: []plugin.Choice{
					{Value: string(modeOff), Label: "关闭 (off)", LabelEN: "Off"},
					{Value: string(modeDel), Label: "删除线 (del)", LabelEN: "Strikethrough (del)"},
					{Value: string(modeBold), Label: "粗体 (bold)", LabelEN: "Bold"},
					{Value: string(modeItalic), Label: "斜体 (italic)", LabelEN: "Italic"},
					{Value: string(modeUnderline), Label: "下划线 (underline)", LabelEN: "Underline"},
					{Value: string(modeMask), Label: "遮罩 (mask)", LabelEN: "Spoiler (mask)"},
					{Value: string(modeAll), Label: "全格式 (all)", LabelEN: "All"},
				}},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	if err := p.host.Bot(p.Name()).SetPage(&plugin.Page{Title: "格式名单", TitleEN: "Text mode lists", Handle: p.page}); err != nil && err != plugin.ErrBotNotReady {
		return err
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "textmode",
		Description: "自动给自己发的消息加格式（粗体/下划线/遮罩等），按会话设置",
		DescEN:      "Auto-format your own messages (bold/underline/spoiler…), per chat",
		Usage:       "textmode · textmode <off/del/bold/italic/underline/mask/all> · textmode whitelist add/remove/list · textmode blacklist add/remove/list · textmode help",
		UsageEN:     "textmode · textmode <off/del/bold/italic/underline/mask/all> · textmode whitelist add/remove/list · textmode blacklist add/remove/list · textmode help",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *TextmodePlugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopListen != nil {
		return nil
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.stopListen = p.host.Listen(p.Name(), p.onMessage)
	return nil
}

func (p *TextmodePlugin) Stop(context.Context) error {
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

func (p *TextmodePlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

// ---------------------------------------------------------------- command

func (p *TextmodePlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	if a := strings.ToLower(ctx.GetArg(0)); a == "help" || a == "h" {
		return ctx.Edit(p.help(ctx))
	}
	switch first := strings.ToLower(ctx.GetArg(0)); first {
	case "":
		return p.showStatus(ctx)
	case "whitelist", "wl":
		return p.handleList(ctx, listWhite)
	case "blacklist", "bl":
		return p.handleList(ctx, listBlack)
	default:
		m, ok := parseMode(first)
		if !ok {
			return ctx.Edit("❌ " + ctx.Tlocal("未知的模式，可选：", "Unknown mode, options: ") +
				plugin.Code("off del bold italic underline mask all") + "\n\n" + p.help(ctx))
		}
		if err := p.store.setChatMode(ctx.Message.ChatID, m); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		return ctx.Edit("✅ " + ctx.Tlocal("本会话模式已设置为", "Chat mode set to") + " **" + modeLabel(ctx.Tlocal, m) + "**")
	}
}

func (p *TextmodePlugin) showStatus(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	chatID := ctx.Message.ChatID
	global := p.globalMode()
	m := p.store.effective(chatID, global)
	chatMode, chatSet := p.store.chatMode(chatID)
	chatText := modeLabel(tl, chatMode)
	if !chatSet {
		chatText = tl("未设置", "not set")
	}
	var b strings.Builder
	b.WriteString("🎨 **" + tl("消息格式状态", "Text mode status") + "**\n")
	b.WriteString("🔍 " + tl("本会话模式", "Chat mode") + "  " + chatText + "\n")
	b.WriteString("🌐 " + tl("全局默认", "Default mode") + "  **" + modeLabel(tl, global) + "**\n")
	b.WriteString("✨ " + tl("生效模式", "Effective mode") + "  **" + modeLabel(tl, m) + "**\n")
	b.WriteString("⚪ " + tl("白名单", "Whitelist") + "  " + yesNo(tl, p.store.inList(listWhite, chatID)) +
		" · ⚫ " + tl("黑名单", "Blacklist") + "  " + yesNo(tl, p.store.inList(listBlack, chatID)) + "\n")
	b.WriteString("\n" + tl("优先级：白名单 > 黑名单 > 会话模式 > 全局默认（会话 off/未设置时跟随全局）",
		"Priority: whitelist > blacklist > per-chat > default (off/unset chats follow the default)"))
	return ctx.Edit(b.String())
}

func (p *TextmodePlugin) handleList(ctx *plugin.CommandContext, kind int) error {
	tl := ctx.Tlocal
	name, nameEN := "白名单", "Whitelist"
	if kind == listBlack {
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
	case "remove":
		removed, err := p.store.listRemove(kind, ctx.Message.ChatID)
		if err != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		if !removed {
			return ctx.Edit("ℹ️ " + tl("当前会话不在"+name+"中", "This chat is not in the "+nameEN))
		}
		return ctx.Edit("✅ " + tl("已将当前会话移出"+name, "Removed this chat from the "+nameEN))
	case "list", "":
		items := p.store.list(kind)
		if len(items) == 0 {
			return ctx.Edit("📝 " + tl(name+"为空", nameEN+" is empty"))
		}
		var b strings.Builder
		b.WriteString("📝 **" + tl(name, nameEN) + " (" + strconv.Itoa(len(items)) + ")**\n")
		for _, id := range items {
			b.WriteString("> " + plugin.Code(id) + "\n")
		}
		return ctx.Edit(strings.TrimRight(b.String(), "\n"))
	}
	return ctx.Edit(p.help(ctx))
}

func (p *TextmodePlugin) globalMode() mode {
	if p.set == nil {
		return modeOff
	}
	if m, ok := parseMode(p.set.String("global_mode")); ok {
		return m
	}
	return modeOff
}

func yesNo(tl func(string, string) string, v bool) string {
	if v {
		return "✅ " + tl("是", "yes")
	}
	return "❌ " + tl("否", "no")
}

func (p *TextmodePlugin) help(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("🎨 **" + tl("消息格式 Text mode", "Text mode") + "**\n\n")
	b.WriteString("**" + tl("用法", "Usage") + "**\n")
	b.WriteString(line("textmode", "查看本会话模式、全局默认与名单状态", "show this chat's mode, the default and list membership"))
	b.WriteString(line("textmode del / bold / italic / underline / mask / all", "设置本会话模式（删除线/粗体/斜体/下划线/遮罩/全格式）", "set this chat's mode (strike/bold/italic/underline/spoiler/all)"))
	b.WriteString(line("textmode off", "清除本会话模式（跟随全局默认；要单独停用某会话用黑名单）", "clear this chat's mode (follows the default; use the blacklist to stop one chat)"))
	b.WriteString(line("textmode whitelist add / remove / list", "管理白名单（当前会话）", "manage the whitelist (this chat)"))
	b.WriteString(line("textmode blacklist add / remove / list", "管理黑名单（当前会话）", "manage the blacklist (this chat)"))
	b.WriteString("\n**" + tl("优先级", "Priority") + "**\n")
	b.WriteString(tl("⚪ 白名单 > ⚫ 黑名单 > 💬 会话模式 > 🌐 全局默认", "⚪ whitelist > ⚫ blacklist > 💬 per-chat > 🌐 default"))
	b.WriteString("\n\n💡 " + tl("全局默认模式在机器人面板设置；名单和会话设置也可以在面板里管理",
		"The default mode is a bot-panel setting; lists and per-chat settings are manageable in the panel too"))
	return b.String()
}

// ---------------------------------------------------------------- bot page

// page lists per-chat modes and the white/blacklists with delete buttons.
// Callback data stays within the 32-byte cap ("m:"/"w:"/"b:" + chat id).
func (p *TextmodePlugin) page(c *plugin.BotContext) (*plugin.View, error) {
	tl := c.Tlocal
	if op, arg, ok := strings.Cut(c.Data, ":"); ok && arg != "" {
		if id, err := strconv.ParseInt(arg, 10, 64); err == nil {
			var opErr error
			handled := true
			switch op {
			case "w":
				_, opErr = p.store.listRemove(listWhite, id)
			case "b":
				_, opErr = p.store.listRemove(listBlack, id)
			case "m":
				_, opErr = p.store.resetChat(id)
			default:
				handled = false
			}
			if handled {
				if opErr != nil {
					c.Alert(tl("保存失败: ", "Save failed: ") + opErr.Error())
				} else {
					c.Toast(tl("已删除", "Deleted"))
				}
			}
		}
	}
	var b strings.Builder
	b.WriteString("🎨 **" + tl("格式名单", "Text mode lists") + "**\n\n")
	chats := p.store.chatModes()
	b.WriteString("**" + tl("已设置的会话", "Chats with a mode") + " (" + strconv.Itoa(len(chats)) + ")**\n")
	if len(chats) == 0 {
		b.WriteString(tl("暂无", "none") + "\n")
	}
	for _, e := range chats {
		b.WriteString("> " + plugin.Code(e.ID) + "  " + string(e.Mode) + "\n")
	}
	lists := []struct {
		kind    int
		prefix  string
		label   string
		labelEN string
	}{
		{listWhite, "w", "白名单", "Whitelist"},
		{listBlack, "b", "黑名单", "Blacklist"},
	}
	listText := map[int][]string{}
	for _, l := range lists {
		items := p.store.list(l.kind)
		listText[l.kind] = items
		b.WriteString("\n**" + tl(l.label, l.labelEN) + " (" + strconv.Itoa(len(items)) + ")**\n")
		if len(items) == 0 {
			b.WriteString(tl("暂无", "none") + "\n")
		}
		for _, id := range items {
			b.WriteString("> " + plugin.Code(id) + "\n")
		}
	}
	v := &plugin.View{Text: strings.TrimRight(b.String(), "\n")}
	for _, e := range chats {
		v.Buttons = append(v.Buttons, plugin.Row(plugin.Btn("💬 "+e.ID+" → "+string(e.Mode), "m:"+e.ID).Danger()))
	}
	for _, l := range lists {
		for _, id := range listText[l.kind] {
			v.Buttons = append(v.Buttons, plugin.Row(plugin.Btn("🗑 📛 "+id, l.prefix+":"+id).Danger()))
		}
	}
	return v, nil
}
