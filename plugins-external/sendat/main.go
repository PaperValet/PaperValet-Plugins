package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // timezone names work without system tzdata

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	sendTimeout  = 30 * time.Second
	maxSleep     = time.Minute      // re-evaluate at least this often (clock jumps, suspend)
	noAPIRetry   = 30 * time.Second // retry when no API client is known yet
	overdueGrace = 24 * time.Hour   // missed one-shot tasks older than this are dropped
	maxMsgLen    = 4096
)

var Metadata = &plugin.PluginMetadata{
	Name:        "sendat",
	Description: "定时发送消息",
	DescEN:      "Send messages on a schedule",
	Version:     "1.1.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type SendAtPlugin struct {
	mu       sync.Mutex
	dir      string
	tasks    []*Task
	nextID   int
	next     map[int]time.Time // in-memory next fire time per active task
	cfg      config
	loc      *time.Location
	api      *tg.Client
	resolver plugin.PeerResolver
	logger   plugin.Logger

	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}
}

func New() *SendAtPlugin {
	return &SendAtPlugin{dir: dataDir, loc: time.Local, next: map[int]time.Time{}, wake: make(chan struct{}, 1)}
}

func (p *SendAtPlugin) Name() string        { return "sendat" }
func (p *SendAtPlugin) Description() string { return Metadata.Description }
func (p *SendAtPlugin) DescEN() string      { return Metadata.DescEN }

func (p *SendAtPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	// Restored tasks must send before any command runs, so take the
	// long-lived client from the host instead of a command context.
	if mgr != nil {
		if h := mgr.Host(); h != nil {
			p.mu.Lock()
			p.api, p.resolver, p.logger = h.API(), h.PeerResolver(), h.Logger(p.Name())
			p.mu.Unlock()
		}
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "sendat",
		Description: "定时发送消息：间隔 / 每天定时 / 单次，任务持久化，支持列出、暂停、恢复、删除",
		DescEN:      "Scheduled messages: interval / daily / one-shot, persistent, with list, pause, resume and delete",
		Usage:       "sendat <时间> | <消息> · sendat list [all] · sendat pause|resume|rm <ID> · sendat tz [时区] · sendat help",
		UsageEN:     "sendat <time> | <message> · sendat list [all] · sendat pause|resume|rm <ID> · sendat tz [zone] · sendat help",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

// Start loads persisted tasks and starts the scheduler.
func (p *SendAtPlugin) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return nil
	}
	if err := p.loadLocked(); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("sendat: load tasks: %w", err)
	}
	now := time.Now()
	p.next = map[int]time.Time{}
	changed := false
	kept := p.tasks[:0]
	for _, t := range p.tasks {
		if t.Mode == modeOnce && now.Sub(time.Unix(t.At, 0)) > overdueGrace {
			changed = true // missed long ago, drop
			continue
		}
		kept = append(kept, t)
		if !t.Pause {
			p.next[t.ID] = p.firstRun(*t, now)
		}
	}
	p.tasks = kept
	if changed {
		_ = p.saveLocked()
	}
	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	done := p.done
	p.mu.Unlock()

	go p.loop(runCtx, done)
	return nil
}

// Stop halts the scheduler and waits for it to exit.
func (p *SendAtPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// firstRun computes the first fire time for a task at (re)start.
// Overdue one-shot tasks fire right away.
func (p *SendAtPlugin) firstRun(t Task, now time.Time) time.Time {
	n := nextRun(t, now, p.loc)
	if t.Mode == modeOnce && n.Before(now) {
		return now
	}
	return n
}

func (p *SendAtPlugin) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *SendAtPlugin) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-timer.C:
		}
		p.runDue(ctx)
		if ctx.Err() != nil {
			return
		}
		d := p.sleepFor(time.Now())
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
	}
}

func (p *SendAtPlugin) sleepFor(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := maxSleep
	for _, at := range p.next {
		if w := at.Sub(now); w < d {
			d = w
		}
	}
	if d < 0 {
		d = 0
	}
	return d
}

