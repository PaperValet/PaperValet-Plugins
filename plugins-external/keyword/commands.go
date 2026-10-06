package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func (p *KeywordPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	sub := strings.ToLower(ctx.GetArg(0))
	if sub == "" || sub == "h" || sub == "help" {
		return ctx.Edit(helpText(ctx))
	}
	switch sub {
	case "list":
		return p.cmdList(ctx, strings.EqualFold(ctx.GetArg(1), "all"))
	case "rm":
		if ctx.ArgCount() < 2 {
			return ctx.Edit("❌ " + ctx.Tlocal("请输入任务ID，如 ", "Give task IDs, e.g. ") + plugin.Code("keyword rm 1,2"))
		}
		return p.cmdRm(ctx, ctx.GetArg(1))
	case "alias":
		return p.cmdAlias(ctx)
	}
	return p.cmdAdd(ctx)
}

func helpText(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("🔑 **" + tl("关键词回复", "Keyword Replies") + "**\n\n")
	b.WriteString("**" + tl("管理", "Manage") + "**\n")
	b.WriteString(line("keyword list", "查看当前聊天的任务", "list this chat's tasks"))
	b.WriteString(line("keyword list all", "查看所有任务", "list every task"))
	b.WriteString(line("keyword rm 1,2,3", "删除指定ID的任务", "delete tasks by ID"))
	b.WriteString(line("keyword alias", "查看当前聊天的继承设置", "show this chat's inheritance"))
	b.WriteString(line(tl("keyword alias <群ID>", "keyword alias <groupID>"), "继承其他群的关键词", "inherit another group's keywords"))
	b.WriteString(line(tl("keyword alias rm", "keyword alias rm"), "删除继承设置", "remove inheritance"))
	b.WriteString("\n**" + tl("添加任务（用 +++ 分段）", "Add a task (segments separated by +++)") + "**\n")
	b.WriteString(plugin.Pre("keyword 关键词\n+++\n回复内容\n+++\n匹配选项\n+++\n执行动作\n+++\n回复删除秒数\n+++\n原消息删除秒数") + "\n")
	b.WriteString("\n**" + tl("匹配选项（空格分隔）", "Match options (space separated)") + "**\n")
	b.WriteString(line("include", "包含匹配（默认）", "contains (default)"))
	b.WriteString(line("exact", "精确匹配", "exact match"))
	b.WriteString(line("regexp", "正则匹配", "regex match"))
	b.WriteString(line("case", "区分大小写", "case-sensitive"))
	b.WriteString(line("ignore_forward", "忽略转发消息", "skip forwarded messages"))
	b.WriteString("\n**" + tl("执行动作（空格分隔）", "Actions (space separated)") + "**\n")
	b.WriteString(line("reply", "回复消息（默认）", "reply to it (default)"))
	b.WriteString(line("delete", "删除触发消息", "delete the trigger"))
	b.WriteString(plugin.Code("ban300 · restrict600") + "  " + tl("封禁/禁言 300/600 秒（需管理员，仅超级群）", "ban/mute for N seconds (admin needed, supergroups only)") + "\n")
	b.WriteString(plugin.Code("cooldown60") + "  " + tl("同一任务 60 秒内只触发一次", "fire at most once per 60s") + "\n")
	b.WriteString("\n**" + tl("消息变量", "Message variables") + "**\n")
	b.WriteString("`$mention` · `$code_id` · `$code_name` · `$delay_delete`\n")
	b.WriteString("\n**" + tl("示例", "Examples") + "**\n")
	b.WriteString(plugin.Pre(tl(
		"keyword 你好\n+++\n欢迎！$mention",
		"keyword hello\n+++\nWelcome! $mention")) + "\n")
	b.WriteString(plugin.Pre(tl(
		"keyword \\\\d{11}\n+++\n🚫 请勿发送手机号\n+++\nregexp\n+++\nreply delete cooldown60\n+++\n10",
		"keyword \\\\d{11}\n+++\n🚫 no phone numbers\n+++\nregexp\n+++\nreply delete cooldown60\n+++\n10")) + "\n")
	b.WriteString("💡 " + tl(
		"任务保存在 data/keyword/，重启不丢；编辑消息默认不触发，可在机器人面板打开",
		"Tasks live in data/keyword/ and survive restarts; edits do not fire by default, enable it in the bot panel"))
	return b.String()
}

