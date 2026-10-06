package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	deleteDelay    = 10 * time.Second // single-group results self-delete
	summaryTTL     = 30 * time.Second // sb/unsb summaries self-delete
	progressEvery  = 3 * time.Second
	batchWorkers   = 4
	maxBatchFlood  = 8 * time.Second
	dialogsPerPage = 100
	dialogsGuard   = 50 // pages per folder; 50×100 dialogs is plenty
	groupsFile     = "groups.json"
	maxMute        = 366 * 86400 // Telegram treats longer mutes as permanent
)

var Metadata = &plugin.PluginMetadata{
	Name:        "ban",
	Description: "封禁、禁言与批量管理",
	DescEN:      "Ban, mute and manage members across groups",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type BanPlugin struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	host   plugin.Host
	log    plugin.Logger
}

func New() *BanPlugin { return &BanPlugin{} }

func (p *BanPlugin) Name() string        { return "ban" }
func (p *BanPlugin) Description() string { return Metadata.Description }
func (p *BanPlugin) DescEN() string      { return Metadata.DescEN }

func (p *BanPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.log = mgr.Host().Logger("ban")
	for _, c := range []*plugin.Command{
		{Name: "ban", Description: "封禁成员并清理其在本群的消息", DescEN: "Ban a member and wipe their messages here",
			Usage: "ban [回复 | @用户名 | ID] [true]", UsageEN: "ban [reply | @username | ID] [true]"},
		{Name: "unban", Description: "解除封禁", DescEN: "Lift a ban",
			Usage: "unban [回复 | @用户名 | ID]", UsageEN: "unban [reply | @username | ID]"},
		{Name: "kick", Description: "踢出成员（封禁后立刻解封）", DescEN: "Kick a member (ban then instant unban)",
			Usage: "kick [回复 | @用户名 | ID] [true]", UsageEN: "kick [reply | @username | ID] [true]"},
		{Name: "mute", Description: "禁言，时长必须带单位，不填则永久", DescEN: "Mute; the duration needs units, no duration means forever",
			Usage: "mute [回复 | @用户名 | ID] [时长 1h30m]", UsageEN: "mute [reply | @username | ID] [duration 1h30m]"},
		{Name: "unmute", Description: "解除禁言", DescEN: "Lift a mute",
			Usage: "unmute [回复 | @用户名 | ID]", UsageEN: "unmute [reply | @username | ID]"},
		{Name: "sb", Description: "在有管理权的群全部封禁该用户", DescEN: "Ban the user in every group you manage",
			Usage: "sb [回复 | @用户名 | ID] [true]", UsageEN: "sb [reply | @username | ID] [true]"},
		{Name: "unsb", Description: "在有管理权的群全部解封该用户", DescEN: "Unban the user in every group you manage",
			Usage: "unsb [回复 | @用户名 | ID] [true]", UsageEN: "unsb [reply | @username | ID] [true]"},
		{Name: "refresh", Description: "刷新有管理权的群列表缓存", DescEN: "Refresh the cached list of managed groups",
			Usage: "refresh", UsageEN: "refresh"},
	} {
		c.Plugin, c.Category, c.OwnerOnly, c.Handler = p.Name(), "group", true, p.handle(c.Name)
		if err := mgr.RegisterCommand(c); err != nil {
			return err
		}
	}
	return nil
}

func (p *BanPlugin) Start(context.Context) error {
	p.lifetime()
	return nil
}

func (p *BanPlugin) Stop(context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

// lifetime returns the plugin-scoped context background work runs on.
func (p *BanPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	return p.ctx
}

// timedSeconds picks the duration argument for mute: the first arg that
// looks like a duration (digits followed by a unit letter). Bare numbers
// are user ids, never durations; a duration-like arg that fails to parse
// comes back as bad so the caller can reject it instead of muting forever.
func timedSeconds(args []string) (secs int, found bool, bad string) {
	for _, a := range args {
		if isMetaFlag(a) || strings.HasPrefix(a, "@") || !durationLike(a) {
			continue
		}
		s, ok := parseDuration(a)
		if !ok {
			return 0, false, a
		}
		return s, true, ""
	}
	return 0, false, ""
}

// durationLike reports whether s starts with a digit and has a letter in
// it, e.g. 5m / 1h30 / 2d10x.
func durationLike(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || s[0] < '0' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return false
}

// parseDuration reads `5m`, `1h30m` or `2d`; a bare number is not a
// duration here. Units are required.
func parseDuration(s string) (secs int, ok bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, false
	}
	unitSecs := map[byte]int{'s': 1, 'm': 60, 'h': 3600, 'd': 86400}
	seen := map[byte]bool{}
	num, digits, hasUnit := 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			num = num*10 + int(c-'0')
			digits = true
			if num > maxMute*10 {
				return 0, false
			}
		case unitSecs[c] > 0:
			if !digits || seen[c] {
				return 0, false
			}
			seen[c], hasUnit = true, true
			secs += num * unitSecs[c]
			if secs > maxMute*10 {
				return 0, false
			}
			num, digits = 0, false
		default:
			return 0, false
		}
	}
	if digits || !hasUnit { // trailing number or no unit at all
		return 0, false
	}
	return secs, true
}

