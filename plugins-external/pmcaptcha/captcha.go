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

// telegramOfficialIDs are Telegram service accounts that must never be
// challenged (777000 sends login codes).
var telegramOfficialIDs = map[int64]bool{777000: true}

const (
	failBlock  = "block"
	failDelete = "delete"
	failReport = "report"

	passUnmute    = "unmute"
	passUnarchive = "unarchive"
	passWhitelist = "wl"
)

// ============================================================
// User resolution
// ============================================================

// resolveArg turns a command argument into a user id: digits directly,
// @name through contacts.resolveUsername.
func (p *PMPlugin) resolveArg(ctx context.Context, arg string) (int64, *tg.User, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return 0, nil, errNoTarget
	}
	if digitsOnly(arg) {
		id, err := parseInt64(arg)
		if err != nil || id <= 0 {
			return 0, nil, errNoTarget
		}
		return id, nil, nil
	}
	name := strings.TrimPrefix(arg, "@")
	if name == "" {
		return 0, nil, errNoTarget
	}
	res, err := p.host.API().ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: name})
	if err != nil {
		return 0, nil, err
	}
	for _, u := range res.Users {
		if user, ok := u.(*tg.User); ok && !user.Deleted {
			return user.ID, user, nil
		}
	}
	return 0, nil, errNoTarget
}

// userInfo fetches a user by id (access hash 0 works when the session has
// seen the peer). Returns nil when the user cannot be resolved.
func (p *PMPlugin) userInfo(ctx context.Context, id int64) *tg.User {
	if id <= 0 || p.host == nil || p.host.API() == nil {
		return nil
	}
	users, err := p.host.API().UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUser{UserID: id}})
	if err != nil || len(users) == 0 {
		return nil
	}
	user, _ := users[0].(*tg.User)
	return user
}

func nameOfUser(u *tg.User) string {
	name := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
	if name == "" {
		name = u.Username
	}
	if name == "" {
		name = fmt.Sprintf("%d", u.ID)
	}
	return name
}

// isBot reports (with a short cache) whether uid is a bot, so bots are
// never challenged, like the source.
func (p *PMPlugin) isBot(ctx context.Context, uid int64) bool {
	p.mu.Lock()
	if p.botFlag == nil {
		p.botFlag = map[int64]bool{}
	}
	flag, ok := p.botFlag[uid]
	p.mu.Unlock()
	if ok {
		return flag
	}
	u := p.userInfo(ctx, uid)
	flag = u != nil && u.Bot
	p.mu.Lock()
	p.botFlag[uid] = flag
	p.mu.Unlock()
	return flag
}

// userLink renders an owner-facing mention line.
func userLink(id int64, name string) string {
	return plugin.Mention(name, id) + " " + plugin.Code(id)
}

// peerOf resolves a user id to an input peer, falling back to a bare
// InputPeerUser (hash 0) when the session has never seen the user.
func (p *PMPlugin) peerOf(ctx context.Context, id int64) tg.InputPeerClass {
	if p.host != nil && p.host.PeerResolver() != nil {
		if peer, err := p.host.PeerResolver().ResolveFromChatID(ctx, id); err == nil {
			if _, ok := peer.(*tg.InputPeerUser); ok {
				return peer
			}
		}
	}
	if u := p.userInfo(ctx, id); u != nil && u.AccessHash != 0 {
		return &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}
	}
	return &tg.InputPeerUser{UserID: id}
}

// ============================================================
// Peer actions (all best-effort, like the source)
// ============================================================

func (p *PMPlugin) archiveChat(ctx context.Context, id int64, folder int) {
	_, err := p.host.API().FoldersEditPeerFolders(ctx, []tg.InputFolderPeer{
		{Peer: p.peerOf(ctx, id), FolderID: folder},
	})
	p.logAction("archive", id, err)
}

