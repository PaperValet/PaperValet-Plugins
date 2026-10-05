package main

// favorite.go implements the classic TeleBox "sticker" plugin: reply to any
// sticker and save it to your pack (auto-created when full/absent), driving
// the @Stickers bot for packs that exist.

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

const botReplyWait = 1500 * time.Millisecond

// handleFavorite implements "sticker" and "sticker to <pack>" on a replied
// sticker message (dispatch has verified the reply).
func (p *StickerPlugin) handleFavorite(ctx *plugin.CommandContext, s sub) error {
	reply, err := ctx.ReplyMessage()
	if err != nil || reply == nil || !isStickerMsg(reply) {
		return p.handleStatus(ctx)
	}
	doc, _ := docOf(reply)
	attr, _ := stickerAttr(doc)
	emoji := strings.TrimSpace(attr.Alt)
	if emoji == "" {
		emoji = randomEmoji()
	}
	kind := classifySticker(doc)

	target := s.pack
	if target == "" {
		p.mu.Lock()
		target = p.cfg.DefaultPack
		p.mu.Unlock()
	}

	me, err := meUser(ctx)
	if err != nil {
		return tgErrText(ctx, err)
	}
	username := usernameOf(me)
	if username == "" && target == "" {
		return ctx.Edit("❌ " + ctx.Tlocal(
			"您没有用户名，无法自动创建贴纸包。请先用 sticker <包名> 设置默认贴纸包",
			"You have no username, packs cannot be auto-created. Set a default pack with sticker <pack> first"))
	}

	_ = ctx.Edit("🔄 " + ctx.Tlocal("正在查找贴纸包...", "Looking for the sticker pack..."))
	packName, create, err := p.findOrCreatePack(ctx, target, username, kind)
	if err != nil {
		return tgErrText(ctx, err)
	}

	if create {
		_ = ctx.Edit("➕ " + ctx.Tlocal("正在创建新贴纸包", "Creating a new sticker pack") + " " + plugin.Code(packName) + "...")
		if err := p.createStickerSet(ctx, me, packName, kind, doc, emoji); err != nil {
			return tgErrText(ctx, err)
		}
	} else {
		_ = ctx.Edit("📥 " + ctx.Tlocal("正在添加到贴纸包", "Adding to sticker pack") + " " + plugin.Code(packName) + "...")
		if err := p.addToStickerSet(ctx, reply, packName, emoji); err != nil {
			return tgErrText(ctx, err)
		}
	}

	if err := ctx.Edit("✅ " + ctx.Tlocal("收藏成功", "Favorited") + "\n\n" +
		plugin.Link(plugin.Escape(packName), "https://t.me/addstickers/"+packName)); err != nil {
		return err
	}
	sleepCtx(ctx.Context(), 5*time.Second) // the source deletes the success card after 5s
	_ = ctx.Delete()
	return nil
}

// lookupSet fetches a sticker set by short name; ok=false means it does not exist.
func lookupSet(ctx *plugin.CommandContext, name string) (*tg.MessagesStickerSet, bool, error) {
	res, err := ctx.API.MessagesGetStickerSet(ctx.Context(), &tg.MessagesGetStickerSetRequest{
		Stickerset: &tg.InputStickerSetShortName{ShortName: name},
		Hash:       0,
	})
	if err != nil {
		if tgerr.Is(err, "STICKERSET_INVALID") {
			return nil, false, nil
		}
		return nil, false, err
	}
	set, ok := res.(*tg.MessagesStickerSet)
	if !ok {
		return nil, true, nil // StickerSetNotModified: treat as existing
	}
	return set, true, nil
}

// findOrCreatePack mirrors the source: verify a named pack (must have room),
// else auto-pick username_<kind>_<n> with room, else create a fresh one.
func (p *StickerPlugin) findOrCreatePack(ctx *plugin.CommandContext, packName, username string, kind stickerKind) (string, bool, error) {
	if packName != "" {
		set, exists, err := lookupSet(ctx, packName)
		if err != nil {
			return "", false, err
		}
		if exists {
			if set != nil && set.Set.Count >= maxPackCap {
				return "", false, fmt.Errorf("pack %s is full (%d/%d)", packName, set.Set.Count, maxPackCap)
			}
			return packName, false, nil
		}
		return packName, true, nil
	}
	suffix := kindSuffix(kind)
	for i := 1; i <= maxPackTry; i++ {
		name := fmt.Sprintf("%s%s_%d", username, suffix, i)
		set, exists, err := lookupSet(ctx, name)
		if err != nil {
			return "", false, err
		}
		if !exists {
			return name, true, nil
		}
		if set != nil && set.Set.Count < maxPackCap {
			return name, false, nil
		}
	}
	return "", false, errors.New("no free auto pack found (tried 50)")
}

