package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	dialogsPerPage    = 100
	dialogsGuard      = 50 // pages per folder
	deleteDialogDelay = 150 * time.Millisecond
)

// handleDeleted routes clean deleted pm|member [rm]: deleted accounts in
// private chats (scan or remove dialogs) or in a group (scan or kick).
func (p *CleanPlugin) handleDeleted(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	action := stringsLower(args, 0)
	if action == "" {
		return p.deny(ctx, "❌ "+tl("指定范围：", "Pick a scope: ")+plugin.Code("pm")+
			" ("+tl("私聊", "private chats")+") / "+plugin.Code("member")+
			" ("+tl("群组", "group")+")")
	}
	rm := stringsLower(args, 1) == "rm"
	switch action {
	case "pm":
		ok, err := p.run(ctx, func(j *job) { p.cleanDeletedPM(j, rm) })
		if err != nil {
			return p.deny(ctx, "❌ "+errText(tl, err))
		}
		if !ok {
			return p.deny(ctx, "⏳ "+tl("已有清理任务在跑", "A clean job is already running"))
		}
		return nil
	case "member":
		peer, err := ctx.ResolvePeer()
		if err != nil {
			return p.deny(ctx, "❌ "+errText(tl, err))
		}
		if _, ok := peer.(*tg.InputPeerChannel); !ok {
			return p.deny(ctx, "❌ "+tl("只能在超级群里使用（普通群请先升级）", "Use this in a supergroup (upgrade basic groups first)"))
		}
		ok, err := p.run(ctx, func(j *job) { p.cleanDeletedMember(j, rm) })
		if err != nil {
			return p.deny(ctx, "❌ "+errText(tl, err))
		}
		if !ok {
			return p.deny(ctx, "⏳ "+tl("已有清理任务在跑", "A clean job is already running"))
		}
		return nil
	}
	return p.deny(ctx, "❌ "+tl("未知范围：", "Unknown scope: ")+plugin.Code(action)+" · "+
		tl("可用", "available")+" "+plugin.Code("pm")+" / "+plugin.Code("member"))
}

// cleanDeletedPM scans (and with rm removes) dialogs with deleted accounts.
// Archived chats (folder 1) are scanned too, like the source.
// deletedDialog is one deleted-account private chat found in the dialog list.
type deletedDialog struct {
	id   int64
	hash int64
}

