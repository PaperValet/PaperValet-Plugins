package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // timezone names work without system tzdata

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	maxSleep   = time.Minute // re-evaluate at least this often
	noAPIRetry = 30 * time.Second
	listRunes  = 3800 // list chunks stay below the message limit
)

var Metadata = &plugin.PluginMetadata{
	Name:        "acron",
	Description: "Cron 定时任务",
	DescEN:      "Cron-scheduled tasks",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type AcronPlugin struct {
	mu     sync.Mutex
	dir    string
	tasks  []*Task
	nextID int
	next   map[int]time.Time // next fire time per enabled task
	loc    *time.Location
	host   plugin.Host
	logger plugin.Logger
	set    plugin.Settings

	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}
}

func New() *AcronPlugin {
	return &AcronPlugin{loc: time.Local, next: map[int]time.Time{}, wake: make(chan struct{}, 1)}
}

func (p *AcronPlugin) Name() string        { return "acron" }
func (p *AcronPlugin) Description() string { return Metadata.Description }
func (p *AcronPlugin) DescEN() string      { return Metadata.DescEN }

func (p *AcronPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	if mgr != nil && mgr.Host() != nil {
		p.host = mgr.Host()
		p.mu.Lock()
		p.logger = p.host.Logger(p.Name())
		p.mu.Unlock()
	}
	if p.host != nil {
		dir, err := p.host.DataDir(p.Name())
		if err == nil {
			p.mu.Lock()
			p.dir = dir
			p.mu.Unlock()
		}
		set, err := p.host.Settings(&plugin.SettingsSpec{
			Plugin:  p.Name(),
			Title:   "⏰ 定时任务",
			TitleEN: "⏰ Cron Tasks",
			Settings: []plugin.Setting{{
				Key: "timezone", Label: "时区", LabelEN: "Timezone",
				Hint:   "IANA 名称如 Asia/Shanghai，留空用系统时区",
				HintEN: "An IANA name like Asia/Shanghai; empty keeps the system zone",
				Kind:   plugin.SettingText, Validate: validTimezone,
			}},
			OnChange: func(key string) {
				if key == "timezone" {
					p.setZone(p.set.String("timezone"))
				}
			},
		})
		if err != nil {
			return err
		}
		p.set = set
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "acron",
		Description: "Cron 定时发送/转发/复制/删除/置顶/执行命令",
		DescEN:      "Cron-scheduled send/forward/copy/delete/pin/run-command",
		Usage:       "acron <send|cmd|copy|forward|del|del_re|pin|unpin> 6字段cron 对话ID|话题或回复ID [参数] · acron ls [all|类型] · acron rm|on|off <ID>",
		UsageEN:     "acron <send|cmd|copy|forward|del|del_re|pin|unpin> 6-field-cron chatID|topicOrReply [args] · acron ls [all|type] · acron rm|on|off <ID>",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

// Start loads persisted tasks and starts the scheduler.
func (p *AcronPlugin) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return nil
	}
	if err := p.loadLocked(); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("acron: load tasks: %w", err)
	}
	p.applyZoneLocked()
	now := time.Now()
	p.next = map[int]time.Time{}
	for _, t := range p.tasks {
		if t.Disabled {
			continue
		}
		if at := p.taskNext(*t, now); !at.IsZero() {
			p.next[t.ID] = at
		}
	}
	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	done := p.done
	p.mu.Unlock()

	go p.loop(runCtx, done)
	return nil
}

