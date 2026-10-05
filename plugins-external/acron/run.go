package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const runTimeout = 60 * time.Second

// runTask executes one task and returns a result summary.
func (p *AcronPlugin) runTask(ctx context.Context, t *Task) (string, error) {
	if p.host == nil {
		return "", errors.New("host unavailable")
	}
	api := p.host.API()
	resolver := p.host.PeerResolver()
	if api == nil {
		return "", errors.New("API client unavailable")
	}
	rctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	peer := t.Peer.input()
	if peer == nil && resolver != nil {
		if t.ChatID != 0 {
			pl, err := resolver.ResolveFromChatID(rctx, t.ChatID)
			if err != nil {
				return "", fmt.Errorf("resolve chat %d: %w", t.ChatID, err)
			}
			peer = pl
		} else if t.Chat != "" {
			name := strings.TrimPrefix(t.Chat, "@")
			pl, err := resolver.ResolveUsername(rctx, name)
			if err != nil {
				return "", fmt.Errorf("resolve %s: %w", t.Chat, err)
			}
			peer = pl
		}
	}
	if peer == nil {
		return "", fmt.Errorf("cannot resolve target chat %q", t.Chat)
	}

	switch t.Type {
	case typeSend:
		return runSend(rctx, api, peer, t)
	case typeCmd:
		return p.runCmd(rctx, api, peer, t)
	case typeCopy, typeForward:
		return runForward(rctx, api, peer, t)
	case typeDel:
		return runDel(rctx, api, peer, t)
	case typeDelRe:
		return runDelRe(rctx, api, peer, t)
	case typePin:
		return runPin(rctx, api, peer, t, false)
	case typeUnpin:
		return runPin(rctx, api, peer, t, true)
	}
	return "", fmt.Errorf("unknown task type %q", t.Type)
}

func runSend(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, t *Task) (string, error) {
	req := &tg.MessagesSendMessageRequest{Peer: peer, Message: t.Message, RandomID: time.Now().UnixNano()}
	if ents := decodeEntities(t.Entities); len(ents) > 0 {
		req.SetEntities(ents)
	}
	if t.ReplyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: t.ReplyTo})
	}
	if _, err := api.MessagesSendMessage(ctx, req); err != nil {
		return "", err
	}
	return "已发送 1 条消息", nil
}

// runCmd sends the command text into the target chat and runs it as the
// owner on the sent message.
func (p *AcronPlugin) runCmd(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, t *Task) (string, error) {
	if p.host == nil {
		return "", errors.New("host unavailable")
	}
	req := &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  t.Message,
		RandomID: time.Now().UnixNano(),
	}
	if t.ReplyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: t.ReplyTo})
	}
	upd, err := api.MessagesSendMessage(ctx, req)
	if err != nil {
		return "", err
	}
	msgID := sentID(upd)
	if msgID <= 0 {
		return "已发送命令消息", nil
	}
	// Fetch the sent message so RunCommand gets a real message context.
	msgs, _, _, err := plugin.GetMessages(ctx, api, peer, msgID)
	if err != nil {
		return "已发送命令消息", nil
	}
	for _, m := range msgs {
		if m.ID == msgID {
			ev := plugin.EventFromMessage(m)
			if _, err := p.host.RunCommand(ctx, ev); err != nil {
				return "已发送命令消息", fmt.Errorf("run command: %w", err)
			}
			return "已执行命令", nil
		}
	}
	return "已发送命令消息", nil
}

// sentID digs the new message id out of a send result; 0 if absent.
func sentID(u tg.UpdatesClass) int {
	switch v := u.(type) {
	case *tg.UpdateShortSentMessage:
		return v.ID
	case *tg.Updates:
		return idFromUpdates(v.Updates)
	case *tg.UpdatesCombined:
		return idFromUpdates(v.Updates)
	}
	return 0
}

