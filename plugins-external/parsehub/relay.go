package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// runLinks submits every link to the bot and reports failures per link,
// then deletes the command message when the panel option allows it.
func (p *ParseHubPlugin) runLinks(ctx *plugin.CommandContext, links []string) {
	api := ctx.API
	if api == nil {
		p.notify(ctx, "❌ "+ctx.Tlocal("客户端未就绪，请稍后重试", "Client not ready, try again later"))
		return
	}

	// Best-effort prep: unblock and mute the bot, /start it once per process.
	p.ensureBotReady(ctx, api)

	baseline, err := p.baselineID(ctx, api)
	if err != nil {
		p.notify(ctx, "❌ "+ctx.Tlocal("读取 @ParseHubot 会话失败: ", "Cannot read the @ParseHubot chat: ")+plugin.Escape(err.Error()))
		return
	}

	for i, link := range links {
		status := "🔄 " + ctx.Tlocal("已提交至 @ParseHubot，正在解析…", "Submitted to @ParseHubot, parsing…")
		if len(links) > 1 {
			status += fmt.Sprintf(" (%d/%d)", i+1, len(links))
		}
		_ = ctx.Edit(status + "\n" + plugin.Code(truncateStr(link, 80)))

		outcome := p.relayParseResult(ctx, api, link, baseline)
		baseline = max(baseline, outcome.lastID)
		if !outcome.forwarded {
			detail := ""
			if outcome.err != nil && outcome.err.Error() != "" && outcome.err.Error() != "undefined" {
				detail = "\n" + ctx.Tlocal("错误信息：", "Error: ") + plugin.Code(truncateStr(outcome.err.Error(), 120))
			}
			p.notify(ctx, fmt.Sprintf("⚠️ %s（%s）\n%s%s",
				ctx.Tlocal("未能获取结果", "Could not get a result"),
				describeReason(ctx, outcome.reason),
				plugin.Code(link), detail)+
				"\n💡 "+ctx.Tlocal("可直接私聊 @ParseHubot", "You can also DM @ParseHubot directly"))
		}
		if i < len(links)-1 {
			select {
			case <-ctx.Context().Done():
				return
			case <-timeAfter(linkGap):
			}
		}
	}

	if p.deleteCmd() {
		if err := ctx.Delete(); err != nil && p.logger != nil {
			p.logger.Warn("parsehub: delete command message", "error", err)
		}
	}
}

// notify replies to the command message; falls back to editing it.
func (p *ParseHubPlugin) notify(ctx *plugin.CommandContext, text string) {
	if err := ctx.Reply(text); err != nil && p.logger != nil {
		p.logger.Warn("parsehub: notify failed", "error", err)
	}
}

func (p *ParseHubPlugin) deleteCmd() bool {
	if p.set == nil {
		return true
	}
	return p.set.Bool("delete_cmd")
}

func (p *ParseHubPlugin) muted() bool {
	if p.set == nil {
		return true
	}
	return p.set.Bool("muted")
}

// ensureBotReady unblocks the bot, optionally mutes the chat and starts the
// bot once per plugin process (restart, not per command).
func (p *ParseHubPlugin) ensureBotReady(ctx *plugin.CommandContext, api *tg.Client) {
	peer, err := p.resolveBot(ctx, api)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("parsehub: resolve bot", "error", err)
		}
		return
	}
	_, _ = api.ContactsUnblock(ctx.Context(), &tg.ContactsUnblockRequest{ID: peer})
	if p.muted() {
		s := tg.InputPeerNotifySettings{}
		s.SetSilent(true)
		s.SetMuteUntil(2147483647)
		_, _ = api.AccountUpdateNotifySettings(ctx.Context(), &tg.AccountUpdateNotifySettingsRequest{
			Peer:     &tg.InputNotifyPeer{Peer: peer},
			Settings: s,
		})
	}

	p.mu.Lock()
	alreadyStarted := p.rel != nil && p.rel.started
	p.mu.Unlock()
	if alreadyStarted {
		return
	}

	p.mu.Lock()
	if p.rel == nil {
		p.rel = &relay{}
	}
	fresh := p.rel.ignoredUpTo == 0 // never saw the bot chat before
	p.mu.Unlock()

	// A chat with history is already started.
	if id, err := p.latestBotMessageID(ctx, api); err != nil || id > 0 {
		if err != nil && p.logger != nil {
			p.logger.Warn("parsehub: history probe", "error", err)
		}
		if id > 0 {
			p.markStarted()
			return
		}
	}

	started := func() {
		// A fresh chat replies to /start with a welcome message; best-effort:
		// wait a moment so the baseline probe (right before submitting) covers it.
		if fresh {
			p.waitForWelcome(ctx, api, 6*time.Second)
		}
		p.markStarted()
	}

	botUser := botInputUser(peer)
	if botUser == nil {
		// contacts.resolveUsername returned something unexpected; a plain
		// /start text works just as well.
		_, _ = api.MessagesSendMessage(ctx.Context(), &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  "/start",
			RandomID: randomID(),
		})
		started()
		return
	}
	if _, err := api.MessagesStartBot(ctx.Context(), &tg.MessagesStartBotRequest{
		Bot:      botUser,
		Peer:     peer,
		RandomID: randomID(),
	}); err != nil {
		if p.logger != nil {
			p.logger.Warn("parsehub: startBot failed, falling back to /start", "error", err)
		}
		_, _ = api.MessagesSendMessage(ctx.Context(), &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  "/start",
			RandomID: randomID(),
		})
	}
	started()
}