// createStickerSet creates a pack with the sticker as its first item.
func (p *StickerPlugin) createStickerSet(ctx *plugin.CommandContext, me *tg.User, packName string, kind stickerKind, doc *tg.Document, emoji string) error {
	owner := "@" + usernameOf(me)
	if owner == "@" {
		owner = me.FirstName
	}
	if owner == "" {
		owner = ctx.Tlocal("我的", "My")
	}
	title := fmt.Sprintf("%s %s", owner, ctx.Tlocal("的收藏", "favorites"))
	if l := kindLabel(kind, ctx.Tlocal); l != "" {
		title += " (" + l + ")"
	}
	_, err := ctx.API.StickersCreateStickerSet(ctx.Context(), &tg.StickersCreateStickerSetRequest{
		UserID:    &tg.InputUserSelf{},
		Title:     title,
		ShortName: packName,
		Stickers: []tg.InputStickerSetItem{
			{Document: inputDocOf(doc), Emoji: emoji},
		},
	})
	if err != nil {
		return friendlyCreateErr(ctx, err)
	}
	return nil
}

// friendlyCreateErr maps known create errors to bilingual messages.
func friendlyCreateErr(ctx *plugin.CommandContext, err error) error {
	switch {
	case tgerr.Is(err, "STICKER_VIDEO_LONG"):
		return errors.New(ctx.Tlocal("视频贴纸时长不能超过3秒", "Video stickers must be at most 3 seconds long"))
	case tgerr.Is(err, "STICKER_PNG_DIMENSIONS"):
		return errors.New(ctx.Tlocal("静态贴纸尺寸必须为 512xN 或 Nx512（一边为512px）", "Static stickers must be 512xN or Nx512 (one side 512px)"))
	case tgerr.Is(err, "STICKERSET_INVALID"):
		return errors.New(ctx.Tlocal("贴纸包名称无效或已被占用（字母、数字、下划线，字母开头）", "Pack name invalid or taken (letters, digits, underscores, starts with a letter)"))
	case tgerr.Is(err, "PEER_ID_INVALID"):
		return errors.New(ctx.Tlocal("无法与 @Stickers 机器人通信，请先私聊它一次", "Cannot talk to the @Stickers bot; message it once first"))
	}
	return err
}

// addToStickerSet drives the @Stickers bot to append to an existing pack,
// mirroring the source's /addsticker conversation.
func (p *StickerPlugin) addToStickerSet(ctx *plugin.CommandContext, stickerMsg *tg.Message, packName, emoji string) error {
	bot, err := resolveStickersBot(ctx)
	if err != nil {
		return err
	}
	c := ctx.Context()
	fail := func(cause error) error {
		_ = sendMessage(ctx, bot, "/cancel")
		return cause
	}
	// Not the owner → the bot cannot help.
	if err := sendMessage(ctx, bot, "/addsticker"); err != nil {
		return err
	}
	sleepCtx(c, botReplyWait)
	if err := sendMessage(ctx, bot, packName); err != nil {
		return fail(err)
	}
	sleepCtx(c, botReplyWait)
	text, err := latestBotText(ctx, bot)
	if err != nil {
		return fail(err)
	}
	if strings.Contains(strings.ToLower(text), "invalid set") {
		return fail(errors.New(ctx.Tlocal(
			fmt.Sprintf("贴纸包 %s 无效或您不是该包的所有者", packName),
			fmt.Sprintf("Pack %s is invalid or you are not its owner", packName))))
	}
	// Forward the sticker message as the sticker to add.
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return fail(err)
	}
	if _, err := ctx.API.MessagesForwardMessages(c, &tg.MessagesForwardMessagesRequest{
		FromPeer: peer,
		ID:       []int{stickerMsg.ID},
		RandomID: []int64{timeNowUnixNano()},
		ToPeer:   bot,
	}); err != nil {
		return fail(err)
	}
	sleepCtx(c, 2500*time.Millisecond)
	text, err = latestBotText(ctx, bot)
	if err != nil {
		return fail(err)
	}
	low := strings.ToLower(text)
	switch {
	case strings.Contains(low, "sorry, the video is too long"), strings.Contains(low, "duration of the video must be 3 seconds or less"):
		return fail(errors.New(ctx.Tlocal("视频贴纸时长不能超过3秒", "Video stickers must be at most 3 seconds long")))
	case strings.Contains(low, "the sticker's dimensions should be"), strings.Contains(low, "dimensions should be one of"):
		return fail(errors.New(ctx.Tlocal("静态贴纸尺寸必须为 512xN 或 Nx512", "Static stickers must be 512xN or Nx512")))
	case !strings.Contains(low, "thanks! now send me an emoji"):
		return fail(errors.New(ctx.Tlocal(
			"添加贴纸时机器人返回未知信息", "The @Stickers bot replied unexpectedly") + ": " + plugin.Escape(truncate(text, 200))))
	}
	if err := sendMessage(ctx, bot, emoji); err != nil {
		return fail(err)
	}
	sleepCtx(c, botReplyWait)
	if err := sendMessage(ctx, bot, "/done"); err != nil {
		return fail(err)
	}
	return nil
}

