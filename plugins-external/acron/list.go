package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// cmdList renders tasks, optionally scoped to all chats or one type.
func (p *AcronPlugin) cmdList(ctx *plugin.CommandContext, rest []string) error {
	tl := ctx.Tlocal
	scopeAll, typeFilter := parseListArgs(rest)
	if scopeAll && !ctx.IsSelf() {
		return ctx.Edit("❌ " + tl("只有主账号可以查看所有任务", "Only the owner can list all tasks"))
	}
	chatID := ctx.Message.ChatID

	p.mu.Lock()
	loc := p.loc
	var tasks []*Task
	for _, t := range p.tasks {
		if !scopeAll && t.ChatID != chatID {
			continue
		}
		if typeFilter != "" && t.Type != typeFilter {
			continue
		}
		tasks = append(tasks, t)
	}
	next := map[int]time.Time{}
	for _, t := range tasks {
		if at, ok := p.next[t.ID]; ok {
			next[t.ID] = at
		}
	}
	p.mu.Unlock()

	// enabled first, then disabled, ids ascending inside each group
	var enabled, disabled []*Task
	for _, t := range tasks {
		if t.Disabled {
			disabled = append(disabled, t)
		} else {
			enabled = append(enabled, t)
		}
	}

	if len(enabled)+len(disabled) == 0 {
		switch {
		case scopeAll && typeFilter != "":
			return ctx.Edit("📋 " + fmt.Sprintf(tl("暂无类型为 %s 的定时任务", "No %s tasks yet"), taskTypeLabel(tl, typeFilter)))
		case scopeAll:
			return ctx.Edit("📋 " + tl("暂无定时任务", "No tasks yet"))
		case typeFilter != "":
			return ctx.Edit("📋 " + fmt.Sprintf(tl("当前会话暂无类型为 %s 的定时任务", "No %s tasks in this chat"), taskTypeLabel(tl, typeFilter)))
		}
		return ctx.Edit("📋 " + tl("当前会话暂无定时任务", "No tasks in this chat"))
	}

	var head string
	switch {
	case scopeAll && typeFilter != "":
		head = fmt.Sprintf("📋 **%s · %s** %s", tl("所有定时任务", "All tasks"), taskTypeLabel(tl, typeFilter), plugin.Code(len(enabled)+len(disabled)))
	case scopeAll:
		head = fmt.Sprintf("📋 **%s** %s", tl("所有定时任务", "All tasks"), plugin.Code(len(enabled)+len(disabled)))
	case typeFilter != "":
		head = fmt.Sprintf("📋 **%s · %s** %s", tl("当前会话任务", "Tasks in this chat"), taskTypeLabel(tl, typeFilter), plugin.Code(len(enabled)+len(disabled)))
	default:
		head = fmt.Sprintf("📋 **%s** %s", tl("当前会话任务", "Tasks in this chat"), plugin.Code(len(enabled)+len(disabled)))
	}

	var body []string
	if len(enabled) > 0 {
		body = append(body, "", "🔛 "+tl("已启用:", "Enabled:"), "")
		for _, t := range enabled {
			body = append(body, p.taskLines(tl, t, next[t.ID], loc, true)...)
		}
	}
	if len(disabled) > 0 {
		body = append(body, "", "⏹ "+tl("已禁用:", "Disabled:"), "")
		for _, t := range disabled {
			body = append(body, p.taskLines(tl, t, time.Time{}, loc, false)...)
		}
	}

	chunks := chunkLines(append([]string{head}, body...), listRunes)
	if err := ctx.Edit(chunks[0]); err != nil {
		return err
	}
	for _, c := range chunks[1:] {
		if err := sendPlain(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// sendPlain sends a markdown text message without a reply.
func sendPlain(ctx *plugin.CommandContext, text string) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	plain, entities := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesSendMessageRequest{Peer: peer, Message: plain, RandomID: time.Now().UnixNano()}
	if len(entities) > 0 {
		req.SetEntities(entities)
	}
	_, err = ctx.API.MessagesSendMessage(ctx.Context(), req)
	return err
}

// taskLines renders one task entry for the list output.
func (p *AcronPlugin) taskLines(tl func(string, string) string, t *Task, nextAt time.Time, loc *time.Location, enabled bool) []string {
	var out []string
	title := plugin.Code("#"+strconv.Itoa(t.ID)) + " · " + plugin.Code(taskTypeLabel(tl, t.Type))
	if t.Remark != "" {
		title += " · " + plugin.Escape(t.Remark)
	}
	out = append(out, title)
	out = append(out, tl("对话", "Chat")+"  "+escapeChat(*t))
	if t.MsgID != 0 && (t.Type == typeDel || t.Type == typePin || t.Type == typeUnpin) {
		out = append(out, tl("消息", "Message")+"  "+plugin.Code(t.MsgID))
	}
	if t.FromMsg != 0 && (t.Type == typeCopy || t.Type == typeForward) {
		out = append(out, tl("源消息", "Source")+"  "+plugin.Code(t.FromMsg))
	}
	if t.ReplyTo != 0 {
		out = append(out, tl("话题/回复", "Topic/reply")+"  "+plugin.Code(t.ReplyTo))
	}
	if t.Type == typeDelRe {
		out = append(out, tl("最近条数", "Recent")+"  "+plugin.Code(t.Limit)+"  "+tl("正则", "regex")+" "+plugin.Code(t.Regex))
	}
	if t.Type == typeCmd {
		out = append(out, tl("命令", "Command")+"  "+plugin.Code(t.Message))
	}
	if enabled && !nextAt.IsZero() {
		out = append(out, tl("下次", "Next")+"  "+plugin.Code(nextAt.In(loc).Format("2006-01-02 15:04:05")))
	}
	if t.LastRunAt != 0 {
		out = append(out, tl("上次", "Last")+"  "+plugin.Code(time.Unix(t.LastRunAt, 0).In(loc).Format("2006-01-02 15:04:05")))
	}
	if t.LastResult != "" {
		out = append(out, tl("结果", "Result")+"  "+plugin.Escape(t.LastResult))
	}
	if t.LastError != "" {
		out = append(out, tl("错误", "Error")+"  "+plugin.Escape(truncate(t.LastError, 120)))
	}
	cmd := buildCopyCommand(p.listPrefix(), *t)
	if strings.Contains(cmd, "\n") {
		out = append(out, tl("复制", "Copy")+":\n"+plugin.Pre(cmd))
	} else {
		out = append(out, tl("复制", "Copy")+"  "+plugin.Code(cmd))
	}
	return append(out, "")
}

func (p *AcronPlugin) listPrefix() string {
	if p.host != nil {
		if ps := p.host.Prefixes(); len(ps) > 0 && ps[0] != "" && ps[0] != "/" {
			return ps[0]
		}
	}
	return ""
}

// parseListArgs understands ls [all] [type].
func parseListArgs(rest []string) (all bool, typeFilter string) {
	for _, a := range rest {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "all":
			all = true
		case validType(a):
			typeFilter = a
		}
	}
	return all, typeFilter
}

// cmdRm deletes a task by id.
func (p *AcronPlugin) cmdRm(ctx *plugin.CommandContext, rest []string) error {
	tl := ctx.Tlocal
	id, ok := oneID(rest)
	if !ok {
		return ctx.Edit("❌ " + tl("请提供定时任务ID: ", "Provide a task id: ") + plugin.Code("acron rm <ID>"))
	}
	p.mu.Lock()
	idx := p.indexLocked(id)
	if idx < 0 {
		p.mu.Unlock()
		return ctx.Edit("❌ " + fmt.Sprintf(tl("未找到任务 #%d", "Task #%d not found"), id))
	}
	p.tasks = append(p.tasks[:idx], p.tasks[idx+1:]...)
	delete(p.next, id)
	err := p.saveLocked()
	p.mu.Unlock()
	p.poke()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + fmt.Sprintf(tl("已删除任务 #%d", "Deleted task #%d"), id))
}

