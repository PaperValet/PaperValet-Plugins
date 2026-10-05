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

// isCommand reports whether text starts with a registered command prefix.
func (p *LotteryPlugin) isCommand(text string) bool {
	if p.host == nil {
		return false
	}
	t := strings.TrimSpace(text)
	for _, pre := range p.host.Prefixes() {
		if pre != "" && strings.HasPrefix(t, pre) {
			return true
		}
	}
	return false
}

// onMessage is the join listener: users send the keyword to join the chat's
// active lottery. Runs on the update path; slow work goes to goroutines.
func (p *LotteryPlugin) onMessage(ctx context.Context, ev *plugin.MessageEvent, edited bool) {
	if edited || ev == nil || ev.Message == nil || ev.IsOut || ev.Text == "" {
		return
	}
	if ev.ChatID > 0 { // private chats cannot host a group lottery
		return
	}
	if p.isCommand(ev.Text) {
		return
	}

	p.mu.Lock()
	if p.stopListen == nil { // not started
		p.mu.Unlock()
		return
	}
	l := p.activeLocked(ev.ChatID)
	if l == nil || strings.TrimSpace(ev.Text) != l.Keyword {
		p.mu.Unlock()
		return
	}
	uid := ev.UserID
	live := p.ctx
	chatID := l.ChatID
	title, keyword := l.Title, l.Keyword
	maxUsers := l.MaxUsers
	creator := l.CreatorID
	// Copy the participant info the join path needs.
	already := false
	for _, part := range l.Participants {
		if part.UserID == uid {
			already = true
			break
		}
	}
	var joinee participant
	if !already {
		joinee = participant{UserID: uid}
	}
	p.mu.Unlock()

	if already {
		p.notifyDuplicate(live, ev, creator)
		return
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.doJoin(live, ev, joinee, title, keyword, maxUsers, chatID, creator)
	}()
}

// notifyDuplicate tells the sender they already joined, then cleans up both
// messages after the delete delay.
func (p *LotteryPlugin) notifyDuplicate(live context.Context, ev *plugin.MessageEvent, langKey int64) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		text := "⚠️ " + p.tlFor(langKey, "您已参加过本次抽奖", "You already joined this lottery")
		msgID, err := p.host.Send(live, ev.ChatID, text, ev.Message.ID)
		if err != nil {
			return
		}
		p.deleteLater(live, ev.ChatID, msgID, ev.Message.ID)
	}()
}

// doJoin validates the participant, records the join, replies with the
// progress card, and triggers the auto-draw when the lottery is full.
func (p *LotteryPlugin) doJoin(live context.Context, ev *plugin.MessageEvent, joinee participant, title, keyword string, maxUsers int, chatID, langKey int64) {
	user, err := p.fetchUser(live, chatID, ev.Message, joinee.UserID)
	if err != nil && user == nil {
		p.log.Debug("lottery: cannot resolve participant", "user", joinee.UserID, "error", err)
	}
	if user != nil {
		joinee.Username = user.Username
		joinee.FirstName = user.FirstName
		joinee.LastName = user.LastName
		if user.Bot {
			p.deleteMessages(live, chatID, ev.Message.ID) // bots never join (source default)
			return
		}
	}

	p.mu.Lock()
	l := p.activeLocked(chatID)
	if l == nil || l.Status != "active" {
		p.mu.Unlock()
		return
	}
	count, jerr := p.joinLocked(l, joinee, time.Now())
	full := count >= l.MaxUsers
	id := l.ID
	saveErr := p.saveLocked()
	p.mu.Unlock()
	if saveErr != nil {
		p.log.Warn("lottery: save after join failed", "error", saveErr)
	}
	if jerr == errDuplicate {
		p.notifyDuplicate(live, ev, langKey)
		return
	}
	if jerr == errFull {
		return
	}

	text := "✅ **" + p.tlFor(langKey, "参与成功", "Joined") + "**\n\n" +
		"🎯 " + p.tlFor(langKey, "活动", "Lottery") + "  " + plugin.Escape(title) + "\n" +
		"🔑 " + p.tlFor(langKey, "关键词", "Keyword") + "  " + plugin.Code(keyword) + "\n" +
		"📊 " + p.tlFor(langKey, "进度", "Progress") + "  " + fmt.Sprintf("**%d/%d**", count, maxUsers) + "\n\n" +
		"🍀 " + p.tlFor(langKey, "祝你好运!", "Good luck!")
	msgID, err := p.host.Send(live, chatID, text, ev.Message.ID)
	if err != nil {
		p.log.Warn("lottery: join reply failed", "error", err)
		return
	}
	p.deleteLater(live, chatID, msgID, ev.Message.ID)

	if full {
		p.drawAsync(id, "auto")
	}
}

