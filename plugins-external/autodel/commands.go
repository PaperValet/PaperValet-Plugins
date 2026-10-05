package main

import (
	"strconv"
	"strings"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func (p *AutoDelPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	sub := strings.ToLower(ctx.GetArg(0))
	if sub == "help" {
		return ctx.Edit(helpText(ctx))
	}
	if sub == "cancel" {
		return p.cmdCancel(ctx, ctx.HasArg("global"))
	}
	if sub == "list" {
		return p.cmdList(ctx)
	}
	if sub == "status" {
		return p.cmdStatus(ctx)
	}
	if sub == "cmd" {
		return p.cmdRule(ctx, ctx.Args[1:])
	}
	// default: try to parse a duration from the joined args (supports
	// "autodel 5 分钟" and "autodel 30s global")
	raw := strings.Join(ctx.Args, " ")
	global := false
	sec := 0
	for _, f := range strings.Fields(raw) {
		if strings.EqualFold(f, "global") {
			global = true
			continue
		}
		if sec == 0 {
			sec = parseDuration(f)
		}
	}
	if sec == 0 {
		return ctx.Edit(helpText(ctx))
	}
	return p.cmdSetTTL(ctx, sec, global)
}

// cmdSetTTL stores the TTL for this chat (or the global entry).
func (p *AutoDelPlugin) cmdSetTTL(ctx *plugin.CommandContext, sec int, global bool) error {
	tl := ctx.Tlocal
	if sec < minTTLSeconds {
		return ctx.Edit("❌ " + tl("自动删除时间不能少于 5 秒", "Auto-delete delay must be at least 5 seconds"))
	}
	key := strconvI(ctx.Message.ChatID)
	scope := tl("本聊天", "this chat")
	if global {
		key, scope = "0", tl("全局", "global")
	}
	p.mu.Lock()
	p.ttl[key] = sec
	err := p.saveTTLLocked()
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ **" + tl("定时删除已设置", "Timed delete set") + "**\n" +
		tl("范围", "Scope") + "  " + scope + "\n" +
		tl("延迟", "Delay") + "  " + plugin.Code(formatSeconds(sec)) + "\n" +
		"⚠️ " + tl("只会删除您自己发送的消息", "Only your own messages are deleted"))
}

func (p *AutoDelPlugin) cmdCancel(ctx *plugin.CommandContext, global bool) error {
	tl := ctx.Tlocal
	key := strconvI(ctx.Message.ChatID)
	scope := tl("本聊天", "this chat")
	if global {
		key, scope = "0", tl("全局", "global")
	}
	p.mu.Lock()
	_, ok := p.ttl[key]
	if ok {
		delete(p.ttl, key)
	}
	var err error
	if ok {
		err = p.saveTTLLocked()
	}
	p.mu.Unlock()
	if !ok {
		return ctx.Edit("❌ " + tl("未开启自动删除", "Auto-delete is not enabled") + " (" + scope + ")")
	}
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("🗑 ✅ " + tl("已取消自动删除", "Auto-delete cancelled") + " (" + scope + ")")
}

// cmdList shows TTL entries and a rule summary.
func (p *AutoDelPlugin) cmdList(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	ttl := make(map[string]int, len(p.ttl))
	for k, v := range p.ttl {
		ttl[k] = v
	}
	rules := append([]CmdRule(nil), p.rules...)
	p.mu.Unlock()

	var b strings.Builder
	b.WriteString("📋 **" + tl("自动删除设置", "Auto-delete settings") + "**\n\n⏱ **" + tl("定时删除 (TTL)", "Timed delete (TTL)") + "**\n")
	if len(ttl) == 0 {
		b.WriteString(tl("暂无设置", "none") + "\n")
	} else {
		for _, k := range sortedNumKeys(ttl) {
			label := tl("全局", "global")
			if k != "0" {
				label = tl("聊天 ", "chat ") + plugin.Code(k)
			}
			b.WriteString("• " + label + "  " + plugin.Code(formatSeconds(ttl[k])) + "\n")
		}
	}
	b.WriteString("\n⚙️ **" + tl("命令规则", "Command rules") + "**  " + plugin.Code(strconv.Itoa(len(rules))) + "\n")
	for _, r := range rules {
		b.WriteString("• #" + plugin.Escape(r.ID) + " " + ruleText(r) + "\n")
	}
	b.WriteString("\n💡 " + tl("命令输出清理在机器人面板设置", "Command-output cleaning is set in the bot panel") + "\n" +
		"🔄 " + tl("同时删除响应", "also deletes responses") + " · 🎯 " + tl("仅无参数调用", "bare calls only"))
	out := strings.TrimRight(b.String(), "\n")
	if len([]rune(out)) > 3900 {
		out = clipLines(out, 3900)
	}
	return ctx.Edit(out)
}

