package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// remarkFromLine extracts the remark: everything after the first n+1
// whitespace-separated tokens (command word plus n arguments).
func remarkFromLine(line string, n int) string {
	fields := strings.Fields(line)
	if len(fields) <= n+1 {
		return ""
	}
	return strings.TrimSpace(strings.Join(fields[n+1:], " "))
}

// cmdAdd creates a task of the given type.
func (p *AcronPlugin) cmdAdd(ctx *plugin.CommandContext, sub string, rest []string, firstLine string) error {
	tl := ctx.Tlocal
	cronExpr, rest, err := parseCronFromArgs(rest)
	if err != nil {
		return ctx.Edit("❌ " + tl("无效的 Cron 表达式（需要 6 个字段：秒 分 时 日 月 周）: ", "Invalid cron expression (needs 6 fields: sec min hour dom mon dow): ") + plugin.Escape(err.Error()))
	}
	if len(rest) == 0 {
		return ctx.Edit("❌ " + tl("请提供对话ID或@用户名", "Provide a chat id or @username"))
	}
	chatArg, replyTo, err := parseChatArg(rest[0])
	if err != nil {
		return ctx.Edit("❌ " + tl("无效的对话参数: ", "Invalid chat argument: ") + plugin.Escape(err.Error()))
	}
	rest = rest[1:]

	// Resolve the target chat once and persist it.
	ref, chatID, display, rerr := resolveChatRef(ctx.Context(), ctx.PeerResolver, ctx.API, chatArg)
	if rerr != nil {
		return ctx.Edit("❌ " + tl("解析对话失败: ", "Failed to resolve chat: ") + plugin.Escape(rerr.Error()))
	}

	t := &Task{
		Type:      sub,
		Cron:      cronExpr,
		Chat:      chatArg,
		ChatID:    chatID,
		Peer:      ref,
		Display:   display,
		ReplyTo:   replyTo,
		CreatedAt: time.Now().Unix(),
	}

	switch sub {
	case typeSend:
		if ctx.Message.ReplyToID == 0 {
			return ctx.Edit("❌ " + tl("请回复一条要定时发送的消息", "Reply to the message to send on schedule"))
		}
		replied, err := ctx.ReplyMessage()
		if err != nil {
			return ctx.Edit("❌ " + tl("读取被回复的消息失败: ", "Cannot read the replied message: ") + plugin.Escape(err.Error()))
		}
		if replied.Media != nil || hasMarkup(replied) {
			return ctx.Edit("❌ " + tl("不支持带多媒体或按钮的消息，可改用 copy/forward", "Media or button messages are not supported; use copy/forward instead"))
		}
		if strings.TrimSpace(replied.Message) == "" {
			return ctx.Edit("❌ " + tl("请回复一条包含文本的消息", "Reply to a message containing text"))
		}
		t.Message = replied.Message
		t.Entities = encodeEntities(replied.Entities)
		t.Remark = remarkFromLine(firstLine, 8)
	case typeCmd:
		lines := strings.SplitN(ctx.Message.Text, "\n", 2)
		if len(lines) < 2 || strings.TrimSpace(lines[1]) == "" {
			return ctx.Edit("❌ " + tl("请在第二行写要执行的命令", "Write the command to run on the second line"))
		}
		t.Message = strings.TrimSpace(lines[1])
		t.Remark = remarkFromLine(firstLine, 8)
	case typeCopy, typeForward:
		if ctx.Message.ReplyToID == 0 {
			return ctx.Edit("❌ " + tl("请回复一条要复制/转发的源消息", "Reply to the source message to copy/forward"))
		}
		replied, err := ctx.ReplyMessage()
		if err != nil {
			return ctx.Edit("❌ " + tl("读取被回复的消息失败: ", "Cannot read the replied message: ") + plugin.Escape(err.Error()))
		}
		fromChatID := plugin.ChatIDOf(replied.PeerID)
		var fromPeer *peerRef
		if ctx.PeerResolver != nil {
			if fp, ferr := ctx.PeerResolver.ResolveFromChatID(ctx.Context(), fromChatID); ferr == nil && fp != nil {
				fromPeer = refFromPeer(fp)
			}
		}
		if fromPeer == nil {
			fromPeer = refFromPeer(peerOfMessage(replied))
		}
		t.FromPeer = fromPeer
		t.FromMsg = replied.ID
		t.FromChat = describePeerOfMessage(replied)
		t.Remark = remarkFromLine(firstLine, 8)
	case typeDel, typeUnpin:
		if len(rest) < 1 {
			return ctx.Edit("❌ " + tl("请提供消息 ID", "Provide a message id"))
		}
		id, err := strconv.Atoi(rest[0])
		if err != nil || id <= 0 {
			return ctx.Edit("❌ " + tl("无效的消息 ID: ", "Invalid message id: ") + plugin.Code(rest[0]))
		}
		t.MsgID = id
		t.Remark = remarkFromLine(firstLine, 9)
	case typeDelRe:
		if len(rest) < 2 {
			return ctx.Edit("❌ " + tl("用法: acron del_re <cron> <对话> <条数> <正则> [备注]", "Usage: acron del_re <cron> <chat> <count> <regex> [remark]"))
		}
		limit, err := strconv.Atoi(rest[0])
		if err != nil || limit <= 0 {
			return ctx.Edit("❌ " + tl("请提供有效的条数限制(正整数): ", "Provide a valid positive count: ") + plugin.Code(rest[0]))
		}
		if _, err := tryParseRegex(rest[1]); err != nil {
			return ctx.Edit("❌ " + tl("无效的正则表达式: ", "Invalid regex: ") + plugin.Escape(err.Error()))
		}
		t.Limit = limit
		t.Regex = rest[1]
		t.Remark = remarkFromLine(firstLine, 10)
	case typePin:
		if len(rest) < 3 {
			return ctx.Edit("❌ " + tl("用法: acron pin <cron> <对话> <消息ID> <通知1/0> <仅自己1/0> [备注]", "Usage: acron pin <cron> <chat> <msgID> <notify 1/0> <pmOneSide 1/0> [remark]"))
		}
		id, err := strconv.Atoi(rest[0])
		if err != nil || id <= 0 {
			return ctx.Edit("❌ " + tl("无效的消息 ID: ", "Invalid message id: ") + plugin.Code(rest[0]))
		}
		t.MsgID = id
		t.Notify = parseBoolArg(rest[1])
		t.PmOneSide = parseBoolArg(rest[2])
		t.Remark = remarkFromLine(firstLine, 11)
	}

	p.mu.Lock()
	nextAt, err := p.addTaskLocked(t)
	loc := p.loc
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存任务失败: ", "Failed to save task: ") + plugin.Escape(err.Error()))
	}
	p.poke()

	prefix := ""
	if p.host != nil {
		if ps := p.host.Prefixes(); len(ps) > 0 && ps[0] != "" && ps[0] != "/" {
			prefix = ps[0]
		}
	}

	var b strings.Builder
	b.WriteString("✅ **" + tl("已添加任务", "Task added") + " " + plugin.Code("#"+strconv.Itoa(t.ID)) + "**\n\n")
	b.WriteString(tl("类型", "Type") + "  " + plugin.Code(taskTypeLabel(tl, t.Type)) + "\n")
	b.WriteString(tl("表达式", "Cron") + "  " + plugin.Code(t.Cron) + "\n")
	b.WriteString(tl("对话", "Chat") + "  " + escapeChat(*t) + "\n")
	if t.ReplyTo != 0 {
		b.WriteString(tl("话题/回复", "Topic/reply") + "  " + plugin.Code(t.ReplyTo) + "\n")
	}
	if t.MsgID != 0 && (t.Type == typeDel || t.Type == typePin || t.Type == typeUnpin) {
		b.WriteString(tl("消息", "Message") + "  " + plugin.Code(t.MsgID) + "\n")
	}
	if t.Type == typePin {
		b.WriteString(tl("通知", "Notify") + "  " + plugin.Code(boolDigit(t.Notify)) + "\n")
		b.WriteString(tl("仅自己置顶", "PM one-side") + "  " + plugin.Code(boolDigit(t.PmOneSide)) + "\n")
	}
	if t.Type == typeDelRe {
		b.WriteString(tl("最近条数", "Recent count") + "  " + plugin.Code(t.Limit) + "\n")
		b.WriteString(tl("正则", "Regex") + "  " + plugin.Code(t.Regex) + "\n")
	}
	if t.Type == typeCmd {
		b.WriteString(tl("命令", "Command") + "  " + plugin.Code(t.Message) + "\n")
	}
	if t.Remark != "" {
		b.WriteString(tl("备注", "Remark") + "  " + plugin.Escape(t.Remark) + "\n")
	}
	if !nextAt.IsZero() {
		b.WriteString(tl("下次执行", "Next run") + "  " + plugin.Code(nextAt.In(loc).Format("2006-01-02 15:04:05")) + "\n")
	}
	cmd := buildCopyCommand(prefix, *t)
	if strings.Contains(cmd, "\n") {
		b.WriteString(tl("复制命令", "Copy command") + ":\n" + plugin.Pre(cmd))
	} else {
		b.WriteString(tl("复制命令", "Copy command") + "  " + plugin.Code(cmd))
	}
	return ctx.Edit(b.String())
}

