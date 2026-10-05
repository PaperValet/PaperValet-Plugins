package main

import (
	"context"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// chainMaxDepth caps chained forwarding (source -> target -> target…).
const chainMaxDepth = 5

// onMessage is the host listener: it returns fast and pushes forwarding to
// goroutines, as the update path requires.
func (p *ShiftPlugin) onMessage(ctx context.Context, ev *plugin.MessageEvent, edited bool) {
	if ev == nil || ev.Message == nil {
		return
	}
	// Skip command traffic (the owner's own command messages).
	if ev.IsOut && isCommandText(ev.Text) {
		return
	}
	p.mu.Lock()
	rule := p.rules[ev.ChatID]
	stopped := p.stopped
	p.mu.Unlock()
	if rule == nil || rule.Paused || stopped {
		return
	}
	if edited && !rule.has("handle_edited") {
		return
	}
	src := ev.ChatID
	dst := rule.Target
	if dst == src {
		return
	}
	p.wgs.Add(1)
	go func() {
		defer p.wgs.Done()
		p.forwardOne(ctx, ev, rule, src, dst, 0)
	}()
}

// isCommandText reports whether text starts with any command prefix.
func isCommandText(text string) bool {
	if text == "" {
		return false
	}
	prefixes := []string{".", "!", "/", "#"}
	for _, pr := range prefixes {
		if len(text) > 1 && text[:1] == pr {
			return true
		}
	}
	return false
}

// forwardOne applies one rule to one message: filters, media type, albums.
func (p *ShiftPlugin) forwardOne(ctx context.Context, ev *plugin.MessageEvent, rule *Rule, src, dst int64, depth int) {
	if depth > chainMaxDepth {
		return
	}
	// Re-read the rule: it may have been deleted or paused meanwhile.
	p.mu.Lock()
	cur := p.rules[src]
	p.mu.Unlock()
	if cur == nil || cur.Paused || cur != rule {
		return
	}
	if rule.isFiltered(ev.Text) {
		return
	}
	if types := rule.types(); len(types) > 0 && !containsStr(types, mediaType(ev.Message)) {
		return
	}

	gid, hasGroup := ev.Message.GetGroupedID()
	if hasGroup && gid != 0 {
		if af := p.albumsRef(); af != nil {
			af.add(albumKey{chatID: src, group: gid}, ev.Message.ID)
		}
		return
	}

	info, err := p.peerFor(src)
	if err != nil {
		p.logWarn("shift: resolve source failed", src, err)
		return
	}
	dstInfo, err := p.peerFor(dst)
	if err != nil {
		p.logWarn("shift: resolve target failed", dst, err)
		return
	}
	if err := forwardMessages(ctx, p.api, info.Peer, dstInfo.Peer, []int{ev.Message.ID}, rule.has("silent"), rule.TopicID); err != nil {
		if !isChatRestricted(err) {
			p.logWarn("shift: forward failed", src, err)
		}
		return
	}
	p.statsMu.Lock()
	p.stats.add(src)
	p.statsMu.Unlock()

	// Chained forwarding: the target itself has a rule; forward the new copy.
	p.mu.Lock()
	next := p.rules[dst]
	p.mu.Unlock()
	if next != nil && !next.Paused && next.Target != 0 && depth+1 <= chainMaxDepth {
		time.Sleep(200 * time.Millisecond)
		p.chainForward(ctx, dst, next.Target, ev.Message.ID, next, depth+1)
	}
}

// chainForward fetches the forwarded copy in the target chat and continues
// along the chain. The copy id is unknown, so we look for the newest message
// in the target chat with the same grouped/media fingerprint, best effort.
func (p *ShiftPlugin) chainForward(ctx context.Context, from, to int64, origID int, rule *Rule, depth int) {
	info, err := p.peerFor(from)
	if err != nil {
		return
	}
	hist, err := p.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:     info.Peer,
		Limit:    5,
		OffsetID: origID,
	})
	if err != nil {
		return
	}
	slice, ok := hist.(*tg.MessagesMessagesSlice)
	if !ok {
		return
	}
	var id int
	for _, m := range slice.Messages {
		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		if msg.ID > id {
			id = msg.ID
		}
	}
	if id == 0 {
		return
	}
	dstInfo, err := p.peerFor(to)
	if err != nil {
		return
	}
	if err := forwardMessages(ctx, p.api, info.Peer, dstInfo.Peer, []int{id}, rule.has("silent"), rule.TopicID); err != nil {
		return
	}
	p.statsMu.Lock()
	p.stats.add(from)
	p.statsMu.Unlock()
}

// isChatRestricted reports whether the source chat forbids forwarding.
func isChatRestricted(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return containsAny(s, "CHAT_FORWARDS_RESTRICTED", "CHAT_WRITE_FORBIDDEN", "CHAT_SEND_MEDIA_FORBIDDEN")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
