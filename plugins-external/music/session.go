package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// session is one conversation with a music bot: it listens to the bot's
// chat for messages newer than `after` and classifies them.
type session struct {
	p      *MusicPlugin
	ctx    context.Context
	src    *source
	peer   *tg.InputPeerUser
	botID  int64
	after  int
	events chan *tg.Message
	stop   func()
}

type result struct {
	msg     *tg.Message
	audio   *tg.Message
	buttons []*tg.KeyboardButtonCallback
	text    string
	err     error
}

func (p *MusicPlugin) open(ctx context.Context, src *source) (*session, error) {
	peer, err := p.botPeer(ctx, src)
	if err != nil {
		return nil, err
	}
	// Prepare first so the /start welcome is not taken as the answer.
	p.prepare(ctx, src, peer)
	s := &session{p: p, ctx: ctx, src: src, peer: peer, botID: peer.UserID, events: make(chan *tg.Message, 32)}
	// Edits count too: bots edit "downloading…" into the result or the
	// error. wait drops anything older than s.after.
	s.stop = p.host.Listen(p.Name(), func(_ context.Context, ev *plugin.MessageEvent, _ bool) {
		if ev == nil || ev.Message == nil || ev.IsOut || ev.ChatID != s.botID {
			return
		}
		select {
		case s.events <- ev.Message:
		default:
			// The buffer is full only if wait already returned; dropping
			// is expected then, but leave a trace for debugging timeouts.
			if p.log != nil {
				p.log.Debug("music: session event buffer full, dropping message", "bot", s.src.Bot, "id", ev.Message.ID)
			}
		}
	})
	return s, nil
}

func (s *session) close() { s.stop() }

func (s *session) send(text string) error {
	upd, err := s.p.host.API().MessagesSendMessage(s.ctx, &tg.MessagesSendMessageRequest{
		Peer: s.peer, Message: text, RandomID: time.Now().UnixNano(), NoWebpage: true,
	})
	if err != nil {
		return err
	}
	if id := sentID(upd); id > s.after {
		s.after = id
	}
	return nil
}

// click presses a callback button. Bots often answer the callback only
// after the upload, so it runs in the background and its error is ignored;
// the audio message is what counts.
func (s *session) click(msgID int, data []byte) {
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		defer cancel()
		_, _ = s.p.host.API().MessagesGetBotCallbackAnswer(ctx, &tg.MessagesGetBotCallbackAnswerRequest{Peer: s.peer, MsgID: msgID, Data: data})
	}()
}

// wait returns the first audio or result list. Plain texts that read like
// errors end the wait; other texts are progress notes ("downloading…")
// that extend the wait to `slow` and are kept as the answer if nothing
// better comes.
func (s *session) wait(first, slow time.Duration) result {
	t := time.NewTimer(first)
	defer t.Stop()
	start := time.Now()
	extended := false
	last := ""
	for {
		select {
		case <-s.ctx.Done():
			if last != "" {
				return result{text: last}
			}
			return result{err: errTimeout}
		case <-t.C:
			if last != "" {
				return result{text: last}
			}
			return result{err: errTimeout}
		case m := <-s.events:
			d, _ := audioDoc(m)
			// s.after is our query or the picked list; the list may be
			// edited into the audio, nothing else from it counts.
			if m.ID < s.after || (m.ID == s.after && d == nil) {
				continue
			}
			if d != nil {
				return result{msg: m, audio: m}
			}
			if b := pickButtons(m, s.src.btn); len(b) > 0 {
				return result{msg: m, buttons: b, text: m.Message}
			}
			if txt := strings.TrimSpace(m.Message); txt != "" {
				last = txt
				if isFailure(txt) {
					return result{msg: m, text: txt}
				}
				if !extended && slow > first {
					extended = true
					t.Reset(slow - time.Since(start))
				}
			}
		}
	}
}

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

func (p *MusicPlugin) botPeer(ctx context.Context, src *source) (*tg.InputPeerUser, error) {
	p.mu.Lock()
	peer := p.peers[src.Key]
	p.mu.Unlock()
	if peer != nil {
		return peer, nil
	}
	in, err := p.host.PeerResolver().ResolveUsername(ctx, src.Bot)
	if err != nil {
		return nil, err
	}
	u, ok := in.(*tg.InputPeerUser)
	if !ok {
		return nil, fmt.Errorf("@%s is not a bot", src.Bot)
	}
	p.mu.Lock()
	p.peers[src.Key] = u
	p.mu.Unlock()
	return u, nil
}

// prepare runs once per bot and process: unblock, mute, and /start when
// the chat has never been opened.
func (p *MusicPlugin) prepare(ctx context.Context, src *source, peer *tg.InputPeerUser) {
	p.mu.Lock()
	done := p.prepared[src.Key]
	p.prepared[src.Key] = true
	p.mu.Unlock()
	if done {
		return
	}
	api := p.host.API()
	_, _ = api.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peer})
	settings := tg.InputPeerNotifySettings{}
	settings.SetMuteUntil(1<<31 - 1)
	settings.SetSilent(true)
	if _, err := api.AccountUpdateNotifySettings(ctx, &tg.AccountUpdateNotifySettingsRequest{Peer: &tg.InputNotifyPeer{Peer: peer}, Settings: settings}); err != nil {
		p.log.Debug("music: mute failed", "bot", src.Bot, "error", err)
	}
	h, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 1})
	if err != nil {
		return
	}
	if mm, ok := h.(interface{ GetMessages() []tg.MessageClass }); ok && len(mm.GetMessages()) == 0 {
		_, _ = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: peer, Message: "/start", RandomID: time.Now().UnixNano()})
		time.Sleep(1500 * time.Millisecond)
	}
}