func (p *AutoDelPlugin) cmdStatus(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	chatKey := strconvI(ctx.Message.ChatID)
	chatSec, chatOK := p.ttl[chatKey]
	globalSec, globalOK := p.ttl["0"]
	nRules := len(p.rules)
	nPending := len(p.pending)
	p.mu.Unlock()

	var b strings.Builder
	b.WriteString("📊 **" + tl("自动删除状态", "Auto-delete status") + "**\n\n")
	if chatOK {
		b.WriteString(tl("本聊天", "This chat") + "  " + plugin.Code(formatSeconds(chatSec)) + "\n")
	} else if globalOK {
		b.WriteString(tl("本聊天", "This chat") + "  " + tl("继承全局 ", "inherits global ") + plugin.Code(formatSeconds(globalSec)) + "\n")
	} else {
		b.WriteString(tl("本聊天", "This chat") + "  " + tl("未设置", "not set") + "\n")
	}
	if globalOK {
		b.WriteString(tl("全局", "Global") + "  " + plugin.Code(formatSeconds(globalSec)) + "\n")
	} else {
		b.WriteString(tl("全局", "Global") + "  " + tl("未设置", "not set") + "\n")
	}
	enabled := p.set != nil && p.set.Bool("cmd_enabled")
	if enabled {
		b.WriteString(tl("命令输出清理", "Command cleaning") + "  ✅ " + tl("已开启", "on") + "\n")
	} else {
		b.WriteString(tl("命令输出清理", "Command cleaning") + "  ⭕ " + tl("已关闭", "off") + "\n")
	}
	b.WriteString(tl("规则数", "Rules") + "  " + plugin.Code(strconv.Itoa(nRules)) + "\n")
	b.WriteString(tl("待删除", "Pending") + "  " + plugin.Code(strconv.Itoa(nPending)))
	return ctx.Edit(b.String())
}

// cmdRule handles autodel cmd add|del|reset.
func (p *AutoDelPlugin) cmdRule(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	if len(args) == 0 {
		return ctx.Edit("❌ " + tl("用法: ", "Usage: ") + plugin.Code("autodel cmd add|del|reset …"))
	}
	switch strings.ToLower(args[0]) {
	case "add":
		return p.cmdRuleAdd(ctx, args[1:])
	case "del":
		return p.cmdRuleDel(ctx, args[1:])
	case "reset":
		return p.cmdRuleReset(ctx)
	}
	return ctx.Edit("❌ " + tl("用法: ", "Usage: ") + plugin.Code("autodel cmd add|del|reset …"))
}

func (p *AutoDelPlugin) cmdRuleAdd(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	var (
		resp, exact bool
		rest        []string
	)
	for _, a := range args {
		switch strings.ToLower(a) {
		case "-r", "--response":
			resp = true
		case "-e", "--exact":
			exact = true
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) < 2 {
		return ctx.Edit("❌ " + tl("参数不足，用法: ", "Not enough arguments, usage: ") +
			plugin.Code("autodel cmd add <"+tl("命令", "cmd")+"> <"+tl("秒", "secs")+"> ["+tl("参数…", "params…")+"] [-r] [-e]"))
	}
	command := strings.ToLower(rest[0])
	delay, err := strconv.Atoi(rest[1])
	if err != nil || delay < 1 {
		return ctx.Edit("❌ " + tl("延迟必须是正整数（秒）", "Delay must be a positive integer (seconds)"))
	}
	params := rest[2:]
	if exact && len(params) > 0 {
		return ctx.Edit("❌ " + tl("精确匹配 (-e) 不能与参数同时使用", "Exact match (-e) cannot be combined with parameters"))
	}

	rule := CmdRule{Command: command, Delay: delay, Parameters: params, DeleteResponse: resp, ExactMatch: exact}
	p.mu.Lock()
	var conflict *CmdRule
	// conflicts: same command+param but different behaviour; same bare-command mode
	for _, r := range p.rules {
		if r.Command != command {
			continue
		}
		if len(params) > 0 && len(r.Parameters) > 0 {
			for _, q := range params {
				if contains(r.Parameters, q) && (r.Delay != delay || r.DeleteResponse != resp) {
					conflict = &r
				}
			}
		}
		if len(params) == 0 && len(r.Parameters) == 0 && r.ExactMatch == exact && (r.Delay != delay || r.DeleteResponse != resp) {
			conflict = &r
		}
	}
	var merged bool
	if conflict == nil {
		// merge params into an identical command+delay+flags rule
		for i := range p.rules {
			r := &p.rules[i]
			if r.Command == command && r.Delay == delay && r.DeleteResponse == resp && r.ExactMatch == exact && len(params) > 0 {
				for _, q := range params {
					if !contains(r.Parameters, q) {
						r.Parameters = append(r.Parameters, q)
					}
				}
				merged = true
				rule = *r
				break
			}
		}
		if !merged {
			next := 1
			for _, r := range p.rules {
				if id, e := strconv.Atoi(r.ID); e == nil && id >= next {
					next = id + 1
				}
			}
			rule.ID = strconv.Itoa(next)
			p.rules = append(p.rules, rule)
		}
	}
	var saveErr error
	if conflict == nil {
		saveErr = p.saveRulesLocked()
		n := len(p.rules)
		p.mu.Unlock()
		if saveErr != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(saveErr.Error()))
		}
		var b strings.Builder
		if merged {
			b.WriteString("✅ " + tl("已合并规则参数", "Rule parameters merged") + "\n")
		} else {
			b.WriteString("✅ " + tl("已添加规则", "Rule added") + " #" + rule.ID + "\n")
		}
		b.WriteString(ruleText(rule) + "\n")
		b.WriteString(tl("规则总数", "Rules") + "  " + plugin.Code(strconv.Itoa(n)))
		return ctx.Edit(b.String())
	}
	c := *conflict
	p.mu.Unlock()
	return ctx.Edit("❌ " + tl("规则冲突: ", "Rule conflict: ") + ruleText(c) + "\n💡 " +
		tl("先删除旧规则 ", "Delete it first with ") + plugin.Code("autodel cmd del "+c.ID))
}

