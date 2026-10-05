package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func timeNow() time.Time { return time.Now().UTC() }

// handle dispatches `shift <sub>`.
func (p *ShiftPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	sub := ""
	if len(ctx.Args) > 0 {
		sub = strings.ToLower(ctx.Args[0])
	}
	rest := ctx.Args[min(1, len(ctx.Args)):]
	switch sub {
	case "", "help":
		return ctx.Edit(helpText(ctx))
	case "set":
		return p.cmdSet(ctx, rest)
	case "list":
		return p.cmdList(ctx)
	case "del", "rm":
		return p.cmdIndices(ctx, rest, "del")
	case "pause":
		return p.cmdIndices(ctx, rest, "pause")
	case "resume":
		return p.cmdIndices(ctx, rest, "resume")
	case "filter":
		return p.cmdFilter(ctx, rest, false)
	case "whitelist":
		return p.cmdFilter(ctx, rest, true)
	case "stats":
		return p.cmdStats(ctx)
	case "backup":
		return p.cmdBackup(ctx, rest)
	}
	return ctx.Edit("❌ " + ctx.Tlocal("未知子命令 ", "Unknown subcommand ") + plugin.Code(sub) + "\n\n💡 " + plugin.Code("shift help"))
}

func helpText(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("🚀 **" + tl("shift — 智能转发", "shift — auto forwarding") + "**\n\n")
	b.WriteString("**" + tl("命令", "Commands") + "**\n")
	b.WriteString(line(tl("shift set <源> <目标> [选项…]", "shift set <source> <target> [options…]"),
		tl("设置规则；源可写 here 表示当前对话", "Create a rule; source may be here for the current chat"),
		"Create a rule; source may be here for the current chat"))
	b.WriteString(line("shift list", tl("查看所有规则", "List all rules"), "List all rules"))
	b.WriteString(line(tl("shift del <序号>", "shift del <n>"), tl("删除规则（支持 1,3-5）", "Delete rules (1,3-5 works)"), "Delete rules (1,3-5 works)"))
	b.WriteString(line(tl("shift pause|resume <序号>", "shift pause|resume <n>"), tl("暂停/恢复规则", "Pause/resume rules"), "Pause/resume rules"))
	b.WriteString(line(tl("shift filter <序号> add|del|list [词…]", "shift filter <n> add|del|list [words…]"),
		tl("关键词黑名单：命中则不转发", "Keyword blacklist: matches are not forwarded"), "Keyword blacklist: matches are not forwarded"))
	b.WriteString(line(tl("shift whitelist <序号> on|off|add|del|list [正则…]", "shift whitelist <n> on|off|add|del|list [regex…]"),
		tl("正则白名单：只转发命中项", "Regex whitelist: only matches are forwarded"), "Regex whitelist: only matches are forwarded"))
	b.WriteString(line("shift stats", tl("查看转发统计（最近 7 天）", "Forwarding stats (last 7 days)"), "Forwarding stats (last 7 days)"))
	b.WriteString(line(tl("shift backup <源> <目标> [数量]", "shift backup <source> <target> [count]"),
		tl("转发历史消息（默认 100，倒序）", "Forward history (default 100, newest first)"), "Forward history (default 100, newest first)"))
	b.WriteString("\n**" + tl("类型选项", "Type options") + "**\n")
	b.WriteString(plugin.Code("text photo document video sticker animation voice audio") + "\n")
	b.WriteString(plugin.Code("silent") + "  " + tl("静音转发；", "Forward silently; ") + plugin.Code("handle_edited") + "  " + tl("也转发编辑", "also forward edits") + "\n")
	b.WriteString(tl("目标可写 ", "Target may use ") + plugin.Code("@用户名|话题ID") + tl(" 指定论坛话题", " to pick a forum topic"))
	return b.String()
}

