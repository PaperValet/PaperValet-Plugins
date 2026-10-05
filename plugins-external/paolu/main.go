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
	pageSize      = 100
	progressEvery = 3 * time.Second
	summaryTTL    = 10 * time.Second
	maxFloodWait  = time.Minute
	rpcTimeout    = 30 * time.Second // per-request timeout so Stop cannot hang on one stuck RPC
)

var Metadata = &plugin.PluginMetadata{
	Name:        "paolu",
	Description: "一键跑路：禁言全员并清空群消息",
	DescEN:      "Mute everyone and wipe the group history",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type PaoluPlugin struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	host    plugin.Host
	running map[int64]bool
}

func New() *PaoluPlugin { return &PaoluPlugin{running: map[int64]bool{}} }

func (p *PaoluPlugin) Name() string        { return "paolu" }
func (p *PaoluPlugin) Description() string { return Metadata.Description }
func (p *PaoluPlugin) DescEN() string      { return Metadata.DescEN }

func (p *PaoluPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "paolu",
		Description: "禁言全员并删除群里所有消息，需要加 confirm 才执行",
		DescEN:      "Mute everyone and delete every message in the group; needs confirm to run",
		Usage:       "paolu [confirm]",
		UsageEN:     "paolu [confirm]",
		Plugin:      p.Name(),
		Category:    "group",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *PaoluPlugin) Start(context.Context) error {
	p.lifetime()
	return nil
}

func (p *PaoluPlugin) Stop(context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

// lifetime returns the plugin-scoped context that background wipes run on.
func (p *PaoluPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	return p.ctx
}

// claim marks chatID busy; it reports false when a wipe already runs there.
func (p *PaoluPlugin) claim(chatID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running[chatID] {
		return false
	}
	p.running[chatID] = true
	return true
}

func (p *PaoluPlugin) release(chatID int64) {
	p.mu.Lock()
	delete(p.running, chatID)
	p.mu.Unlock()
}

func (p *PaoluPlugin) handle(ctx *plugin.CommandContext) error {
	if strings.ToLower(ctx.GetArg(0)) != "confirm" {
		return ctx.Edit(warning(ctx.Tlocal))
	}
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()))
	}
	switch peer.(type) {
	case *tg.InputPeerChannel, *tg.InputPeerChat:
	default:
		return ctx.Edit("❌ " + ctx.Tlocal("只能在群组里使用", "Use this in a group"))
	}
	g, err := loadGroup(ctx.Context(), ctx.API, peer)
	if err != nil {
		return ctx.Edit(fail(ctx.Tlocal, err))
	}
	if g == nil {
		return ctx.Edit("❌ " + ctx.Tlocal("只能在群组里使用", "Use this in a group"))
	}
	if !g.canWipe() {
		return ctx.Edit("❌ " + ctx.Tlocal(
			"需要群主身份，或同时拥有封禁成员和删除消息的管理员权限",
			"You need to own the group, or be an admin who can both ban users and delete messages"))
	}
	if !p.claim(ctx.Message.ChatID) {
		return ctx.Edit("⏳ " + ctx.Tlocal("这个群已经在跑路中了", "A wipe is already running in this group"))
	}

	life := p.lifetime()
	run := *ctx
	run.Ctx = life
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.release(ctx.Message.ChatID)
		p.wipe(&run, peer, g)
	}()
	return nil
}

// group is the subset of chat state paolu needs.
type group struct {
	Title   string
	Creator bool
	Ban     bool
	Delete  bool
}

func (g *group) canWipe() bool { return g.Creator || (g.Ban && g.Delete) }