func (p *AutoDelPlugin) cmdRuleDel(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	if len(args) == 0 {
		return ctx.Edit("❌ " + tl("用法: ", "Usage: ") + plugin.Code("autodel cmd del <"+tl("ID 或 命令", "ID or command")+">"))
	}
	input := strings.ToLower(args[0])
	p.mu.Lock()
	// by ID first
	for i, r := range p.rules {
		if r.ID == input {
			p.rules = append(p.rules[:i], p.rules[i+1:]...)
			err := p.saveRulesLocked()
			p.mu.Unlock()
			if err != nil {
				return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
			}
			return ctx.Edit("🗑 ✅ " + tl("已删除规则", "Rule deleted") + " #" + plugin.Escape(r.ID) + "\n" + ruleText(r))
		}
	}
	// by command name: list matching rules for the user
	var matches []CmdRule
	for _, r := range p.rules {
		if r.Command == input {
			matches = append(matches, r)
		}
	}
	p.mu.Unlock()
	if len(matches) == 0 {
		return ctx.Edit("❌ " + tl("未找到匹配的规则: ", "No matching rule: ") + plugin.Code(input))
	}
	var b strings.Builder
	b.WriteString("📋 " + tl("命令的规则，用 ID 删除:", "Rules for this command, delete by ID:") + "\n")
	for _, r := range matches {
		b.WriteString("• #" + plugin.Escape(r.ID) + " " + ruleText(r) + "\n")
		b.WriteString("  " + plugin.Code("autodel cmd del "+r.ID) + "\n")
	}
	return ctx.Edit(strings.TrimRight(b.String(), "\n"))
}

func (p *AutoDelPlugin) cmdRuleReset(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	p.rules = defaultRules()
	err := p.saveRulesLocked()
	n := len(p.rules)
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + tl("已重置为默认规则", "Rules reset to defaults") + "\n" +
		tl("规则总数", "Rules") + "  " + plugin.Code(strconv.Itoa(n)))
}

func helpText(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("🗑 **" + tl("定时自动删除", "Auto-delete") + "**\n\n")
	b.WriteString("**" + tl("定时删除", "Timed delete") + "**\n")
	b.WriteString(line("autodel 30s", "本聊天 30 秒后删除", "this chat, delete after 30s"))
	b.WriteString(line("autodel 5m global", "全局 5 分钟", "global, 5 minutes"))
	b.WriteString(line("autodel cancel [global]", "取消设置", "cancel the setting"))
	b.WriteString("\n**" + tl("命令规则", "Command rules") + "**\n")
	b.WriteString(line("autodel cmd add ping 30", "ping 命令 30 秒后删除", "delete ping after 30s"))
	b.WriteString(line(tl("autodel cmd add speedtest 60 -r", "autodel cmd add speedtest 60 -r"), "同时删除响应", "also delete responses"))
	b.WriteString(line(tl("autodel cmd add tpm 60 list ls", "autodel cmd add tpm 60 list ls"), "仅这些参数触发", "only these first params"))
	b.WriteString(line(tl("autodel cmd add ping 30 -e", "autodel cmd add ping 30 -e"), "仅无参数调用", "bare calls only"))
	b.WriteString(line("autodel cmd del <ID|命令>", "删除规则 / 查看命令的规则", "delete a rule / list a command's rules"))
	b.WriteString(line("autodel cmd reset", "重置默认规则", "reset default rules"))
	b.WriteString("\n**" + tl("查看", "View") + "**\n")
	b.WriteString(line("autodel list", "TTL 设置 + 规则摘要", "TTL entries + rule summary"))
	b.WriteString(line("autodel status", "本群/全局 TTL + 规则计数", "chat/global TTL + rule count"))
	b.WriteString("\n**" + tl("时长格式", "Duration format") + "**\n")
	b.WriteString(plugin.Code("30s 5m 2h 1d") + " · " + plugin.Code("30 seconds") + " · " + plugin.Code(tl("5分钟 2小时 1天", "5fen 2xiaoshi 1tian")) + "\n")
	b.WriteString("⚠️ " + tl("最短 5 秒；只会删除您自己发送的消息；命令输出清理在机器人面板开启", "Minimum 5 seconds; only your own messages are deleted; command-output cleaning is enabled in the bot panel"))
	return b.String()
}

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
	return b.String() + "…"
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
