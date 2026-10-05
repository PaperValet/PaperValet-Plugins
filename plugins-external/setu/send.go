// Delivering the bot's media into the command chat: referenced by id and
// access hash (no re-upload), spoilered per settings, replying to the same
// message the command replied to — like the source's messages.sendMedia
// call with inputMediaPhoto/inputMediaDocument.

package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// sendMedia re-sends the bot's photo or document by reference into the
// command chat.
func (p *SetuPlugin) sendMedia(ctx *plugin.CommandContext, media tg.MessageMediaClass, replyTo int) error {
	var input tg.InputMediaClass
	spoiler := p.set.Bool("spoiler")
	switch m := media.(type) {
	case *tg.MessageMediaPhoto:
		ph, ok := m.Photo.(*tg.Photo)
		if !ok {
			return errors.New("bot sent a broken photo reference")
		}
		im := &tg.InputMediaPhoto{ID: &tg.InputPhoto{
			ID: ph.ID, AccessHash: ph.AccessHash, FileReference: ph.FileReference,
		}}
		if spoiler {
			im.SetSpoiler(true)
		}
		input = im
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return errors.New("bot sent a broken document reference")
		}
		im := &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference,
		}}
		if spoiler {
			im.SetSpoiler(true)
		}
		input = im
	default:
		return fmt.Errorf("unsupported media %T", media)
	}

	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	req := &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    input,
		Message:  "",
		RandomID: rand.Int64(),
	}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	if _, err := ctx.API.MessagesSendMedia(ctx.Context(), req); err != nil {
		return err
	}

	// Mark the bot conversation read, like the source (best effort).
	if peer, err := p.botPeer(ctx.Context()); err == nil {
		_, _ = ctx.API.MessagesReadHistory(ctx.Context(), &tg.MessagesReadHistoryRequest{
			Peer: peer, MaxID: int(time.Now().Unix()),
		})
	}
	return nil
}

// errText renders failures: timeouts, flood waits, blocked bot and raw
// Telegram errors.
func (p *SetuPlugin) errText(tl func(string, string) string, err error) string {
	switch {
	case errors.Is(err, errNoReply):
		return tl("@"+botUsername+" 没有回应，请稍后重试", "@"+botUsername+" did not answer, try again later")
	case errors.Is(err, context.DeadlineExceeded):
		return tl("请求超时，请稍后重试", "The request timed out, try again later")
	}
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "Flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	if e, ok := tgerr.As(err); ok {
		switch e.Type {
		case "USER_BLOCKED", "USER_IS_BLOCKED":
			return tl("无法访问机器人，请先私聊 @"+botUsername+" 并发送 /start", "Cannot reach the bot; open a private chat with @"+botUsername+" and send /start")
		case "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID":
			return tl("机器人不存在了", "the bot no longer exists")
		case "CHAT_SEND_MEDIA_FORBIDDEN":
			return tl("这个聊天不允许发送媒体", "this chat forbids sending media")
		}
		return plugin.Code(e.Type)
	}
	return plugin.Escape(err.Error())
}
