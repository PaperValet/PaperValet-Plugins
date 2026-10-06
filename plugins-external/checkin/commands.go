package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const pendingTTL = 10 * time.Minute

// handle dispatches `checkin` subcommands.
func (p *CheckinPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	// Refresh the live client/resolver under the lock: the scheduler
	// goroutine (runSingle/pollHistory) reads the same fields.
	p.mu.Lock()
	if ctx.API != nil {
		p.api = ctx.API
	}
	if ctx.PeerResolver != nil {
		p.resolver = ctx.PeerResolver
	}
	running := p.cancel != nil
	p.mu.Unlock()
	if !running {
		if err := p.Start(context.Background()); err != nil {
			return ctx.Edit("❌ " + plugin.Escape(err.Error()))
		}
	}

	tl := ctx.Tlocal
	sub := ""
	if len(ctx.Args) > 0 {
		sub = strings.ToLower(ctx.Args[0])
	}
	rest := ctx.Args[1:]
	switch sub {
	case "help":
		return ctx.Edit(p.helpText(tl))
	case "add":
		return p.cmdAdd(ctx, rest)
	case "del":
		return p.cmdDel(ctx, rest)
	case "list":
		return p.cmdList(ctx)
	case "toggle":
		return p.cmdToggle(ctx, rest)
	case "test":
		return p.cmdTest(ctx, rest)
	case "status":
		return p.cmdStatus(ctx)
	case "reset":
		return p.cmdReset(ctx)
	case "":
		return p.cmdRun(ctx)
	}
	return ctx.Edit("❌ " + tl("未知子命令，使用 ", "unknown subcommand, use ") + plugin.Code("checkin help"))
}

func (p *CheckinPlugin) helpText(tl func(zh, en string) string) string {
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("✅ **" + tl("每日自动签到", "Daily Auto Check-In") + "**\n\n")
	b.WriteString("**" + tl("用法", "Usage") + "**\n")
	b.WriteString(line("checkin", "立即执行全部签到", "run all sign-ins now"))
	b.WriteString(line("checkin status", "查看配置和下次执行时间", "show the config and next run"))
	b.WriteString(line("checkin reset", "重置今日状态并重新排期", "reset today's state and reschedule"))
	b.WriteString(line("checkin add <ID> <名称> <目标> [data:回调|text:按钮]", "添加签到目标，然后回复提示消息发送签到命令", "add a target, then reply to the prompt with the sign-in command"))
	b.WriteString(line("checkin del <ID>", "删除签到目标", "delete a target"))
	b.WriteString(line("checkin list", "列出签到目标", "list targets"))
	b.WriteString(line("checkin toggle <ID>", "启用/禁用签到目标", "enable/disable a target"))
	b.WriteString(line("checkin test <ID>", "立即测试一个签到目标", "test one target now"))
	b.WriteString("\n**" + tl("设置", "Settings") + "**\n")
	b.WriteString(tl("执行时间窗口、推送和时区在机器人面板里设置（/menu）", "The run window, push and timezone are set in the bot panel (/menu)") + "\n")
	b.WriteString("\n**" + tl("示例", "Example") + "**\n")
	b.WriteString(plugin.Code("checkin add storm Storm签到 @storm_bot data:checkin") + "\n")
	b.WriteString(tl("然后回复提示消息 ", "then reply to the prompt with ") + plugin.Code("/sign 123456") + "\n\n")
	b.WriteString("💡 " + tl("目标保存在 data/checkin/config.json，重启后自动恢复", "Targets are saved in data/checkin/config.json and restored after restart"))
	return b.String()
}