// deleteLater removes the pair of join messages after the delete delay.
func (p *LotteryPlugin) deleteLater(ctx context.Context, chatID int64, ids ...int) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(deleteDelay):
		}
		p.deleteMessages(ctx, chatID, ids...)
	}()
}

// deleteMessages removes messages in chatID with the channel-aware helper.
func (p *LotteryPlugin) deleteMessages(ctx context.Context, chatID int64, ids ...int) {
	if len(ids) == 0 || p.host == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	peer, err := p.host.PeerResolver().ResolveFromChatID(cctx, chatID)
	if err != nil {
		p.log.Debug("lottery: resolve chat for delete failed", "chat", chatID, "error", err)
		return
	}
	if err := plugin.DeleteMessages(cctx, p.host.API(), peer, ids...); err != nil {
		p.log.Debug("lottery: delete messages failed", "chat", chatID, "error", err)
	}
}

// fetchUser resolves the sender of msg (or any user in the chat) to a
// *tg.User so names and botness are known. Nil when unresolvable.
func (p *LotteryPlugin) fetchUser(ctx context.Context, chatID int64, msg *tg.Message, userID int64) (*tg.User, error) {
	if userID == 0 || p.host == nil || p.host.API() == nil {
		return nil, errors.New("no user id")
	}
	cctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	resolver := p.host.PeerResolver()
	if msg != nil && msg.ID > 0 {
		peer, err := resolver.ResolveFromChatID(cctx, chatID)
		if err == nil {
			// users.getUsers with the id works when the user is in
			// dialogs; the message-scoped resolver handles channels.
			if ip, err := resolver.ResolveUserFromMessage(cctx, peer, msg.ID, userID); err == nil {
				if pu, ok := ip.(*tg.InputPeerUser); ok && pu.AccessHash != 0 {
					users, err := p.host.API().UsersGetUsers(cctx, []tg.InputUserClass{&tg.InputUser{UserID: pu.UserID, AccessHash: pu.AccessHash}})
					if err == nil && len(users) > 0 {
						if u, ok := users[0].(*tg.User); ok {
							return u, nil
						}
					}
				}
			}
		}
	}
	// Plain id: works for contacts and chats with the bot account.
	users, err := p.host.API().UsersGetUsers(cctx, []tg.InputUserClass{&tg.InputUser{UserID: userID}})
	if err == nil && len(users) > 0 {
		if u, ok := users[0].(*tg.User); ok {
			return u, nil
		}
	}
	return nil, err
}

// isAdmin reports whether userID is creator or admin of chatID's group.
func (p *LotteryPlugin) isAdmin(ctx context.Context, chatID, userID int64) bool {
	if userID == 0 {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	peer, err := p.host.PeerResolver().ResolveFromChatID(cctx, chatID)
	if err != nil {
		return false
	}
	switch v := peer.(type) {
	case *tg.InputPeerChannel:
		res, err := p.host.API().ChannelsGetParticipant(cctx, &tg.ChannelsGetParticipantRequest{
			Channel:     &tg.InputChannel{ChannelID: v.ChannelID, AccessHash: v.AccessHash},
			Participant: &tg.InputPeerUser{UserID: userID},
		})
		if err != nil {
			return false
		}
		switch res.Participant.(type) {
		case *tg.ChannelParticipantAdmin, *tg.ChannelParticipantCreator:
			return true
		}
		return false
	case *tg.InputPeerChat:
		full, err := p.host.API().MessagesGetFullChat(cctx, v.ChatID)
		if err != nil {
			return false
		}
		cf, ok := full.FullChat.(*tg.ChatFull)
		if !ok {
			return false
		}
		cps, ok := cf.Participants.(*tg.ChatParticipants)
		if !ok {
			return false
		}
		for _, cp := range cps.Participants {
			switch v := cp.(type) {
			case *tg.ChatParticipantAdmin:
				if v.UserID == userID {
					return true
				}
			case *tg.ChatParticipantCreator:
				if v.UserID == userID {
					return true
				}
			}
		}
		return false
	}
	return false
}

// drawAsync runs the draw in the background (join path or auto timer).
func (p *LotteryPlugin) drawAsync(id int64, via string) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if err := p.performDraw(p.lifetime(), id, via); err != nil {
			p.log.Warn("lottery: draw failed", "id", id, "via", via, "error", err)
		}
	}()
}

