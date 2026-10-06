package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	memberPage    = 200
	memberScanMax = 50000
	adminsPage    = 200
	removePause   = 1500 * time.Millisecond // between two editBanned calls
)

// memberPlan is one parsed `clean member` invocation.
type memberPlan struct {
	mode      int // 1..5
	day       int
	onlyScan  bool
	maxRemove int // 0 = unlimited
	targetArg string
	invalid   string // bilingual error when parsing failed
}

// parseMemberArgs parses everything after `clean member`.
func parseMemberArgs(args []string) memberPlan {
	var p memberPlan
	rest := args
	if len(rest) > 0 {
		n, ok := atoi(rest[0])
		if ok && n >= 1 && n <= 5 {
			p.mode = n
			rest = rest[1:]
		}
	}
	for _, a := range rest {
		low := strings.ToLower(a)
		switch {
		case low == "search":
			p.onlyScan = true
		case low == "rm" || low == "confirm" || low == "all":
			// execute now; mode 5 needs the explicit go-ahead
		case strings.HasPrefix(low, "limit:"):
			if n, ok := atoi(a[len("limit:"):]); ok && n > 0 {
				p.maxRemove = n
			}
		case strings.HasPrefix(a, "chat:"):
			p.targetArg = a[len("chat:"):]
		default:
			if p.day == 0 {
				if n, ok := atoi(a); ok && n > 0 {
					p.day = n
					continue
				}
			}
			if p.invalid == "" && !strings.HasPrefix(a, "-") && a != "" {
				p.invalid = a
			}
		}
	}
	if p.mode == 0 {
		p.invalid = "" // no mode yet: handled by caller
	}
	return p
}

// validate checks the mode-specific day requirements (source semantics:
// modes 1-2 clamp the day to at least 7).
func (p *memberPlan) validate(tl func(string, string) string) error {
	switch p.mode {
	case 0:
		return errors.New(tl("缺少模式：", "Missing mode: ") + "1-5")
	case 1, 2:
		if p.day == 0 {
			return fmt.Errorf("%s %s",
				tl("模式", "Mode")+strconv.Itoa(p.mode)+tl(" 需要天数，例如 ", " needs a day count, e.g. "),
				"clean member "+strconv.Itoa(p.mode)+" 30 search")
		}
		if p.day < 7 {
			p.day = 7
		}
	case 3:
		if p.day == 0 {
			return fmt.Errorf("%s %s",
				tl("模式 3 需要发言条数，例如 ", "Mode 3 needs a message count, e.g. "),
				"clean member 3 5 search")
		}
	case 4, 5:
	default:
		return fmt.Errorf("%s 1-5", tl("未知模式，可用 1-5", "Unknown mode, use 1-5"))
	}
	return nil
}

func (p *memberPlan) label(tl func(string, string) string) string {
	switch p.mode {
	case 1:
		return fmt.Sprintf(tl("未上线超过 %d 天", "offline for over %d days"), p.day)
	case 2:
		return fmt.Sprintf(tl("未发言超过 %d 天", "silent for over %d days"), p.day)
	case 3:
		return fmt.Sprintf(tl("发言少于 %d 条", "fewer than %d messages"), p.day)
	case 4:
		return tl("已注销的账户", "deleted accounts")
	case 5:
		return tl("所有普通成员", "all ordinary members")
	}
	return strconv.Itoa(p.mode)
}

