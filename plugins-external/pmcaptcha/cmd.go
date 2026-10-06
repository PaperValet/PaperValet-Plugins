package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// handle routes pmc subcommands.
func (p *PMPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	tl := ctx.Tlocal
	sub := firstArg(ctx.Args)
	if sub == "" || sub == "help" || sub == "h" {
		return ctx.Edit(helpText(tl))
	}
	switch sub {
	case "status":
		return p.cmdStatus(ctx)
	case "test":
		return p.cmdTest(ctx)
	case "wl":
		return p.cmdWhitelist(ctx)
	case "add":
		return p.cmdWlAdd(ctx, 1)
	case "del":
		return p.cmdWlDel(ctx, 1)
	case "record":
		return p.cmdRecord(ctx)
	}
	return ctx.Edit("❌ " + tl("未知子命令：", "Unknown subcommand: ") + plugin.Code(sub) + "\n" +
		tl("可用命令见 ", "See ") + plugin.Code("pmc help"))
}

func helpText(tl func(string, string) string) string {
	var b strings.Builder
	b.WriteString("🔒 **" + tl("私信验证码 (pmcaptcha)", "PM captcha (pmcaptcha)") + "**\n\n")
	b.WriteString("**" + tl("查看", "View") + "**\n")
	b.WriteString(plugin.Code("pmc status") + "  " + tl("查看配置与运行状态", "show settings and state") + "\n")
	b.WriteString(plugin.Code("pmc record") + "  " + tl("通过/失败记录摘要", "pass/fail record summary") + "\n\n")
	b.WriteString("**" + tl("白名单", "Whitelist") + "**\n")
	b.WriteString(plugin.Code(tl("回复消息 + pmc add", "reply + pmc add")) + "  " + tl("把对方加入白名单", "whitelist that user") + "\n")
	b.WriteString(plugin.Code(tl("pmc wl add <ID/@用户>", "pmc wl add <ID/@user>")) + "  " + tl("加入白名单", "add to whitelist") + "\n")
	b.WriteString(plugin.Code(tl("pmc wl del <ID/@用户>", "pmc wl del <ID/@user>")) + "  " + tl("移出白名单", "remove from whitelist") + "\n")
	b.WriteString(plugin.Code("pmc wl clear") + "  " + tl("清空白名单", "clear the whitelist") + "\n")
	b.WriteString(plugin.Code(tl("pmc wl pass <ID/@用户>", "pmc wl pass <ID/@user>")) + "  " + tl("标记通过并加白名单", "mark passed and whitelist") + "\n")
	b.WriteString(plugin.Code("pmc wl") + "  " + tl("查看白名单", "show the whitelist") + "\n\n")
	b.WriteString("**" + tl("记录", "Records") + "**\n")
	b.WriteString(plugin.Code("pmc record verified") + "  " + tl("通过列表", "passed list") + "\n")
	b.WriteString(plugin.Code("pmc record failed") + "  " + tl("失败列表", "failed list") + "\n")
	b.WriteString(plugin.Code(tl("pmc record del verified|failed <ID|all>", "pmc record del verified|failed <ID|all>")) + "  " + tl("删除记录", "delete records") + "\n\n")
	b.WriteString("**" + tl("测试", "Test") + "**\n")
	b.WriteString(plugin.Code(tl("pmc test [ID]", "pmc test [ID]")) + "  " + tl("给自己发一道验证题（试流程）", "send yourself a challenge to test the flow") + "\n\n")
	b.WriteString("💡 " + tl("开关、超时、次数、失败/通过动作在机器人面板设置；陌生人私信默认自动静音+归档并出题，答对放行，答错/超时执行失败动作",
		"Toggle, timeout, tries and actions live in the bot panel; stranger DMs are muted+archived and challenged by default; passing unlocks, failing triggers the fail action"))
	return b.String()
}