// cmdSet creates or replaces the rule for a source chat.
func (p *ShiftPlugin) cmdSet(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	src, dst, opts, topicID, err := parseSetArgs(args)
	if err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()) + "\n\n" + tl("用法：", "Usage: ") + plugin.Code("shift set <源> <目标> [选项…]"))
	}
	if src == "" {
		src = "here"
	}
	dstPeer, topic, _ := parseTarget(dst)
	if dstPeer == "" {
		return ctx.Edit("❌ " + tl("目标为空", "Empty target"))
	}
	if topic > 0 {
		topicID = topic
	}
	srcInfo, err := resolveTarget(ctx.Context(), p.resolverView(), src, ctx.Message.ChatID, p.selfIDView())
	if err != nil {
		return ctx.Edit("❌ " + tl("源对话无效 ", "Invalid source ") + plugin.Code(src) + "\n" + plugin.Escape(errText(err)))
	}
	dstInfo, err := resolveTarget(ctx.Context(), p.resolverView(), dstPeer, ctx.Message.ChatID, p.selfIDView())
	if err != nil {
		return ctx.Edit("❌ " + tl("目标对话无效 ", "Invalid target ") + plugin.Code(dstPeer) + "\n" + plugin.Escape(errText(err)))
	}
	if srcInfo.ChatID == dstInfo.ChatID {
		return ctx.Edit("❌ " + tl("不能设置自己到自己的转发规则", "Source and target must differ"))
	}

	// Circular forwarding check: following the chain from the target must not
	// reach back to the source.
	p.mu.Lock()
	visited := map[int64]bool{srcInfo.ChatID: true}
	cur := dstInfo.ChatID
	circular := false
	var cycleAt int64
	for i := 0; i < 20 && p.rules[cur] != nil; i++ {
		if visited[cur] {
			circular, cycleAt = true, cur
			break
		}
		visited[cur] = true
		cur = p.rules[cur].Target
	}
	count := len(p.rules)
	p.mu.Unlock()
	if circular {
		return ctx.Edit("❌ " + tl("检测到循环转发：", "Circular forwarding detected: ") + plugin.Code(cycleAt))
	}
	if count >= maxRules && p.ruleOf(srcInfo.ChatID) == nil {
		return ctx.Edit(fmt.Sprintf("❌ "+tl("规则数量已达上限（%d）", "Too many rules (max %d)"), maxRules))
	}

	// Best-effort display names.
	p.fetchTitleBestEffort(ctx, srcInfo)
	p.fetchTitleBestEffort(ctx, dstInfo)

	rule := &Rule{
		Target:    dstInfo.ChatID,
		Options:   opts,
		Paused:    false,
		TopicID:   topicID,
		CreatedAt: timeNow(),
	}
	p.keepFilters(srcInfo.ChatID, rule)
	p.mu.Lock()
	if _, exists := p.rules[srcInfo.ChatID]; !exists {
		p.order = append(p.order, srcInfo.ChatID)
	}
	p.rules[srcInfo.ChatID] = rule
	err = p.saveLocked()
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存规则失败: ", "Failed to save rule: ") + plugin.Escape(errText(err)))
	}

	var b strings.Builder
	b.WriteString("✅ **" + tl("转发规则已设置", "Forwarding rule set") + "**\n\n")
	b.WriteString("📤 " + tl("源", "Source") + "  " + plugin.Bold(srcInfo.label()) + "\n")
	b.WriteString("📥 " + tl("目标", "Target") + "  " + plugin.Bold(dstInfo.label()) + "\n")
	if topicID > 0 {
		b.WriteString("💬 " + tl("话题", "Topic") + "  " + plugin.Code(topicID) + "\n")
	}
	if types := rule.types(); len(types) > 0 {
		b.WriteString("🎯 " + tl("类型", "Types") + "  " + plugin.Code(strings.Join(types, ",")) + "\n")
	}
	if rule.has("silent") {
		b.WriteString("🔇 " + tl("静音转发", "Silent") + "\n")
	}
	if rule.has("handle_edited") {
		b.WriteString("✏️ " + tl("监听编辑", "Handle edits") + "\n")
	}
	return ctx.Edit(b.String())
}

