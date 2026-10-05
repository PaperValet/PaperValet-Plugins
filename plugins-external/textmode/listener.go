package main

// listener.go — the auto-format path. Mirrors mode.ts listenMessageHandler:
// only the account's own outgoing plain-text messages, never commands (any
// configured prefix or "/"), never edits (our own edits come back as edited).
// The message is re-sent through messages.editMessage with the text verbatim
// and its existing entities plus one new entity (or four, for "all") covering
// the whole text — raw API, because PaperValet's Markdown layer has no
// underline.

import (
	"context"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// editTimeout bounds one edit call.
const editTimeout = 15 * time.Second

// utf16Len counts UTF-16 code units (Telegram entity offsets).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// modeEntities returns the entity constructors for m (all four for modeAll).
func modeEntities(m mode) []func(off, length int) tg.MessageEntityClass {
	switch m {
	case modeDel:
		return []func(int, int) tg.MessageEntityClass{strikeEntity}
	case modeBold:
		return []func(int, int) tg.MessageEntityClass{boldEntity}
	case modeItalic:
		return []func(int, int) tg.MessageEntityClass{italicEntity}
	case modeUnderline:
		return []func(int, int) tg.MessageEntityClass{underlineEntity}
	case modeMask:
		return []func(int, int) tg.MessageEntityClass{spoilerEntity}
	case modeAll:
		return []func(int, int) tg.MessageEntityClass{strikeEntity, boldEntity, italicEntity, underlineEntity}
	default:
		return nil
	}
}

func strikeEntity(off, length int) tg.MessageEntityClass {
	return &tg.MessageEntityStrike{Offset: off, Length: length}
}
func boldEntity(off, length int) tg.MessageEntityClass {
	return &tg.MessageEntityBold{Offset: off, Length: length}
}
func italicEntity(off, length int) tg.MessageEntityClass {
	return &tg.MessageEntityItalic{Offset: off, Length: length}
}
func underlineEntity(off, length int) tg.MessageEntityClass {
	return &tg.MessageEntityUnderline{Offset: off, Length: length}
}
func spoilerEntity(off, length int) tg.MessageEntityClass {
	return &tg.MessageEntitySpoiler{Offset: off, Length: length}
}

// buildEntities keeps the message's existing entities and appends one per
// constructor covering the whole text.
func buildEntities(existing []tg.MessageEntityClass, text string, ctors []func(off, length int) tg.MessageEntityClass) []tg.MessageEntityClass {
	out := make([]tg.MessageEntityClass, 0, len(existing)+len(ctors))
	if len(existing) > 0 {
		out = append(out, existing...)
	}
	n := utf16Len(text)
	for _, ctor := range ctors {
		out = append(out, ctor(0, n))
	}
	return out
}

// onMessage runs on the update path: filter fast, edit in a goroutine.
func (p *TextmodePlugin) onMessage(_ context.Context, ev *plugin.MessageEvent, edited bool) {
	if edited || ev == nil || ev.Message == nil || !ev.IsOut {
		return
	}
	text := strings.TrimSpace(ev.Text)
	if text == "" {
		return
	}
	if isCommand(text, p.host.Prefixes()) {
		return
	}
	m := p.store.shouldProcess(ev.ChatID, p.globalMode())
	if m == modeOff {
		return
	}
	ctors := modeEntities(m)
	if len(ctors) == 0 {
		return
	}
	// Copy what the edit needs, then run it off the update path.
	chatID, msgID := ev.ChatID, ev.Message.ID
	entities := buildEntities(ev.Entities, text, ctors)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if err := p.editMessage(p.lifetime(), chatID, msgID, text, entities); err != nil {
			if tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
				return
			}
			if d, ok := tgerr.AsFloodWait(err); ok {
				p.log.Warn("textmode: flood wait", "chat", chatID, "seconds", int(d.Seconds()))
				return
			}
			p.log.Debug("textmode: edit failed", "chat", chatID, "msg", msgID, "error", err)
		}
	}()
}

// isCommand reports whether text starts with a configured command prefix.
func isCommand(text string, prefixes []string) bool {
	if strings.HasPrefix(text, "/") {
		return true
	}
	for _, pref := range prefixes {
		if pref != "" && strings.HasPrefix(text, pref) {
			return true
		}
	}
	return false
}

// editMessage re-applies text with the full-length entity layer. The text is
// sent verbatim (no Markdown parsing) and existing entities are preserved,
// so nothing the user wrote is mangled.
func (p *TextmodePlugin) editMessage(ctx context.Context, chatID int64, msgID int, text string, entities []tg.MessageEntityClass) error {
	ctx, cancel := context.WithTimeout(ctx, editTimeout)
	defer cancel()
	peer, err := p.host.PeerResolver().ResolveFromChatID(ctx, chatID)
	if err != nil {
		return err
	}
	req := &tg.MessagesEditMessageRequest{
		Peer:     peer,
		ID:       msgID,
		Message:  text,
		Entities: entities,
	}
	_, err = p.host.API().MessagesEditMessage(ctx, req)
	return err
}
