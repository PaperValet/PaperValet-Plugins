package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var errStopped = errors.New("plugin is stopping")

const (
	backupDefault = 100 // messages forwarded when no count is given
	backupMax     = 5000
	backupBatch   = 100 // messages per history page
)

// parseBackupArgs validates `shift backup <源> <目标> [count]`.
func parseBackupArgs(args []string) (src, dst string, count int, err error) {
	if len(args) < 2 {
		return "", "", 0, fmt.Errorf("need source and target")
	}
	src, dst = args[0], args[1]
	count = backupDefault
	if len(args) > 2 {
		n, e := parseInt(strings.TrimPrefix(args[2], "#"))
		if e != nil || n <= 0 {
			return "", "", 0, fmt.Errorf("invalid count %q", args[2])
		}
		count = min(n, backupMax)
	}
	return src, dst, count, nil
}

// cmdBackup forwards the newest `count` history messages of src to dst.
// Unlike rules, it runs synchronously with progress edits, like TeleBox.
func (p *ShiftPlugin) cmdBackup(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	srcInput, dstInput, count, err := parseBackupArgs(args)
	if err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()) + "\n" + tl("用法：", "Usage: ") + plugin.Code(tl("shift backup <源> <目标> [数量]", "shift backup <source> <target> [count]")))
	}
	dstPeer, topic, _ := parseTarget(dstInput)
	if dstPeer == "" {
		return ctx.Edit("❌ " + tl("目标为空", "Empty target"))
	}
	srcInfo, err := resolveTarget(ctx.Context(), p.resolverView(), srcInput, ctx.Message.ChatID, p.selfIDView())
	if err != nil {
		return ctx.Edit("❌ " + tl("源对话无效 ", "Invalid source ") + plugin.Code(srcInput) + "\n" + plugin.Escape(errText(err)))
	}
	dstInfo, err := resolveTarget(ctx.Context(), p.resolverView(), dstPeer, ctx.Message.ChatID, p.selfIDView())
	if err != nil {
		return ctx.Edit("❌ " + tl("目标对话无效 ", "Invalid target ") + plugin.Code(dstPeer) + "\n" + plugin.Escape(errText(err)))
	}
	if srcInfo.ChatID == dstInfo.ChatID {
		return ctx.Edit("❌ " + tl("源和目标相同", "Source equals target"))
	}

	jctx, done, err := p.beginJob(ctx.Context())
	if err != nil {
		return ctx.Edit("❌ " + tl("插件正在停止", "Plugin is stopping"))
	}
	defer done()

	_ = ctx.Edit(fmt.Sprintf("🔄 "+tl("开始备份：%s → %s，最多 %d 条…", "Starting backup: %s → %s, up to %d messages…"),
		plugin.Bold(srcInfo.label()), plugin.Bold(dstInfo.label()), count))

	var forwarded, failed int
	var lastErr string
	offsetID := 0
	for doneCount := 0; doneCount < count; {
		batch := min(backupBatch, count-doneCount)
		var (
			hist tg.MessagesMessagesClass
			e    error
		)
		e = callWithFloodWait(jctx, func() (e error) {
			hist, e = ctx.API.MessagesGetHistory(jctx, &tg.MessagesGetHistoryRequest{
				Peer:     srcInfo.Peer,
				Limit:    batch,
				OffsetID: offsetID,
			})
			return e
		})
		if e != nil {
			if jctx.Err() != nil {
				return nil
			}
			lastErr = errText(e)
			break
		}
		slice, ok := hist.(*tg.MessagesMessagesSlice)
		if !ok {
			if mm, ok2 := hist.(*tg.MessagesMessages); ok2 {
				slice = &tg.MessagesMessagesSlice{Messages: mm.Messages, Chats: mm.Chats, Users: mm.Users}
			} else {
				break
			}
		}
		if len(slice.Messages) == 0 {
			break
		}
		var ids []int
		for _, m := range slice.Messages {
			msg, ok := m.(*tg.Message)
			if !ok {
				continue
			}
			ids = append(ids, msg.ID)
			offsetID = msg.ID
		}
		if len(ids) == 0 {
			break
		}
		e = forwardMessages(jctx, ctx.API, srcInfo.Peer, dstInfo.Peer, ids, false, topic)
		if e != nil {
			if jctx.Err() != nil {
				return nil
			}
			failed += len(ids)
			lastErr = errText(e)
		} else {
			forwarded += len(ids)
		}
		doneCount += len(ids)
		if forwarded > 0 && forwarded%300 < backupBatch {
			_ = ctx.Edit(fmt.Sprintf("⏳ "+tl("备份进行中… 已转发 %d/%d", "Backing up… forwarded %d/%d"), forwarded, count))
		}
		time.Sleep(300 * time.Millisecond)
	}

	var b strings.Builder
	b.WriteString("✅ **" + tl("备份完成", "Backup Finished") + "**\n\n")
	b.WriteString(tl("已转发", "Forwarded") + "  " + plugin.Code(forwarded) + "\n")
	if failed > 0 {
		b.WriteString(tl("失败", "Failed") + "  " + plugin.Code(failed) + "\n")
	}
	if lastErr != "" {
		b.WriteString("⚠️ " + plugin.Escape(lastErr) + "\n")
	}
	return ctx.Edit(strings.TrimRight(b.String(), "\n"))
}

// beginJob registers a cancellable backup job.
func (p *ShiftPlugin) beginJob(parent context.Context) (context.Context, func(), error) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, nil, errStopped
	}
	c, cancel := context.WithCancel(parent)
	gen := p.wgAdd()
	p.mu.Unlock()
	return c, func() {
		cancel()
		p.wgDone(gen)
	}, nil
}