// performDraw executes the whole draw: shuffle participants, assign prizes,
// DM the winners, post the result card and complete the lottery.
func (p *LotteryPlugin) performDraw(ctx context.Context, id int64, via string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	p.mu.Lock()
	l := p.findLocked(id)
	if l == nil || l.Status != "active" {
		p.mu.Unlock()
		return errors.New("lottery not active")
	}
	chatID, title := l.ChatID, l.Title
	msgID := l.MessageID
	warehouse := l.Warehouse
	winnerCnt := l.WinnerCnt
	// Delete the announcement before announcing results (like the source).
	p.mu.Unlock()
	if msgID > 0 {
		p.deleteMessages(ctx, chatID, msgID)
	}

	p.mu.Lock()
	l = p.findLocked(id)
	if l == nil || l.Status != "active" { // raced with another draw
		p.mu.Unlock()
		return nil
	}
	parts := append([]participant(nil), l.Participants...)
	creator := l.CreatorID
	langKey := creator // group messages follow the creator's language
	if len(parts) == 0 {
		l.Status = "completed"
		p.saveLocked()
		p.mu.Unlock()
		text := "🎊 **" + p.tlFor(langKey, "开奖结果", "Draw Result") + "**\n\n" +
			"🏆 " + p.tlFor(langKey, "活动", "Lottery") + "  " + plugin.Escape(title) + "\n\n" +
			"😅 " + p.tlFor(langKey, "很遗憾，没有人参与抽奖", "Sadly, nobody joined this lottery")
		if _, err := p.host.Send(ctx, chatID, text, 0); err != nil {
			p.log.Warn("lottery: empty result send failed", "error", err)
		}
		return nil
	}
	wins := drawWinners(parts, winnerCnt)
	now := time.Now()
	for i := range wins {
		prize := p.tlFor(langKey, "恭喜中奖！", "Congratulations!")
		if name, ok := p.nextPrizeLocked(warehouse); ok {
			prize = name
		}
		l.Winners = append(l.Winners, winner{
			UserID:     wins[i].UserID,
			Username:   wins[i].Username,
			FirstName:  wins[i].FirstName,
			LastName:   wins[i].LastName,
			Prize:      prize,
			Status:     "pending",
			AssignedAt: now.Unix(),
			ExpiresAt:  now.Add(claimTimeout).Unix(),
		})
	}
	l.Status = "completed"
	l.AutoDrawAt = 0
	p.saveLocked()
	winners := append([]winner(nil), l.Winners...)
	p.mu.Unlock()

	// DM every winner their prize (auto-send mode, like the source).
	sent := 0
	for _, w := range winners {
		if err := p.dmWinner(ctx, w, title, creator); err != nil {
			p.log.Warn("lottery: winner DM failed", "user", w.UserID, "error", err)
			continue
		}
		sent++
		select {
		case <-ctx.Done():
		case <-time.After(notifyInterval):
		}
	}

	var b strings.Builder
	b.WriteString("🎊 **" + p.tlFor(langKey, "开奖结果", "Draw Result") + "**\n\n")
	b.WriteString("🏆 " + p.tlFor(langKey, "活动", "Lottery") + "  " + plugin.Escape(title) + "\n\n")
	for _, w := range winners {
		name := winnerName(w)
		b.WriteString(statusIcon(w.Status) + " " + plugin.Mention(plugin.Escape(name), w.UserID) + "\n")
	}
	b.WriteString("\n🎉 " + p.tlFor(langKey, "恭喜以上中奖用户！", "Congratulations to the winners!") + "\n")
	if sent > 0 {
		b.WriteString("📞 " + fmt.Sprintf(p.tlFor(langKey, "奖品已私信发送（%d/%d 成功）", "Prizes sent via DM (%d/%d delivered)"), sent, len(winners)) + "\n")
	}
	b.WriteString("⏰ " + fmt.Sprintf(p.tlFor(langKey, "领奖时效：%d 小时", "Claim window: %d hours"), int(claimTimeout.Hours())) + "\n")
	b.WriteString("🙏 " + p.tlFor(langKey, "感谢所有用户的参与！", "Thanks to everyone who joined!"))
	if _, err := p.host.Send(ctx, chatID, b.String(), 0); err != nil {
		p.log.Warn("lottery: result send failed", "error", err)
	}
	return nil
}