// cmdAdd starts the reply-based add flow: print a prompt, remember it, and
// capture the sign-in command from the initiating user's reply.
func (p *CheckinPlugin) cmdAdd(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	if len(args) < 3 {
		return ctx.Edit("❌ " + tl("格式: ", "format: ") +
			plugin.Code("checkin add <ID> <名称> <目标> [data:回调|text:按钮]"))
	}
	id, name, target := args[0], args[1], args[2]
	data, text := parseMatcher(args[3:])
	if target[0] != '@' && !chatIDRe.MatchString(target) {
		return ctx.Edit("❌ " + tl("目标必须是 @用户名 或数字 chat id", "the target must be a @username or a numeric chat id"))
	}
	p.mu.Lock()
	dup := len(p.cfg.Targets) >= maxTargets
	for _, t := range p.cfg.Targets {
		if t.ID == id {
			dup = true
			break
		}
	}
	p.mu.Unlock()
	if dup {
		return ctx.Edit("❌ " + tl("签到目标已达上限或 ID 已存在", "too many targets, or the ID already exists"))
	}

	var b strings.Builder
	b.WriteString("📝 " + tl("请**回复此消息**发送签到命令（可含空格，10 分钟内有效）",
		"Please **reply to this message** with the sign-in command (spaces allowed, valid for 10 minutes)") + "\n\n")
	b.WriteString("ID  " + plugin.Code(id) + "\n")
	b.WriteString(tl("名称", "Name") + "  " + plugin.Code(name) + "\n")
	b.WriteString(tl("目标", "Target") + "  " + plugin.Code(target) + "\n")
	if data != "" {
		b.WriteString(tl("回调", "Callback") + "  " + plugin.Code(data) + "\n")
	}
	if text != "" {
		b.WriteString(tl("按钮", "Button") + "  " + plugin.Code(text) + "\n")
	}
	if err := ctx.Edit(b.String()); err != nil {
		return err
	}
	p.mu.Lock()
	p.pending[ctx.Message.ChatID] = &pendingAdd{
		promptID:  ctx.Message.Message.ID,
		senderID:  ctx.Message.UserID,
		expiresAt: time.Now().Add(pendingTTL),
		target:    Target{ID: id, Name: name, Target: target, CallbackData: data, ButtonText: text, Enabled: true},
	}
	p.mu.Unlock()
	return nil
}

// onMessage captures the reply that completes an add flow. Registered with
// host.Listen in Start.
func (p *CheckinPlugin) onMessage(ctx context.Context, ev *plugin.MessageEvent, edited bool) {
	if edited || ev == nil || ev.Message == nil {
		return
	}
	p.mu.Lock()
	pending, ok := p.pending[ev.ChatID]
	p.mu.Unlock()
	if !ok {
		return
	}
	if time.Now().After(pending.expiresAt) {
		p.mu.Lock()
		delete(p.pending, ev.ChatID)
		p.mu.Unlock()
		return
	}
	if ev.IsOut || ev.ReplyToID != pending.promptID || ev.UserID != pending.senderID {
		return
	}
	tl := func(zh, en string) string { return p.langText(ev.UserID, zh, en) }
	command := strings.TrimSpace(ev.Text)
	if command == "" {
		_, _ = p.host.Send(ctx, ev.ChatID,
			"❌ "+tl("签到命令不能为空，请重新回复提示消息", "The sign-in command must not be empty; reply to the prompt again"),
			ev.Message.ID)
		return
	}
	p.mu.Lock()
	delete(p.pending, ev.ChatID)
	updated := false
	t := pending.target
	t.Command = command
	t.Enabled = true
	for i, old := range p.cfg.Targets {
		if old.ID == t.ID {
			p.cfg.Targets[i] = &t
			updated = true
			break
		}
	}
	if !updated {
		p.cfg.Targets = append(p.cfg.Targets, &t)
	}
	if p.cfg.NotifyChat == 0 {
		p.cfg.NotifyChat = ev.ChatID
	}
	err := p.saveLocked()
	p.mu.Unlock()
	p.poke()

	action := tl("已添加签到目标", "Sign-in target added")
	if updated {
		action = tl("已更新签到目标", "Sign-in target updated")
	}
	text := "✅ " + action + " **" + plugin.Escape(t.Name) + "** (" + plugin.Code(t.ID) + ")\n" +
		tl("命令", "Command") + ": " + plugin.Code(command)
	if err != nil {
		text += "\n❌ " + tl("保存失败: ", "save failed: ") + plugin.Escape(err.Error())
	}
	_, _ = p.host.Send(ctx, ev.ChatID, text, ev.Message.ID)
}