// cmdStatus shows settings and counters.
func (p *PMPlugin) cmdStatus(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	o := p.options()
	p.mu.Lock()
	pending := len(p.challenges)
	p.mu.Unlock()
	wl, nv, nf := p.records.counts()
	onOff := func(b bool) string {
		if b {
			return "✅"
		}
		return "❌"
	}
	var b strings.Builder
	b.WriteString("📊 **" + tl("私信验证状态", "PM captcha status") + "**\n\n")
	b.WriteString(tl("插件", "Plugin") + "  " + onOff(o.enabled) + "\n")
	b.WriteString(tl("发题", "Challenge") + "  " + onOff(o.captchaOn) + "\n")
	b.WriteString(tl("超时", "Timeout") + "  " + plugin.Code(timeoutLabel(o.timeout)) + "\n")
	b.WriteString(tl("次数", "Tries") + "  " + plugin.Code(triesLabel(o.tries)) + "\n")
	b.WriteString(tl("失败动作", "On failure") + "  " + plugin.Escape(failActionLabel(o.failAction)) + "\n")
	b.WriteString(tl("通过动作", "On pass") + "  " + plugin.Escape(passActionLabel(o.passAction)) + "\n")
	if o.unlockHours > 0 {
		b.WriteString(tl("通过有效", "Pass valid") + "  " + fmt.Sprintf("**%d h**", o.unlockHours) + "\n")
	}
	b.WriteString("\n" + tl("白名单", "Whitelist") + "  " + plugin.Code(wl) + "\n")
	b.WriteString(tl("待验证", "Pending") + "  " + plugin.Code(pending) + "\n")
	b.WriteString(tl("通过记录", "Passed") + "  " + plugin.Code(nv) + "\n")
	b.WriteString(tl("失败记录", "Failed") + "  " + plugin.Code(nf))
	return ctx.Edit(b.String())
}

func timeoutLabel(secs int) string {
	if secs > 0 {
		return fmt.Sprintf("%d s", secs)
	}
	return "∞"
}

func triesLabel(n int) string {
	if n > 0 {
		return fmt.Sprintf("%d", n)
	}
	return "∞"
}

// cmdTest sends the caller a challenge to try the flow (owner-only).
func (p *PMPlugin) cmdTest(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	target := ctx.SelfID
	if arg := ctx.GetArg(1); arg != "" {
		id, _, err := p.resolveArg(ctx.Context(), arg)
		if err != nil {
			return ctx.Edit("❌ " + floodText(tl, err))
		}
		target = id
	}
	if target == ctx.SelfID {
		// Challenging the owner can never be answered: the owner's own
		// messages take the outbound fast path and never reach
		// handleReply, so the challenge would time out and run the
		// failure actions against the owner's own chat (mute+archive,
		// and with fail_action=delete/report even delete-history or a
		// self-report). Refuse self-targets outright.
		return ctx.Edit("❌ " + tl("不能对自己发验证题（自己无法作答，超时会触发失败动作）",
			"Cannot test on yourself: you can't answer, and timeout would run the fail actions on your own chat"))
	}
	p.removeChallenge(target)
	p.sendChallenge(ctx.Context(), target)
	if !p.hasChallenge(target) {
		return ctx.Edit("❌ " + tl("发送验证题失败", "Failed to send the challenge"))
	}
	return ctx.Edit("✅ " + tl("已发送验证题，回复答案即可测试", "Challenge sent; reply with the answer to test"))
}

// targetOf picks the target user for wl subcommands: reply first, then arg.
func (p *PMPlugin) targetOf(ctx *plugin.CommandContext, argIndex int) (int64, string, error) {
	if ctx.Message.ReplyToID != 0 {
		if peer, err := ctx.ResolvePeer(); err == nil {
			msgs, users, _, gerr := plugin.GetMessages(ctx.Context(), ctx.API, peer, ctx.Message.ReplyToID)
			if gerr == nil {
				for _, m := range msgs {
					if m.ID != ctx.Message.ReplyToID {
						continue
					}
					if from, ok := m.FromID.(*tg.PeerUser); ok {
						name := ""
						for _, us := range users {
							if u, ok := us.(*tg.User); ok && u.ID == from.UserID {
								name = nameOfUser(u)
							}
						}
						return from.UserID, name, nil
					}
				}
			}
		}
	}
	arg := ctx.GetArg(argIndex)
	if arg == "" {
		return 0, "", errNoTarget
	}
	id, u, err := p.resolveArg(ctx.Context(), arg)
	if err != nil {
		return 0, "", err
	}
	name := ""
	if u != nil {
		name = nameOfUser(u)
	}
	return id, name, nil
}