// latestBotText reads the bot's most recent incoming message in the private
// chat with the bot (our own outgoing messages are skipped).
func latestBotText(ctx *plugin.CommandContext, bot tg.InputPeerClass) (string, error) {
	hist, err := ctx.API.MessagesGetHistory(ctx.Context(), &tg.MessagesGetHistoryRequest{
		Peer:  bot,
		Limit: 5,
	})
	if err != nil {
		return "", err
	}
	mod, ok := hist.AsModified()
	if !ok {
		return "", nil
	}
	for _, m := range mod.GetMessages() {
		msg, ok := m.(*tg.Message)
		if !ok || msg.Out {
			continue
		}
		return msg.Message, nil
	}
	return "", nil
}

func sleepCtx(c context.Context, d time.Duration) {
	if c == nil {
		time.Sleep(d)
		return
	}
	select {
	case <-c.Done():
	case <-time.After(d):
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// handleConfigPack implements "sticker <pack>" and "sticker cancel".
func (p *StickerPlugin) handleConfigPack(ctx *plugin.CommandContext, s sub) error {
	if s.kind == subCancel {
		p.mu.Lock()
		p.cfg.DefaultPack = ""
		cfg := p.cfg
		p.mu.Unlock()
		if err := saveConfig(p.cfgPath, cfg); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("保存失败", "Save failed") + ": " + plugin.Escape(err.Error()))
		}
		return ctx.Edit("✅ " + ctx.Tlocal("已取消默认贴纸包", "Default sticker pack cleared"))
	}
	pack := s.pack
	if !validPackName(pack) {
		return ctx.Edit("❌ " + ctx.Tlocal(
			"贴纸包名只能包含字母、数字和下划线，且必须以字母开头",
			"Pack names may only contain letters, digits and underscores, starting with a letter"))
	}
	_ = ctx.Edit("🔄 " + ctx.Tlocal("正在验证贴纸包", "Validating sticker pack") + " " + plugin.Code(pack) + "...")
	if _, exists, err := lookupSet(ctx, pack); err != nil {
		return tgErrText(ctx, err)
	} else if !exists {
		return ctx.Edit("❌ " + ctx.Tlocal(
			"无法访问贴纸包，请确保它存在且您有权访问",
			"Cannot access the sticker pack; make sure it exists and you can access it"))
	}
	p.mu.Lock()
	p.cfg.DefaultPack = pack
	cfg := p.cfg
	p.mu.Unlock()
	if err := saveConfig(p.cfgPath, cfg); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("保存失败", "Save failed") + ": " + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + ctx.Tlocal("默认贴纸包已设置为", "Default sticker pack set to") + " " + plugin.Code(pack))
}

// handleStatus implements "sticker" with no sticker reply and "sticker status".
func (p *StickerPlugin) handleStatus(ctx *plugin.CommandContext) error {
	p.mu.Lock()
	pack := p.cfg.DefaultPack
	p.mu.Unlock()
	var b strings.Builder
	b.WriteString("🧩 **" + ctx.Tlocal("贴纸收藏设置", "Sticker favorite settings") + "**\n\n")
	if pack != "" {
		b.WriteString(ctx.Tlocal("当前默认贴纸包", "Current default pack") + ": " + plugin.Link(plugin.Escape(pack), "https://t.me/addstickers/"+pack) + "\n")
	} else if me, err := meUser(ctx); err == nil {
		if u := usernameOf(me); u != "" {
			b.WriteString(ctx.Tlocal("未设置默认贴纸包，将自动使用", "No default pack set; auto packs") + " " + plugin.Code(u+"_...") + "\n")
		} else {
			b.WriteString("❌ " + ctx.Tlocal("未设置默认贴纸包，且您没有用户名，收藏前必须先设置一个默认包", "No default pack and no username: set a default pack before favoriting") + "\n")
		}
	} else {
		b.WriteString(ctx.Tlocal("未设置默认贴纸包", "No default pack set") + "\n")
	}
	b.WriteString("\n" + ctx.Tlocal("转换设置（表情/边长/质量/背景）在机器人面板里调整", "Conversion settings (emoji/size/quality/background) live in the bot panel"))
	return ctx.Edit(b.String())
}