// runDue sends every task whose time has come.
func (p *SendAtPlugin) runDue(ctx context.Context) {
	now := time.Now()
	p.mu.Lock()
	var due []Task
	for _, t := range p.tasks {
		if at, ok := p.next[t.ID]; ok && !t.Pause && !at.After(now) {
			due = append(due, *t)
		}
	}
	api, resolver := p.api, p.resolver
	p.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].ID < due[j].ID })

	for _, t := range due {
		if ctx.Err() != nil {
			return
		}
		if api == nil {
			p.mu.Lock()
			if _, ok := p.next[t.ID]; ok {
				p.next[t.ID] = time.Now().Add(noAPIRetry)
			}
			p.mu.Unlock()
			continue
		}
		err := sendTask(ctx, api, resolver, t)
		p.afterRun(t.ID, err)
	}
}

func (p *SendAtPlugin) afterRun(id int, sendErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.indexLocked(id)
	if idx < 0 {
		delete(p.next, id)
		return
	}
	t := p.tasks[idx]
	now := time.Now()
	t.LastRun = now.Unix()
	if sendErr != nil {
		t.LastError = truncate(sendErr.Error(), 200)
		if p.logger != nil {
			p.logger.Warn("sendat: task send failed", "task", id, "error", sendErr)
		}
	} else {
		t.LastError = ""
		t.Count++
	}
	finished := t.Mode == modeOnce
	if !finished && t.TimeLimit > 0 {
		t.TimeLimit--
		finished = t.TimeLimit == 0
	}
	if finished {
		p.tasks = append(p.tasks[:idx], p.tasks[idx+1:]...)
		delete(p.next, id)
	} else if !t.Pause {
		p.next[id] = nextRun(*t, now, p.loc)
	}
	if err := p.saveLocked(); err != nil && p.logger != nil {
		p.logger.Warn("sendat: save failed", "error", err)
	}
}

func sendTask(ctx context.Context, api *tg.Client, resolver plugin.PeerResolver, t Task) error {
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	peer := t.Peer.input()
	if peer == nil {
		if resolver == nil {
			return fmt.Errorf("cannot resolve chat %d", t.ChatID)
		}
		var err error
		if peer, err = resolver.ResolveFromChatID(sctx, t.ChatID); err != nil {
			return fmt.Errorf("resolve chat %d: %w", t.ChatID, err)
		}
	}
	req := &tg.MessagesSendMessageRequest{Peer: peer, Message: t.Msg, RandomID: randomID()}
	if ents := decodeEntities(t.Entities); len(ents) > 0 {
		req.SetEntities(ents)
	}
	_, err := api.MessagesSendMessage(sctx, req)
	return err
}

// ---------------------------------------------------------------- storage

func (p *SendAtPlugin) loadLocked() error {
	var sf storeFile
	if err := readJSON(filepath.Join(p.dir, tasksFile), &sf); err != nil {
		return err
	}
	var cfg config
	if err := readJSON(filepath.Join(p.dir, configFile), &cfg); err != nil {
		return err
	}
	p.cfg = cfg
	p.loc = time.Local
	if cfg.Timezone != "" {
		if loc, err := time.LoadLocation(cfg.Timezone); err == nil {
			p.loc = loc
		}
	}
	p.tasks = p.tasks[:0]
	maxID := 0
	for _, t := range sf.Tasks {
		if t == nil || t.ID <= 0 || t.Msg == "" {
			continue
		}
		switch t.Mode {
		case modeOnce, modeDaily:
		case modeInterval:
			if t.PeriodSec <= 0 {
				continue
			}
		default:
			continue
		}
		if t.ID > maxID {
			maxID = t.ID
		}
		p.tasks = append(p.tasks, t)
	}
	p.nextID = sf.NextID
	if p.nextID <= maxID {
		p.nextID = maxID + 1
	}
	return nil
}

func (p *SendAtPlugin) saveLocked() error {
	return writeJSON(filepath.Join(p.dir, tasksFile), storeFile{NextID: p.nextID, Tasks: p.tasks})
}