// langText picks zh/en for a user through the host's language service.
func (p *CheckinPlugin) langText(userID int64, zh, en string) string {
	if p.host != nil && p.host.Lang(userID) == "en-US" {
		return en
	}
	return zh
}

func (p *CheckinPlugin) cmdDel(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	if len(args) < 1 || args[0] == "" {
		return ctx.Edit("❌ " + tl("格式: ", "format: ") + plugin.Code("checkin del <ID>"))
	}
	id := args[0]
	p.mu.Lock()
	rest := p.cfg.Targets[:0]
	found := false
	for _, t := range p.cfg.Targets {
		if t.ID == id {
			found = true
			continue
		}
		rest = append(rest, t)
	}
	var err error
	if found {
		p.cfg.Targets = rest
		err = p.saveLocked()
	}
	p.mu.Unlock()
	if !found {
		return ctx.Edit("❌ " + tl("未找到目标: ", "target not found: ") + plugin.Code(id))
	}
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + tl("已删除签到目标: ", "deleted target: ") + plugin.Code(id))
}

func (p *CheckinPlugin) cmdToggle(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	if len(args) < 1 || args[0] == "" {
		return ctx.Edit("❌ " + tl("格式: ", "format: ") + plugin.Code("checkin toggle <ID>"))
	}
	id := args[0]
	p.mu.Lock()
	var t *Target
	for _, x := range p.cfg.Targets {
		if x.ID == id {
			t = x
			break
		}
	}
	var err error
	if t != nil {
		t.Enabled = !t.Enabled
		err = p.saveLocked()
	}
	p.mu.Unlock()
	if t == nil {
		return ctx.Edit("❌ " + tl("未找到目标: ", "target not found: ") + plugin.Code(id))
	}
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "save failed: ") + plugin.Escape(err.Error()))
	}
	state := tl("已启用", "enabled")
	if !t.Enabled {
		state = tl("已禁用", "disabled")
	}
	return ctx.Edit("✅ " + state + " " + tl("签到目标", "target") + ": " + plugin.Code(t.Name) + " (" + plugin.Code(id) + ")")
}

func (p *CheckinPlugin) cmdList(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	targets := make([]*Target, len(p.cfg.Targets))
	copy(targets, p.cfg.Targets)
	p.mu.Unlock()
	if len(targets) == 0 {
		return ctx.Edit("📝 " + tl("当前没有签到目标，用 ", "No sign-in targets yet; use ") + plugin.Code("checkin add"))
	}
	enabled := 0
	for _, t := range targets {
		if t != nil && t.Enabled {
			enabled++
		}
	}
	var b strings.Builder
	b.WriteString("📝 **" + tl("签到目标", "Sign-in Targets") + "** (" + fmt.Sprint(enabled) + "/" + fmt.Sprint(len(targets)) + " " + tl("启用", "enabled") + ")\n\n")
	for i, t := range targets {
		if t == nil {
			continue
		}
		status := "🔴"
		if t.Enabled {
			status = "🟢"
		}
		b.WriteString(status + " **" + fmt.Sprint(i+1) + ". " + plugin.Escape(t.Name) + "**\n")
		b.WriteString("ID  " + plugin.Code(t.ID) + "\n")
		b.WriteString(tl("目标", "Target") + "  " + plugin.Code(t.Target) + "\n")
		b.WriteString(tl("命令", "Command") + "  " + plugin.Code(t.Command) + "\n")
		if t.CallbackData != "" {
			b.WriteString(tl("回调", "Callback") + "  " + plugin.Code(t.CallbackData) + "\n")
		}
		if t.ButtonText != "" {
			b.WriteString(tl("按钮", "Button") + "  " + plugin.Code(t.ButtonText) + "\n")
		}
		b.WriteString("\n")
	}
	return ctx.Edit(strings.TrimRight(b.String(), "\n"))
}