func (p *BanPlugin) handle(name string) plugin.Handler {
	return func(ctx *plugin.CommandContext) error {
		if ctx.Message == nil || ctx.Message.Message == nil || ctx.API == nil {
			return plugin.ErrNoMessage
		}
		if len(ctx.Args) == 1 && strings.EqualFold(ctx.Args[0], "help") {
			return ctx.Edit(helpText(ctx.Tlocal))
		}
		switch name {
		case "refresh":
			return p.refresh(ctx)
		case "sb":
			return p.superAction(ctx, "ban")
		case "unsb":
			return p.superAction(ctx, "unban")
		default:
			return p.single(ctx, name)
		}
	}
}

// refresh rebuilds the managed-group cache.
func (p *BanPlugin) refresh(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	_ = ctx.Edit("🔄 " + tl("正在扫描会话…", "Scanning dialogs…"))
	groups, err := p.scanGroups(ctx.Context())
	if err != nil {
		return p.oops(ctx, "❌ "+errText(tl, err))
	}
	if path, perr := p.groupsPath(); perr == nil {
		if werr := writeGroups(path, groups); werr != nil {
			p.warn(ctx, "write groups cache failed", werr)
		}
	}
	channels := 0
	for _, g := range groups {
		if g.Kind == "channel" {
			channels++
		}
	}
	s := fmt.Sprintf("✅ **%s**\n\n> %s %d（%s %d）",
		tl("已刷新管理群缓存", "Managed groups refreshed"),
		tl("共", "total"), len(groups),
		tl("超级群", "supergroups"), channels)
	return ctx.Edit(s)
}

// superAction is sb/unsb: confirm, resolve, then batch in the background.
func (p *BanPlugin) superAction(ctx *plugin.CommandContext, action string) error {
	tl := ctx.Tlocal
	groups, err := p.loadGroups(ctx.Context(), false)
	if err != nil {
		return p.oops(ctx, "❌ "+errText(tl, err))
	}
	if len(groups) == 0 {
		return p.oops(ctx, "❌ "+tl("没有有管理权的群组，先发 refresh 扫描一次", "No managed groups yet; run refresh first"))
	}
	t, name, err := p.resolveTarget(ctx, true)
	if errors.Is(err, errNoTarget) {
		return p.oops(ctx, "❌ "+tl("指定目标：回复消息，或用 @用户名 / 用户ID", "Give a target: reply, @username or user ID"))
	}
	if err != nil {
		return p.oops(ctx, "❌ "+errText(tl, err))
	}
	if t.hash == 0 {
		return p.oops(ctx, "❌ "+tl(
			"解析不到这个 ID（会话里没见过，管理群里也没有）。回复对方的一条消息再试",
			"Cannot resolve this ID (never seen here or in managed groups). Reply to one of their messages instead"))
	}
	if t.id == ctx.SelfID {
		return p.oops(ctx, "❌ "+tl("不能对自己使用", "You cannot target yourself"))
	}
	if !ctx.HasArg("true") && !ctx.HasArg("confirm") {
		if n := p.adminEverywhere(ctx, t, groups); n > 0 {
			return p.oops(ctx, fmt.Sprintf("⚠️ %s（%d %s）",
				tl("目标在其中一些群是管理员，加 true 确认执行", "The target is an admin in some of these groups; add true to confirm"),
				n, tl("个群", "groups")))
		}
	}
	verb := tl("封禁", "Ban")
	if action == "unban" {
		verb = tl("解封", "Unban")
	}
	if err := ctx.Edit(fmt.Sprintf("⚡ **%s %s**\n\n⏳ %s %d %s…",
		verb, plugin.Bold(name), tl("处理中，共", "working through"), len(groups), tl("个群", "groups"))); err != nil {
		return err
	}
	if !batch.claim(t.id) {
		return p.oops(ctx, "⏳ "+tl("这个目标还有批量操作在跑", "A batch is still running for this target"))
	}
	p.batch(ctx, action, t, name, groups)
	return nil
}