// cmdToggle enables/disables a task by id.
func (p *AcronPlugin) cmdToggle(ctx *plugin.CommandContext, rest []string, on bool) error {
	tl := ctx.Tlocal
	id, ok := oneID(rest)
	if !ok {
		return ctx.Edit("❌ " + tl("请提供定时任务ID: ", "Provide a task id: ") + plugin.Code("acron "+map[bool]string{true: "on", false: "off"}[on]+" <ID>"))
	}
	p.mu.Lock()
	idx := p.indexLocked(id)
	if idx < 0 {
		p.mu.Unlock()
		return ctx.Edit("❌ " + fmt.Sprintf(tl("未找到任务 #%d", "Task #%d not found"), id))
	}
	t := p.tasks[idx]
	if on && t.Disabled == false {
		p.mu.Unlock()
		return ctx.Edit("❌ " + fmt.Sprintf(tl("任务 #%d 已处于启用状态", "Task #%d is already enabled"), id))
	}
	if !on && t.Disabled {
		p.mu.Unlock()
		return ctx.Edit("❌ " + fmt.Sprintf(tl("任务 #%d 已处于禁用状态", "Task #%d is already disabled"), id))
	}
	t.Disabled = !on
	var nextAt time.Time
	if on {
		if at := p.taskNext(*t, time.Now()); !at.IsZero() {
			p.next[id] = at
			nextAt = at
		}
	} else {
		delete(p.next, id)
	}
	loc := p.loc
	err := p.saveLocked()
	p.mu.Unlock()
	p.poke()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	if on {
		out := "▶️ " + fmt.Sprintf(tl("已启用任务 #%d", "Enabled task #%d"), id)
		if !nextAt.IsZero() {
			out += "\n" + tl("下次执行", "Next run") + "  " + plugin.Code(nextAt.In(loc).Format("2006-01-02 15:04:05"))
		}
		return ctx.Edit(out)
	}
	return ctx.Edit("⏸️ " + fmt.Sprintf(tl("已禁用任务 #%d", "Disabled task #%d"), id))
}

// oneID parses a single task id argument, reporting failure.
func oneID(rest []string) (int, bool) {
	if len(rest) == 0 {
		return 0, false
	}
	s := strings.TrimPrefix(strings.TrimSpace(rest[0]), "#")
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// chunkLines packs lines into chunks of at most max runes.
func chunkLines(lines []string, max int) []string {
	var out []string
	var cur strings.Builder
	n := 0
	for _, l := range lines {
		ln := len([]rune(l)) + 1
		if cur.Len() > 0 && n+ln > max {
			out = append(out, strings.TrimRight(cur.String(), "\n"))
			cur.Reset()
			n = 0
		}
		cur.WriteString(l)
		cur.WriteString("\n")
		n += ln
	}
	if cur.Len() > 0 {
		out = append(out, strings.TrimRight(cur.String(), "\n"))
	}
	return out
}
