package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	blockedPage  = 100
	blockedGuard = 500 // max getBlocked pages, like the source
	minDelay     = 200 * time.Millisecond
	allDelay     = time.Second
	maxDelay     = 5 * time.Second
)

// handleBlocked routes clean blocked pm|member [all]: unblocking your own
// blocklist, or unblocking banned entities in a group.
func (p *CleanPlugin) handleBlocked(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	action := stringsLower(args, 0)
	if action == "" {
		return p.deny(ctx, "❌ "+tl("指定范围：", "Pick a scope: ")+plugin.Code("pm")+
			" ("+tl("清理自己的拉黑名单", "clear your own blocklist")+") / "+plugin.Code("member")+
			" ("+tl("解封群里的封禁实体", "unban banned entities in this group")+")")
	}
	all := stringsLower(args, 1) == "all"
	switch action {
	case "pm":
		ok, err := p.run(ctx, func(j *job) { p.cleanBlockedPM(j, all) })
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
		ok, err := p.run(ctx, func(j *job) { p.unblockMember(j, all) })
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

// cleanBlockedPM unblocks the whole account blocklist. Smart mode skips
// bots, scam and fake accounts; all mode unblocks everything.
func (p *CleanPlugin) cleanBlockedPM(j *job, all bool) {
	tl := j.Tlocal
	_ = j.Edit("🧹 **" + tl("清理拉黑名单", "Clearing blocklist") + "**\n\n⏳ " +
		tl("正在读取拉黑列表…", "Reading the blocklist…") + " · " + modeSmartAll(tl, all))

	var ok, failed, skipped int
	total := 0
	start := time.Now()
	// Unblocking removes the entry, so the offset only advances by the
	// entries skipped in a batch (matching the source's pagination).
	offset := 0
	for page := 0; page < blockedGuard; page++ {
		if err := j.Context().Err(); err != nil {
			return
		}
		var res tg.ContactsBlockedClass
		err := retry(j.Context(), func() error {
			r, e := j.API.ContactsGetBlocked(j.Context(), &tg.ContactsGetBlockedRequest{
				Offset: offset, Limit: blockedPage,
			})
			res = r
			return e
		})
		if err != nil {
			p.finishErr(j.CommandContext, err)
			return
		}
		peers := blockedPeersOf(res)
		users := usersByBlockedID(res)
		if len(peers) == 0 {
			break
		}
		if page == 0 {
			if sl, ok := res.(*tg.ContactsBlockedSlice); ok && sl.Count > total {
				total = sl.Count // server-side estimate for the progress bar
			}
		}
		skippedInBatch := 0
		for _, pb := range peers {
			if err := j.Context().Err(); err != nil {
				return
			}
			pu, oku := pb.PeerID.(*tg.PeerUser)
			if !oku {
				continue // channels and groups stay; the source only unblocks users
			}
			u := users[pu.UserID]
			total++
			if !all && (u.Bot || u.Scam || u.Fake) {
				skipped++
				skippedInBatch++
				continue
			}
			err := retry(j.Context(), func() error {
				_, e := j.API.ContactsUnblock(j.Context(), &tg.ContactsUnblockRequest{
					ID: &tg.InputPeerUser{UserID: pu.UserID, AccessHash: u.AccessHash},
				})
				return e
			})
			switch {
			case err == nil:
				ok++
			case fatalErr(err):
				p.finishErr(j.CommandContext, err)
				return
			default:
				failed++
			}
			sleepCtx(j.Context(), unblockDelay(u, all))
		}
		offset += skippedInBatch
		if len(peers) < blockedPage {
			break
		}
		j.progress(blockedProgress(tl, ok+failed+skipped, total, ok, failed, skipped, all, time.Since(start)))
	}
	p.finish(j.CommandContext, blockedResult(tl, ok, failed, skipped, total, all), resultTTL)
}

// blockedPeersOf returns the blocked entries of a getBlocked result.
func blockedPeersOf(res tg.ContactsBlockedClass) []tg.PeerBlocked {
	switch v := res.(type) {
	case *tg.ContactsBlocked:
		return v.Blocked
	case *tg.ContactsBlockedSlice:
		return v.Blocked
	}
	return nil
}

// usersByBlockedID maps the user ids of a blocked page to full users.
func usersByBlockedID(res tg.ContactsBlockedClass) map[int64]*tg.User {
	out := map[int64]*tg.User{}
	var users []tg.UserClass
	switch v := res.(type) {
	case *tg.ContactsBlocked:
		users = v.Users
	case *tg.ContactsBlockedSlice:
		users = v.Users
	}
	for _, u := range users {
		if v, ok := u.(*tg.User); ok {
			out[v.ID] = v
		}
	}
	return out
}

// unblockDelay picks the sleep between unblocks, like the source's dynamic
// delay (bots and scam/fake accounts wait longer).
func unblockDelay(u *tg.User, all bool) time.Duration {
	d := minDelay
	switch {
	case u == nil:
	case u.Bot:
		d = 1500 * time.Millisecond
	case u.Scam, u.Fake:
		d = 800 * time.Millisecond
	}
	if all && d < allDelay {
		d = allDelay
	}
	return min(d, maxDelay)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func modeSmartAll(tl func(string, string) string, all bool) string {
	if all {
		return tl("全量模式", "full mode")
	}
	return tl("智能模式（跳过机器人 / 诈骗账号）", "smart mode (skips bots / scam accounts)")
}

func blockedProgress(tl func(string, string) string, processed, total, ok, failed, skipped int, all bool, took time.Duration) string {
	if total < processed {
		total = processed // entries appeared while cleaning
	}
	return fmt.Sprintf("🧹 **%s**\n\n> %s %d/%d\n> %s %d · %s %d · %s %d\n> %s · %s\n\n⏳ %s",
		tl("清理拉黑名单", "Clearing blocklist"),
		tl("进度", "Progress"), processed, total,
		tl("成功", "OK"), ok, tl("失败", "Failed"), failed, tl("跳过", "Skipped"), skipped,
		modeSmartAll(tl, all), remaining(tl, processed, total, took),
		tl("正在清理…", "Cleaning…"))
}

// blockedResult mirrors the source's final report.
func blockedResult(tl func(string, string) string, ok, failed, skipped, total int, all bool) string {
	status, emoji := tl("成功完成", " finished"), "✅"
	if failed > 0 && failed > ok {
		status, emoji = tl("部分完成", "partly finished"), "⚠️"
	} else if ok == 0 {
		status, emoji = tl("无需清理", "nothing to clean"), "ℹ️"
	}
	rate := 0
	if total > 0 {
		rate = ok * 100 / total
	}
	return fmt.Sprintf("%s **%s%s**\n\n> %s %d · %s %d · %s %d · %s %d\n> %s %d%% · %s",
		emoji, tl("清理拉黑名单", "Clearing blocklist"), status,
		tl("总计", "Total"), total, tl("成功", "OK"), ok, tl("失败", "Failed"), failed, tl("跳过", "Skipped"), skipped,
		tl("成功率", "Success rate"), rate, modeSmartAll(tl, all))
}

// remaining estimates the time left from the pace so far.
func remaining(tl func(string, string) string, done, total int, took time.Duration) string {
	if done <= 0 || total <= done {
		return tl("即将完成", "almost done")
	}
	est := took / time.Duration(done) * time.Duration(total-done)
	if est < time.Second {
		return tl("即将完成", "almost done")
	}
	if est < time.Minute {
		return fmt.Sprintf(tl("约 %d 秒", "~%ds"), int(est.Seconds())+1)
	}
	return fmt.Sprintf(tl("约 %d 分钟", "~%dm"), int(est.Minutes())+1)
}

// stringsLower returns args[i] lowercased ("" when out of range).
func stringsLower(args []string, i int) string {
	if i < 0 || i >= len(args) {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(args[i]))
}