// single runs ban/unban/kick/mute/unmute in the current group.
func (p *BanPlugin) single(ctx *plugin.CommandContext, action string) error {
	tl := ctx.Tlocal
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return p.oops(ctx, "❌ "+errText(tl, err))
	}
	var channel *tg.InputPeerChannel
	basic := false
	switch v := peer.(type) {
	case *tg.InputPeerChannel:
		channel = v
	case *tg.InputPeerChat:
		basic = true
	default:
		return p.oops(ctx, "❌ "+tl("只能在群组里使用", "Use this in a group"))
	}
	if basic {
		switch action {
		case "unban", "unmute", "mute":
			return p.oops(ctx, "❌ "+tl("普通群不支持禁言和解封，请先升级为超级群", "Basic groups have no mutes or unbans; upgrade to a supergroup first"))
		}
	}

	secs := 0
	if action == "mute" {
		s, found, bad := timedSeconds(ctx.Args)
		if bad != "" {
			return p.oops(ctx, "❌ "+tl("时长格式不对：", "Invalid duration: ")+plugin.Code(bad)+"\n"+
				tl("单位 s/m/h/d，可组合如 1h30m；不填时长则为永久", "units s/m/h/d, combinable like 1h30m; omit the duration for forever"))
		}
		if !found {
			s = 0 // no duration argument: permanent mute
		}
		if s > maxMute {
			return p.oops(ctx, "❌ "+tl("时长最多 366 天", "The duration can be 366 days at most"))
		}
		secs = s
	}

	t, name, err := p.resolveTarget(ctx, !basic)
	if errors.Is(err, errNoTarget) {
		return p.oops(ctx, "❌ "+tl("指定目标：回复消息，或用 @用户名 / 用户ID", "Give a target: reply, @username or user ID"))
	}
	if err != nil {
		return p.oops(ctx, "❌ "+errText(tl, err))
	}
	if t.id == ctx.SelfID {
		return p.oops(ctx, "❌ "+tl("不能对自己使用", "You cannot target yourself"))
	}

	confirm := ctx.HasArg("true") || ctx.HasArg("confirm")
	if basic {
		if err := p.basicGuard(ctx, peer, t, confirm); err != nil {
			if err == errNeedConfirm {
				return p.oops(ctx, "⚠️ "+tl("目标是管理员，加 true 确认执行", "The target is an admin; add true to confirm"))
			}
			if err == errNoRights {
				return p.oops(ctx, "❌ "+tl("需要管理员权限", "Admin rights are needed"))
			}
			return p.oops(ctx, "❌ "+errText(tl, err))
		}
	} else {
		if !p.meCanBan(ctx, channel) {
			if action == "mute" {
				// meCanBan accepts delete-message admins too, but muting
				// needs the ban right itself; name it correctly.
				return p.oops(ctx, "❌ "+tl("需要封禁成员的管理员权限（禁言）", "The admin right to ban members is needed (muting)"))
			}
			return p.oops(ctx, "❌ "+tl("需要封禁成员的管理员权限", "The admin right to ban members is needed"))
		}
		if !confirm && p.isTargetAdmin(ctx, channel, t) {
			return p.oops(ctx, "⚠️ "+tl("目标是管理员，加 true 确认执行", "The target is an admin; add true to confirm"))
		}
	}

	verbs := map[string]string{
		"ban": tl("封禁", "Banning"), "unban": tl("解封", "Unbanning"),
		"kick": tl("踢出", "Kicking"), "mute": tl("禁言", "Muting"),
		"unmute": tl("解禁言", "Unmuting"),
	}
	if basic && action == "ban" {
		verbs["ban"] = tl("移出", "Removing") // basic groups have no bans
	}
	dur := ""
	if action == "mute" {
		if secs == 0 {
			dur = " · " + tl("永久", "forever")
		} else {
			dur = " · " + formatDuration(tl, secs)
		}
	}
	_ = ctx.Edit("⏳ " + verbs[action] + " " + plugin.Bold(name) + dur + "…")

	wiped := false
	if action == "ban" && !basic {
		wiped = p.deleteHistoryCurrent(ctx, channel, t)
	}
	var actErr error
	switch action {
	case "ban":
		if basic {
			actErr = retryFlood(ctx.Context(), func() error {
				_, err := ctx.API.MessagesDeleteChatUser(ctx.Context(), &tg.MessagesDeleteChatUserRequest{
					ChatID: mustChatID(peer), UserID: &tg.InputUser{UserID: t.id, AccessHash: t.hash},
				})
				return err
			})
		} else {
			actErr = retryFlood(ctx.Context(), func() error {
				return editBanned(ctx.Context(), ctx.API, &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash}, t.peer(), "ban")
			})
		}
	case "kick":
		if basic {
			actErr = retryFlood(ctx.Context(), func() error {
				_, err := ctx.API.MessagesDeleteChatUser(ctx.Context(), &tg.MessagesDeleteChatUserRequest{
					ChatID: mustChatID(peer), UserID: &tg.InputUser{UserID: t.id, AccessHash: t.hash},
				})
				return err
			})
		} else {
			ch := &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash}
			actErr = retryFlood(ctx.Context(), func() error {
				return editBanned(ctx.Context(), ctx.API, ch, t.peer(), "ban")
			})
			if actErr == nil {
				if uerr := retryFlood(ctx.Context(), func() error {
					return editBanned(ctx.Context(), ctx.API, ch, t.peer(), "unban")
				}); uerr != nil {
					// The ban landed; the unban did not. The user is
					// banned, not kicked — surface that clearly.
					return fmt.Errorf("%s\n> ⚠️ %s", uerr.Error(),
						tl("已封禁但解封失败，请手动 unban", "banned but the unban failed; run unban manually"))
				}
			}
		}
	case "unban", "unmute":
		actErr = retryFlood(ctx.Context(), func() error {
			return editBanned(ctx.Context(), ctx.API, &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash}, t.peer(), "unban")
		})
	case "mute":
		until := 0
		if secs > 0 {
			until = int(time.Now().Add(time.Duration(secs) * time.Second).Unix())
		}
		actErr = retryFlood(ctx.Context(), func() error {
			_, err := ctx.API.ChannelsEditBanned(ctx.Context(), &tg.ChannelsEditBannedRequest{
				Channel:      &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
				Participant:  t.peer(),
				BannedRights: fullMuteRights(until),
			})
			return err
		})
	}
	if actErr != nil {
		if action == "ban" && wiped {
			// deleteHistoryCurrent ran before the ban and already removed
			// the target's messages; the user must know that happened.
			return p.oops(ctx, "❌ "+errText(tl, actErr)+
				"\n> ⚠️ "+tl("其在本群的消息已被清理（封禁本身未生效）", "their messages here were already wiped (the ban itself failed)"))
		}
		return p.oops(ctx, "❌ "+errText(tl, actErr))
	}

	note := ""
	if action == "ban" && !basic {
		if wiped {
			note = "\n> 🗑 " + tl("已清理其在本群的消息", "their messages here were wiped")
		} else {
			note = "\n> ℹ️ " + tl("消息清理需要删除消息权限，本次跳过", "wiping messages needs the delete right; skipped")
		}
	}
	untilNote := ""
	if action == "mute" && secs > 0 {
		untilNote = "\n> " + tl("解除时间：", "Until: ") + time.Now().Add(time.Duration(secs)*time.Second).Format("2006-01-02 15:04")
	}
	return p.done(ctx, fmt.Sprintf("✅ **%s %s**\n\n%s%s%s",
		verbs[action], plugin.Bold(name), durLabel(action, dur), untilNote, note))
}

