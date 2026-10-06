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

const (
	pageSize  = 200
	hardLimit = 10000 // Telegram stops listing members past this offset
	// pageGap paces getParticipants so a large scan does not immediately
	// trip a FLOOD_WAIT.
	pageGap = 150 * time.Millisecond
	// maxFloodRetries bounds how many sub-minute FLOOD_WAITs one scan
	// waits out before giving up (a longer wait is a hard stop anyway).
	maxFloodRetries = 3
)

var Metadata = &plugin.PluginMetadata{
	Name:        "premium",
	Description: "统计群里的 Premium 会员",
	DescEN:      "Count Premium members in a group",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type PremiumPlugin struct{}

func New() *PremiumPlugin { return &PremiumPlugin{} }

func (p *PremiumPlugin) Name() string        { return "premium" }
func (p *PremiumPlugin) Description() string { return Metadata.Description }
func (p *PremiumPlugin) DescEN() string      { return Metadata.DescEN }

func (p *PremiumPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "premium",
		Description: "统计当前群组的 Premium 会员人数和占比",
		DescEN:      "Count Telegram Premium members and their share in this group",
		Usage:       "premium [force]",
		UsageEN:     "premium [force]",
		Plugin:      p.Name(),
		Category:    "group",
		// A full scan of a large group is minutes of API calls; keep it
		// owner-initiated and not spam-triggerable in groups.
		OwnerOnly: true,
		RateLimit: 60,
		Handler:   p.handle,
	})
}

func (p *PremiumPlugin) Start(context.Context) error { return nil }
func (p *PremiumPlugin) Stop(context.Context) error  { return nil }

// tally is the result of one scan. seen dedupes members, since the recent
// list can shift between pages while a large group is scanned. missing
// counts participants whose user object was not in the page, so the report
// can say the numbers are slightly low instead of lying silently.
type tally struct {
	Premium, Users, Bots, Deleted, Seen, Missing int
	seen                                         map[int64]bool
}

func (t *tally) add(u *tg.User) {
	if t.seen == nil {
		t.seen = map[int64]bool{}
	}
	if t.seen[u.ID] {
		return
	}
	t.seen[u.ID] = true
	t.Seen++
	switch {
	case u.Bot:
		t.Bots++
	case u.Deleted:
		t.Deleted++
	default:
		t.Users++
		if u.Premium {
			t.Premium++
		}
	}
}

func (t tally) percent() string {
	if t.Users == 0 {
		return "0.00"
	}
	return fmt.Sprintf("%.2f", float64(t.Premium)*100/float64(t.Users))
}

func (p *PremiumPlugin) help(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🎁 **premium**\n\n"+plugin.Code("premium")+" 统计本群 Premium 会员\n"+plugin.Code("premium force")+" 超过 1 万人时强制统计（只能扫前 1 万人）\n\n机器人和死号自动排除",
		"🎁 **premium**\n\n"+plugin.Code("premium")+" count Premium members in this group\n"+plugin.Code("premium force")+" force it on groups over 10k members (only the first 10k are scanned)\n\nBots and deleted accounts are excluded")
}

func (p *PremiumPlugin) handle(ctx *plugin.CommandContext) error {
	arg := strings.ToLower(ctx.GetArg(0))
	if arg != "" && arg != "force" {
		return ctx.Edit(p.help(ctx))
	}
	force := arg == "force"
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()))
	}
	_ = ctx.Edit("⏳ " + ctx.Tlocal("正在统计…", "Counting…"))

	var t tally
	total := 0
	switch pr := peer.(type) {
	case *tg.InputPeerChannel:
		ch := &tg.InputChannel{ChannelID: pr.ChannelID, AccessHash: pr.AccessHash}
		full, err := ctx.API.ChannelsGetFullChannel(ctx.Context(), ch)
		if err != nil {
			return ctx.Edit(fail(ctx, err))
		}
		if cf, ok := full.FullChat.(*tg.ChannelFull); ok {
			total = cf.ParticipantsCount
		}
		if total >= hardLimit && !force {
			return ctx.Edit(ctx.Tlocal(
				"😵 **人数过多**\n\n太…太多人了…会坏掉的…\n执意要跑就发 "+plugin.Code("premium force"),
				"😵 **Too many members**\n\nThat many people might break me…\nSend "+plugin.Code("premium force")+" to run anyway"))
		}
		if err := scanChannel(ctx, ch, &t); err != nil {
			return ctx.Edit(fail(ctx, err))
		}
	case *tg.InputPeerChat:
		full, err := ctx.API.MessagesGetFullChat(ctx.Context(), pr.ChatID)
		if err != nil {
			return ctx.Edit(fail(ctx, err))
		}
		cf, ok := full.FullChat.(*tg.ChatFull)
		if !ok {
			return ctx.Edit("❌ " + ctx.Tlocal("读取群信息失败", "Could not read the group"))
		}
		cps, ok := cf.Participants.(*tg.ChatParticipants)
		if !ok {
			return ctx.Edit("❌ " + ctx.Tlocal("看不到成员列表", "The member list is hidden"))
		}
		users := usersByID(full.Users)
		for _, cp := range cps.Participants {
			if u, ok := users[chatParticipantID(cp)]; ok {
				t.add(u)
			}
		}
		total = t.Seen
	default:
		return ctx.Edit("❌ " + ctx.Tlocal("只能在群组里使用", "Use this in a group"))
	}
	return ctx.Edit(report(ctx.Tlocal, t, total >= hardLimit))
}