// rawArgs returns the text after the command word, newlines preserved.
func rawArgs(ctx *plugin.CommandContext) string {
	full := ctx.Message.Message.Message
	s := full
	if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
		return s[i+1:]
	}
	return ""
}

func (p *KeywordPlugin) cmdAdd(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	raw := rawArgs(ctx)
	t := newTask(0, ctx.Message.ChatID)
	if err := t.parseTask(raw); err != nil {
		return ctx.Edit("❌ " + tl("参数错误: ", "Invalid arguments: ") + plugin.Escape(err.Error()) + "\n\n" + plugin.Code("keyword help"))
	}
	if t.Regexp {
		re, err := compileRegexp(t.Key, t.CaseSensitive)
		if err != nil {
			return ctx.Edit("❌ " + tl("正则表达式无效: ", "Invalid regexp: ") + plugin.Escape(err.Error()))
		}
		t.re = re // cache: the hot path must not compile again
	}
	p.mu.Lock()
	t.ID = p.nextID
	p.nextID++
	p.tasks = append(p.tasks, t)
	err := p.saveLocked()
	if err != nil {
		p.tasks = p.tasks[:len(p.tasks)-1]
		p.nextID--
	}
	n := len(p.tasks)
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	var b strings.Builder
	b.WriteString("✅ **" + fmt.Sprintf(tl("已添加任务 #%d", "Task #%d added"), t.ID) + "**\n")
	b.WriteString(tl("关键词", "Keyword") + "  " + plugin.Code(truncate(t.Key, 60)) + "\n")
	b.WriteString(tl("回复", "Reply") + "  " + plugin.Code(truncate(t.Msg, 60)) + "\n")
	if flags := taskFlags(t, tl); flags != "" {
		b.WriteString(flags)
	}
	b.WriteString(fmt.Sprintf(tl("当前任务  %d 个", "%d tasks now"), n))
	return ctx.Edit(b.String())
}

// taskFlags renders the option/action summary lines for one task.
func taskFlags(t *task, tl func(string, string) string) string {
	var opts []string
	if t.Exact {
		opts = append(opts, tl("精确", "exact"))
	} else {
		opts = append(opts, tl("包含", "contains"))
	}
	if t.Regexp {
		opts = append(opts, tl("正则", "regex"))
	}
	if t.CaseSensitive {
		opts = append(opts, tl("区分大小写", "case-sensitive"))
	}
	if t.IgnoreForward {
		opts = append(opts, tl("忽略转发", "skip forwards"))
	}
	var acts []string
	if t.Delete {
		acts = append(acts, tl("删原文", "delete trigger"))
	}
	if t.Ban > 0 {
		acts = append(acts, fmt.Sprintf(tl("封禁%d秒", "ban %ds"), t.Ban))
	}
	if t.Restrict > 0 {
		acts = append(acts, fmt.Sprintf(tl("禁言%d秒", "mute %ds"), t.Restrict))
	}
	if t.Cooldown > 0 {
		acts = append(acts, fmt.Sprintf(tl("冷却%d秒", "cooldown %ds"), t.Cooldown))
	}
	if t.DelayDelete > 0 {
		acts = append(acts, fmt.Sprintf(tl("回复%d秒后删", "reply deleted in %ds"), t.DelayDelete))
	}
	if t.SourceDelayDelete > 0 {
		acts = append(acts, fmt.Sprintf(tl("原文%d秒后删", "trigger deleted in %ds"), t.SourceDelayDelete))
	}
	s := tl("匹配", "Match") + "  " + plugin.Code(strings.Join(opts, " ")) + "\n"
	if len(acts) > 0 {
		s += tl("动作", "Actions") + "  " + plugin.Code(strings.Join(acts, " ")) + "\n"
	}
	return s
}