// durLabel puts the duration on its own line for mute results.
func durLabel(action, dur string) string {
	if action == "mute" && dur != "" {
		return "> " + strings.TrimPrefix(dur, " · ") + "\n"
	}
	return ""
}

var (
	errNeedConfirm = errors.New("NEED_CONFIRM")
	errNoRights    = errors.New("NO_RIGHTS")
	errNotMember   = errors.New("USER_NOT_PARTICIPANT")
)

// basicGuard checks rights and target-admin status in a basic group. It
// also rejects targets who are not members (nothing to remove there).
func (p *BanPlugin) basicGuard(ctx *plugin.CommandContext, peer tg.InputPeerClass, t target, confirm bool) error {
	full, err := ctx.API.MessagesGetFullChat(ctx.Context(), mustChatID(peer))
	if err != nil {
		return err
	}
	fullChat, ok := full.FullChat.(*tg.ChatFull)
	if !ok {
		return errNoRights
	}
	participants, ok := fullChat.Participants.(*tg.ChatParticipants)
	if !ok {
		return errNoRights // forbidden: we cannot even list members
	}
	selfAdmin, targetAdmin, targetMember := false, false, false
	for _, pc := range participants.Participants {
		switch v := pc.(type) {
		case *tg.ChatParticipantCreator:
			if v.UserID == ctx.SelfID {
				selfAdmin = true
			}
			if v.UserID == t.id {
				targetAdmin = true
			}
		case *tg.ChatParticipantAdmin:
			if v.UserID == ctx.SelfID {
				selfAdmin = true
			}
			if v.UserID == t.id {
				targetAdmin = true
			}
		default:
			if v.GetUserID() == t.id {
				targetMember = true
			}
		}
	}
	if !selfAdmin {
		return errNoRights
	}
	if !targetMember && !targetAdmin {
		return errNotMember
	}
	if targetAdmin && !confirm {
		return errNeedConfirm
	}
	return nil
}