// handleMember runs the clean_member feature: scan or remove group members
// by the five modes.
func (p *CleanPlugin) handleMember(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	if len(args) == 0 {
		return ctx.Edit(memberHelp(tl))
	}
	plan := parseMemberArgs(args)
	if plan.invalid != "" {
		return p.deny(ctx, "❌ "+tl("无法识别的参数：", "Unrecognized argument: ")+plugin.Code(plan.invalid)+"\n\n"+memberHelp(tl))
	}
	if err := plan.validate(tl); err != nil {
		return p.deny(ctx, "❌ "+err.Error()+"\n\n"+memberHelp(tl))
	}

	// Resolve the target chat: chat: argument or the current group.
	peer, perr := ctx.ResolvePeer()
	if perr != nil {
		return p.deny(ctx, "❌ "+errText(tl, perr))
	}
	var target *tg.InputPeerChannel
	var chatTitle string
	if plan.targetArg != "" {
		id, err := strconv.ParseInt(plan.targetArg, 10, 64)
		if err != nil {
			return p.deny(ctx, "❌ "+tl("chat: 需要群组 ID，例如 ", "chat: needs a group id, e.g. ")+"chat:-1001234567890")
		}
		// Accept both -100… chat ids and raw channel ids.
		if id > 0 {
			// raw channel id: not enough to resolve without a hash; the peer
			// store resolves full chat ids only.
			return p.deny(ctx, "❌ "+tl("请使用 -100 开头的群组 ID", "Use the -100… group id form"))
		}
		resolved, rerr := ctx.PeerResolver.ResolveFromChatID(ctx.Context(), id)
		if rerr != nil {
			return p.deny(ctx, "❌ "+tl("无法访问指定群组：", "Cannot access that group: ")+errText(tl, rerr))
		}
		ch, ok := resolved.(*tg.InputPeerChannel)
		if !ok {
			return p.deny(ctx, "❌ "+tl("目标不是群组", "The target is not a group"))
		}
		target = ch
		chatTitle = fmt.Sprintf("%d", id)
	} else {
		switch v := peer.(type) {
		case *tg.InputPeerChannel:
			target = v
		case *tg.InputPeerChat:
			return p.deny(ctx, "❌ "+tl("普通群不支持成员清理，请先升级为超级群", "Basic groups are not supported; upgrade to a supergroup first"))
		default:
			return p.deny(ctx, "❌ "+tl("请在群组中使用，或用 chat: 指定群组", "Use this in a group, or pass chat:"))
		}
		chatTitle = tl("当前群组", "this group")
	}

	if !plan.onlyScan && !ctx.HasArg("confirm") && !(plan.mode != 5 && ctx.HasArg("rm")) {
		// Destructive modes: preview first, then confirm. Mode 5 (remove
		// every ordinary member) only ever accepts the explicit confirm.
		if plan.mode == 5 {
			return p.deny(ctx, "⚠️ "+tl("模式 5 会移出所有普通成员！只能加 confirm 执行：", "Mode 5 removes every ordinary member! Only confirm runs it: ")+plugin.Code("clean member 5 confirm"))
		}
		return p.deny(ctx, "⚠️ "+tl("将移出成员：", "This removes members: ")+plugin.Bold(plan.label(tl))+
			"\n"+tl("加 confirm 执行，或先加 search 预览", "Add confirm to run, or search to preview first"))
	}

	ok, err := p.run(ctx, func(j *job) { p.cleanMembers(j, target, chatTitle, &plan) })
	if err != nil {
		return p.deny(ctx, "❌ "+errText(tl, err))
	}
	if !ok {
		return p.deny(ctx, "⏳ "+tl("已有清理任务在跑", "A clean job is already running"))
	}
	return nil
}

// memberInfo is one candidate row (report/CSV data).
type memberInfo struct {
	ID         int64  `json:"id"`
	Username   string `json:"username"`
	FirstName  string `json:"first_name"`
	LastName   string `json:"last_name"`
	Deleted    bool   `json:"deleted"`
	LastOnline string `json:"last_online"`
	Error      string `json:"error,omitempty"`
}

// memberResult aggregates one member-clean run.
type memberResult struct {
	Scanned int
	Found   int
	Removed int
	Failed  []memberInfo
	Users   []memberInfo
}