func (p *KeywordPlugin) cmdList(ctx *plugin.CommandContext, all bool) error {
	tl := ctx.Tlocal
	if all && !ctx.IsSelf() {
		return ctx.Edit("❌ " + tl("只有主账号可以查看所有任务", "Only the owner can list all tasks"))
	}
	p.mu.Lock()
	var list []*task
	for _, t := range p.tasks {
		if all || t.ChatID == ctx.Message.ChatID {
			list = append(list, t)
		}
	}
	p.mu.Unlock()

	title := tl("当前聊天的关键词任务", "Keyword tasks in this chat")
	if all {
		title = tl("所有关键词任务", "All keyword tasks")
	}
	if len(list) == 0 {
		if all {
			return ctx.Edit("ℹ️ " + tl("当前没有任何关键词任务", "No keyword tasks yet"))
		}
		return ctx.Edit("ℹ️ " + tl("当前聊天没有任何关键词任务", "No keyword tasks in this chat"))
	}
	var b strings.Builder
	b.WriteString("📋 **" + title + "**  " + plugin.Code(len(list)) + "\n\n")
	for _, t := range list {
		b.WriteString(plugin.Code(t.ID) + " · " + plugin.Code(truncate(t.Key, 40)) + "\n")
		b.WriteString("  " + plugin.Code(truncate(t.Msg, 60)) + "\n")
		if all {
			b.WriteString("  " + tl("会话", "Chat") + " " + plugin.Code(t.ChatID) + "\n")
		}
		if f := taskFlags(t, tl); f != "" {
			for _, l := range strings.Split(strings.TrimRight(f, "\n"), "\n") {
				b.WriteString("  " + strings.TrimLeft(l, " ") + "\n")
			}
		}
	}
	return ctx.Edit(clipLines(strings.TrimRight(b.String(), "\n"), 3900))
}

func (p *KeywordPlugin) cmdRm(ctx *plugin.CommandContext, idsArg string) error {
	tl := ctx.Tlocal
	ids, err := parseTaskIDs(idsArg)
	if err != nil {
		return ctx.Edit("❌ " + tl("请输入正确的参数: ", "Invalid arguments: ") + plugin.Escape(idsArg))
	}
	owner := ctx.IsSelf()
	success, failed := 0, 0
	p.mu.Lock()
	for _, id := range ids {
		idx := -1
		for i, t := range p.tasks {
			if t.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			failed++
			continue
		}
		if p.tasks[idx].ChatID != ctx.Message.ChatID && !owner {
			failed++
			continue
		}
		p.tasks = append(p.tasks[:idx], p.tasks[idx+1:]...)
		delete(p.cool, id)
		success++
	}
	var saveErr error
	var orphaned []int64
	if success > 0 {
		saveErr = p.saveLocked()
		// Chats aliasing a group whose tasks are now all gone inherit
		// nothing; list them so the owner can remove the dead alias.
		tasksPerChat := map[int64]int{}
		for _, t := range p.tasks {
			tasksPerChat[t.ChatID]++
		}
		for from, to := range p.alias {
			if tasksPerChat[to] == 0 {
				orphaned = append(orphaned, from)
			}
		}
	}
	p.mu.Unlock()
	msg := "✅ " + fmt.Sprintf(tl("已删除任务 %d 个，失败 %d 个", "Deleted %d tasks, %d failed"), success, failed) +
		errSuffix(saveErr, tl)
	if len(orphaned) > 0 {
		sort.Slice(orphaned, func(i, j int) bool { return orphaned[i] < orphaned[j] })
		var ids []string
		for _, from := range orphaned {
			ids = append(ids, plugin.Code(from))
		}
		msg += "\n⚠️ " + tl("以下会话继承的群已无任务：", "These chats inherit from a group with no tasks left: ") +
			strings.Join(ids, ", ")
	}
	return ctx.Edit(msg)
}

func (p *KeywordPlugin) cmdAlias(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	chatID := ctx.Message.ChatID
	arg := strings.TrimSpace(ctx.GetArg(1))
	if arg == "" {
		p.mu.Lock()
		to, ok := p.alias[chatID]
		p.mu.Unlock()
		if ok {
			return ctx.Edit("🔗 " + tl("当前聊天继承自 ", "This chat inherits from ") + plugin.Code(to))
		}
		return ctx.Edit("ℹ️ " + tl("当前聊天没有继承设置", "No inheritance set for this chat"))
	}
	if strings.EqualFold(arg, "rm") {
		p.mu.Lock()
		_, ok := p.alias[chatID]
		delete(p.alias, chatID)
		var err error
		if ok {
			err = p.saveLocked()
		}
		p.mu.Unlock()
		if !ok {
			return ctx.Edit("ℹ️ " + tl("当前聊天没有继承设置", "No inheritance set for this chat"))
		}
		return ctx.Edit("✅ " + tl("已删除继承设置", "Inheritance removed") + errSuffix(err, tl))
	}
	cid, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || cid == 0 {
		return ctx.Edit("❌ " + tl("请输入正确的群ID", "Give a valid group ID"))
	}
	p.mu.Lock()
	p.alias[chatID] = cid
	err = p.saveLocked()
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + tl("已添加继承: ", "Inheritance added: ") + plugin.Code(cid))
}