// waitForWelcome gives a freshly /started bot chat a moment to post its
// welcome message so the next baseline probe covers it. Best-effort: timing
// out simply means the baseline stays lower; the relay tolerates that.
func (p *ParseHubPlugin) waitForWelcome(ctx *plugin.CommandContext, api *tg.Client, wait time.Duration) {
	deadline := timeNow().Add(wait)
	for {
		select {
		case <-ctx.Context().Done():
			return
		case <-timeAfter(time.Second):
		}
		if !timeNow().Before(deadline) {
			return
		}
		if id, err := p.latestBotMessageID(ctx, api); err == nil && id > 0 {
			return
		}
	}
}

func (p *ParseHubPlugin) markStarted() {
	p.mu.Lock()
	if p.rel == nil {
		p.rel = &relay{}
	}
	p.rel.started = true
	p.mu.Unlock()
}

// resolveBot resolves @ParseHubot to an input peer and caches it. It tries
// the host resolver first (cached), then contacts.resolveUsername.
func (p *ParseHubPlugin) resolveBot(ctx *plugin.CommandContext, api *tg.Client) (tg.InputPeerClass, error) {
	p.mu.Lock()
	cached := (*relay)(nil)
	if p.rel != nil {
		cached = p.rel
	}
	var botPeer tg.InputPeerClass
	if cached != nil {
		botPeer = cached.botPeer
	}
	p.mu.Unlock()
	if botPeer != nil {
		return botPeer, nil
	}
	var peer tg.InputPeerClass
	if ctx.PeerResolver != nil {
		if v, err := ctx.PeerResolver.ResolveUsername(ctx.Context(), botUsername); err == nil && v != nil {
			peer = v
		}
	}
	if peer == nil {
		res, err := api.ContactsResolveUsername(ctx.Context(), &tg.ContactsResolveUsernameRequest{
			Username: botUsername,
		})
		if err != nil {
			return nil, err
		}
		pc, ok := res.Peer.(tg.PeerClass)
		if !ok || pc == nil {
			return nil, fmt.Errorf("resolve %s: unexpected peer type %T", botUsername, res.Peer)
		}
		peer, err = peerToInput(res, pc)
		if err != nil {
			return nil, err
		}
	}
	p.mu.Lock()
	if p.rel == nil {
		p.rel = &relay{}
	}
	p.rel.botPeer = peer
	p.mu.Unlock()
	return peer, nil
}

// peerToInput converts a resolved peer into an InputPeer using the users and
// chats returned by contacts.resolveUsername (needed for access hashes).
func peerToInput(res *tg.ContactsResolvedPeer, peer tg.PeerClass) (tg.InputPeerClass, error) {
	switch v := peer.(type) {
	case *tg.PeerUser:
		for _, u := range res.Users {
			if user, ok := u.(*tg.User); ok && user.ID == v.UserID {
				return user.AsInputPeer(), nil
			}
		}
		return nil, fmt.Errorf("resolve %s: user %d not in response", botUsername, v.UserID)
	case *tg.PeerChannel:
		for _, c := range res.Chats {
			if ch, ok := c.(*tg.Channel); ok && ch.ID == v.ChannelID {
				return ch.AsInputPeer(), nil
			}
		}
		return nil, fmt.Errorf("resolve %s: channel %d not in response", botUsername, v.ChannelID)
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: v.ChatID}, nil
	}
	return nil, fmt.Errorf("resolve %s: unsupported peer %T", botUsername, peer)
}