func (p *PMPlugin) muteChat(ctx context.Context, id int64, mute bool) {
	s := tg.InputPeerNotifySettings{}
	if mute {
		s.SetMuteUntil(2147483647)
		s.SetShowPreviews(false)
		s.SetSilent(true)
	} else {
		s.SetMuteUntil(0)
		s.SetShowPreviews(true)
		s.SetSilent(false)
	}
	_, err := p.host.API().AccountUpdateNotifySettings(ctx, &tg.AccountUpdateNotifySettingsRequest{
		Peer:     &tg.InputNotifyPeer{Peer: p.peerOf(ctx, id)},
		Settings: s,
	})
	p.logAction("mute", id, err)
}

func (p *PMPlugin) blockUser(ctx context.Context, id int64) {
	_, err := p.host.API().ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: p.peerOf(ctx, id)})
	p.logAction("block", id, err)
}

func (p *PMPlugin) deleteHistory(ctx context.Context, id int64, revoke bool) {
	req := &tg.MessagesDeleteHistoryRequest{Peer: p.peerOf(ctx, id), MaxID: 0}
	if revoke {
		req.SetRevoke(true)
	}
	_, err := p.host.API().MessagesDeleteHistory(ctx, req)
	p.logAction("delete history", id, err)
}

func (p *PMPlugin) reportSpam(ctx context.Context, id int64) {
	_, err := p.host.API().AccountReportPeer(ctx, &tg.AccountReportPeerRequest{
		Peer: p.peerOf(ctx, id), Reason: &tg.InputReportReasonSpam{}, Message: "spam",
	})
	p.logAction("report", id, err)
}

// runFailActions runs on challenge failure: always archive + mute, then the
// configured extra action. Never against the owner's own chat, even if some
// path managed to open a self challenge (defense in depth for cmdTest).
func (p *PMPlugin) runFailActions(ctx context.Context, id int64) {
	if p.host != nil && id != 0 && id == p.host.SelfID() {
		return
	}
	p.archiveChat(ctx, id, 1)
	p.muteChat(ctx, id, true)
	switch p.options().failAction {
	case failBlock:
		p.blockUser(ctx, id)
	case failDelete:
		p.deleteHistory(ctx, id, true)
	case failReport:
		p.reportSpam(ctx, id)
	}
}

// runPassActions runs the configured action after a user passes.
func (p *PMPlugin) runPassActions(ctx context.Context, id int64) {
	switch p.options().passAction {
	case passUnmute:
		p.muteChat(ctx, id, false)
	case passUnarchive:
		p.archiveChat(ctx, id, 0)
	case passWhitelist:
		p.records.addWhitelist(id)
		_ = p.saveRecords()
	}
}

func (p *PMPlugin) logAction(action string, id int64, err error) {
	if err == nil || p.log == nil {
		return
	}
	p.log.Warn("pmcaptcha: "+action+" failed", "user", id, "error", err)
}

// ============================================================
// Verification flow
// ============================================================

// beginChallenge mutes+archives a stranger and sends the challenge.
func (p *PMPlugin) beginChallenge(ctx context.Context, userID int64) {
	if !p.options().captchaOn {
		// No question to send: mute+archive only (idempotent), like the
		// source. No challenge record is kept in this mode.
		if p.hasChallenge(userID) {
			return
		}
		p.muteChat(ctx, userID, true)
		p.archiveChat(ctx, userID, 1)
		return
	}
	// Atomically claim the slot BEFORE any network work: two near-simultaneous
	// first messages from the same stranger used to both pass the hasChallenge
	// check and run the whole flow twice (double mute/archive and question).
	if !p.claimChallenge(userID) {
		return
	}
	p.muteChat(ctx, userID, true)
	p.archiveChat(ctx, userID, 1)
	p.sendChallenge(ctx, userID)
}

// claimChallenge reserves the challenge slot for userID with a placeholder
// (empty answer) that the real challenge replaces once sent. It returns
// false when a challenge or placeholder already exists.
func (p *PMPlugin) claimChallenge(uid int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.challenges[uid]; ok {
		return false
	}
	p.challenges[uid] = &challenge{started: time.Now()}
	return true
}

