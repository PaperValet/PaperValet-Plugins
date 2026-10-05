package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	pendingMaxAge  = 24 * time.Hour
	restartRetry   = 10 * time.Second // retry delay for restored deletions
	deleteTimeout  = 30 * time.Second
	historyLimit   = 100 // messages scanned when deleting responses
	maxResponses   = 3   // responses deleted alongside a -r rule
	historyTimeout = 30 * time.Second
)

// onMessage runs on the update path: it only classifies the message and
// schedules deletions in goroutines.
func (p *AutoDelPlugin) onMessage(_ context.Context, ev *plugin.MessageEvent, edited bool) {
	if edited || ev == nil || ev.Message == nil || ev.Text == "" {
		return
	}
	if !ev.IsOut && ev.ChatID != p.host.SelfID() {
		return // only own messages (or those in Saved Messages)
	}
	// TTL part never applies to commands; cmd part only applies to commands.
	prefix, rest := splitPrefix(ev.Text, p.host.Prefixes())
	if prefix == "" {
		p.onTTLMessage(ev)
		return
	}
	p.onCommand(prefix, rest, ev)
}

// onTTLMessage schedules a TTL deletion for a plain own message.
func (p *AutoDelPlugin) onTTLMessage(ev *plugin.MessageEvent) {
	p.mu.Lock()
	sec, ok := p.ttl[strconvI(ev.ChatID)]
	if !ok {
		sec, ok = p.ttl["0"]
	}
	if !ok || sec < minTTLSeconds {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.schedule(ev.ChatID, ev.Message.ID, time.Duration(sec)*time.Second, false)
}

// onCommand matches cmd rules for a prefixed own message.
func (p *AutoDelPlugin) onCommand(prefix, rest string, ev *plugin.MessageEvent) {
	if p.set == nil || !p.set.Bool("cmd_enabled") {
		return
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return
	}
	cmd := fields[0]
	if resolved := p.resolveAlias(cmd); resolved != "" {
		fields[0] = resolved
		cmd = resolved
	}
	var params []string
	if len(fields) > 1 {
		params = fields[1:]
	}
	p.mu.Lock()
	rule := matchRule(p.rules, strings.ToLower(cmd), params)
	p.mu.Unlock()
	if rule == nil {
		return
	}
	p.schedule(ev.ChatID, ev.Message.ID, time.Duration(rule.Delay)*time.Second, rule.DeleteResponse)
}

// resolveAlias maps a user alias to the command word it expands to
// (aliases hold full command lines like "exec ./x.sh"; only the first
// word counts here).
func (p *AutoDelPlugin) resolveAlias(cmd string) string {
	if p.mgr == nil {
		return ""
	}
	aliases := p.mgr.Commands().UserAliases()
	if aliases == nil {
		return ""
	}
	line, ok := aliases[cmd]
	if !ok {
		return ""
	}
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// schedule persists the deletion then deletes after d (finding response
// messages too when resp is set).
func (p *AutoDelPlugin) schedule(chatID int64, msgID int, d time.Duration, resp bool) {
	p.mu.Lock()
	if p.cancel == nil {
		p.mu.Unlock()
		return // stopped
	}
	for _, pd := range p.pending {
		if pd.CID == chatID && pd.MID == msgID {
			p.mu.Unlock()
			return // already scheduled
		}
	}
	p.pending = append(p.pending, PendingDel{CID: chatID, MID: msgID, At: time.Now().Add(d).Unix(), Resp: resp})
	err := p.savePendingLocked()
	ctx := p.ctx
	p.mu.Unlock()
	if err != nil && p.log != nil {
		p.log.Warn("autodel: persist pending failed", "error", err)
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.deleteNow(context.Background(), chatID, msgID, resp)
	}()
}

// deleteNow deletes msgID (and its responses when resp) and drops the
// pending record.
func (p *AutoDelPlugin) deleteNow(ctx context.Context, chatID int64, msgID int, resp bool) {
	api := p.host.API()
	resolver := p.host.PeerResolver()
	peer, err := resolver.ResolveFromChatID(ctx, chatID)
	if err != nil {
		p.removePending(chatID, msgID)
		p.logWarn("resolve chat failed", chatID, err)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	ids := []int{msgID}
	if resp {
		ids = append(ids, p.findResponses(dctx, api, peer, chatID, msgID)...)
	}
	err = plugin.DeleteMessages(dctx, api, peer, ids...)
	cancel()
	p.removePending(chatID, msgID)
	if err != nil {
		p.logWarn("delete failed", chatID, err)
	}
}

// findResponses locates up to maxResponses own messages right after the
// command: recent own messages from history (normal chats), or ids greater
// than the command's (Saved Messages).
func (p *AutoDelPlugin) findResponses(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, chatID int64, cmdID int) []int {
	hctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()
	res, err := api.MessagesGetHistory(hctx, &tg.MessagesGetHistoryRequest{
		Peer: peer, Limit: historyLimit,
	})
	if err != nil {
		return nil
	}
	mod, ok := res.AsModified()
	if !ok {
		return nil
	}
	saved := chatID == p.host.SelfID()
	selfID := p.host.SelfID()
	var out []int
	for _, m := range mod.GetMessages() {
		msg, ok := m.(*tg.Message)
		if !ok || msg.ID == cmdID {
			continue
		}
		if len(out) >= maxResponses {
			break
		}
		if saved {
			if msg.ID > cmdID {
				out = append(out, msg.ID)
			}
			continue
		}
		if plugin.SenderID(msg) == selfID {
			out = append(out, msg.ID)
		}
	}
	return out
}

func (p *AutoDelPlugin) removePending(chatID int64, msgID int) {
	p.mu.Lock()
	for i, pd := range p.pending {
		if pd.CID == chatID && pd.MID == msgID {
			p.pending = append(p.pending[:i], p.pending[i+1:]...)
			break
		}
	}
	if err := p.savePendingLocked(); err != nil && p.log != nil {
		p.log.Warn("autodel: save pending failed", "error", err)
	}
	p.mu.Unlock()
}

// restorePending re-schedules deletions that survived a restart: overdue
// ones (>24h dropped at load, older-but-recent fired after restartRetry).
func (p *AutoDelPlugin) restorePending() {
	p.mu.Lock()
	now := time.Now()
	p.pending = prunePending(p.pending, now)
	err := p.savePendingLocked()
	pending := append([]PendingDel(nil), p.pending...)
	ctx := p.ctx
	p.mu.Unlock()
	if err != nil && p.log != nil {
		p.log.Warn("autodel: save pending failed", "error", err)
	}
	for _, pd := range pending {
		d := time.Until(time.Unix(pd.At, 0))
		if d < 0 {
			d = restartRetry
		}
		p.wg.Add(1)
		go func(pd PendingDel, d time.Duration) {
			defer p.wg.Done()
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			p.deleteNow(context.Background(), pd.CID, pd.MID, pd.Resp)
		}(pd, d)
	}
}

// splitPrefix returns the matched prefix and the rest of the text.
func splitPrefix(text string, prefixes []string) (prefix, rest string) {
	for _, pre := range prefixes {
		if pre != "" && strings.HasPrefix(text, pre) {
			return pre, text[len(pre):]
		}
	}
	return "", text
}

func strconvI(i int64) string { return strconv.FormatInt(i, 10) }

func (p *AutoDelPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

func (p *AutoDelPlugin) logWarn(msg string, chatID int64, err error) {
	if p.log != nil {
		p.log.Warn("autodel: "+msg, "chat", chatID, "error", err)
	}
}
