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

// runResult is the outcome of one sign-in.
type runResult struct {
	Target  Target
	OK      bool
	Message string
}

const (
	replyTimeout = 10 * time.Second // wait for the target bot's reply
	sourceAuto   = "auto"
	sourceManual = "manual"
)

// resolveTarget turns a stored target string (@username or numeric id) into an
// input peer plus the PaperValet chat-id form.
func (p *CheckinPlugin) resolveTarget(ctx context.Context, t Target) (tg.InputPeerClass, int64, error) {
	s := strings.TrimSpace(t.Target)
	if s == "" {
		return nil, 0, fmt.Errorf("empty target")
	}
	if strings.HasPrefix(s, "@") && len(s) > 1 {
		peer, err := p.resolver.ResolveUsername(ctx, strings.TrimPrefix(s, "@"))
		if err != nil {
			return nil, 0, err
		}
		return peer, plugin.ChatIDOfInput(peer), nil
	}
	var id int64
	if _, err := fmt.Sscan(s, &id); err != nil || id == 0 {
		return nil, 0, fmt.Errorf("target must be @username or numeric id: %s", s)
	}
	peer, err := p.resolver.ResolveFromChatID(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	return peer, id, nil
}

// sendCommand sends the sign-in command to a target and returns the sent id.
func (p *CheckinPlugin) sendCommand(ctx context.Context, peer tg.InputPeerClass, command string) (int, error) {
	res, err := p.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  command,
		RandomID: time.Now().UnixNano(),
	})
	if err != nil {
		return 0, err
	}
	return sentID(res), nil
}

// sentID extracts the id of the message created by a send result.
func sentID(u tg.UpdatesClass) int {
	id := 0
	var list []tg.UpdateClass
	switch v := u.(type) {
	case *tg.UpdateShortSentMessage:
		return v.ID
	case *tg.Updates:
		list = v.Updates
	case *tg.UpdatesCombined:
		list = v.Updates
	}
	for _, up := range list {
		switch x := up.(type) {
		case *tg.UpdateNewMessage:
			if x.Message.GetID() > id {
				id = x.Message.GetID()
			}
		case *tg.UpdateNewChannelMessage:
			if x.Message.GetID() > id {
				id = x.Message.GetID()
			}
		case *tg.UpdateMessageID:
			if x.ID > id {
				id = x.ID
			}
		}
	}
	return id
}

// findCallbackData scans a message's inline keyboard for the configured
// callback-data or button-text matcher and returns the raw callback bytes.
func findCallbackData(msg *tg.Message, t *Target) []byte {
	if msg == nil {
		return nil
	}
	markup, ok := msg.ReplyMarkup.(*tg.ReplyInlineMarkup)
	if !ok {
		return nil
	}
	for _, row := range markup.Rows {
		for _, b := range row.Buttons {
			btn, ok := b.(*tg.KeyboardButtonCallback)
			if !ok {
				continue
			}
			if t.CallbackData != "" {
				if string(btn.Data) == t.CallbackData {
					return btn.Data
				}
			} else if btn.Text == t.ButtonText {
				return btn.Data
			}
		}
	}
	return nil
}

// pollHistory polls the target's history after minID until pick hits or the
// timeout expires, mirroring the TeleBox poll helper.
func (p *CheckinPlugin) pollHistory(
	ctx context.Context,
	peer tg.InputPeerClass,
	minID int,
	pick func([]*tg.Message) *tg.Message,
) *tg.Message {
	deadline := time.Now().Add(replyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollEvery):
		}
		res, err := p.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:     peer,
			OffsetID: minID,
			Limit:    10,
		})
		if err != nil {
			if d, ok := tgerr.AsFloodWait(err); ok && d <= time.Minute {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return nil
				}
			}
			continue
		}
		msgs := historyMessages(res)
		if hit := pick(msgs); hit != nil {
			return hit
		}
	}
	return nil
}