func scanChannel(ctx *plugin.CommandContext, ch *tg.InputChannel, t *tally) error {
	last := time.Now()
	floods := 0
	for offset := 0; offset < hardLimit; {
		res, err := ctx.API.ChannelsGetParticipants(ctx.Context(), &tg.ChannelsGetParticipantsRequest{
			Channel: ch, Filter: &tg.ChannelParticipantsRecent{}, Offset: offset, Limit: pageSize,
		})
		if err != nil {
			if d, ok := tgerr.AsFloodWait(err); ok && d < time.Minute && floods < maxFloodRetries {
				floods++
				select {
				case <-ctx.Context().Done():
					return ctx.Context().Err()
				case <-time.After(d + time.Second):
				}
				continue
			}
			return err
		}
		floods = 0
		ps, ok := res.(*tg.ChannelsChannelParticipants)
		if !ok {
			return nil
		}
		// Users also carries inviters and promoters who may have left, so
		// count only ids that appear as participants.
		users := usersByID(ps.Users)
		for _, part := range ps.Participants {
			id := channelParticipantID(part)
			if id == 0 {
				continue
			}
			if u, ok := users[id]; ok {
				t.add(u)
			} else if !t.seen[id] {
				t.Missing++
			}
		}
		n := len(ps.Participants)
		offset += n
		if n == 0 || offset >= ps.Count {
			return nil
		}
		select {
		case <-ctx.Context().Done():
			return ctx.Context().Err()
		case <-time.After(pageGap):
		}
		if time.Since(last) > 3*time.Second {
			last = time.Now()
			_ = ctx.Edit(fmt.Sprintf(ctx.Tlocal("⏳ 正在统计… 已处理 %d 人", "⏳ Counting… %d members done"), t.Seen))
		}
	}
	return nil
}

func usersByID(list []tg.UserClass) map[int64]*tg.User {
	m := make(map[int64]*tg.User, len(list))
	for _, u := range list {
		if v, ok := u.(*tg.User); ok {
			m[v.ID] = v
		}
	}
	return m
}

func chatParticipantID(p tg.ChatParticipantClass) int64 {
	switch v := p.(type) {
	case *tg.ChatParticipant:
		return v.UserID
	case *tg.ChatParticipantAdmin:
		return v.UserID
	case *tg.ChatParticipantCreator:
		return v.UserID
	}
	return 0
}

func channelParticipantID(p tg.ChannelParticipantClass) int64 {
	switch v := p.(type) {
	case *tg.ChannelParticipant:
		return v.UserID
	case *tg.ChannelParticipantSelf:
		return v.UserID
	case *tg.ChannelParticipantAdmin:
		return v.UserID
	case *tg.ChannelParticipantCreator:
		return v.UserID
	}
	return 0 // banned / left members are not counted
}

func report(tl func(string, string) string, t tally, truncated bool) string {
	s := fmt.Sprintf(tl(
		"🎁 **分遗产咯**\n\n> 大会员 **%d** / 总用户 **%d**\n> 占比 **%s%%**\n> 已过滤 %d 个 Bot、%d 个死号\n> 本次处理 %d 人",
		"🎁 **Premium census**\n\n> Premium **%d** / users **%d**\n> Share **%s%%**\n> Skipped %d bots, %d deleted accounts\n> Scanned %d members"),
		t.Premium, t.Users, t.percent(), t.Bots, t.Deleted, t.Seen)
	if truncated {
		s += "\n\n⚠️ " + tl("Telegram 只允许遍历前 1 万人，数据可能不完整", "Telegram only lists the first 10k members, so this may be incomplete")
	}
	if t.Missing > 0 {
		s += "\n\n⚠️ " + fmt.Sprintf(tl("另有 %d 个成员信息缺失，未计入统计", "%d members could not be read and are not counted"), t.Missing)
	}
	return s
}

func fail(ctx *plugin.CommandContext, err error) string {
	out := "❌ **" + ctx.Tlocal("统计失败", "Count failed") + "**\n\n"
	if d, ok := tgerr.AsFloodWait(err); ok {
		return out + fmt.Sprintf(ctx.Tlocal("请求过于频繁，请 %d 秒后重试", "Flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		switch e.Type {
		case "CHAT_ADMIN_REQUIRED":
			return out + ctx.Tlocal("需要管理员权限才能查看成员列表", "Admin rights are needed to list members")
		case "CHANNEL_PRIVATE":
			return out + ctx.Tlocal("无法访问该群组", "Cannot access this group")
		}
		return out + plugin.Code(e.Type)
	}
	return out + plugin.Escape(err.Error())
}