func (p *CheckinPlugin) cmdTest(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	if len(args) < 1 || args[0] == "" {
		return ctx.Edit("❌ " + tl("格式: ", "format: ") + plugin.Code("checkin test <ID>"))
	}
	p.mu.Lock()
	var t *Target
	for _, x := range p.cfg.Targets {
		if x.ID == args[0] {
			t = x
			break
		}
	}
	p.mu.Unlock()
	if t == nil {
		return ctx.Edit("❌ " + tl("未找到目标: ", "target not found: ") + plugin.Code(args[0]))
	}
	if err := ctx.Edit("🚀 " + tl("正在测试 ", "Testing ") + plugin.Escape(t.Name) + "..."); err != nil {
		return err
	}
	r := p.runSingle(ctx.Context(), *t)
	word := tl("测试成功", "test succeeded")
	if !r.OK {
		word = tl("测试失败", "test failed")
	}
	return ctx.Edit((map[bool]string{true: "✅", false: "❌"})[r.OK] + " **" + plugin.Escape(t.Name) + "** " + word + "\n\n" +
		plugin.Escape(truncateRunes(r.Message, 3000)))
}

func (p *CheckinPlugin) cmdStatus(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	conf := p.cfg
	loc := p.loc
	p.mu.Unlock()
	enabled := 0
	for _, t := range conf.Targets {
		if t != nil && t.Enabled {
			enabled++
		}
	}
	var b strings.Builder
	b.WriteString("⚙️ **" + tl("CheckIn 配置", "CheckIn Config") + "**\n\n")
	b.WriteString("⏰ " + tl("执行时间", "Run window") + ": " + plugin.Escape(windowText(conf.RunTime, conf.RunTimeEnd)) + "\n")
	b.WriteString("🎲 " + tl("额外随机延迟", "Extra random delay") + ": " + tl(fmt.Sprintf("%d 分钟", conf.RandomDelay), fmt.Sprintf("%d min", conf.RandomDelay)) + "\n")
	if conf.NextRunAt > 0 {
		b.WriteString("📅 " + tl("下次执行", "Next run") + ": " + plugin.Code(time.Unix(conf.NextRunAt, 0).In(loc).Format("2006-01-02 15:04:05")) + "\n")
	} else {
		b.WriteString("📅 " + tl("下次执行", "Next run") + ": " + tl("未计划", "not planned") + "\n")
	}
	if conf.LastRunDate != "" {
		b.WriteString("🗓 " + tl("上次执行", "Last run") + ": " + plugin.Code(conf.LastRunDate) + "\n")
	} else {
		b.WriteString("🗓 " + tl("上次执行", "Last run") + ": " + tl("无", "none") + "\n")
	}
	b.WriteString("🎯 " + tl("启用目标", "Enabled targets") + ": " + fmt.Sprintf("%d/%d", enabled, len(conf.Targets)) + "\n")
	if p.set != nil {
		if chat := p.set.String("notify_chat"); chat != "" {
			b.WriteString("📣 " + tl("汇总推送", "Summary push") + ": " + plugin.Code(chat) + "\n")
		}
	}
	return ctx.Edit(b.String())
}

func (p *CheckinPlugin) cmdReset(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	p.cfg.LastRunDate = ""
	p.rescheduleLocked(time.Now())
	next, loc := p.cfg.NextRunAt, p.loc
	p.mu.Unlock()
	p.poke()
	b := "✅ " + tl("已重置今日状态", "Today's state has been reset") + "\n" + tl("下次执行", "Next run") + ": "
	if next > 0 {
		b += plugin.Code(time.Unix(next, 0).In(loc).Format("2006-01-02 15:04:05"))
	} else {
		b += tl("未计划", "not planned")
	}
	return ctx.Edit(b)
}