// meCanBan checks the account's own rights in a supergroup.
func (p *BanPlugin) meCanBan(ctx *plugin.CommandContext, channel *tg.InputPeerChannel) bool {
	res, err := ctx.API.ChannelsGetParticipant(ctx.Context(), &tg.ChannelsGetParticipantRequest{
		Channel:     &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
		Participant: &tg.InputPeerSelf{},
	})
	if err != nil {
		return false
	}
	switch v := res.Participant.(type) {
	case *tg.ChannelParticipantCreator:
		return true
	case *tg.ChannelParticipantAdmin:
		r := v.AdminRights
		return r.BanUsers || r.DeleteMessages
	}
	return false
}

// isTargetAdmin reports whether the target administers this supergroup.
// Unknown (left) targets count as not admin.
func (p *BanPlugin) isTargetAdmin(ctx *plugin.CommandContext, channel *tg.InputPeerChannel, t target) bool {
	res, err := ctx.API.ChannelsGetParticipant(ctx.Context(), &tg.ChannelsGetParticipantRequest{
		Channel:     &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
		Participant: t.peer(),
	})
	if err != nil {
		return false
	}
	switch res.Participant.(type) {
	case *tg.ChannelParticipantCreator, *tg.ChannelParticipantAdmin:
		return true
	}
	return false
}

// deleteHistoryCurrent wipes the target's messages in the current chat.
func (p *BanPlugin) deleteHistoryCurrent(ctx *plugin.CommandContext, channel *tg.InputPeerChannel, t target) bool {
	_, err := ctx.API.ChannelsDeleteParticipantHistory(ctx.Context(), &tg.ChannelsDeleteParticipantHistoryRequest{
		Channel:     &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
		Participant: t.peer(),
	})
	if err != nil && !tgerr.Is(err, "CHAT_ADMIN_REQUIRED", "USER_NOT_PARTICIPANT", "CHANNEL_INVALID", "USER_ID_INVALID") {
		p.warn(ctx, "delete participant history failed", err)
	}
	return err == nil
}

// banRights revokes everything, including reading, until the given time
// (0 = forever).
func banRights(until int) tg.ChatBannedRights {
	r := fullMuteRights(until)
	r.ViewMessages = true
	return r
}

// fullMuteRights revokes every send right until the given time (0 =
// forever).
func fullMuteRights(until int) tg.ChatBannedRights {
	return tg.ChatBannedRights{
		SendMessages: true, SendMedia: true, SendStickers: true, SendGifs: true,
		SendGames: true, SendInline: true, EmbedLinks: true, SendPolls: true,
		SendPhotos: true, SendVideos: true, SendRoundvideos: true, SendAudios: true,
		SendVoices: true, SendDocs: true, SendPlain: true, UntilDate: until,
	}
}

// mustChatID extracts the raw chat id from an InputPeerChat.
func mustChatID(peer tg.InputPeerClass) int64 {
	return peer.(*tg.InputPeerChat).ChatID
}

// formatDuration renders seconds with the largest units first.
func formatDuration(tl func(string, string) string, secs int) string {
	parts := []struct {
		n      int
		zh, en string
	}{
		{secs / 86400, "天", "d"},
		{secs % 86400 / 3600, "小时", "h"},
		{secs % 3600 / 60, "分钟", "m"},
		{secs % 60, "秒", "s"},
	}
	var b strings.Builder
	for _, u := range parts {
		if u.n == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(tl("", " "))
		}
		b.WriteString(fmt.Sprintf("%d", u.n))
		b.WriteString(tl(u.zh, u.en))
	}
	return b.String()
}