func (p *AcronPlugin) Stop(ctx context.Context) error {
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

// taskNext computes the next fire time for a task, zero when the cron
// expression has no future fire time.
func (p *AcronPlugin) taskNext(t Task, now time.Time) time.Time {
	sched, err := parseCronExpr(t.Cron)
	if err != nil {
		return time.Time{}
	}
	return sched.Next(now.In(p.loc))
}

func (p *AcronPlugin) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *AcronPlugin) loop(ctx context.Context, done chan struct{}) {
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

func (p *AcronPlugin) sleepFor(now time.Time) time.Duration {
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

// runDue runs every enabled task whose fire time has come.
func (p *AcronPlugin) runDue(ctx context.Context) {
	now := time.Now()
	p.mu.Lock()
	var due []*Task
	for _, t := range p.tasks {
		if t.Disabled {
			continue
		}
		if at, ok := p.next[t.ID]; ok && !at.After(now) {
			due = append(due, t)
		}
	}
	p.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].ID < due[j].ID })

	for _, t := range due {
		if ctx.Err() != nil {
			return
		}
		if p.host == nil || p.host.API() == nil {
			p.mu.Lock()
			if _, ok := p.next[t.ID]; ok {
				p.next[t.ID] = time.Now().Add(noAPIRetry)
			}
			p.mu.Unlock()
			continue
		}
		snap := *t
		res, err := p.runTask(ctx, &snap)
		p.afterRun(t.ID, res, err)
	}
}

// afterRun records the outcome and reschedules.
func (p *AcronPlugin) afterRun(id int, res string, runErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.indexLocked(id)
	if idx < 0 {
		delete(p.next, id)
		return
	}
	t := p.tasks[idx]
	t.LastRunAt = time.Now().Unix()
	if runErr != nil {
		t.LastError = truncate(runErrorText(p.tl(), runErr), 200)
		t.LastResult = ""
		if p.logger != nil {
			p.logger.Warn("acron: task run failed", "task", id, "error", runErr)
		}
	} else {
		t.LastError = ""
		t.LastResult = res
	}
	if at := p.taskNext(*t, time.Now()); !at.IsZero() {
		p.next[id] = at
	} else {
		delete(p.next, id)
	}
	if err := p.saveLocked(); err != nil && p.logger != nil {
		p.logger.Warn("acron: save failed", "error", err)
	}
}