func (p *CleanPlugin) cleanDeletedPM(j *job, rm bool) {
	tl := j.Tlocal
	_ = j.Edit("🔍 **" + tl("扫描已注销私聊", "Scanning deleted-account private chats") + "**\n\n⏳ " +
		tl("正在扫描对话列表（含归档）…", "Scanning dialogs (archived included)…"))

	byID := map[int64]deletedDialog{}
	users := map[int64]*tg.User{}
	for _, folder := range []int{0, 1} {
		offsetDate, offsetID := 0, 0
		var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
		for page := 0; page < dialogsGuard; page++ {
			if err := j.Context().Err(); err != nil {
				return
			}
			var res tg.MessagesDialogsClass
			err := retry(j.Context(), func() error {
				req := &tg.MessagesGetDialogsRequest{
					OffsetDate: offsetDate,
					OffsetID:   offsetID,
					OffsetPeer: offsetPeer,
					Limit:      dialogsPerPage,
					Hash:       0,
				}
				if folder != 0 {
					req.SetFolderID(folder) // archived chats live in folder 1
				}
				r, e := j.API.MessagesGetDialogs(j.Context(), req)
				res = r
				return e
			})
			if err != nil {
				if folder == 1 && fatalErr(err) {
					break // archived scan failing is not fatal, like the source
				}
				p.finishErr(j.CommandContext, err)
				return
			}
			pageData, ok := splitDialogs(res)
			if !ok || len(pageData.dialogs) == 0 {
				break
			}
			for _, u := range pageData.users {
				if v, ok := u.(*tg.User); ok {
					users[v.ID] = v
				}
			}
			for _, d := range pageData.dialogs {
				dialog, ok := d.(*tg.Dialog)
				if !ok {
					continue
				}
				pu, ok := dialog.Peer.(*tg.PeerUser)
				if !ok {
					continue
				}
				if u, ok := users[pu.UserID]; ok && u.Deleted && u.ID != 0 {
					byID[u.ID] = deletedDialog{id: u.ID, hash: u.AccessHash}
				}
			}
			if len(pageData.dialogs) < dialogsPerPage {
				break
			}
			last := pageData.dialogs[len(pageData.dialogs)-1]
			if d, okd := last.(*tg.Dialog); okd {
				offsetID = d.TopMessage
				offsetPeer = peerInputOf(d.Peer, chatIndex(pageData.chats))
			}
			offsetDate = 0
			if len(pageData.msgs) > 0 {
				if m, okm := pageData.msgs[len(pageData.msgs)-1].(*tg.Message); okm && m.Date > 0 {
					offsetDate = m.Date
				}
			}
		}
	}

	if len(byID) == 0 {
		p.finish(j.CommandContext, "✅ "+tl("扫描完成：没有发现已注销账号的私聊", "Scan finished: no deleted-account private chats"), resultTTL)
		return
	}

	var removed, failed int
	if rm {
		for id, d := range byID {
			if err := j.Context().Err(); err != nil {
				return
			}
			err := retry(j.Context(), func() error {
				_, e := j.API.MessagesDeleteHistory(j.Context(), &tg.MessagesDeleteHistoryRequest{
					Peer:  &tg.InputPeerUser{UserID: id, AccessHash: d.hash},
					MaxID: 0x7FFFFFFF,
				})
				return e
			})
			if err == nil {
				removed++
			} else if fatalErr(err) {
				p.finishErr(j.CommandContext, err)
				return
			} else {
				failed++
			}
			sleepCtx(j.Context(), deleteDialogDelay)
		}
	}

	s := "✅ **" + tl("扫描完成", "Scan finished") + "**\n\n"
	if rm {
		s = "✅ **" + tl("清理完成", "Cleanup finished") + "**\n\n"
	}
	s += fmt.Sprintf("> %s %d\n", tl("已注销对话", "Deleted-account dialogs"), len(byID))
	if rm {
		s += fmt.Sprintf("> %s %d", tl("已移除", "Removed"), removed)
		if failed > 0 {
			s += fmt.Sprintf(" · %s %d", tl("失败", "failed"), failed)
		}
		s += "\n"
	} else {
		s += "\n💡 " + tl("发送 ", "Send ") + plugin.Code("clean deleted pm rm") +
			tl(" 移除这些对话", " to remove these dialogs") + "\n"
	}
	for _, d := range firstN(byID, 15) {
		s += "\n• " + plugin.Mention(fmt.Sprintf("%d", d), d)
	}
	if len(byID) > 15 {
		s += fmt.Sprintf("\n\n… %s %d", tl("还有", "and"), len(byID)-15)
	}
	p.finish(j.CommandContext, s, resultTTL)
}