func boolDigit(b bool) int {
	if b {
		return 1
	}
	return 0
}

// addTaskLocked assigns the task id, registers it and persists everything,
// rolling the id counter back when the save fails so it is not burned on a
// task that was never stored. Callers hold mu.
func (p *AcronPlugin) addTaskLocked(t *Task) (nextAt time.Time, err error) {
	t.ID = p.nextID
	p.nextID++
	p.tasks = append(p.tasks, t)
	if at := p.taskNext(*t, time.Now()); !at.IsZero() {
		p.next[t.ID] = at
	}
	nextAt = p.next[t.ID]
	if err := p.saveLocked(); err != nil {
		p.tasks = p.tasks[:len(p.tasks)-1]
		delete(p.next, t.ID)
		p.nextID-- // nothing persisted; reuse the id on the next attempt
		return time.Time{}, err
	}
	return nextAt, nil
}

// hasMarkup reports whether the message carries an inline keyboard.
func hasMarkup(m *tg.Message) bool {
	return m != nil && m.ReplyMarkup != nil
}

// peerOfMessage returns the input peer for the chat a message belongs to.
// The message's own PeerID identifies its chat; access hashes come from
// full message data when present, otherwise the raw id form is used.
func peerOfMessage(m *tg.Message) tg.InputPeerClass {
	if m == nil {
		return nil
	}
	switch v := m.PeerID.(type) {
	case *tg.PeerUser:
		return &tg.InputPeerUser{UserID: v.UserID}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: v.ChatID}
	case *tg.PeerChannel:
		return &tg.InputPeerChannel{ChannelID: v.ChannelID}
	}
	return nil
}

// describePeerOfMessage renders a short description of a message's chat.
func describePeerOfMessage(m *tg.Message) string {
	if m == nil {
		return ""
	}
	return describeInputPeer(peerOfMessage(m))
}
