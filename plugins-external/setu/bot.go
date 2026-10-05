// Conversation with @FinelyGirlsBot: resolve the peer, prepare the chat,
// send the command and classify the reply — ported from the TeleBox botmzt
// plugin's getBotResponse.

package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var errNoReply = errors.New("no reply")

// botReply is the classified answer of the bot.
type botReply struct {
	Media  tg.MessageMediaClass // photo or document, nil for text replies
	Text   string               // last text seen (error or check-in result)
	lastID int                  // newest bot message id seen (for read marking)
}

// askBot runs one command round-trip with the image bot.
func (p *SetuPlugin) askBot(cat *category) (*botReply, error) {
	run, cancel := context.WithTimeout(p.lifetime(), replyTimeout+15*time.Second)
	defer cancel()

	peer, err := p.botPeer(run)
	if err != nil {
		return nil, err
	}
	p.prepare(run, peer)

	events := make(chan *tg.Message, 16)
	stop := p.host.Listen(p.Name(), func(_ context.Context, ev *plugin.MessageEvent, _ bool) {
		if ev == nil || ev.Message == nil || ev.IsOut || ev.ChatID != peer.UserID {
			return
		}
		select {
		case events <- ev.Message:
		default:
		}
	})
	defer stop()

	sent, err := p.host.API().MessagesSendMessage(run, &tg.MessagesSendMessageRequest{
		Peer: peer, Message: "/" + cat.Command, RandomID: time.Now().UnixNano(), NoWebpage: true,
	})
	if err != nil {
		return nil, err
	}
	after := sentID(sent)

	wait, wcancel := context.WithTimeout(p.lifetime(), replyTimeout)
	defer wcancel()
	return p.waitReply(wait, events, after, cat.Text)
}

// waitReply accepts only messages newer than after. For image commands a
// photo or document finishes the wait; error keywords fail it; other texts
// (progress notes like "searching…") are kept and the wait goes on. For the
// check-in any reply is the result. On timeout the last progress text is NOT
// a result — it goes through errNoReply like a silent bot.
func (p *SetuPlugin) waitReply(ctx context.Context, events <-chan *tg.Message, after int, anyText bool) (*botReply, error) {
	last := ""
	maxID := after
	for {
		select {
		case <-ctx.Done():
			if anyText && last != "" {
				return &botReply{Text: last, lastID: maxID}, nil
			}
			return nil, errNoReply
		case m := <-events:
			if m.ID <= after {
				continue // stale message from before our command
			}
			if m.ID > maxID {
				maxID = m.ID
			}
			if m.Media != nil {
				if _, empty := m.Media.(*tg.MessageMediaEmpty); !empty {
					return &botReply{Media: m.Media, Text: m.Message, lastID: m.ID}, nil
				}
			}
			txt := strings.TrimSpace(m.Message)
			if txt != "" {
				last = txt
				if anyText || isBotError(txt) {
					return &botReply{Text: txt, lastID: m.ID}, nil
				}
			}
		}
	}
}

// botPeer resolves and caches the bot's InputPeerUser.
func (p *SetuPlugin) botPeer(ctx context.Context) (*tg.InputPeerUser, error) {
	p.mu.Lock()
	peer := p.peer
	p.mu.Unlock()
	if peer != nil {
		return peer, nil
	}
	in, err := p.host.PeerResolver().ResolveUsername(ctx, botUsername)
	if err != nil {
		return nil, err
	}
	u, ok := in.(*tg.InputPeerUser)
	if !ok {
		return nil, errors.New("@" + botUsername + " is not a user bot")
	}
	p.mu.Lock()
	p.peer = u
	p.mu.Unlock()
	return u, nil
}

// prepare unblocks the bot and, when the chat was never opened, /starts it
// so the actual command is not the first message (some bots stay silent
// otherwise). Errors are best-effort, like the source.
func (p *SetuPlugin) prepare(ctx context.Context, peer *tg.InputPeerUser) {
	api := p.host.API()
	_, _ = api.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peer})
	h, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 1})
	if err != nil {
		return
	}
	if mm, ok := h.(interface{ GetMessages() []tg.MessageClass }); ok && len(mm.GetMessages()) == 0 {
		_, _ = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peer, Message: "/start", RandomID: time.Now().UnixNano(),
		})
		select {
		case <-ctx.Done():
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

// sentID digs the new message id out of a send result.
func sentID(u tg.UpdatesClass) int {
	switch v := u.(type) {
	case *tg.UpdateShortSentMessage:
		return v.ID
	case *tg.Updates:
		for _, x := range v.Updates {
			if m, ok := x.(*tg.UpdateMessageID); ok {
				return m.ID
			}
		}
	}
	return 0
}

// errorKeywords mirror the source's list: bot texts matching one of these
// are reported as errors instead of waiting for an image that never comes.
var errorKeywords = []string{"没有找到", "未找到", "错误", "失败", "不存在", "无法", "无效", "error", "failed", "not found", "invalid", "unavailable"}

func isBotError(text string) bool {
	t := strings.ToLower(text)
	for _, k := range errorKeywords {
		if strings.Contains(t, k) {
			return true
		}
	}
	return false
}

// clipText shortens remote text for display.
func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