// sendChallenge (re)sends a challenge to userID, replacing any old one.
func (p *PMPlugin) sendChallenge(ctx context.Context, userID int64) {
	if old := p.removeChallenge(userID); old != nil && old.answer != "" {
		p.deleteMsgs(ctx, userID, old.msgIDs)
	}
	o := p.options()
	q := genMathQuestion()
	text := challengeText(q.question, o, 0)
	msgID, err := p.host.Send(ctx, userID, text, 0)
	if err != nil {
		// Release the placeholder claimed by beginChallenge so a later
		// message can retry instead of leaving the user in limbo.
		p.removeChallenge(userID)
		if p.log != nil {
			p.log.Warn("pmcaptcha: send challenge failed", "user", userID, "error", err)
		}
		return
	}
	ch := &challenge{answer: q.answer, msgIDs: []int{msgID}, started: time.Now()}
	p.setChallenge(userID, ch)
	p.armTimer(userID, o.timeout)
}

// challengeText renders the challenge message (Chinese, matching the source).
func challengeText(question string, o options, tries int) string {
	var b strings.Builder
	b.WriteString("🔒 **人机验证**\n\n请回复以下算式的答案：\n\n")
	b.WriteString(plugin.Code(question + " = ?"))
	b.WriteString("\n\n")
	b.WriteString(strings.Join(challengeFooter(o, tries), "\n"))
	return b.String()
}

// challengeFooter renders the time/tries/fail-action footer lines.
func challengeFooter(o options, tries int) []string {
	var lines []string
	if o.timeout > 0 {
		lines = append(lines, fmt.Sprintf("⏱ 验证时间：**%d 秒**", o.timeout))
	}
	if o.tries > 0 {
		lines = append(lines, fmt.Sprintf("🔢 剩余次数：**%d 次**", o.tries-tries))
	}
	lines = append(lines, "⚠️ 验证失败将会："+plugin.Escape(failActionLabel(o.failAction)))
	return lines
}

func failActionLabel(action string) string {
	switch action {
	case failBlock:
		return "屏蔽"
	case failDelete:
		return "删除对话（双方）"
	case failReport:
		return "举报"
	default:
		return "仅归档并静音"
	}
}

func passActionLabel(action string) string {
	switch action {
	case passUnmute:
		return "取消静音"
	case passUnarchive:
		return "取消归档"
	case passWhitelist:
		return "加入白名单"
	default:
		return "无"
	}
}

// handleReply grades a message from a user with an open challenge. State
// transitions are claimed atomically under p.mu; network I/O runs after.
func (p *PMPlugin) handleReply(ctx context.Context, userID int64, input string, msgID int) {
	o := p.options()
	p.mu.Lock()
	ch := p.challenges[userID]
	if ch == nil {
		p.mu.Unlock()
		return
	}
	if ch.answer == "" {
		// Placeholder claimed by beginChallenge: the real question has not
		// been sent (or just failed to send) — nothing to grade yet.
		p.mu.Unlock()
		return
	}
	if ch.expired(o.timeout, time.Now()) {
		delete(p.challenges, userID)
		p.mu.Unlock()
		p.finishTimeout(ctx, userID, ch)
		return
	}
	if msgID != 0 {
		ch.msgIDs = append(ch.msgIDs, msgID)
	}
	if answerCorrect(input, ch.answer) {
		delete(p.challenges, userID)
		p.mu.Unlock()
		p.finishPass(ctx, userID, ch)
		return
	}
	if strings.TrimSpace(input) == "" {
		p.mu.Unlock()
		return
	}
	ch.tries++
	if o.tries > 0 && ch.tries >= o.tries {
		delete(p.challenges, userID)
		p.mu.Unlock()
		p.finishMaxTries(ctx, userID, ch)
		return
	}
	tries := ch.tries
	p.mu.Unlock()

	hint := "请重试。"
	if o.tries > 0 {
		hint = fmt.Sprintf("请重试（剩余次数：%d）。", o.tries-tries)
	}
	p.sendPlain(ctx, userID, "❌ 答案错误，"+hint)
}

