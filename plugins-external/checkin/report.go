package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// cmdRun runs every enabled sign-in now (bare `checkin`), like the source's
// runManual: show progress, run in the background, then delete the command
// message and push the summary.
func (p *CheckinPlugin) cmdRun(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	if len(p.enabledTargets()) == 0 {
		return ctx.Edit("❌ " + tl("没有启用的签到目标，先用 ", "no enabled targets; use ") + plugin.Code("checkin add"))
	}
	p.mu.Lock()
	busy := p.running
	p.mu.Unlock()
	if busy {
		return ctx.Edit("⏳ " + tl("签到任务正在执行", "a sign-in run is already in progress"))
	}
	if err := ctx.Edit("🚀 " + tl("开始执行所有签到任务...", "Running all sign-in tasks...")); err != nil {
		return err
	}
	msgID := ctx.Message.Message.ID
	chatID := ctx.Message.ChatID
	peer := ctx.PeerResolver
	go func() {
		runCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p.mu.Lock()
		p.running = true
		p.mu.Unlock()
		p.runAll(runCtx, sourceManual)
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
		// The summary lands in the notify chat; remove the progress message.
		if resolved, err := peer.ResolveFromChatID(runCtx, chatID); err == nil {
			_ = plugin.DeleteMessages(runCtx, p.api, resolved, msgID)
		}
	}()
	return nil
}

// report builds the summary and pushes it to the configured chat.
func (p *CheckinPlugin) report(ctx context.Context, source string, results []runResult) {
	if len(results) == 0 {
		return
	}
	ownerID := p.selfID()
	tl := func(zh, en string) string { return p.langText(ownerID, zh, en) }
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
		}
	}
	var b strings.Builder
	b.WriteString("🤖 **" + tl("CheckIn 签到汇总", "CheckIn Summary") + "**\n")
	b.WriteString(tl("时间", "Time") + ": " + plugin.Code(time.Now().Format("2006-01-02 15:04:05")) + "\n")
	src := tl("自动定时任务", "scheduled run")
	if source == sourceManual {
		src = tl("手动触发", "manual run")
	}
	b.WriteString(tl("来源", "Source") + ": " + plugin.Escape(src) + "\n")
	b.WriteString(tl("结果", "Result") + ": " + fmt.Sprintf(tl("%d 成功 / %d 失败", "%d ok / %d failed"), ok, len(results)-ok) + "\n\n")
	for i, r := range results {
		icon := "✅"
		if !r.OK {
			icon = "❌"
		}
		line := icon + " **" + fmt.Sprint(i+1) + ". " + plugin.Escape(r.Target.Name) + "**\n   " +
			plugin.Escape(truncateRunes(r.Message, 200)) + "\n"
		if b.Len()+len(line) > maxText {
			b.WriteString("…")
			break
		}
		b.WriteString(line)
	}

	chat := p.notifyChat()
	if chat == 0 {
		return
	}
	_, _ = p.host.Send(ctx, chat, b.String(), 0)
}

// notifyChat resolves the summary push destination: the panel chat id, else
// the chat where targets were added.
func (p *CheckinPlugin) notifyChat() int64 {
	if p.set != nil {
		if s := p.set.String("notify_chat"); s != "" {
			var id int64
			if _, err := fmt.Sscan(s, &id); err == nil && id != 0 {
				return id
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.NotifyChat
}

func (p *CheckinPlugin) selfID() int64 {
	if p.host != nil {
		return p.host.SelfID()
	}
	return 0
}

// page renders the targets with toggle/delete buttons in the companion bot.
func (p *CheckinPlugin) page(c *plugin.BotContext) (*plugin.View, error) {
	tl := c.Tlocal
	if op, id, ok := strings.Cut(c.Data, ":"); ok {
		p.mu.Lock()
		idx := -1
		for i, t := range p.cfg.Targets {
			if t.ID == id {
				idx = i
				break
			}
		}
		switch {
		case idx < 0:
		case op == "rm":
			p.cfg.Targets = append(p.cfg.Targets[:idx], p.cfg.Targets[idx+1:]...)
			_ = p.saveLocked()
		case op == "toggle":
			p.cfg.Targets[idx].Enabled = !p.cfg.Targets[idx].Enabled
			_ = p.saveLocked()
		}
		p.mu.Unlock()
		p.poke()
		if idx < 0 {
			c.Toast(tl("未找到目标", "target not found"))
		}
	}

	p.mu.Lock()
	targets := make([]*Target, len(p.cfg.Targets))
	copy(targets, p.cfg.Targets)
	next, loc := p.cfg.NextRunAt, p.loc
	p.mu.Unlock()

	v := &plugin.View{Text: "✅ **" + tl("签到目标", "Sign-in Targets") + "**  " + fmt.Sprint(len(targets))}
	if next > 0 {
		v.Text += "\n" + tl("下次自动签到", "Next auto run") + "  " + plugin.Code(time.Unix(next, 0).In(loc).Format("01-02 15:04"))
	}
	if len(targets) == 0 {
		v.Text += "\n\n" + tl("还没有目标，在聊天里发 `checkin add <ID> <名称> <目标>` 添加", "No targets yet; send `checkin add <id> <name> <target>` in a chat")
		return v, nil
	}
	for _, t := range targets {
		status := "🔴"
		if t.Enabled {
			status = "🟢"
		}
		v.Text += fmt.Sprintf("\n\n%s **%s**  `%s`\n", status, plugin.Escape(t.Name), plugin.Escape(t.ID))
		v.Text += tl("目标", "Target") + "  " + plugin.Code(t.Target) + "\n"
		v.Text += tl("命令", "Command") + "  " + plugin.Code(truncateRunes(t.Command, 40)) + "\n"
		action := "⏸"
		if !t.Enabled {
			action = "▶️"
		}
		v.Buttons = append(v.Buttons, plugin.Row(
			plugin.Btn(action, "toggle:"+t.ID),
			plugin.Btn("🗑 "+t.ID, "rm:"+t.ID).Danger(),
		))
	}
	return v, nil
}

// tgImports keeps the tg import when nothing else uses it.
var _ = tg.ReplyInlineMarkup{}