// cmdWhitelist routes pmc wl …
func (p *PMPlugin) cmdWhitelist(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	action := firstArg(ctx.Args[1:])
	switch action {
	case "", "list":
		return p.cmdWlList(ctx)
	case "add":
		return p.cmdWlAdd(ctx, 2)
	case "del":
		return p.cmdWlDel(ctx, 2)
	case "clear":
		p.records.clearWhitelist()
		if err := p.saveRecords(); err != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		return ctx.Edit("✅ " + tl("白名单已清空", "Whitelist cleared"))
	case "pass":
		return p.cmdWlPass(ctx)
	}
	return ctx.Edit("❌ " + tl("未知子命令：", "Unknown subcommand: ") + plugin.Code(action))
}

func (p *PMPlugin) cmdWlAdd(ctx *plugin.CommandContext, argIndex int) error {
	tl := ctx.Tlocal
	id, name, err := p.targetOf(ctx, argIndex)
	if err != nil {
		return p.targetErr(ctx, err)
	}
	if u := p.userInfo(ctx.Context(), id); u != nil && u.Bot {
		return ctx.Edit("❌ " + tl("无法将机器人加入白名单", "Bots cannot be whitelisted"))
	}
	p.records.addWhitelist(id)
	if name == "" {
		name = p.displayName(ctx.Context(), id)
	}
	if err := p.saveRecords(); err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + fmt.Sprintf(tl("%s 已加入白名单", "%s whitelisted"), userLink(id, name)))
}

func (p *PMPlugin) cmdWlDel(ctx *plugin.CommandContext, argIndex int) error {
	tl := ctx.Tlocal
	id, _, err := p.targetOf(ctx, argIndex)
	if err != nil {
		return p.targetErr(ctx, err)
	}
	p.records.delWhitelist(id)
	if err := p.saveRecords(); err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + fmt.Sprintf(tl("%s 已从白名单移除", "%s removed from the whitelist"), userLink(id, p.displayName(ctx.Context(), id))))
}

func (p *PMPlugin) cmdWlPass(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	id, name, err := p.targetOf(ctx, 2)
	if err != nil {
		return p.targetErr(ctx, err)
	}
	if old := p.removeChallenge(id); old != nil {
		p.deleteMsgs(ctx.Context(), id, old.msgIDs)
	}
	p.records.addWhitelist(id)
	p.records.addVerified(id, p.displayName(ctx.Context(), id), "")
	p.records.delFailed(id)
	if name == "" {
		name = p.displayName(ctx.Context(), id)
	}
	if err := p.saveRecords(); err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + fmt.Sprintf(tl("%s 已手动通过并加入白名单", "%s manually passed and whitelisted"), userLink(id, name)))
}

func (p *PMPlugin) cmdWlList(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	ids := p.records.whitelistIDs()
	if len(ids) == 0 {
		return ctx.Edit("📋 **" + tl("白名单为空", "Whitelist is empty") + "**")
	}
	var b strings.Builder
	b.WriteString("📋 **" + fmt.Sprintf(tl("白名单 (%d)", "Whitelist (%d)"), len(ids)) + "**\n\n")
	for _, id := range ids {
		b.WriteString("> " + userLink(id, p.displayName(ctx.Context(), id)) + "\n")
	}
	return ctx.Edit(clipLines(b.String(), 3900))
}

func (p *PMPlugin) targetErr(ctx *plugin.CommandContext, err error) error {
	tl := ctx.Tlocal
	if err == errNoTarget {
		return ctx.Edit("❌ " + tl("指定目标：回复消息，或用 @用户名 / 用户ID", "Give a target: reply, @username or user ID"))
	}
	return ctx.Edit("❌ " + floodText(tl, err))
}