func errSuffix(err error, tl func(string, string) string) string {
	if err == nil {
		return ""
	}
	return "\n❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error())
}

// sorted returns tasks sorted by id.
func sorted(list []*task) []*task {
	out := append([]*task(nil), list...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// page lists keyword tasks with per-task delete buttons (bot panel).
func (p *KeywordPlugin) page(c *plugin.BotContext) (*plugin.View, error) {
	tl := c.Tlocal
	if op, arg, ok := strings.Cut(c.Data, ":"); ok && op == "rm" {
		id, err := strconv.Atoi(arg)
		if err == nil {
			p.mu.Lock()
			idx := -1
			for i, t := range p.tasks {
				if t.ID == id {
					idx = i
					break
				}
			}
			if idx >= 0 {
				p.tasks = append(p.tasks[:idx], p.tasks[idx+1:]...)
				delete(p.cool, id)
			}
			var saveErr error
			if idx >= 0 {
				saveErr = p.saveLocked()
			}
			p.mu.Unlock()
			switch {
			case saveErr != nil:
				c.Alert(tl("保存失败: ", "Save failed: ") + saveErr.Error())
			case idx >= 0:
				c.Toast(fmt.Sprintf(tl("已删除 #%d", "Deleted #%d"), id))
			default:
				c.Toast(tl("已经不在列表里了", "Already gone"))
			}
		}
	}
	if strings.HasPrefix(c.Data, "alias:") {
		chatID, _ := strconv.ParseInt(strings.TrimPrefix(c.Data, "alias:"), 10, 64)
		p.mu.Lock()
		_, had := p.alias[chatID]
		delete(p.alias, chatID)
		var saveErr error
		if had {
			saveErr = p.saveLocked()
		}
		p.mu.Unlock()
		if saveErr != nil {
			c.Alert(tl("保存失败: ", "Save failed: ") + saveErr.Error())
		} else if had {
			c.Toast(tl("已删除继承", "Inheritance removed"))
		}
	}

	p.mu.Lock()
	list := sorted(p.tasks)
	alias := make(map[int64]int64, len(p.alias))
	for k, v := range p.alias {
		alias[k] = v
	}
	p.mu.Unlock()

	v := &plugin.View{Text: "🔑 **" + tl("关键词任务", "Keyword tasks") + "**  " + plugin.Code(len(list))}
	if len(list) == 0 && len(alias) == 0 {
		v.Text += "\n\n" + tl("还没有任务，在聊天里用 `keyword` 命令添加", "No tasks yet; add one with the `keyword` command in a chat")
		return v, nil
	}
	for _, t := range list {
		v.Text += fmt.Sprintf("\n\n**#%d** · %s\n", t.ID, plugin.Code(truncate(t.Key, 24)))
		v.Text += plugin.Code(truncate(t.Msg, 40)) + "\n"
		v.Text += tl("会话", "Chat") + " " + plugin.Code(t.ChatID) + "\n"
		if f := taskFlags(t, tl); f != "" {
			v.Text += f
		}
		v.Buttons = append(v.Buttons, plugin.Row(
			plugin.Btn(fmt.Sprintf("🗑 #%d", t.ID), fmt.Sprintf("rm:%d", t.ID)).Danger()))
	}
	for from, to := range alias {
		v.Text += fmt.Sprintf("\n\n🔗 %s → %s\n", plugin.Code(from), plugin.Code(to))
		v.Buttons = append(v.Buttons, plugin.Row(
			plugin.Btn(fmt.Sprintf(tl("🗑 继承 %d", "🗑 Inherit %d"), from), fmt.Sprintf("alias:%d", from)).Danger()))
	}
	return v, nil
}