func (p *SendAtPlugin) indexLocked(id int) int {
	for i, t := range p.tasks {
		if t.ID == id {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------- commands

func (p *SendAtPlugin) capture(ctx *plugin.CommandContext) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.API != nil {
		p.api = ctx.API
	}
	if ctx.PeerResolver != nil {
		p.resolver = ctx.PeerResolver
	}
	if ctx.Logger != nil {
		p.logger = ctx.Logger
	}
}

func (p *SendAtPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	p.capture(ctx)
	p.mu.Lock()
	running := p.cancel != nil
	p.mu.Unlock()
	if !running {
		if err := p.Start(context.Background()); err != nil {
			return ctx.Edit("❌ " + plugin.Escape(err.Error()))
		}
	}

	sub := ""
	if len(ctx.Args) > 0 {
		sub = strings.ToLower(ctx.Args[0])
	}
	rest := []string{}
	if len(ctx.Args) > 1 {
		rest = ctx.Args[1:]
	}
	switch sub {
	case "", "help":
		return ctx.Edit(helpText(ctx))
	case "list":
		return p.cmdList(ctx, len(rest) > 0 && strings.ToLower(rest[0]) == "all")
	case "rm":
		return p.cmdChange(ctx, rest, "rm")
	case "pause":
		return p.cmdChange(ctx, rest, "pause")
	case "resume":
		return p.cmdChange(ctx, rest, "resume")
	case "tz":
		return p.cmdTZ(ctx, rest)
	}
	return p.cmdAdd(ctx)
}

func helpText(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("⏰ **" + tl("定时发送消息", "Scheduled Messages") + "**\n\n")
	b.WriteString("**" + tl("用法", "Usage") + "**\n")
	b.WriteString(line(tl("sendat 时间 | 消息内容", "sendat time | message"), "添加定时任务", "add a task"))
	b.WriteString(line("sendat list", "查看本会话的任务", "list tasks of this chat"))
	b.WriteString(line("sendat list all", "查看所有任务（仅主账号）", "list all tasks (owner only)"))
	b.WriteString(line("sendat rm <ID>", "删除任务", "delete a task"))
	b.WriteString(line("sendat pause <ID>", "暂停任务", "pause a task"))
	b.WriteString(line("sendat resume <ID>", "恢复任务", "resume a task"))
	b.WriteString(line(tl("sendat tz [时区]", "sendat tz [zone]"), "查看/设置每日任务时区，如 Asia/Shanghai", "show/set the zone for daily tasks, e.g. Asia/Shanghai"))
	b.WriteString("\n**" + tl("示例", "Examples") + "**\n")
	b.WriteString(line(tl("sendat 16:00:00 date | 投票截止！", "sendat 16:00:00 date | Voting closed!"), "下一次 16:00 发送一次", "send once at the next 16:00"))
	b.WriteString(line(tl("sendat every 23:59:59 date | 又是无所事事的一天呢。", "sendat every 23:59:59 date | Another idle day."), "每天 23:59:59 发送", "send daily at 23:59:59"))
	b.WriteString(line(tl("sendat every 1 minutes | 又过去了一分钟。", "sendat every 1 minutes | Another minute."), "每分钟发送", "send every minute"))
	b.WriteString(line(tl("sendat 3 times 1 minutes | 此消息将出现三次。", "sendat 3 times 1 minutes | Shown three times."), "每分钟发送，共 3 次", "every minute, 3 times"))
	b.WriteString(line(tl("sendat +5m | 五分钟后", "sendat +5m | in five minutes"), "5 分钟后发送一次", "send once in 5 minutes"))
	b.WriteString(line(tl("sendat 2026-01-01 00:00 | 新年快乐", "sendat 2026-01-01 00:00 | Happy new year"), "指定日期时间发送一次", "send once at a date"))
	b.WriteString("\n**" + tl("时间单位", "Units") + "**\n")
	b.WriteString(plugin.Code("seconds minutes hours days date times every") + "\n")
	b.WriteString(tl("也可写作 30s / 5m / 2h / 1d / 1h30m；消息里的格式（粗体、链接等）会保留", "Short forms 30s / 5m / 2h / 1d / 1h30m also work; message formatting (bold, links…) is kept") + "\n\n")
	b.WriteString("💡 " + tl("任务保存在 data/sendat/tasks.json，重启后自动恢复", "Tasks are saved in data/sendat/tasks.json and restored after restart"))
	return b.String()
}

// messageSource returns the raw command text after the command word and
// its UTF-16 offset in the original message, or ok=false when the text
// cannot be mapped back (e.g. user alias expansion).
func messageSource(ctx *plugin.CommandContext) (raw string, base int, ok bool) {
	full := ctx.Message.Message.Message
	s := strings.TrimLeft(full, " \t\r\n")
	i := strings.IndexAny(s, " \t\r\n")
	if i < 0 {
		return "", 0, false
	}
	off := len(full) - len(s) + i
	raw = full[off:]
	if strings.Join(strings.Fields(raw), " ") != strings.Join(ctx.Args, " ") {
		return "", 0, false
	}
	return raw, utf16Len(full[:off]), true
}

func (p *SendAtPlugin) cmdAdd(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	raw, base, mapped := messageSource(ctx)
	if !mapped {
		raw, base = ctx.RawArgs, -1
	}
	specStr, ms, me, err := splitCommand(raw)
	if err != nil {
		return ctx.Edit("❌ " + tl("命令格式错误，请使用 ", "Bad format, use ") + plugin.Code(tl("sendat 时间 | 消息内容", "sendat time | message")))
	}
	msg := raw[ms:me]
	if msg == "" {
		return ctx.Edit("❌ " + tl("消息内容不能为空", "Message must not be empty"))
	}
	if utf16Len(msg) > maxMsgLen {
		return ctx.Edit("❌ " + tl("消息过长（最多 4096 字符）", "Message too long (max 4096 characters)"))
	}

	p.mu.Lock()
	loc := p.loc
	p.mu.Unlock()
	sp, err := parseSpec(specStr, loc)
	if err != nil {
		return ctx.Edit("❌ " + tl("时间格式错误: ", "Bad time spec: ") + plugin.Escape(err.Error()) + "\n\n💡 " + plugin.Code("sendat help"))
	}
	now := time.Now()
	task, err := buildTask(sp, now, loc)
	if err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()))
	}

	peer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit("❌ " + tl("会话解析失败: ", "Failed to resolve chat: ") + plugin.Escape(err.Error()))
	}
	task.ChatID = ctx.Message.ChatID
	task.Peer = refFromPeer(peer)
	task.Msg = msg
	if base >= 0 {
		start := base + utf16Len(raw[:ms])
		ents := sliceEntities(ctx.Message.Message.Entities, start, start+utf16Len(msg))
		task.Entities = encodeEntities(filterSendable(ents))
	}

	p.mu.Lock()
	task.ID = p.nextID
	p.nextID++
	t := task
	p.tasks = append(p.tasks, &t)
	p.next[t.ID] = p.firstRun(t, now)
	nextAt := p.next[t.ID]
	err = p.saveLocked()
	if err != nil {
		p.tasks = p.tasks[:len(p.tasks)-1]
		delete(p.next, t.ID)
	}
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存任务失败: ", "Failed to save task: ") + plugin.Escape(err.Error()))
	}
	p.poke()

	var b strings.Builder
	b.WriteString("✅ **" + fmt.Sprintf(tl("已添加任务 #%d", "Task #%d added"), t.ID) + "**\n\n")
	b.WriteString(tl("计划", "Schedule") + "  " + plugin.Code(scheduleText(t, loc, tl)) + "\n")
	b.WriteString(tl("下次", "Next") + "  " + plugin.Code(nextAt.In(loc).Format("2006-01-02 15:04:05 MST")) + "\n")
	b.WriteString(tl("消息", "Message") + "  " + plugin.Code(truncate(t.Msg, 50)))
	return ctx.Edit(b.String())
}