// cleanMembers is the body of clean member: page through participants,
// filter by mode, optionally kick each match, and write a CSV report.
func (p *CleanPlugin) cleanMembers(j *job, target *tg.InputPeerChannel, title string, plan *memberPlan) {
	tl := j.Tlocal
	verb := tl("搜索", "Scan")
	if !plan.onlyScan {
		verb = tl("清理", "Clean")
	}
	_ = j.Edit(fmt.Sprintf("🧹 **%s %s**\n\n> %s %s\n⏳ %s",
		verb, plan.label(tl), tl("群组", "Group"), plugin.Escape(title),
		tl("正在获取管理员列表…", "Fetching admins…")))

	admins, err := p.adminIDs(j, target)
	if err != nil {
		p.finishErr(j.CommandContext, err)
		return
	}
	adminCount := len(admins)

	var res memberResult
	stop := false
	offset := 0
	removedBefore := 0
	seen := map[int64]bool{} // dedupes across windows that do not advance
	for !stop && offset <= memberScanMax {
		if err := j.Context().Err(); err != nil {
			return
		}
		var page tg.ChannelsChannelParticipantsClass
		err := retry(j.Context(), func() error {
			r, e := j.API.ChannelsGetParticipants(j.Context(), &tg.ChannelsGetParticipantsRequest{
				Channel: &tg.InputChannel{ChannelID: target.ChannelID, AccessHash: target.AccessHash},
				Filter:  &tg.ChannelParticipantsRecent{},
				Offset:  offset,
				Limit:   memberPage,
				Hash:    0,
			})
			page = r
			return e
		})
		if err != nil {
			p.finishErr(j.CommandContext, err)
			return
		}
		cp, ok := page.(*tg.ChannelsChannelParticipants)
		if !ok || len(cp.Participants) == 0 {
			break
		}
		users := map[int64]*tg.User{}
		for _, u := range cp.Users {
			if v, ok := u.(*tg.User); ok {
				users[v.ID] = v
			}
		}
		for _, pc := range cp.Participants {
			if err := j.Context().Err(); err != nil {
				return
			}
			uid := participantUserID(pc)
			if uid == 0 {
				continue
			}
			if seen[uid] {
				continue // slid back into this window after a removal
			}
			seen[uid] = true
			res.Scanned++
			if admins[uid] || uid == j.SelfID {
				continue
			}
			u := users[uid]
			if u == nil {
				continue
			}
			days, msgCount, known := p.memberStats(j, target, u, plan)
			_ = days
			match, skip := memberMatches(plan, u, lastOnlineDays(u), msgCount, known)
			if skip {
				continue
			}
			if !match {
				continue
			}
			res.Found++
			row := memberInfo{
				ID: u.ID, Username: u.Username, FirstName: u.FirstName, LastName: u.LastName,
				Deleted: u.Deleted, LastOnline: statusLabel(u),
			}
			res.Users = append(res.Users, row)
			if !plan.onlyScan {
				if plan.maxRemove > 0 && res.Removed >= plan.maxRemove {
					stop = true
					break
				}
				if err := p.kickDeleted(j, u); err != nil {
					row.Error = errText(tl, err)
					res.Failed = append(res.Failed, row)
				} else {
					res.Removed++
				}
				if plan.maxRemove > 0 && res.Removed >= plan.maxRemove {
					stop = true
					break
				}
				j.progress(fmt.Sprintf("🧹 %s %s\n\n> %s %d · %s %d\n> %s %d\n\n⏳ %s",
					verb, plan.label(tl),
					tl("已扫描", "Scanned"), res.Scanned,
					tl("已找到", "Found"), res.Found,
					tl("已移出", "Removed"), res.Removed,
					tl("正在清理…", "Cleaning…")))
				sleepCtx(j.Context(), removePause)
			}
		}
		if len(cp.Participants) < memberPage {
			break
		}
		// Each removal shifts the remaining list up by one, so the offset
		// advances only by the participants that are still in place.
		removedNow := res.Removed + len(res.Failed) - removedBefore
		removedBefore = res.Removed + len(res.Failed)
		offset += len(cp.Participants) - removedNow
		sleepCtx(j.Context(), 100*time.Millisecond)
	}

	report := ""
	if path, err := p.writeReport(j, target, title, plan, &res, adminCount); err == nil {
		report = path
	} else if p.log != nil {
		p.log.Warn("clean: write member report failed", "error", err)
	}

	s := fmt.Sprintf("✅ **%s %s**\n\n> %s %s\n", verb, plan.label(tl), tl("群组", "Group"), plugin.Escape(title))
	s += fmt.Sprintf("> %s %d · %s %d\n", tl("已扫描", "Scanned"), res.Scanned, tl("符合条件", "Matches"), res.Found)
	if !plan.onlyScan {
		s += fmt.Sprintf("> %s %d", tl("已移出", "Removed"), res.Removed)
		if plan.maxRemove > 0 {
			s += fmt.Sprintf(" / %s %d", tl("上限", "cap"), plan.maxRemove)
		}
		if n := len(res.Failed); n > 0 {
			s += fmt.Sprintf(" · %s %d", tl("失败", "failed"), n)
		}
		s += "\n"
	}
	if report != "" {
		s += "\n📁 " + tl("报告：", "Report: ") + plugin.Code(report)
	}
	if plan.onlyScan && res.Found > 0 {
		s += "\n\n💡 " + tl("执行清理：", "To clean: ") + plugin.Code("clean member "+strconv.Itoa(plan.mode)+memberDayArg(plan))
	}
	p.finish(j.CommandContext, s, memberResultTTL)
}

func memberDayArg(p *memberPlan) string {
	if p.day > 0 {
		return " " + strconv.Itoa(p.day) + " confirm"
	}
	return " confirm"
}