// dmWinner sends the prize notification to the winner's private chat.
func (p *LotteryPlugin) dmWinner(ctx context.Context, w winner, title string, creator int64) error {
	if p.host == nil || p.host.API() == nil {
		return errors.New("no api client")
	}
	cctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	peer, err := p.host.PeerResolver().ResolveFromChatID(cctx, w.UserID)
	if err != nil {
		// Not in dialogs: try the cross-group fallback via the lottery chat.
		return fmt.Errorf("resolve winner %d: %w", w.UserID, err)
	}
	text := "🎉 **" + p.tlFor(w.UserID, "恭喜中奖！", "You won!") + "**\n\n" +
		"🏆 " + p.tlFor(w.UserID, "活动", "Lottery") + "  " + plugin.Escape(title) + "\n" +
		"🎁 " + p.tlFor(w.UserID, "奖品", "Prize") + "  " + plugin.Escape(w.Prize) + "\n\n" +
		"🎊 " + p.tlFor(w.UserID, "感谢参与，祝好运！", "Thanks for joining, good luck!")
	plain, ents := plugin.ParseMarkdown(text, func(int64) (tg.InputUserClass, error) { return nil, nil })
	req := &tg.MessagesSendMessageRequest{Peer: peer, Message: plain, RandomID: randID()}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	if _, err := p.host.API().MessagesSendMessage(cctx, req); err != nil {
		if d, ok := tgerr.AsFloodWait(err); ok {
			return fmt.Errorf("flood wait %s", d)
		}
		return err
	}
	// Mark sent.
	p.mu.Lock()
	if l := p.findLockedByWinner(w.UserID); l != nil {
		for i := range l.Winners {
			if l.Winners[i].UserID == w.UserID {
				l.Winners[i].Status = "sent"
			}
		}
		p.saveLocked()
	}
	p.mu.Unlock()
	return nil
}

// findLockedByWinner finds the lottery holding a winner (DM marking path).
func (p *LotteryPlugin) findLockedByWinner(userID int64) *lottery {
	for _, l := range p.store.data.Lotteries {
		for _, w := range l.Winners {
			if w.UserID == userID {
				return l
			}
		}
	}
	return nil
}

// winnerName renders a participant's display name.
func winnerName(w winner) string {
	first := strings.TrimSpace(w.FirstName + " " + w.LastName)
	switch {
	case first != "" && w.Username != "":
		return first + " @" + w.Username
	case first != "":
		return first
	case w.Username != "":
		return "@" + w.Username
	default:
		return fmt.Sprintf("用户 %d", w.UserID)
	}
}

func statusIcon(status string) string {
	switch status {
	case "sent":
		return "✅"
	case "pending":
		return "⏳"
	case "expired":
		return "❌"
	}
	return "❓"
}

// tlFor picks a localized string for a user id (group chat messages use
// the lottery creator's language when known, else zh).
func (p *LotteryPlugin) tlFor(userID int64, zh, en string) string {
	if p.host != nil && p.host.Lang(userID) == "en-US" {
		return en
	}
	return zh
}