// loadGroup fetches the chat behind peer. It returns nil for broadcast
// channels and anything that is not a group.
func loadGroup(ctx context.Context, api *tg.Client, peer tg.InputPeerClass) (*group, error) {
	var res tg.MessagesChatsClass
	var err error
	switch pr := peer.(type) {
	case *tg.InputPeerChannel:
		res, err = api.ChannelsGetChannels(ctx, []tg.InputChannelClass{
			&tg.InputChannel{ChannelID: pr.ChannelID, AccessHash: pr.AccessHash},
		})
	case *tg.InputPeerChat:
		res, err = api.MessagesGetChats(ctx, []int64{pr.ChatID})
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, c := range res.GetChats() {
		if g := groupOf(c); g != nil {
			return g, nil
		}
	}
	return nil, nil
}

func groupOf(c tg.ChatClass) *group {
	switch v := c.(type) {
	case *tg.Channel:
		if v.Broadcast || v.Left {
			return nil
		}
		g := &group{Title: v.Title, Creator: v.Creator}
		if r, ok := v.GetAdminRights(); ok {
			g.Ban, g.Delete = r.BanUsers, r.DeleteMessages
		}
		return g
	case *tg.Chat:
		if v.Deactivated || v.Left {
			return nil
		}
		g := &group{Title: v.Title, Creator: v.Creator}
		if r, ok := v.GetAdminRights(); ok {
			g.Ban, g.Delete = r.BanUsers, r.DeleteMessages
		}
		return g
	}
	return nil
}

// muteRights forbids everything a member could do except read.
func muteRights() tg.ChatBannedRights {
	return tg.ChatBannedRights{
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
	}
}

// result is what one wipe achieved.
type result struct {
	Muted   bool
	MuteErr error
	Deleted int
	Failed  int
	Err     error
}

func (p *PaoluPlugin) wipe(ctx *plugin.CommandContext, peer tg.InputPeerClass, g *group) {
	var r result
	tl := ctx.Tlocal
	_ = ctx.Edit("🚨 **" + tl("一键跑路", "Wipe") + "**\n\n⏳ " + tl("正在禁言全员…", "Muting everyone…"))

	r.MuteErr = retry(ctx.Context(), func() error {
		return callErr(ctx.Context(), func(c context.Context) error {
			_, err := ctx.API.MessagesEditChatDefaultBannedRights(c, &tg.MessagesEditChatDefaultBannedRightsRequest{
				Peer: peer, BannedRights: muteRights(),
			})
			return err
		})
	})
	if tgerr.Is(r.MuteErr, "CHAT_NOT_MODIFIED") {
		r.MuteErr = nil
	}
	r.Muted = r.MuteErr == nil

	r.Err = p.deleteHistory(ctx, peer, &r)
	if ctx.Context().Err() != nil {
		return // plugin stopped; leave the progress message as is
	}

	// Deliver the result before destroying the places it could be shown:
	// try a fresh summary message first; if that fails fall back to
	// editing the command message (still alive), and only then delete the
	// command message.
	sum := summary(tl, g.Title, r)
	msgID, sendErr := call(ctx.Context(), func(c context.Context) (int, error) {
		return p.host.Send(c, ctx.Message.ChatID, sum, 0)
	})
	if sendErr != nil {
		p.warn(ctx, "send summary failed", sendErr)
		if err := ctx.Edit(sum); err != nil {
			p.warn(ctx, "edit command message with summary failed", err)
		}
	}
	if err := ctx.Delete(); err != nil {
		p.warn(ctx, "delete command message failed", err)
	}
	if sendErr != nil {
		return
	}
	select {
	case <-ctx.Context().Done():
		return
	case <-time.After(summaryTTL):
	}
	dctx, cancel := context.WithTimeout(p.lifetime(), 15*time.Second)
	defer cancel()
	if err := plugin.DeleteMessages(dctx, ctx.API, peer, msgID); err != nil {
		p.warn(ctx, "delete summary failed", err)
	}
}

// call bounds one Telegram RPC with a per-request timeout so a stuck call
// cannot stretch the wipe (and with it Stop's wg.Wait) indefinitely.
func call[T any](parent context.Context, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(parent, rpcTimeout)
	defer cancel()
	return fn(ctx)
}

// callErr is call for requests whose result is ignored.
func callErr(parent context.Context, fn func(context.Context) error) error {
	_, err := call(parent, func(c context.Context) (struct{}, error) {
		return struct{}{}, fn(c)
	})
	return err
}

// deleteHistory walks the history from newest to oldest and deletes every
// page, skipping the command message so progress can still be shown.
func (p *PaoluPlugin) deleteHistory(ctx *plugin.CommandContext, peer tg.InputPeerClass, r *result) error {
	tl := ctx.Tlocal
	cmdID := ctx.Message.Message.ID
	last := time.Time{}
	offset := 0
	for {
		if err := ctx.Context().Err(); err != nil {
			return err
		}
		var page tg.MessagesMessagesClass
		err := retry(ctx.Context(), func() error {
			var err error
			page, err = call(ctx.Context(), func(c context.Context) (tg.MessagesMessagesClass, error) {
				return ctx.API.MessagesGetHistory(c, &tg.MessagesGetHistoryRequest{
					Peer: peer, OffsetID: offset, Limit: pageSize,
				})
			})
			return err
		})
		if err != nil {
			return err
		}
		mod, ok := page.AsModified()
		if !ok {
			return nil
		}
		ids, next := pageIDs(mod.GetMessages(), cmdID)
		if next == 0 {
			return nil
		}
		offset = next
		if len(ids) > 0 {
			ok, failed, err := p.deleteBatch(ctx, peer, ids)
			r.Deleted += ok
			r.Failed += failed
			if err != nil {
				return err
			}
		}
		if time.Since(last) >= progressEvery {
			last = time.Now()
			_ = ctx.Edit(progress(tl, r))
		}
	}
}

// deleteBatch deletes ids at once; if Telegram refuses the batch (an
// undeletable service message poisons the whole call) it retries one by one.
func (p *PaoluPlugin) deleteBatch(ctx *plugin.CommandContext, peer tg.InputPeerClass, ids []int) (ok, failed int, err error) {
	err = retry(ctx.Context(), func() error {
		return callErr(ctx.Context(), func(c context.Context) error {
			return plugin.DeleteMessages(c, ctx.API, peer, ids...)
		})
	})
	if err == nil {
		return len(ids), 0, nil
	}
	if fatal(err) || ctx.Context().Err() != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		err := retry(ctx.Context(), func() error {
			return callErr(ctx.Context(), func(c context.Context) error {
				return plugin.DeleteMessages(c, ctx.API, peer, id)
			})
		})
		switch {
		case err == nil:
			ok++
		case fatal(err) || ctx.Context().Err() != nil:
			return ok, failed, err
		default:
			failed++
		}
	}
	return ok, failed, nil
}