// firstN returns the first n ids of m, sorted for stable output.
func firstN(m map[int64]deletedDialog, n int) []int64 {
	ids := make([]int64, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	sortInt64s(ids)
	if len(ids) > n {
		ids = ids[:n]
	}
	return ids
}

// dialogPage holds the concrete lists one dialogs result carries.
type dialogPage struct {
	dialogs []tg.DialogClass
	chats   []tg.ChatClass
	users   []tg.UserClass
	msgs    []tg.MessageClass
}

// splitDialogs unpacks a dialogs result into its concrete lists.
func splitDialogs(res tg.MessagesDialogsClass) (page dialogPage, ok bool) {
	switch v := res.(type) {
	case *tg.MessagesDialogs:
		page = dialogPage{v.Dialogs, v.Chats, v.Users, v.Messages}
		return page, true
	case *tg.MessagesDialogsSlice:
		page = dialogPage{v.Dialogs, v.Chats, v.Users, v.Messages}
		return page, true
	}
	return dialogPage{}, false
}

// chatIndex maps channel ids from a dialogs page for offset-peer hashes.
func chatIndex(chats []tg.ChatClass) map[int64]tg.ChatClass {
	m := map[int64]tg.ChatClass{}
	for _, c := range chats {
		switch v := c.(type) {
		case *tg.Channel:
			m[v.ID] = v
		case *tg.Chat:
			m[-v.ID] = v
		}
	}
	return m
}

// peerInputOf builds the next-page offset peer from a dialog peer.
func peerInputOf(peer tg.PeerClass, chats map[int64]tg.ChatClass) tg.InputPeerClass {
	switch v := peer.(type) {
	case *tg.PeerUser:
		return &tg.InputPeerUser{UserID: v.UserID}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: v.ChatID}
	case *tg.PeerChannel:
		if c, ok := chats[v.ChannelID].(*tg.Channel); ok && c.AccessHash != 0 {
			return &tg.InputPeerChannel{ChannelID: v.ChannelID, AccessHash: c.AccessHash}
		}
		return &tg.InputPeerChannel{ChannelID: v.ChannelID}
	}
	return &tg.InputPeerEmpty{}
}

func sortInt64s(v []int64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// cleanDeletedMember scans (and with rm kicks) deleted accounts in the
// current group. Needs ban rights when rm is set.
func (p *CleanPlugin) cleanDeletedMember(j *job, rm bool) {
	tl := j.Tlocal
	_ = j.Edit("🔍 **" + tl("扫描群内已注销账号", "Scanning deleted accounts in this group") + "**\n\n⏳ " +
		tl("正在扫描成员列表…", "Scanning members…"))

	if rm && !meCanBan(j.Context(), j.API, channelOf(j.peer)) {
		p.finish(j.CommandContext, "❌ "+tl("没有封禁用户的权限，无法清理", "You cannot ban members here; cannot clean"), resultTTL)
		return
	}

	var found, removed, failed int
	type entry struct {
		id   int64
		hash int64
		err  string
		ok   bool
	}
	var entries []entry
	scanned := 0
	offset := 0
	for offset <= memberScanMax {
		if err := j.Context().Err(); err != nil {
			return
		}
		var res tg.ChannelsChannelParticipantsClass
		err := retry(j.Context(), func() error {
			r, e := j.API.ChannelsGetParticipants(j.Context(), &tg.ChannelsGetParticipantsRequest{
				Channel: channelOf(j.peer),
				Filter:  &tg.ChannelParticipantsRecent{},
				Offset:  offset,
				Limit:   memberPage,
				Hash:    0,
			})
			res = r
			return e
		})
		if err != nil {
			p.finishErr(j.CommandContext, err)
			return
		}
		cp, ok := res.(*tg.ChannelsChannelParticipants)
		if !ok || len(cp.Participants) == 0 {
			break
		}
		for _, u := range cp.Users {
			if v, ok := u.(*tg.User); ok && v.Deleted {
				found++
				e := entry{id: v.ID, hash: v.AccessHash}
				if rm {
					if err := p.kickDeleted(j, v); err != nil {
						failed++
						e.err = errText(tl, err)
					} else {
						removed++
						e.ok = true
					}
				}
				entries = append(entries, e)
			}
		}
		scanned += len(cp.Participants)
		j.progress(fmt.Sprintf("🔍 %s\n\n> %s %d\n> %s %d\n\n⏳ %s",
			tl("扫描群内已注销账号", "Scanning deleted accounts"),
			tl("已扫描", "Scanned"), scanned,
			tl("已找到", "Found"), found,
			tl("正在扫描…", "Scanning…")))
		if len(cp.Participants) < memberPage {
			break
		}
		offset += memberPage
		sleepCtx(j.Context(), 100*time.Millisecond)
	}

	if found == 0 {
		p.finish(j.CommandContext, "✅ "+tl("扫描完成：群里没有已注销账号", "Scan finished: no deleted accounts in this group"), resultTTL)
		return
	}

	s := "✅ **" + tl("扫描完成", "Scan finished") + "**\n\n" +
		fmt.Sprintf("> %s %d\n", tl("已注销账号", "Deleted accounts"), found)
	if rm {
		s = "✅ **" + tl("清理完成", "Cleanup finished") + "**\n\n" +
			fmt.Sprintf("> %s %d\n", tl("已注销账号", "Deleted accounts"), found) +
			fmt.Sprintf("> %s %d", tl("已移出", "Removed"), removed)
		if failed > 0 {
			s += fmt.Sprintf(" · %s %d", tl("失败", "failed"), failed)
		}
		s += "\n"
	}
	for i, e := range entries {
		if i >= 15 {
			break
		}
		line := "\n• " + plugin.Mention(fmt.Sprintf("%d", e.id), e.id)
		if rm {
			if e.ok {
				line = "\n• ✅ " + plugin.Mention(fmt.Sprintf("%d", e.id), e.id)
			} else {
				line = "\n• ❌ " + plugin.Mention(fmt.Sprintf("%d", e.id), e.id) + " · " + plugin.Escape(e.err)
			}
		}
		s += line
	}
	if found > 15 {
		s += fmt.Sprintf("\n\n… %s %d %s", tl("还有", "and"), found-15, tl("个未显示", "not shown"))
	}
	if !rm {
		s += "\n\n💡 " + tl("发送 ", "Send ") + plugin.Code("clean deleted member rm") +
			tl(" 清理这些账号", " to clean them up")
	} else if failed > 0 {
		s += "\n\n💡 " + tl("失败常见原因：无封禁权限、目标是管理员、或缺少 access_hash",
			"Common causes: no ban right, target is an admin, or a missing access hash")
	}
	p.finish(j.CommandContext, s, memberResultTTL)
}

// kickDeleted bans and immediately unbans a deleted account, so it leaves
// the group but does not pile up in the ban list.
func (p *CleanPlugin) kickDeleted(j *job, u *tg.User) error {
	ch := channelOf(j.peer)
	peer := &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}
	err := retry(j.Context(), func() error {
		_, e := j.API.ChannelsEditBanned(j.Context(), &tg.ChannelsEditBannedRequest{
			Channel:      ch,
			Participant:  peer,
			BannedRights: kickRights(time.Now().Add(time.Minute).Unix()),
		})
		return e
	})
	if err != nil {
		return err
	}
	sleepCtx(j.Context(), 500*time.Millisecond)
	return retry(j.Context(), func() error {
		_, e := j.API.ChannelsEditBanned(j.Context(), &tg.ChannelsEditBannedRequest{
			Channel:      ch,
			Participant:  peer,
			BannedRights: tg.ChatBannedRights{},
		})
		return e
	})
}