// memberMatches reports whether the user satisfies the mode. skip is set
// when the needed signal is unknown (mode 1 without a readable status, or
// a failed messages.search in modes 2/3), matching the source's continue.
func memberMatches(plan *memberPlan, u *tg.User, offlineDays, msgCount, known int) (match, skip bool) {
	switch plan.mode {
	case 1:
		if offlineDays < 0 {
			return false, true
		}
		return offlineDays > plan.day, false
	case 2:
		if known&knownLastMsg == 0 {
			return false, true
		}
		return msgCount == 0, false // no message since the cutoff
	case 3:
		if known&knownTotalMsgs == 0 {
			return false, true
		}
		return msgCount < plan.day, false
	case 4:
		return u.Deleted, false
	case 5:
		return true, false
	}
	return false, false
}

// knownLastMsg / knownTotalMsgs mark which message stats a search returned.
const (
	knownLastMsg = 1 << iota
	knownTotalMsgs
)

// memberStats fills the per-user signal for modes 2 and 3 with
// messages.search: mode 2 counts messages newer than the cutoff, mode 3
// the total count. Modes 1/4/5 need no search at all.
func (p *CleanPlugin) memberStats(j *job, target *tg.InputPeerChannel, u *tg.User, plan *memberPlan) (days, msgs, known int) {
	days = lastOnlineDays(u)
	if plan.mode != 2 && plan.mode != 3 {
		return days, 0, knownLastMsg | knownTotalMsgs
	}
	req := &tg.MessagesSearchRequest{
		Peer:   target,
		Filter: &tg.InputMessagesFilterEmpty{},
		Limit:  1,
	}
	req.SetFromID(&tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash})
	if plan.mode == 2 {
		req.MinDate = int(time.Now().Add(-time.Duration(plan.day) * 24 * time.Hour).Unix())
	}
	var res tg.MessagesMessagesClass
	err := retry(j.Context(), func() error {
		r, e := j.API.MessagesSearch(j.Context(), req)
		res = r
		return e
	})
	if err != nil {
		return days, 0, 0 // unknown: skip like the source
	}
	flag := knownTotalMsgs
	if plan.mode == 2 {
		flag = knownLastMsg
	}
	return days, messagesCount(res), flag
}

// messagesCount returns the total count a messages result reports (0 for
// the plain messages.messages variant).
func messagesCount(res tg.MessagesMessagesClass) int {
	switch v := res.(type) {
	case *tg.MessagesMessagesSlice:
		return v.Count
	case *tg.MessagesChannelMessages:
		return v.Count
	}
	return 0
}

// lastOnlineDays returns days since last seen; -1 when unknown.
func lastOnlineDays(u *tg.User) int {
	switch s := u.Status.(type) {
	case *tg.UserStatusOnline, *tg.UserStatusRecently:
		return 0
	case *tg.UserStatusOffline:
		if s.WasOnline <= 0 {
			return -1
		}
		d := int(time.Since(time.Unix(int64(s.WasOnline), 0)).Hours() / 24)
		if d < 0 {
			d = 0
		}
		return d
	case *tg.UserStatusLastWeek:
		return 7
	case *tg.UserStatusLastMonth:
		return 30
	}
	return -1
}

func statusLabel(u *tg.User) string {
	switch u.Status.(type) {
	case *tg.UserStatusOnline:
		return "online"
	case *tg.UserStatusRecently:
		return "recently"
	case *tg.UserStatusOffline:
		return "offline"
	case *tg.UserStatusLastWeek:
		return "last_week"
	case *tg.UserStatusLastMonth:
		return "last_month"
	}
	return ""
}

// adminIDs fetches the group's admin user ids.
func (p *CleanPlugin) adminIDs(j *job, target *tg.InputPeerChannel) (map[int64]bool, error) {
	out := map[int64]bool{}
	var res tg.ChannelsChannelParticipantsClass
	err := retry(j.Context(), func() error {
		r, e := j.API.ChannelsGetParticipants(j.Context(), &tg.ChannelsGetParticipantsRequest{
			Channel: &tg.InputChannel{ChannelID: target.ChannelID, AccessHash: target.AccessHash},
			Filter:  &tg.ChannelParticipantsAdmins{},
			Offset:  0,
			Limit:   adminsPage,
			Hash:    0,
		})
		res = r
		return e
	})
	if err != nil {
		return nil, err
	}
	cp, ok := res.(*tg.ChannelsChannelParticipants)
	if !ok {
		return out, nil
	}
	for _, u := range cp.Users {
		if v, ok := u.(*tg.User); ok {
			out[v.ID] = true
		}
	}
	return out, nil
}