func (p *AcronPlugin) loadLocked() error {
	var sf storeFile
	if err := readJSON(filepath.Join(p.dir, tasksFile), &sf); err != nil {
		return err
	}
	p.tasks = p.tasks[:0]
	maxID := 0
	for _, t := range sf.Tasks {
		if t == nil || t.ID <= 0 || !validType(t.Type) {
			continue
		}
		if _, err := parseCronExpr(t.Cron); err != nil {
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

func (p *AcronPlugin) saveLocked() error {
	return writeJSON(filepath.Join(p.dir, tasksFile), storeFile{NextID: p.nextID, Tasks: p.tasks})
}

func (p *AcronPlugin) indexLocked(id int) int {
	for i, t := range p.tasks {
		if t.ID == id {
			return i
		}
	}
	return -1
}

func validType(tp string) bool {
	for _, v := range taskTypes {
		if v == tp {
			return true
		}
	}
	return false
}

// tl returns a Tlocal picker using the owner's panel language.
func (p *AcronPlugin) tl() func(string, string) string {
	lang := "zh-CN"
	if p.host != nil {
		if id := p.host.SelfID(); id != 0 {
			lang = p.host.Lang(id)
		}
	}
	return func(zh, en string) string {
		if lang == "en-US" {
			return en
		}
		return zh
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// validTimezone checks and normalizes a panel timezone.
func validTimezone(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if strings.EqualFold(s, "local") {
		return "", nil
	}
	l, err := time.LoadLocation(s)
	if err != nil {
		return "", plugin.Invalid("不是有效的时区名，例如 Asia/Shanghai", "not a valid zone, e.g. Asia/Shanghai")
	}
	return l.String(), nil
}

// setZone applies a timezone and reschedules everything.
func (p *AcronPlugin) setZone(name string) {
	loc := time.Local
	if name != "" {
		if l, err := time.LoadLocation(name); err == nil {
			loc = l
		}
	}
	p.mu.Lock()
	p.loc = loc
	p.applyZoneLocked()
	now := time.Now()
	p.next = map[int]time.Time{}
	for _, t := range p.tasks {
		if t.Disabled {
			continue
		}
		if at := p.taskNext(*t, now); !at.IsZero() {
			p.next[t.ID] = at
		}
	}
	p.mu.Unlock()
	p.poke()
}

// applyZoneLocked reads the panel timezone once (no rescheduling; caller holds mu).
func (p *AcronPlugin) applyZoneLocked() {
	if p.set == nil {
		return
	}
	name := p.set.String("timezone")
	if name == "" {
		return
	}
	if l, err := time.LoadLocation(name); err == nil {
		p.loc = l
	}
}

// escapeChat renders a task's chat for the list output.
func escapeChat(t Task) string {
	if t.Display != "" {
		return plugin.Escape(t.Display)
	}
	if t.Chat != "" {
		return plugin.Code(t.Chat)
	}
	return plugin.Code(t.ChatID)
}

func (p *AcronPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	sub := ""
	if len(ctx.Args) > 0 {
		sub = strings.ToLower(ctx.Args[0])
	}
	rest := []string{}
	if len(ctx.Args) > 1 {
		rest = ctx.Args[1:]
	}

	// The raw first line is needed for remarks (may contain spaces).
	firstLine := ctx.Message.Message.Message
	if i := strings.IndexAny(firstLine, "\r\n"); i >= 0 {
		firstLine = firstLine[:i]
	}

	switch sub {
	case "", "help":
		return ctx.Edit(helpText(ctx))
	case "ls":
		return p.cmdList(ctx, rest)
	case "rm":
		return p.cmdRm(ctx, rest)
	case "on", "off":
		return p.cmdToggle(ctx, rest, sub == "on")
	}
	if !validType(sub) {
		return ctx.Edit("❌ " + fmt.Sprintf(ctx.Tlocal("未知子命令: %s", "Unknown subcommand: %s"), plugin.Code(sub)) + "\n\n" + plugin.Code("acron help"))
	}
	return p.cmdAdd(ctx, sub, rest, firstLine)
}

func helpText(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	cronExample := "0 0 2 * * *"
	var b strings.Builder
	b.WriteString("⏰ **" + tl("Cron 定时任务", "Cron Tasks") + "**\n\n")
	b.WriteString(tl("表达式为 6 个字段：秒 分 时 日 月 周，如 `", "6-field expression: sec min hour dom mon dow, e.g. `") + cronExample + "`\n\n")
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	b.WriteString("**" + tl("任务类型", "Task types") + "**\n")
	b.WriteString(line("acron send <cron> <对话ID|@名|话题ID> [备注]",
		"回复一条纯文本消息，按计划重发（保留格式）", "reply to a text message to re-send it on schedule (formatting kept)"))
	b.WriteString(line("acron cmd <cron> <对话ID|@名> [备注]\n<命令行>",
		"按计划把命令发到对话并执行", "send the command line to a chat on schedule and run it"))
	b.WriteString(line("acron copy <cron> <对话ID|@名|话题ID> [备注]",
		"回复一条消息，按计划无转发头复制发送", "reply to a message to re-send it without the forward header"))
	b.WriteString(line("acron forward <cron> <对话ID|@名|话题ID> [备注]",
		"回复一条消息，按计划转发", "reply to a message to forward it on schedule"))
	b.WriteString(line("acron del <cron> <对话ID|@名> <消息ID> [备注]",
		"按计划删除指定消息", "delete a message id on schedule"))
	b.WriteString(line("acron del_re <cron> <对话ID|@名> <条数> <正则> [备注]",
		"按计划删除最近条数内匹配正则的消息", "delete recent messages matching a regex on schedule"))
	b.WriteString(line("acron pin <cron> <对话ID|@名> <消息ID> <通知1/0> <仅自己1/0> [备注]",
		"按计划置顶消息", "pin a message on schedule"))
	b.WriteString(line("acron unpin <cron> <对话ID|@名> <消息ID> [备注]",
		"按计划取消置顶", "unpin a message on schedule"))
	b.WriteString("\n**" + tl("管理", "Manage") + "**\n")
	b.WriteString(line("acron ls [all|类型]", "列出任务（本会话/全部/按类型）", "list tasks (this chat / all / by type)"))
	b.WriteString(line("acron rm <ID>", "删除任务", "delete a task"))
	b.WriteString(line("acron on|off <ID>", "启用/禁用任务", "enable/disable a task"))
	b.WriteString("\n💡 " + tl("时区在机器人面板里设置", "The timezone is set in the bot panel"))
	return b.String()
}