// pageIDs returns the deletable ids of one history page and the offset of
// the next page (0 when the page is empty).
func pageIDs(msgs []tg.MessageClass, skip int) (ids []int, next int) {
	for _, m := range msgs {
		id := m.GetID()
		if next == 0 || id < next {
			next = id
		}
		if id == skip {
			continue
		}
		if _, empty := m.(*tg.MessageEmpty); empty {
			continue
		}
		ids = append(ids, id)
	}
	return ids, next
}

// retry runs fn, sleeping through short flood waits.
func retry(ctx context.Context, fn func() error) error {
	for {
		err := fn()
		d, ok := tgerr.AsFloodWait(err)
		if !ok || d > maxFloodWait {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d + time.Second):
		}
	}
}

// fatal reports errors that mean we lost the rights or the chat, so going on
// would only fail again.
func fatal(err error) bool {
	if _, ok := tgerr.AsFloodWait(err); ok {
		return true
	}
	return tgerr.Is(err, "CHAT_ADMIN_REQUIRED", "CHANNEL_PRIVATE", "CHAT_FORBIDDEN", "PEER_ID_INVALID", "CHANNEL_INVALID")
}

func (p *PaoluPlugin) warn(ctx *plugin.CommandContext, msg string, err error) {
	if ctx.Logger != nil {
		ctx.Logger.Warn("paolu: "+msg, "error", err)
	}
}

func warning(tl func(string, string) string) string {
	return tl(
		"⚠️ **一键跑路**\n\n> 禁言全体成员\n> 删除群里的所有消息\n\n此操作不可撤销。确定要跑就发 "+plugin.Code("paolu confirm"),
		"⚠️ **Wipe the group**\n\n> Mute every member\n> Delete every message in the group\n\nThis cannot be undone. Send "+plugin.Code("paolu confirm")+" to go ahead")
}

func progress(tl func(string, string) string, r *result) string {
	s := "🚨 **" + tl("一键跑路", "Wipe") + "**\n\n> " + muteLine(tl, *r) + "\n> " +
		fmt.Sprintf(tl("已删除 %d 条消息", "Deleted %d messages"), r.Deleted)
	if r.Failed > 0 {
		s += "\n> " + fmt.Sprintf(tl("%d 条删不掉", "%d could not be deleted"), r.Failed)
	}
	return s + "\n\n⏳ " + tl("正在删除…", "Deleting…")
}

func muteLine(tl func(string, string) string, r result) string {
	if r.Muted {
		return "🔇 " + tl("已禁言全员", "Everyone is muted")
	}
	return "⚠️ " + tl("禁言失败", "Mute failed") + " · " + errText(tl, r.MuteErr)
}

func summary(tl func(string, string) string, title string, r result) string {
	head := "✅ **" + tl("跑路完成", "Wipe finished") + "**"
	if r.Err != nil || !r.Muted {
		head = "⚠️ **" + tl("跑路未完全成功", "Wipe partly failed") + "**"
	}
	s := head
	if title != "" {
		s += "\n" + plugin.Escape(title)
	}
	s += "\n\n> " + muteLine(tl, r) + "\n> " +
		fmt.Sprintf(tl("🗑 已删除 %d 条消息", "🗑 Deleted %d messages"), r.Deleted)
	if r.Failed > 0 {
		s += "\n> " + fmt.Sprintf(tl("%d 条删不掉", "%d could not be deleted"), r.Failed)
	}
	if r.Err != nil {
		s += "\n> " + tl("删除中断", "Stopped early") + " · " + errText(tl, r.Err)
	}
	return s + "\n\n" + tl("本消息 10 秒后自动删除", "This message deletes itself in 10 seconds")
}

func errText(tl func(string, string) string, err error) string {
	if err == nil {
		return ""
	}
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		switch e.Type {
		case "CHAT_ADMIN_REQUIRED":
			return tl("管理员权限不足", "missing admin rights")
		case "CHANNEL_PRIVATE", "CHAT_FORBIDDEN":
			return tl("无法访问该群组", "cannot access this group")
		}
		return plugin.Code(e.Type)
	}
	return plugin.Escape(err.Error())
}

func fail(tl func(string, string) string, err error) string {
	return "❌ **" + tl("跑路失败", "Wipe failed") + "**\n\n" + errText(tl, err)
}