func botInputUser(peer tg.InputPeerClass) tg.InputUserClass {
	if u, ok := peer.(*tg.InputPeerUser); ok {
		return &tg.InputUser{UserID: u.UserID, AccessHash: u.AccessHash}
	}
	return nil
}

// latestBotMessageID returns the newest message id in the bot chat, 0 when
// the chat is empty.
func (p *ParseHubPlugin) latestBotMessageID(ctx *plugin.CommandContext, api *tg.Client) (int, error) {
	msgs, err := p.botHistory(ctx, api, 1)
	if err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}
	return msgs[0].ID, nil
}

// botHistory fetches the most recent fetchLimit messages of the bot chat,
// newest first.
func (p *ParseHubPlugin) botHistory(ctx *plugin.CommandContext, api *tg.Client, limit int) ([]*tg.Message, error) {
	peer, err := p.resolveBot(ctx, api)
	if err != nil {
		return nil, err
	}
	res, err := api.MessagesGetHistory(ctx.Context(), &tg.MessagesGetHistoryRequest{
		Peer:  peer,
		Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	var out []*tg.Message
	switch v := res.(type) {
	case *tg.MessagesMessages:
		out = toMessages(v.Messages)
	case *tg.MessagesMessagesSlice:
		out = toMessages(v.Messages)
	case *tg.MessagesChannelMessages:
		out = toMessages(v.Messages)
	case *tg.MessagesMessagesNotModified:
		return nil, nil
	}
	return out, nil
}

func toMessages(in []tg.MessageClass) []*tg.Message {
	out := make([]*tg.Message, 0, len(in))
	for _, m := range in {
		if msg, ok := m.(*tg.Message); ok && msg != nil {
			out = append(out, msg)
		}
	}
	return out
}

// baselineID is the id the relay treats as "already seen": the newest bot
// message right before submitting, so only fresh replies count as results.
// The persisted value wins when it is higher (e.g. history was cleared).
func (p *ParseHubPlugin) baselineID(ctx *plugin.CommandContext, api *tg.Client) (int, error) {
	id, err := p.latestBotMessageID(ctx, api)
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	if p.rel == nil {
		p.rel = &relay{}
	}
	if p.rel.ignoredUpTo > id {
		id = p.rel.ignoredUpTo
	}
	p.rel.started = true
	p.rel.ignoredUpTo = id
	p.mu.Unlock()
	if err := p.saveState(); err != nil && p.logger != nil {
		p.logger.Warn("parsehub: save state", "error", err)
	}
	return id, nil
}

// saveState persists the current baseline.
func (p *ParseHubPlugin) saveState() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveStateLocked()
}

// relayParseResult sends one link to the bot, polls the bot chat until the
// progress messages settle into final results, then forwards them.
func (p *ParseHubPlugin) relayParseResult(ctx *plugin.CommandContext, api *tg.Client, link string, baseline int) relayOutcome {
	cctx := ctx.Context()
	botPeer, err := p.resolveBot(ctx, api)
	if err != nil {
		return relayOutcome{lastID: baseline, reason: reasonSendFailed, err: err}
	}
	if _, err := api.MessagesSendMessage(cctx, &tg.MessagesSendMessageRequest{
		Peer:     botPeer,
		Message:  link,
		RandomID: randomID(),
	}); err != nil {
		return relayOutcome{lastID: baseline, reason: reasonSendFailed, err: err}
	}

	progressIDs := map[int]bool{}
	finals := map[int]*tg.Message{}

	deadline := timeNow().Add(maxWait)
	lastID := baseline
	var lastFinal, lastProgress time.Time
	lastProgress = timeNow()

	for {
		select {
		case <-cctx.Done():
			return relayOutcome{lastID: lastID, reason: reasonTimeout}
		case <-timeAfter(pollInterval):
		}
		if !timeNow().Before(deadline) {
			break
		}

		msgs, err := p.botHistory(ctx, api, fetchLimit)
		if err != nil {
			return relayOutcome{lastID: lastID, reason: reasonFetchFailed, err: err}
		}

		// Oldest first so earlier progress messages register before finals.
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			switch classifyMessage(m, baseline, maxKey(finals)) {
			case actionProgress:
				progressIDs[m.ID] = true
				lastID = max(lastID, m.ID)
				lastProgress = timeNow()
				if len(finals) == 0 {
					// Only extend while no final result exists yet.
					if d := timeNow().Add(progressExtend); d.After(deadline) {
						deadline = d
					}
					if cap := timeNow().Add(hardCap); deadline.After(cap) {
						deadline = cap
					}
				}
			case actionNoise:
				// Empty or service noise: advance the high-water mark only.
				lastID = max(lastID, m.ID)
			case actionFinal:
				progressIDs, lastID, lastFinal = record(finals, progressIDs, m, lastID, lastFinal)
			}
		}

		// Results are in and the bot went quiet: done.
		if len(finals) > 0 && len(progressIDs) == 0 && timeNow().Sub(lastFinal) >= resultIdle {
			break
		}
		// Progress stuck without turning final: forward what we have.
		if len(finals) > 0 && len(progressIDs) > 0 &&
			timeNow().Sub(lastProgress) >= resultIdle*3 && timeNow().Sub(lastFinal) >= resultIdle {
			break
		}
	}

	if len(finals) == 0 {
		return relayOutcome{lastID: lastID, reason: reasonTimeout}
	}

	ids := make([]int, 0, len(finals))
	for id := range finals {
		ids = append(ids, id)
	}
	sortIDs(ids)

	from, err := p.resolveBot(ctx, api)
	if err != nil {
		return relayOutcome{lastID: lastID, reason: reasonFetchFailed, err: err}
	}
	to, err := ctx.ResolvePeer()
	if err != nil {
		return relayOutcome{lastID: lastID, reason: reasonFetchFailed, err: err}
	}

	var forwarded bool
	var fallbacks []string
	for start := 0; start < len(ids); start += forwardChunk {
		end := min(start+forwardChunk, len(ids))
		chunk := ids[start:end]
		if _, err := api.MessagesForwardMessages(cctx, &tg.MessagesForwardMessagesRequest{
			FromPeer:   from,
			ToPeer:     to,
			ID:         chunk,
			RandomID:   randomIDs(len(chunk)),
			DropAuthor: true,
		}); err != nil {
			if p.logger != nil {
				p.logger.Warn("parsehub: forward chunk failed", "error", err)
			}
			// Text fallback so the user still sees what the bot produced.
			for _, id := range chunk {
				if m := finals[id]; m != nil && strings.TrimSpace(m.Message) != "" {
					fallbacks = append(fallbacks, strings.TrimSpace(m.Message))
				}
			}
			continue
		}
		forwarded = true
	}
	if !forwarded && len(fallbacks) > 0 {
		text := "📨 " + ctx.Tlocal("@ParseHubot 返回内容：", "Content returned by @ParseHubot:") + "\n\n" +
			strings.Join(fallbacks, "\n\n")
		if len([]rune(text)) > 3900 {
			text = string([]rune(text)[:3900]) + "…"
		}
		req := &tg.MessagesSendMessageRequest{
			Peer:     to,
			Message:  text,
			RandomID: randomID(),
		}
		if ctx.Message != nil && ctx.Message.Message != nil {
			req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: ctx.Message.Message.ID})
		}
		if _, err := api.MessagesSendMessage(cctx, req); err == nil {
			forwarded = true
		}
	} else if !forwarded {
		return relayOutcome{lastID: lastID, reason: reasonForwardFailed}
	}
	return relayOutcome{lastID: lastID, forwarded: forwarded}
}

// truncateStr clips s to n runes with an ellipsis.
func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// record captures a final bot message, refreshing it on edits.
func record(finals map[int]*tg.Message, progressIDs map[int]bool, m *tg.Message, lastID int, lastFinal time.Time) (map[int]bool, int, time.Time) {
	delete(progressIDs, m.ID)
	finals[m.ID] = m
	return progressIDs, max(lastID, m.ID), timeNow()
}

// describeReason maps a relay failure reason to user-facing text.
func describeReason(ctx *plugin.CommandContext, r relayReason) string {
	switch r {
	case reasonTimeout:
		return ctx.Tlocal("等待超时", "timed out")
	case reasonFetchFailed:
		return ctx.Tlocal("获取机器人消息失败", "failed to read the bot chat")
	case reasonSendFailed:
		return ctx.Tlocal("向机器人发送链接失败", "failed to send the link to the bot")
	case reasonForwardFailed:
		return ctx.Tlocal("转发结果失败", "failed to forward the result")
	default:
		return ctx.Tlocal("客户端未就绪", "client not ready")
	}
}

func maxKey(m map[int]*tg.Message) int {
	k := 0
	for key := range m {
		if key > k {
			k = key
		}
	}
	return k
}

func sortIDs(ids []int) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}