// filterSendable drops entities that cannot be sent back as-is
// (mention-by-id needs an input user with access hash).
func filterSendable(ents []tg.MessageEntityClass) []tg.MessageEntityClass {
	out := ents[:0]
	for _, e := range ents {
		switch e.(type) {
		case *tg.MessageEntityMentionName, *tg.InputMessageEntityMentionName:
			continue
		}
		out = append(out, e)
	}
	return out
}

func (p *SendAtPlugin) cmdList(ctx *plugin.CommandContext, all bool) error {
	tl := ctx.Tlocal
	if all && !ctx.IsSelf() {
		return ctx.Edit("❌ " + tl("只有主账号可以查看所有任务", "Only the owner can list all tasks"))
	}
	p.mu.Lock()
	loc := p.loc
	var tasks []Task
	next := map[int]time.Time{}
	for _, t := range p.tasks {
		if all || t.ChatID == ctx.Message.ChatID {
			tasks = append(tasks, *t)
			if at, ok := p.next[t.ID]; ok {
				next[t.ID] = at
			}
		}
	}
	p.mu.Unlock()

	if len(tasks) == 0 {
		if all {
			return ctx.Edit("📝 " + tl("没有已注册的任务", "No tasks registered"))
		}
		return ctx.Edit("📝 " + tl("本会话没有已注册的任务", "No tasks in this chat"))
	}
	var b strings.Builder
	if all {
		b.WriteString("📋 **" + tl("所有任务", "All Tasks") + "**")
	} else {
		b.WriteString("📋 **" + tl("本会话任务", "Tasks in This Chat") + "**")
	}
	b.WriteString(" " + plugin.Code(len(tasks)) + "\n")
	for _, t := range tasks {
		title := fmt.Sprintf("#%d", t.ID)
		if t.Pause {
			title += tl(" [已暂停]", " [paused]")
		}
		b.WriteString("\n**" + plugin.Escape(title) + "**\n")
		b.WriteString(tl("计划", "Schedule") + "  " + plugin.Code(scheduleText(t, loc, tl)) + "\n")
		if at, ok := next[t.ID]; ok {
			b.WriteString(tl("下次", "Next") + "  " + plugin.Code(at.In(loc).Format("2006-01-02 15:04:05")) + "\n")
		}
		if all {
			b.WriteString(tl("会话", "Chat") + "  " + plugin.Code(t.ChatID) + "\n")
		}
		if t.Count > 0 {
			b.WriteString(tl("已发送", "Sent") + "  " + plugin.Code(t.Count) + "\n")
		}
		if t.LastError != "" {
			b.WriteString(tl("上次错误", "Last error") + "  " + plugin.Code(truncate(t.LastError, 80)) + "\n")
		}
		b.WriteString(tl("消息", "Message") + "  " + plugin.Code(truncate(t.Msg, 50)) + "\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	if len([]rune(out)) > 3900 {
		out = string([]rune(out)[:3900]) + "\n…"
	}
	return ctx.Edit(out)
}

func (p *SendAtPlugin) cmdChange(ctx *plugin.CommandContext, args []string, op string) error {
	tl := ctx.Tlocal
	if len(args) == 0 {
		return ctx.Edit("❌ " + tl("请输入有效的任务ID", "Please give a valid task ID"))
	}
	var ids []int
	for _, a := range args {
		for _, s := range strings.Split(a, ",") {
			s = strings.TrimPrefix(strings.TrimSpace(s), "#")
			if s == "" {
				continue
			}
			id, err := strconv.Atoi(s)
			if err != nil || id <= 0 {
				return ctx.Edit("❌ " + tl("请输入有效的任务ID: ", "Invalid task ID: ") + plugin.Code(s))
			}
			ids = append(ids, id)
		}
	}
	owner := ctx.IsSelf()
	var okIDs, failed []string
	p.mu.Lock()
	now := time.Now()
	for _, id := range ids {
		idx := p.indexLocked(id)
		if idx < 0 {
			failed = append(failed, fmt.Sprintf(tl("#%d 不存在", "#%d not found"), id))
			continue
		}
		t := p.tasks[idx]
		if t.ChatID != ctx.Message.ChatID && !owner {
			failed = append(failed, fmt.Sprintf(tl("#%d 不属于本会话", "#%d belongs to another chat"), id))
			continue
		}
		switch op {
		case "rm":
			p.tasks = append(p.tasks[:idx], p.tasks[idx+1:]...)
			delete(p.next, id)
		case "pause":
			if t.Pause {
				failed = append(failed, fmt.Sprintf(tl("#%d 已是暂停状态", "#%d already paused"), id))
				continue
			}
			t.Pause = true
			delete(p.next, id)
		case "resume":
			if !t.Pause {
				failed = append(failed, fmt.Sprintf(tl("#%d 未暂停", "#%d is not paused"), id))
				continue
			}
			t.Pause = false
			p.next[id] = p.firstRun(*t, now)
		}
		okIDs = append(okIDs, fmt.Sprintf("#%d", id))
	}
	var saveErr error
	if len(okIDs) > 0 {
		saveErr = p.saveLocked()
	}
	p.mu.Unlock()
	p.poke()

	var b strings.Builder
	if len(okIDs) > 0 {
		switch op {
		case "rm":
			b.WriteString("✅ " + tl("已删除任务 ", "Deleted task "))
		case "pause":
			b.WriteString("⏸️ " + tl("已暂停任务 ", "Paused task "))
		case "resume":
			b.WriteString("▶️ " + tl("已恢复任务 ", "Resumed task "))
		}
		b.WriteString(plugin.Code(strings.Join(okIDs, " ")))
	}
	for _, f := range failed {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("❌ " + plugin.Escape(f))
	}
	if saveErr != nil {
		b.WriteString("\n❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(saveErr.Error()))
	}
	return ctx.Edit(b.String())
}

func (p *SendAtPlugin) cmdTZ(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	if len(args) == 0 {
		p.mu.Lock()
		loc := p.loc
		p.mu.Unlock()
		return ctx.Edit("🌐 **" + tl("时区", "Timezone") + "**\n\n" +
			tl("当前", "Current") + "  " + plugin.Code(loc.String()) + "\n" +
			tl("时间", "Now") + "  " + plugin.Code(time.Now().In(loc).Format("2006-01-02 15:04:05 MST")) + "\n\n" +
			"💡 " + plugin.Code("sendat tz Asia/Shanghai") + tl("，", ", ") + plugin.Code("sendat tz local") + tl(" 恢复系统时区", " resets to system zone"))
	}
	if !ctx.IsSelf() {
		return ctx.Edit("❌ " + tl("只有主账号可以修改时区", "Only the owner can change the timezone"))
	}
	name := args[0]
	loc := time.Local
	if !strings.EqualFold(name, "local") {
		l, err := time.LoadLocation(name)
		if err != nil {
			return ctx.Edit("❌ " + tl("未知时区: ", "Unknown timezone: ") + plugin.Code(name))
		}
		loc = l
		name = l.String()
	} else {
		name = ""
	}
	p.mu.Lock()
	old := p.cfg
	p.cfg.Timezone = name
	err := writeJSON(filepath.Join(p.dir, configFile), p.cfg)
	if err != nil {
		p.cfg = old
	} else {
		p.loc = loc
		now := time.Now()
		for _, t := range p.tasks {
			if !t.Pause && t.Mode == modeDaily {
				p.next[t.ID] = nextRun(*t, now, loc)
			}
		}
	}
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	p.poke()
	return ctx.Edit("✅ " + tl("时区已设置为 ", "Timezone set to ") + plugin.Code(loc.String()))
}

// ---------------------------------------------------------------- helpers

func randomID() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.LittleEndian.Uint64(b[:]))
}