// finishPass completes a passed challenge.
func (p *PMPlugin) finishPass(ctx context.Context, userID int64, ch *challenge) {
	p.deleteMsgs(ctx, userID, ch.msgIDs)
	p.records.addVerified(userID, p.displayName(ctx, userID), "")
	p.records.delFailed(userID)
	_ = p.saveRecords()
	p.runPassActions(ctx, userID)
	p.sendPlain(ctx, userID, "✅ 验证通过！欢迎与我对话。")
}

// finishMaxTries completes a challenge that ran out of tries.
func (p *PMPlugin) finishMaxTries(ctx context.Context, userID int64, ch *challenge) {
	p.deleteMsgs(ctx, userID, ch.msgIDs)
	p.records.addFailed(userID, p.displayName(ctx, userID), reasonMaxTries)
	p.records.delVerified(userID)
	_ = p.saveRecords()
	p.sendPlain(ctx, userID, "❌ 验证失败次数过多，对话已被限制。")
	p.runFailActions(ctx, userID)
}

// armTimer starts the expiry timer for a challenge.
func (p *PMPlugin) armTimer(userID int64, timeout int) {
	if timeout <= 0 {
		return
	}
	life := p.lifetime()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTimer(time.Duration(timeout) * time.Second)
		defer t.Stop()
		select {
		case <-life.Done():
			return
		case <-t.C:
		}
		p.onTimeout(life, userID)
	}()
}

// onTimeout finishes an expired challenge.
func (p *PMPlugin) onTimeout(ctx context.Context, userID int64) {
	o := p.options()
	p.mu.Lock()
	ch := p.challenges[userID]
	if ch == nil || !ch.expired(o.timeout, time.Now()) {
		p.mu.Unlock()
		return
	}
	delete(p.challenges, userID)
	p.mu.Unlock()
	p.finishTimeout(ctx, userID, ch)
}

// finishTimeout completes an expired challenge.
func (p *PMPlugin) finishTimeout(ctx context.Context, userID int64, ch *challenge) {
	p.deleteMsgs(ctx, userID, ch.msgIDs)
	p.records.addFailed(userID, p.displayName(ctx, userID), reasonTimeout)
	p.records.delVerified(userID)
	_ = p.saveRecords()
	p.sendPlain(ctx, userID, "⏰ 验证超时，对话已被限制。")
	p.runFailActions(ctx, userID)
}

// deleteMsgs removes the plugin's own messages in the user's chat.
func (p *PMPlugin) deleteMsgs(ctx context.Context, userID int64, ids []int) {
	if len(ids) == 0 {
		return
	}
	if err := plugin.DeleteMessages(ctx, p.host.API(), p.peerOf(ctx, userID), ids...); err != nil && p.log != nil {
		p.log.Debug("pmcaptcha: delete challenge messages failed", "user", userID, "error", err)
	}
}

// sendPlain posts a literal line to a user.
func (p *PMPlugin) sendPlain(ctx context.Context, userID int64, text string) {
	if _, err := p.host.Send(ctx, userID, plugin.Escape(text), 0); err != nil && p.log != nil {
		p.log.Debug("pmcaptcha: send to user failed", "user", userID, "error", err)
	}
}

// floodText turns an error into a bilingual line for the owner.
func floodText(tl func(string, string) string, err error) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "Flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		switch e.Type {
		case "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID":
			return tl("这个用户名不存在", "That username does not exist")
		case "PEER_ID_INVALID", "USER_ID_INVALID", "INPUT_USER_DEACTIVATED":
			return tl("找不到这个用户", "Cannot find that user")
		default:
			return plugin.Code(e.Type)
		}
	}
	return plugin.Escape(err.Error())
}