// kickRights bans everything, including reading, until t (0 = forever).
func kickRights(until int64) tg.ChatBannedRights {
	return tg.ChatBannedRights{
		ViewMessages:    true,
		SendMessages:    true,
		SendMedia:       true,
		SendStickers:    true,
		SendGifs:        true,
		SendGames:       true,
		SendInline:      true,
		EmbedLinks:      true,
		SendPolls:       true,
		ChangeInfo:      true,
		InviteUsers:     true,
		PinMessages:     true,
		ManageTopics:    true,
		SendPhotos:      true,
		SendVideos:      true,
		SendRoundvideos: true,
		SendAudios:      true,
		SendVoices:      true,
		SendDocs:        true,
		SendPlain:       true,
		UntilDate:       int(until),
	}
}

// meCanBan reports whether the account may ban members in channel.
func meCanBan(ctx context.Context, api *tg.Client, ch *tg.InputChannel) bool {
	res, err := api.ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{
		Channel:     ch,
		Participant: &tg.InputPeerSelf{},
	})
	if err != nil {
		return false
	}
	switch v := res.Participant.(type) {
	case *tg.ChannelParticipantCreator:
		return true
	case *tg.ChannelParticipantAdmin:
		return v.AdminRights.BanUsers || v.AdminRights.DeleteMessages
	}
	return false
}