// keepFilters preserves keywords and whitelist settings when a rule is
// re-created for the same source.
func (p *ShiftPlugin) keepFilters(src int64, rule *Rule) {
	p.mu.Lock()
	old := p.rules[src]
	p.mu.Unlock()
	if old == nil {
		return
	}
	rule.Filters = old.Filters
	rule.Whitelist = old.Whitelist
	rule.WhitelistMode = old.WhitelistMode
	rule.Paused = old.Paused
}

func (p *ShiftPlugin) ruleOf(src int64) *Rule {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rules[src]
}

func (p *ShiftPlugin) resolverView() pluginResolver {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resolver
}

func (p *ShiftPlugin) selfIDView() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.selfID
}

func (p *ShiftPlugin) fetchTitleBestEffort(ctx *plugin.CommandContext, ci *chatInfo) {
	api := ctx.API
	if api == nil {
		p.mu.Lock()
		api = p.api
		p.mu.Unlock()
	}
	fetchTitle(ctx.Context(), api, ci)
}

// cmdList renders all rules in insertion order.
func (p *ShiftPlugin) cmdList(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	rules := p.ordered()
	if len(rules) == 0 {
		return ctx.Edit("🚫 " + tl("暂无转发规则，用 ", "No rules yet; use ") + plugin.Code("shift set") + tl(" 创建", " to create one"))
	}
	var b strings.Builder
	b.WriteString("📋 **" + tl("转发规则", "Forwarding Rules") + "**  " + plugin.Code(len(rules)) + "\n")
	for i, r := range rules {
		src := p.ruleSource(i)
		status := "▶️"
		if r.Paused {
			status = "⏸️"
		}
		b.WriteString(fmt.Sprintf("\n%s %d. %s → %s\n", status, i+1, plugin.Bold(ruleTitle(r.SourceTitle, src)), plugin.Bold(ruleTitle(r.TargetTitle, r.Target))))
		if r.TopicID > 0 {
			b.WriteString("   💬 " + tl("话题", "Topic") + "  " + plugin.Code(r.TopicID) + "\n")
		}
		if types := r.types(); len(types) > 0 {
			b.WriteString("   🎯 " + tl("类型", "Types") + "  " + plugin.Code(strings.Join(types, ",")) + "\n")
		} else {
			b.WriteString("   🎯 " + tl("类型", "Types") + "  " + tl("全部", "all") + "\n")
		}
		if r.has("silent") {
			b.WriteString("   🔇 " + tl("静音", "Silent") + "\n")
		}
		if r.has("handle_edited") {
			b.WriteString("   ✏️ " + tl("监听编辑", "Edits") + "\n")
		}
		if r.WhitelistMode {
			b.WriteString("   ✅ " + tl("白名单", "Whitelist") + "  " + plugin.Code(len(r.Whitelist)) + "\n")
		} else if len(r.Filters) > 0 {
			b.WriteString("   🛡️ " + tl("过滤词", "Filters") + "  " + plugin.Code(len(r.Filters)) + "\n")
		}
	}
	out := b.String()
	if len([]rune(out)) > 3900 {
		out = string([]rune(out)[:3900]) + "\n…"
	}
	return ctx.Edit(out)
}

// ruleSource returns the source chat id of the i-th rule in order.
func (p *ShiftPlugin) ruleSource(i int) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.order) {
		return 0
	}
	return p.order[i]
}

func ruleTitle(stored string, id int64) string {
	if stored != "" {
		return stored
	}
	return strconv.FormatInt(id, 10)
}

