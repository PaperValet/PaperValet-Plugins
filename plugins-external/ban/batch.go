package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// batchState claims one running batch per target, so a second sb for the
// same user while the first is still running answers politely.
type batchState struct {
	mu  sync.Mutex
	run map[int64]bool
}

var batch = &batchState{run: map[int64]bool{}}

func (b *batchState) claim(id int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.run[id] {
		return false
	}
	b.run[id] = true
	return true
}

func (b *batchState) release(id int64) {
	b.mu.Lock()
	delete(b.run, id)
	b.mu.Unlock()
}

// errBasicNoUnban marks basic groups skipped by unsb: they have no
// cross-group unban semantics.
var errBasicNoUnban = errors.New("BASIC_GROUP_NO_UNBAN")

// batch launches one cross-group action in the background on the plugin
// lifetime context. action is "ban" or "unban".
func (p *BanPlugin) batch(ctx *plugin.CommandContext, action string, t target, name string, groups []managedGroup) {
	life := p.lifetime()
	run := *ctx
	run.Ctx = life
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer batch.release(t.id)
		p.runBatch(&run, action, t, name, groups)
	}()
}

// runBatch is the synchronous body of a cross-group batch.
func (p *BanPlugin) runBatch(ctx *plugin.CommandContext, action string, t target, name string, groups []managedGroup) {
	tl := ctx.Tlocal
	start := time.Now()
	out := newOutcome()

	var basic, channels []managedGroup
	for _, g := range groups {
		if g.Kind == "channel" {
			channels = append(channels, g)
		} else {
			basic = append(basic, g)
		}
	}
	total := len(channels) + len(basic)

	// Progress ticker: edit at most every 3s, under the results lock.
	var mu sync.Mutex
	done := 0
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(progressEvery)
		defer tick.Stop()
		verb := tl("封禁中", "Banning")
		if action == "unban" {
			verb = tl("解封中", "Unbanning")
		}
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				mu.Lock()
				d := done
				mu.Unlock()
				_ = ctx.Edit(fmt.Sprintf("⚡ %s %s\n\n⏳ %d/%d",
					verb, plugin.Bold(name), d, total))
			}
		}
	}()

	// sb also wipes the target's messages in the chat it was called from.
	delDone := make(chan bool, 1)
	if action == "ban" {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			if peer, err := ctx.ResolvePeer(); err == nil {
				if ch, ok := peer.(*tg.InputPeerChannel); ok {
					delDone <- p.deleteHistoryCurrent(ctx, ch, t)
					return
				}
			}
			delDone <- false
		}()
	} else {
		delDone <- false
	}

	sem := make(chan struct{}, batchWorkers)
	var wg sync.WaitGroup
	apply := func(job func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			job()
		}()
	}
	for _, g := range channels {
		apply(func() {
			err := retryFlood(ctx.Context(), func() error {
				return editBanned(ctx.Context(), ctx.API, g.channel(), t.peer(), action)
			})
			mu.Lock()
			defer mu.Unlock()
			done++
			if err != nil {
				out.fail(errCode(err))
			} else {
				out.ok++
			}
		})
	}
	for _, g := range basic {
		apply(func() {
			var err error
			if action == "ban" {
				err = retryFlood(ctx.Context(), func() error {
					_, err := ctx.API.MessagesDeleteChatUser(ctx.Context(), &tg.MessagesDeleteChatUserRequest{
						ChatID: g.ID, UserID: &tg.InputUser{UserID: t.id, AccessHash: t.hash},
					})
					return err
				})
			} else {
				err = errBasicNoUnban
			}
			mu.Lock()
			defer mu.Unlock()
			done++
			switch {
			case err == nil:
				out.ok++
			case err == errBasicNoUnban:
				out.skipped++
			default:
				out.fail(errCode(err))
			}
		})
	}
	wg.Wait()
	close(stop)
	<-stopped
	deleted := <-delDone

	p.finish(ctx, action, name, total, out, deleted, time.Since(start))
}

// editBanned applies one ban or unban rights set to a channel.
func editBanned(ctx context.Context, api *tg.Client, ch *tg.InputChannel, participant tg.InputPeerClass, action string) error {
	var rights tg.ChatBannedRights
	if action == "ban" {
		rights = banRights(0)
	}
	_, err := api.ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
		Channel: ch, Participant: participant, BannedRights: rights,
	})
	return err
}

// retryFlood retries fn once through a short flood wait (≤ maxBatchFlood).
func retryFlood(ctx context.Context, fn func() error) error {
	err := fn()
	if err == nil {
		return nil
	}
	d, ok := tgerr.AsFloodWait(err)
	if !ok || d > maxBatchFlood {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d + time.Second):
	}
	return fn()
}

// finish renders the batch summary and schedules its auto-delete.
func (p *BanPlugin) finish(ctx *plugin.CommandContext, action string, name string, total int, out *batchOutcome, deleted bool, took time.Duration) {
	tl := ctx.Tlocal
	title := tl("批量封禁完成", "Batch ban finished")
	if action == "unban" {
		title = tl("批量解封完成", "Batch unban finished")
	}
	s := "✅ **" + title + "**\n\n" +
		"> " + plugin.Bold(name) + "\n" +
		fmt.Sprintf("> %s %d/%d", tl("成功", "succeeded"), out.ok, total)
	if out.skipped > 0 {
		s += fmt.Sprintf(" · %s %d", tl("跳过", "skipped"), out.skipped)
	}
	if out.failed > 0 {
		s += fmt.Sprintf(" · %s %d", tl("失败", "failed"), out.failed)
		if reasons := out.topReasons(3); reasons != "" {
			s += "（" + plugin.Escape(reasons) + "）"
		}
	}
	s += "\n"
	if deleted {
		s += "> 🗑 " + tl("已清理其在当前群的消息", "their messages here were wiped") + "\n"
	}
	if out.skipped > 0 && action == "unban" {
		s += "\nℹ️ " + tl("基础群没有跨群解封，已跳过", "Basic groups have no cross-group unban; skipped") + "\n"
	}
	s += "\n" + fmt.Sprintf(tl("⏱ %.1fs · 本消息 30 秒后自动删除", "⏱ %.1fs · this message deletes itself in 30 seconds"), took.Seconds())
	if err := ctx.Edit(s); err != nil && ctx.Logger != nil {
		ctx.Logger.Warn("ban: edit summary failed", "error", err)
	}
	p.autoDelete(ctx, summaryTTL)
}