func helpText(tl func(string, string) string) string {
	return "🛡 **" + tl("成员管理", "Member management") + "**\n\n" +
		"> " + plugin.Code("ban") + " " + tl("封禁并清理消息（回复 / @用户名 / ID）", "ban and wipe messages (reply / @username / ID)") + "\n" +
		"> " + plugin.Code("unban") + " " + tl("解封", "lift a ban") + "\n" +
		"> " + plugin.Code("kick") + " " + tl("踢出（封禁后立刻解封）", "kick (ban, then instant unban)") + "\n" +
		"> " + plugin.Code("mute [时长]") + " " + tl("禁言，如 1h30m；不填永久", "mute, e.g. 1h30m; forever when omitted") + "\n" +
		"> " + plugin.Code("unmute") + " " + tl("解禁言", "lift a mute") + "\n" +
		"> " + plugin.Code("sb") + " / " + plugin.Code("unsb") + " " + tl("在所有有管理权的群批量封禁 / 解封", "ban / unban in every group you manage") + "\n" +
		"> " + plugin.Code("refresh") + " " + tl("刷新管理群缓存", "refresh the managed-group cache") + "\n\n" +
		tl("目标是管理员时要加 true 确认；数字 ID 不要求对方在群里。批量操作后台执行，结果 30 秒后自动删除",
			"Add true to confirm when the target is an admin; a numeric ID works even if they are not here. Batch runs in the background; its summary self-deletes in 30 seconds")
}

// errText turns an error into a bilingual line.
func errText(tl func(string, string) string, err error) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "Flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		switch e.Type {
		case "CHAT_ADMIN_REQUIRED", "RIGHT_FORBIDDEN", "CHAT_WRITE_FORBIDDEN":
			return tl("需要封禁成员的管理员权限", "The admin right to ban members is needed")
		case "USER_ADMIN_INVALID":
			return tl("不能对管理员执行此操作", "Admins cannot be targeted")
		case "USER_NOT_PARTICIPANT", "PARTICIPANT_ID_INVALID":
			return tl("对方已不在群里", "They are no longer in the group")
		case "CHANNEL_PRIVATE", "CHAT_FORBIDDEN", "CHANNEL_INVALID", "CHAT_ID_INVALID":
			return tl("无法访问该群组", "Cannot access this group")
		case "USER_ID_INVALID", "PEER_ID_INVALID", "INPUT_USER_DEACTIVATED":
			return tl("找不到这个用户", "Cannot find that user")
		case "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID":
			return tl("这个用户名不存在", "That username does not exist")
		}
		return plugin.Code(e.Type)
	}
	if err == errUnresolved {
		return tl("解析不到这个 ID，回复对方的消息再试", "Cannot resolve this ID; reply to one of their messages")
	}
	if err == errNoTarget {
		return tl("指定目标：回复消息，或用 @用户名 / 用户ID", "Give a target: reply, @username or user ID")
	}
	if err == errNotMember {
		return tl("对方已不在群里", "They are no longer in the group")
	}
	return plugin.Escape(err.Error())
}

// oops shows an error in the command message and deletes it after
// deleteDelay. The help text never routes through here.
func (p *BanPlugin) oops(ctx *plugin.CommandContext, text string) error {
	if err := ctx.Edit(text); err != nil {
		return err
	}
	p.autoDelete(ctx, deleteDelay)
	return nil
}

// done shows a success result and deletes it after delay.
func (p *BanPlugin) done(ctx *plugin.CommandContext, text string) error {
	if err := ctx.Edit(text); err != nil {
		return err
	}
	p.autoDelete(ctx, deleteDelay)
	return nil
}

// autoDelete removes the command message after d on the lifetime context.
func (p *BanPlugin) autoDelete(ctx *plugin.CommandContext, d time.Duration) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		select {
		case <-p.lifetime().Done():
			return
		case <-time.After(d):
		}
		dctx, cancel := context.WithTimeout(p.lifetime(), 15*time.Second)
		defer cancel()
		del := *ctx
		del.Ctx = dctx
		if err := del.Delete(); err != nil && ctx.Logger != nil {
			if !tgerr.Is(err, "MESSAGE_ID_INVALID", "MESSAGE_DELETE_FORBIDDEN") {
				ctx.Logger.Warn("ban: delete result failed", "error", err)
			}
		}
	}()
}

func (p *BanPlugin) warn(ctx *plugin.CommandContext, msg string, err error) {
	if p.log != nil {
		p.log.Warn("ban: "+msg, "error", err)
	}
}