// cmdIndices handles del / pause / resume over "1,3-5".
func (p *ShiftPlugin) cmdIndices(ctx *plugin.CommandContext, args []string, op string) error {
	tl := ctx.Tlocal
	if len(args) == 0 {
		return ctx.Edit("❌ " + tl("请提供序号，如 ", "Give rule numbers, e.g. ") + plugin.Code("shift "+op+" 1,3-5"))
	}
	p.mu.Lock()
	total := len(p.order)
	idx, invalid := parseIndices(args[0], total)
	var changed int
	// Apply from the highest index so deletions keep earlier indexes valid.
	sorted := append([]int(nil), idx...)
	sort.Sort(sort.Reverse(sort.IntSlice(sorted)))
	for _, i := range sorted {
		if i < 0 || i >= len(p.order) {
			continue
		}
		src := p.order[i]
		r := p.rules[src]
		switch op {
		case "del":
			delete(p.rules, src)
			p.order = append(p.order[:i], p.order[i+1:]...)
		case "pause":
			if r != nil {
				r.Paused = true
			}
		case "resume":
			if r != nil {
				r.Paused = false
			}
		}
		changed++
	}
	var err error
	if changed > 0 {
		err = p.saveLocked()
	}
	p.mu.Unlock()

	var b strings.Builder
	switch {
	case changed > 0 && err == nil:
		verbs := map[string]string{"del": tl("已删除", "Deleted"), "pause": tl("已暂停", "Paused"), "resume": tl("已恢复", "Resumed")}
		b.WriteString("✅ " + verbs[op] + " " + plugin.Code(changed) + " " + tl("条规则", "rule(s)"))
	case err != nil:
		b.WriteString("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(errText(err)))
	default:
		b.WriteString("❌ " + tl("没有匹配的规则", "No matching rules"))
	}
	for _, s := range invalid {
		b.WriteString("\n⚠️ " + tl("无效序号: ", "Invalid number: ") + plugin.Code(s))
	}
	return ctx.Edit(b.String())
}

// cmdFilter manages the keyword blacklist (wl=false) or regex whitelist.
func (p *ShiftPlugin) cmdFilter(ctx *plugin.CommandContext, args []string, wl bool) error {
	tl := ctx.Tlocal
	if len(args) < 2 {
		usage := "shift filter <序号> add|del|list [词…]"
		usageEN := "shift filter <n> add|del|list [words…]"
		if wl {
			usage = "shift whitelist <序号> on|off|add|del|list [正则…]"
			usageEN = "shift whitelist <n> on|off|add|del|list [regex…]"
		}
		return ctx.Edit("❌ " + tl("参数不足\n用法：", "Not enough arguments\nUsage: ") + plugin.Code(tl(usage, usageEN)))
	}
	p.mu.Lock()
	total := len(p.order)
	idx, _ := parseIndices(args[0], total)
	if len(idx) == 0 {
		p.mu.Unlock()
		return ctx.Edit("❌ " + tl("无效的序号: ", "Invalid number: ") + plugin.Code(args[0]))
	}
	action := strings.ToLower(args[1])
	words := args[2:]

	// whitelist on/off act on all selected rules
	if wl && (action == "on" || action == "off") {
		changed := 0
		for _, i := range idx {
			if r := p.ruleAt(i); r != nil {
				r.WhitelistMode = action == "on"
				changed++
			}
		}
		var err error
		if changed > 0 {
			err = p.saveLocked()
		}
		p.mu.Unlock()
		if err != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(errText(err)))
		}
		return ctx.Edit("✅ " + tl("已更新白名单模式", "Whitelist mode updated") + "  " + plugin.Code(changed))
	}

	if len(idx) != 1 {
		p.mu.Unlock()
		return ctx.Edit("❌ " + tl("此操作一次只能作用于一条规则", "This action applies to one rule at a time"))
	}
	r := p.ruleAt(idx[0])
	if r == nil {
		p.mu.Unlock()
		return ctx.Edit("❌ " + tl("规则不存在", "Rule not found"))
	}
	var b strings.Builder
	switch action {
	case "add":
		if len(words) == 0 {
			p.mu.Unlock()
			return ctx.Edit("❌ " + tl("请提供内容", "Nothing to add"))
		}
		for _, w := range words {
			if wl {
				r.Whitelist = appendUnique(r.Whitelist, w)
			} else {
				r.Filters = appendUnique(r.Filters, w)
			}
		}
		if err := p.saveLocked(); err != nil {
			p.mu.Unlock()
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(errText(err)))
		}
		p.mu.Unlock()
		b.WriteString("✅ " + tl("已添加", "Added") + "  " + plugin.Code(len(words)))
	case "del":
		if len(words) == 0 {
			p.mu.Unlock()
			return ctx.Edit("❌ " + tl("请提供内容", "Nothing to remove"))
		}
		for _, w := range words {
			if wl {
				r.Whitelist = removeMatch(r.Whitelist, w)
			} else {
				r.Filters = removeMatch(r.Filters, w)
			}
		}
		if err := p.saveLocked(); err != nil {
			p.mu.Unlock()
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(errText(err)))
		}
		p.mu.Unlock()
		b.WriteString("✅ " + tl("已删除", "Removed") + "  " + plugin.Code(len(words)))
	case "list":
		list := r.Filters
		key := tl("过滤词", "Filters")
		if wl {
			list = r.Whitelist
			key = tl("白名单正则", "Whitelist regexes")
			if r.WhitelistMode {
				b.WriteString("✅ " + tl("白名单模式已启用", "Whitelist mode is on") + "\n\n")
			} else {
				b.WriteString("⬜ " + tl("白名单模式未启用", "Whitelist mode is off") + "\n\n")
			}
		}
		p.mu.Unlock()
		b.WriteString("**" + key + "**\n")
		if len(list) == 0 {
			b.WriteString(tl("（空）", "(none)"))
		}
		for _, w := range list {
			b.WriteString("• " + plugin.Code(w) + "\n")
		}
	default:
		p.mu.Unlock()
		return ctx.Edit("❌ " + tl("无效操作: ", "Invalid action: ") + plugin.Code(action))
	}
	return ctx.Edit(strings.TrimRight(b.String(), "\n"))
}