func idFromUpdates(list []tg.UpdateClass) int {
	for _, up := range list {
		switch x := up.(type) {
		case *tg.UpdateMessageID:
			return x.ID
		case *tg.UpdateNewMessage:
			if m, ok := x.Message.(*tg.Message); ok {
				return m.ID
			}
		case *tg.UpdateNewChannelMessage:
			if m, ok := x.Message.(*tg.Message); ok {
				return m.ID
			}
		}
	}
	return 0
}

func runForward(ctx context.Context, api *tg.Client, dest tg.InputPeerClass, t *Task) (string, error) {
	from := t.FromPeer.input()
	if from == nil {
		return "", errors.New("source chat not resolved")
	}
	req := &tg.MessagesForwardMessagesRequest{
		FromPeer: from,
		ID:       []int{t.FromMsg},
		RandomID: []int64{time.Now().UnixNano()},
		ToPeer:   dest,
	}
	if t.Type == typeCopy {
		req.SetDropAuthor(true)
	}
	if t.ReplyTo > 0 {
		// A topic id targets the topic root; a plain reply id stays a reply.
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: t.ReplyTo})
	}
	if _, err := api.MessagesForwardMessages(ctx, req); err != nil {
		return "", err
	}
	if t.Type == typeCopy {
		return "已复制发送 1 条消息", nil
	}
	return "已转发 1 条消息", nil
}

func runDel(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, t *Task) (string, error) {
	if err := plugin.DeleteMessages(ctx, api, peer, t.MsgID); err != nil {
		return "", err
	}
	return fmt.Sprintf("已删除消息 %d", t.MsgID), nil
}

func runDelRe(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, t *Task) (string, error) {
	limit := t.Limit
	if limit <= 0 {
		limit = 100
	}
	re, err := tryParseRegex(t.Regex)
	if err != nil {
		return "", fmt.Errorf("bad regex: %w", err)
	}
	// Scan the most recent `limit` messages, collecting matching ids.
	var ids []int
	scanned := 0
	offsetID := 0
	for scanned < limit {
		pageSize := 100
		if r := limit - scanned; r < pageSize {
			pageSize = r
		}
		page, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, OffsetID: offsetID, Limit: pageSize})
		if err != nil {
			return "", err
		}
		mod, ok := page.AsModified()
		if !ok {
			break
		}
		msgs := mod.GetMessages()
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			msg, ok := m.(*tg.Message)
			if !ok {
				continue
			}
			scanned++
			offsetID = msg.ID
			if re.MatchString(msg.Message) {
				ids = append(ids, msg.ID)
			}
		}
		if len(msgs) < pageSize {
			break
		}
	}
	if len(ids) == 0 {
		return "无匹配消息", nil
	}
	if err := plugin.DeleteMessages(ctx, api, peer, ids...); err != nil {
		return "", err
	}
	return fmt.Sprintf("匹配并删除 %d 条", len(ids)), nil
}

func runPin(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, t *Task, unpin bool) (string, error) {
	req := &tg.MessagesUpdatePinnedMessageRequest{Peer: peer, ID: t.MsgID}
	if unpin {
		req.SetUnpin(true)
	} else {
		if !t.Notify {
			req.SetSilent(true)
		}
		if t.PmOneSide {
			req.SetPmOneside(true)
		}
	}
	if _, err := api.MessagesUpdatePinnedMessage(ctx, req); err != nil {
		return "", err
	}
	if unpin {
		return fmt.Sprintf("已取消置顶消息 %d", t.MsgID), nil
	}
	return fmt.Sprintf("已置顶消息 %d", t.MsgID), nil
}

// runErrorText turns an API error into a short bilingual message.
func runErrorText(tl func(string, string) string, err error) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf(tl("触发限流，需等待 %d 秒", "flood wait %d seconds"), int(d.Seconds())+1)
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		switch e.Type {
		case "CHAT_ADMIN_REQUIRED":
			return tl("需要管理员权限", "Admin rights are needed")
		case "CHAT_WRITE_FORBIDDEN":
			return tl("没有在该对话发送消息的权限", "No permission to post in that chat")
		}
		return e.Type
	}
	return err.Error()
}