// historyMessages extracts plain messages from a get-history result, newest
// first, skipping service messages and empty entries.
func historyMessages(res tg.MessagesMessagesClass) []*tg.Message {
	mod, ok := res.AsModified()
	if !ok {
		return nil
	}
	var out []*tg.Message
	for _, m := range mod.GetMessages() {
		if msg, ok := m.(*tg.Message); ok {
			out = append(out, msg)
		}
	}
	return out
}

// messageEdited reports whether b is an edited/newer version of a.
func messageEdited(a, b *tg.Message) bool {
	if a == nil || b == nil {
		return false
	}
	if b.ID != a.ID {
		return b.ID > a.ID
	}
	return b.EditDate != a.EditDate || b.Message != a.Message
}

// runSingle signs in to one target: send the command, wait for the reply,
// click the sign-in button when configured, and collect the outcome.
func (p *CheckinPlugin) runSingle(ctx context.Context, t Target) runResult {
	fail := func(err error) runResult {
		return runResult{Target: t, OK: false, Message: errText(err)}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*replyTimeout)
	defer cancel()
	peer, _, err := p.resolveTarget(ctx, t)
	if err != nil {
		return fail(fmt.Errorf("resolve %s: %w", t.Target, err))
	}
	sentID, err := p.sendCommand(ctx, peer, t.Command)
	if err != nil {
		if d, ok := tgerr.AsFloodWait(err); ok {
			return fail(fmt.Errorf("FLOOD_WAIT %ds", int(d.Seconds())))
		}
		return fail(err)
	}

	needButton := hasMatcher(&t)
	var first *tg.Message
	if needButton {
		first = p.pollHistory(ctx, peer, sentID-1, func(msgs []*tg.Message) *tg.Message {
			for _, m := range msgs {
				if !m.Out && findCallbackData(m, &t) != nil {
					return m
				}
			}
			return nil
		})
	} else {
		first = p.pollHistory(ctx, peer, sentID-1, func(msgs []*tg.Message) *tg.Message {
			for _, m := range msgs {
				if !m.Out {
					return m
				}
			}
			return nil
		})
	}
	if first == nil {
		if needButton {
			return fail(errors.New("no reply with a sign-in button"))
		}
		return fail(errors.New("no sign-in reply"))
	}
	if !needButton {
		return runResult{Target: t, OK: true, Message: first.Message}
	}

	// Click the button. Telegram times out when the bot does not answer the
	// callback, but the click still took effect.
	answer := ""
	if res, err := p.api.MessagesGetBotCallbackAnswer(ctx, &tg.MessagesGetBotCallbackAnswerRequest{
		Peer:  peer,
		MsgID: first.ID,
		Data:  findCallbackData(first, &t),
	}); err == nil {
		answer = res.Message
	} else if !isBotTimeout(err) {
		return fail(err)
	}
	if answer != "" {
		return runResult{Target: t, OK: true, Message: answer}
	}

	// No popup: take the bot's next message or an edit of the button message.
	reply := p.pollHistory(ctx, peer, first.ID-1, func(msgs []*tg.Message) *tg.Message {
		for _, m := range msgs {
			if !m.Out && messageEdited(first, m) {
				return m
			}
		}
		return nil
	})
	if reply != nil && reply.Message != "" {
		return runResult{Target: t, OK: true, Message: reply.Message}
	}
	return runResult{Target: t, OK: true, Message: "sign-in button clicked"}
}

// isBotTimeout reports whether err is the BOT_RESPONSE_TIMEOUT that Telegram
// returns when a bot does not answer a callback query.
func isBotTimeout(err error) bool {
	if err == nil {
		return false
	}
	if tgerr.Is(err, "BOT_RESPONSE_TIMEOUT") {
		return true
	}
	return strings.Contains(err.Error(), "BOT_RESPONSE_TIMEOUT")
}

// errText renders an error for the summary.
func errText(err error) string {
	if err == nil {
		return ""
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		return e.Type
	}
	return truncateRunes(err.Error(), 200)
}