// cmdRecord routes pmc record …
func (p *PMPlugin) cmdRecord(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	sub := firstArg(ctx.Args[1:])
	switch sub {
	case "", "list":
		var b strings.Builder
		b.WriteString("📋 **" + tl("验证记录摘要", "Verification summary") + "**\n\n")
		_, nv, nf := p.records.counts()
		b.WriteString(fmt.Sprintf(tl("✅ 通过 **%d** 人\n❌ 失败 **%d** 人", "✅ Passed **%d**\n❌ Failed **%d**"), nv, nf) + "\n\n")
		b.WriteString(plugin.Code("pmc record verified") + "  " + tl("通过列表", "passed list") + "\n")
		b.WriteString(plugin.Code("pmc record failed") + "  " + tl("失败列表", "failed list"))
		return ctx.Edit(b.String())
	case "verified":
		return p.recordList(ctx, true)
	case "failed":
		return p.recordList(ctx, false)
	case "del":
		target := firstArg(ctx.Args[2:])
		what := ctx.GetArg(3)
		if target != "verified" && target != "failed" {
			return ctx.Edit("❌ " + tl("用法：", "Usage: ") + plugin.Code("pmc record del verified|failed <ID|all>"))
		}
		if what == "all" {
			if target == "verified" {
				p.records.clearVerified()
			} else {
				p.records.clearFailed()
			}
			if err := p.saveRecords(); err != nil {
				return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
			}
			return ctx.Edit("✅ " + tl("记录已清空", "Records cleared"))
		}
		id, err := parseInt64(what)
		if err != nil || id <= 0 {
			return ctx.Edit("❌ " + tl("无效 ID：", "Invalid ID: ") + plugin.Code(what))
		}
		if target == "verified" {
			p.records.delVerified(id)
		} else {
			p.records.delFailed(id)
		}
		if err := p.saveRecords(); err != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		return ctx.Edit("✅ " + fmt.Sprintf(tl("用户 %s 的记录已删除", "User %s's record deleted"), plugin.Code(id)))
	default:
		return ctx.Edit("❌ " + tl("未知子命令：", "Unknown subcommand: ") + plugin.Code(sub))
	}
}

func (p *PMPlugin) recordList(ctx *plugin.CommandContext, verified bool) error {
	tl := ctx.Tlocal
	var b strings.Builder
	if verified {
		ids := p.records.verifiedIDs()
		shown := 0
		b.WriteString("✅ **" + tl("验证通过", "Passed") + "**\n")
		for _, id := range ids {
			if p.records.inWhitelist(id) {
				continue
			}
			v, _ := p.records.getVerified(id)
			shown++
			b.WriteString("\n> " + userLink(id, orDefault(v.Name, strconv.FormatInt(id, 10))) + "\n> `" + fmtTime(v.Time) + "`\n")
		}
		if shown == 0 {
			return ctx.Edit("📋 **" + tl("验证通过记录为空", "No passed records") + "**")
		}
		b.WriteString("\n" + fmt.Sprintf(tl("共 %d 人", "%d total"), shown))
	} else {
		ids := p.records.failedIDs()
		shown := 0
		b.WriteString("❌ **" + tl("验证失败", "Failed") + "**\n")
		for _, id := range ids {
			if p.records.inWhitelist(id) || p.records.isVerified(id) {
				continue
			}
			f, _ := p.records.getFailed(id)
			shown++
			b.WriteString("\n> " + userLink(id, orDefault(f.Name, strconv.FormatInt(id, 10))) + "\n> `" + fmtTime(f.Time) + "` · " + reasonLabel(tl, f.Reason) + "\n")
		}
		if shown == 0 {
			return ctx.Edit("📋 **" + tl("验证失败记录为空", "No failed records") + "**")
		}
		b.WriteString("\n" + fmt.Sprintf(tl("共 %d 人", "%d total"), shown))
	}
	return ctx.Edit(clipLines(b.String(), 3900))
}

func reasonLabel(tl func(string, string) string, reason string) string {
	switch reason {
	case reasonTimeout:
		return "⏰ " + tl("超时", "timeout")
	case reasonMaxTries:
		return "❌ " + tl("次数耗尽", "max tries")
	}
	return reason
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// clipLines cuts s to at most n runes at a line boundary, so no Markdown
// span is split.
func clipLines(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	var b strings.Builder
	size := 0
	for _, l := range strings.Split(s, "\n") {
		ln := len([]rune(l)) + 1
		if size+ln > n-2 {
			break
		}
		b.WriteString(l + "\n")
		size += ln
	}
	return strings.TrimRight(b.String(), "\n") + "\n…"
}