// writeReport writes the CSV report into the plugin data dir.
func (p *CleanPlugin) writeReport(j *job, target *tg.InputPeerChannel, title string, plan *memberPlan, res *memberResult, adminCount int) (string, error) {
	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("report_%d_%d_%s.csv", plugin.ChannelChatID(target.ChannelID), plan.mode, time.Now().Format("20060102_150405")))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	f.Write([]byte{0xEF, 0xBB, 0xBF})
	w := csv.NewWriter(f)
	defer w.Flush()
	rows := [][]string{
		{tlText(j, "群组清理报告", "group clean report")},
		{tlText(j, "群组", "group"), title},
		{tlText(j, "模式", "mode"), plan.label(j.Tlocal)},
		{tlText(j, "扫描时间", "time"), time.Now().Format("2006-01-02 15:04:05")},
		{tlText(j, "已扫描", "scanned"), strconv.Itoa(res.Scanned)},
		{tlText(j, "符合条件", "matches"), strconv.Itoa(res.Found)},
		{tlText(j, "已移出", "removed"), strconv.Itoa(res.Removed)},
		{tlText(j, "失败", "failed"), strconv.Itoa(len(res.Failed))},
		{},
		{"ID", tlText(j, "用户名", "username"), tlText(j, "姓名", "name"), tlText(j, "最后上线", "last online"), tlText(j, "已注销", "deleted")},
	}
	for _, u := range res.Users {
		rows = append(rows, []string{
			strconv.FormatInt(u.ID, 10), u.Username,
			strings.TrimSpace(u.FirstName + " " + u.LastName),
			u.LastOnline, boolText(j, u.Deleted),
		})
	}
	if len(res.Failed) > 0 {
		rows = append(rows, []string{tlText(j, "失败列表", "failures")})
		for _, u := range res.Failed {
			rows = append(rows, []string{
				strconv.FormatInt(u.ID, 10), u.Username,
				strings.TrimSpace(u.FirstName + " " + u.LastName),
				u.LastOnline, boolText(j, u.Deleted), u.Error,
			})
		}
	}
	return path, w.WriteAll(rows)
}

func tlText(j *job, zh, en string) string { return j.Tlocal(zh, en) }

func boolText(j *job, b bool) string {
	if b {
		return tlText(j, "是", "yes")
	}
	return tlText(j, "否", "no")
}

// targetID returns the numeric id of the target chat for report names.
func targetID(j *job) int64 {
	if v, ok := j.peer.(*tg.InputPeerChannel); ok {
		return plugin.ChannelChatID(v.ChannelID)
	}
	return j.Message.ChatID
}

func memberHelp(tl func(string, string) string) string {
	return "🧹 **" + tl("群成员清理", "Member cleanup") + "**\n\n" +
		plugin.Code("clean member <模式 mode> [参数 args] [chat:-100…] [limit:N] [search] [confirm]") + "\n\n" +
		"> 1 " + tl("天数 days — 未上线超过 N 天", "offline for over N days") + "\n" +
		"> 2 " + tl("天数 days — 未发言超过 N 天（按最后一条消息）", "silent for over N days (by last message)") + "\n" +
		"> 3 " + tl("条数 count — 发言少于 N 条", "fewer than N messages") + "\n" +
		"> 4 " + tl("已注销的账户", "deleted accounts") + "\n" +
		"> 5 " + tl("所有普通成员（危险 danger）", "all ordinary members (danger)") + "\n\n" +
		"> " + plugin.Code("search") + " " + tl("只扫描预览，不踢人", "scan only, kick nobody") + "\n" +
		"> " + plugin.Code("confirm") + " " + tl("确认执行清理", "confirm the cleanup") + "\n" +
		"> " + plugin.Code("limit:100") + " " + tl("最多移出 100 人", "remove at most 100") + "\n" +
		"> " + plugin.Code("chat:-100…") + " " + tl("指定其他群组", "target another group") + "\n\n" +
		tl("示例：", "Examples: ") + plugin.Code("clean member 1 30 search") + " · " +
		plugin.Code("clean member 2 60 limit:50 confirm") + " · " + plugin.Code("clean member 4 chat:-1001234567890")
}

// participantUserID extracts the user id of a channel participant entry.
func participantUserID(pc tg.ChannelParticipantClass) int64 {
	switch v := pc.(type) {
	case *tg.ChannelParticipant:
		return v.UserID
	case *tg.ChannelParticipantSelf:
		return v.UserID
	case *tg.ChannelParticipantAdmin:
		return v.UserID
	case *tg.ChannelParticipantCreator:
		return v.UserID
	case *tg.ChannelParticipantBanned:
		if pu, ok := v.Peer.(*tg.PeerUser); ok {
			return pu.UserID
		}
	}
	return 0
}