func (p *ShiftPlugin) ruleAt(i int) *Rule {
	if i < 0 || i >= len(p.order) {
		return nil
	}
	return p.rules[p.order[i]]
}

// cmdStats renders per-source totals and the last 7 days.
func (p *ShiftPlugin) cmdStats(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.statsMu.Lock()
	totals := p.stats.totals()
	days := map[int64][]dayCount{}
	for src := range totals {
		days[src] = p.stats.recent(src, 7)
	}
	p.statsMu.Unlock()
	if len(totals) == 0 {
		return ctx.Edit("📊 " + tl("暂无转发统计数据", "No forwarding stats yet"))
	}
	var srcs []int64
	for src := range totals {
		srcs = append(srcs, src)
	}
	sort.Slice(srcs, func(i, j int) bool { return totals[srcs[i]] > totals[srcs[j]] })
	var b strings.Builder
	b.WriteString("📊 **" + tl("转发统计", "Forwarding Stats") + "**\n")
	for _, src := range srcs {
		r := p.ruleOf(src)
		title := strconv.FormatInt(src, 10)
		if r != nil && r.SourceTitle != "" {
			title = r.SourceTitle
		}
		b.WriteString("\n📤 " + plugin.Bold(title) + "\n")
		b.WriteString("📈 " + tl("总转发", "Total") + "  " + plugin.Code(totals[src]) + "\n")
		for _, d := range days[src] {
			b.WriteString("  · " + plugin.Code(d.Date) + "  " + plugin.Code(d.Count) + "\n")
		}
	}
	out := b.String()
	if len([]rune(out)) > 3900 {
		out = string([]rune(out)[:3900]) + "\n…"
	}
	return ctx.Edit(out)
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func removeMatch(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len([]rune(s)) > 160 {
		s = string([]rune(s)[:160]) + "…"
	}
	return s
}
