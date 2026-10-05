package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	stickerPage      = 100
	stickerDefault   = 2000
	stickerMax       = 2000
	stickerThrottle  = 1200 * time.Millisecond
	stickerResultTTL = 10 * time.Second
)

// handleSticker clears recent sticker messages in the current group, up to
// count (default and cap 2000). From clear_sticker.
func (p *CleanPlugin) handleSticker(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	if len(args) > 0 && strings.EqualFold(args[0], "help") {
		return ctx.Edit(helpSticker(tl))
	}
	max := stickerDefault
	if len(args) > 0 {
		n, ok := atoi(args[0])
		if !ok || n < 1 {
			return p.deny(ctx, "❌ "+tl("数量必须是正整数，例如：", "Count must be a positive integer, e.g. ")+plugin.Code("clean sticker 100"))
		}
		if n > stickerMax {
			n = stickerMax
		}
		max = n
	}
	ok, err := p.run(ctx, func(j *job) { p.clearStickers(j, max) })
	if err != nil {
		return p.deny(ctx, "❌ "+errText(tl, err))
	}
	if !ok {
		return p.deny(ctx, "⏳ "+tl("已有清理任务在跑", "A clean job is already running"))
	}
	return nil
}

// clearStickers is the body of clean sticker: walk the history from newest
// to oldest and delete every sticker message, page by page.
func (p *CleanPlugin) clearStickers(j *job, max int) {
	tl := j.Tlocal
	_ = j.Edit("🧹 **" + tl("清理贴纸消息", "Clearing stickers") + "**\n\n🔍 " +
		fmt.Sprintf(tl("正在搜索贴纸消息，目标 %d 条…", "Searching for stickers, target %d…"), max))

	deleted, scanned, pages := 0, 0, 0
	offsetID := 0
	for deleted < max {
		if err := j.Context().Err(); err != nil {
			return
		}
		var page tg.MessagesMessagesClass
		err := retry(j.Context(), func() error {
			r, e := j.API.MessagesGetHistory(j.Context(), &tg.MessagesGetHistoryRequest{
				Peer:     j.peer,
				OffsetID: offsetID,
				Limit:    stickerPage,
			})
			page = r
			return e
		})
		if err != nil {
			p.finishErr(j.CommandContext, err)
			return
		}
		mod, ok := page.AsModified()
		if !ok {
			return
		}
		msgs := mod.GetMessages()
		if len(msgs) == 0 {
			return
		}
		next := 0
		var ids []int
		for _, mc := range msgs {
			id := mc.GetID()
			if id < next || next == 0 {
				next = id
			}
			if m, ok := mc.(*tg.Message); ok && isSticker(m) && id != j.Message.Message.ID {
				ids = append(ids, id)
			}
		}
		if len(ids) > max-deleted {
			ids = ids[:max-deleted]
		}
		if len(ids) > 0 {
			// Delete as one batch; a poisoned id falls back to one-by-one.
			if err := retry(j.Context(), func() error {
				return plugin.DeleteMessages(j.Context(), j.API, j.peer, ids...)
			}); err == nil {
				deleted += len(ids)
			} else if fatalErr(err) {
				p.finishErr(j.CommandContext, err)
				return
			} else {
				for _, id := range ids {
					if err := retry(j.Context(), func() error {
						return plugin.DeleteMessages(j.Context(), j.API, j.peer, id)
					}); err == nil {
						deleted++
					} else if fatalErr(err) {
						p.finishErr(j.CommandContext, err)
						return
					}
				}
			}
		}
		scanned += len(msgs)
		pages++
		offsetID = next
		j.progress(stickerProgress(tl, deleted, max, scanned))
		if len(msgs) < stickerPage {
			break
		}
		select {
		case <-j.Context().Done():
			return
		case <-time.After(stickerThrottle):
		}
	}

	if deleted == 0 {
		p.finish(j.CommandContext, "ℹ️ "+tl("没有找到贴纸消息", "No sticker messages found"), resultTTL)
		return
	}
	p.finish(j.CommandContext, fmt.Sprintf("✅ **%s**\n\n> %s\n> %s",
		tl("贴纸清理完成", "Stickers cleared"),
		fmt.Sprintf(tl("已删除 %d 条贴纸消息", "Deleted %d sticker messages"), deleted),
		fmt.Sprintf(tl("扫描了 %d 条消息", "Scanned %d messages"), scanned)),
		stickerResultTTL)
}

func stickerProgress(tl func(string, string) string, deleted, max, scanned int) string {
	return fmt.Sprintf("🧹 **%s**\n\n> %s %d / %d\n> %s %d\n\n⏳ %s",
		tl("清理贴纸消息", "Clearing stickers"),
		tl("已删除", "Deleted"), deleted, max,
		tl("已扫描", "Scanned"), scanned,
		tl("正在删除…", "Deleting…"))
}

// isSticker reports whether the message carries a sticker document.
func isSticker(m *tg.Message) bool {
	media, ok := m.Media.(*tg.MessageMediaDocument)
	if !ok {
		return false
	}
	doc, ok := media.Document.(*tg.Document)
	if !ok {
		return false
	}
	for _, a := range doc.Attributes {
		if _, ok := a.(*tg.DocumentAttributeSticker); ok {
			return true
		}
	}
	return false
}

func helpSticker(tl func(string, string) string) string {
	return "🧹 **" + tl("清理贴纸消息", "Clear stickers") + "**\n\n" +
		"> " + plugin.Code("clean sticker [数量 count]") + " " +
		tl("删除群里的贴纸消息，默认最多 2000 条", "Delete sticker messages in the group, at most 2000 by default")
}
