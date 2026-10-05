// Package main implements the bs plugin: forward ("保送") the replied
// message and the N-1 messages after it to one or more configured
// targets, sequentially (first success stops) or broadcast (all targets).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	dataFile      = "config.json"
	modeSequence  = "sequence"
	modeBroadcast = "broadcast"
	maxSpan       = 500 // search span cap for message collection
	batchSize     = 100 // ids per GetMessages call
)

var Metadata = &plugin.PluginMetadata{
	Name:        "bs",
	Description: "回复消息一键保送到多个目标",
	DescEN:      "Forward replied messages to multiple targets",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// BsPlugin is the plugin instance.
type BsPlugin struct {
	mu     sync.Mutex
	dir    string
	db     *bsDB
	host   plugin.Host
	set    plugin.Settings
	logger plugin.Logger
}

func New() *BsPlugin { return &BsPlugin{db: &bsDB{NextID: 1, Mode: modeSequence}} }

func (p *BsPlugin) Name() string        { return "bs" }
func (p *BsPlugin) Description() string { return Metadata.Description }
func (p *BsPlugin) DescEN() string      { return Metadata.DescEN }

func (p *BsPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	host := mgr.Host()
	p.host = host
	p.logger = host.Logger(p.Name())
	dir, err := host.DataDir(p.Name())
	if err != nil {
		return err
	}
	p.dir = dir
	if err := p.loadDB(); err != nil {
		return fmt.Errorf("bs: load config: %w", err)
	}
	set, err := host.Settings(&plugin.SettingsSpec{
		Plugin:  p.Name(),
		Title:   "🚚 保送",
		TitleEN: "🚚 Forwarder",
		Settings: []plugin.Setting{{
			Key: "mode", Label: "发送模式", LabelEN: "Send mode",
			Hint:   "顺序=首个成功即停；群发=每个目标都尝试",
			HintEN: "sequence stops at the first success; broadcast tries every target",
			Kind:   plugin.SettingChoice, Default: modeSequence,
			Choices: []plugin.Choice{
				{Value: modeSequence, Label: "顺序（首个成功即停）", LabelEN: "sequence (stop at first success)"},
				{Value: modeBroadcast, Label: "群发（全部目标）", LabelEN: "broadcast (all targets)"},
			},
		}},
	})
	if err != nil {
		return err
	}
	p.set = set
	p.mu.Lock()
	p.db.Mode = set.String("mode")
	p.mu.Unlock()
	if err := host.Bot(p.Name()).SetPage(&plugin.Page{
		Title: "保送目标", TitleEN: "Forward targets",
		Handle: p.page,
	}); err != nil && !errors.Is(err, plugin.ErrBotNotReady) {
		return err
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "bs",
		Description: "回复消息一键保送到多个目标：从被回复消息起连续转发 N 条，支持话题、顺序/群发模式",
		DescEN:      "Forward the replied message (and the next N-1) to configured targets, with topics and sequence/broadcast modes",
		Usage:       "bs（回复）· bs <N>（回复）· bs add <对话ID|@名|me>[|话题ID] · bs list · bs del <ID> · bs on|off <ID> · bs help",
		UsageEN:     "bs (reply) · bs <N> (reply) · bs add <chatID|@name|me>[|topicID] · bs list · bs del <ID> · bs on|off <ID> · bs help",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

// Start re-reads the panel mode (it may have changed while stopped).
func (p *BsPlugin) Start(_ context.Context) error {
	if p.set != nil {
		p.mu.Lock()
		p.db.Mode = p.set.String("mode")
		p.mu.Unlock()
	}
	return nil
}

func (p *BsPlugin) Stop(_ context.Context) error { return nil }

// ---------------------------------------------------------------- storage

type target struct {
	ID        int      `json:"id"`
	Target    string   `json:"target"` // original user input
	TopicID   int      `json:"topic_id,omitempty"`
	Display   string   `json:"display,omitempty"`
	Disabled  bool     `json:"disabled,omitempty"`
	Peer      *peerRef `json:"peer,omitempty"`
	CreatedAt int64    `json:"created_at"`
}

type bsDB struct {
	NextID  int       `json:"next_id"`
	Mode    string    `json:"mode,omitempty"`
	Targets []*target `json:"targets"`
}

func (p *BsPlugin) loadDB() error {
	var db bsDB
	b, err := os.ReadFile(filepath.Join(p.dir, dataFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			p.db = &bsDB{NextID: 1, Mode: modeSequence}
			return nil
		}
		return err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &db); err != nil {
			return err
		}
	}
	if db.NextID < 1 {
		db.NextID = 1
	}
	if db.Mode != modeBroadcast {
		db.Mode = modeSequence
	}
	kept := db.Targets[:0]
	maxID := 0
	for _, t := range db.Targets {
		if t == nil || t.ID <= 0 || strings.TrimSpace(t.Target) == "" {
			continue
		}
		if t.Peer != nil && t.Peer.input() == nil {
			t.Peer = nil // unknown peer type; resolve from Target next run
		}
		if t.ID > maxID {
			maxID = t.ID
		}
		kept = append(kept, t)
	}
	db.Targets = kept
	if db.NextID <= maxID {
		db.NextID = maxID + 1
	}
	p.db = &db
	return nil
}

func (p *BsPlugin) saveLocked() error {
	return writeJSON(filepath.Join(p.dir, dataFile), p.db)
}

// activeTargets returns a snapshot of enabled targets ordered by id.
func (p *BsPlugin) activeTargets() []*target {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*target
	for _, t := range p.db.Targets {
		if !t.Disabled {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *BsPlugin) mode() string {
	if p.set != nil {
		if m := p.set.String("mode"); m == modeBroadcast || m == modeSequence {
			return m
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.db.Mode
}

// ---------------------------------------------------------------- command

func helpText(tl func(zh, en string) string) string {
	cmd := func(c, zh, en string) string { return plugin.Code(c) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("🚚 **" + tl("保送 — 转发被回复的消息", "bs — forward replied messages") + "**\n\n")
	b.WriteString(cmd("bs", "回复消息：保送它（默认 1 条）", "reply to a message: forward it (default 1)"))
	b.WriteString(cmd("bs <N>", "从被回复消息起连续保送 N 条（跳过已删）", "forward N consecutive messages from the replied one (deleted are skipped)"))
	b.WriteString(cmd("bs add <对话ID|@名|me>[|话题ID]", "添加目标，可用全角 ｜ 分隔话题", "add a target; full-width ｜ also separates the topic"))
	b.WriteString(cmd("bs list", "列出所有目标", "list all targets"))
	b.WriteString(cmd("bs del <ID>", "删除目标", "delete a target"))
	b.WriteString(cmd("bs on|off <ID>", "启用 / 禁用目标", "enable / disable a target"))
	b.WriteString("\n**" + tl("设置（机器人面板）", "Settings (bot panel)") + "**\n")
	b.WriteString(tl("发送模式（顺序=首个成功即停 / 群发=全部尝试）在机器人的 /menu 里调整；目标也可以在 bs 页面里管理\n",
		"Send mode (sequence stops at first success / broadcast tries all) is in the bot's /menu; targets can also be managed on the bs page\n"))
	return b.String()
}

func (p *BsPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil || ctx.API == nil {
		return plugin.ErrNoMessage
	}
	sub := ""
	if len(ctx.Args) > 0 {
		sub = strings.ToLower(ctx.Args[0])
	}
	switch {
	case sub == "" || isAllDigits(sub):
		return p.cmdForward(ctx)
	case sub == "help":
		return ctx.Edit(helpText(ctx.Tlocal))
	case sub == "add":
		return p.cmdAdd(ctx)
	case sub == "list":
		return p.cmdList(ctx)
	case sub == "del":
		return p.cmdDel(ctx)
	case sub == "on":
		return p.cmdToggle(ctx, false)
	case sub == "off":
		return p.cmdToggle(ctx, true)
	}
	return ctx.Edit("❓ **" + ctx.Tlocal("未知子命令", "unknown subcommand") + "**\n\n" + helpText(ctx.Tlocal))
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (p *BsPlugin) cmdAdd(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	raw := strings.TrimSpace(ctx.GetArg(1))
	if raw == "" {
		return ctx.Edit("❌ " + tl("请提供对话 ID、@名称或 me", "provide a chat id, @name or me"))
	}
	spec, err := parseTargetSpec(raw)
	if err != nil || spec.Chat == "" {
		return ctx.Edit("❌ " + tl("无效的目标", "invalid target"))
	}
	peer, display, err := p.resolveEntity(ctx, spec.Chat)
	if err != nil {
		return ctx.Edit("❌ **" + tl("无法解析目标", "cannot resolve target") + "**\n" + plugin.Code(err.Error()))
	}
	p.mu.Lock()
	t := &target{
		ID:        p.db.NextID,
		Target:    spec.Chat,
		TopicID:   spec.TopicID,
		Display:   display,
		Peer:      refFromPeer(peer),
		CreatedAt: time.Now().Unix(),
	}
	p.db.NextID++
	p.db.Targets = append(p.db.Targets, t)
	saveErr := p.saveLocked()
	p.mu.Unlock()
	if saveErr != nil {
		return ctx.Edit("❌ " + plugin.Code(saveErr.Error()))
	}
	line := fmt.Sprintf("✅ "+tl("目标 %d 已添加", "target %d added")+": %s", t.ID, plugin.Escape(display))
	if spec.TopicID > 0 {
		line += tl(" ｜ 话题 ", " | topic ") + plugin.Code(spec.TopicID)
	}
	return ctx.Edit(line)
}

func (p *BsPlugin) cmdList(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	targets := append([]*target(nil), p.db.Targets...)
	p.mu.Unlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })

	modeLabel := tl("顺序（首个成功即停）", "sequence (stop at first success)")
	if p.mode() == modeBroadcast {
		modeLabel = tl("群发（全部目标）", "broadcast (all targets)")
	}
	var b strings.Builder
	b.WriteString("🚚 **" + tl("保送目标", "Forward targets") + "**  " + tl("模式", "mode") + ": " + plugin.Bold(modeLabel) + "\n")
	if len(targets) == 0 {
		b.WriteString("\n" + tl("暂无目标，用 ", "No targets yet; add one with ") + plugin.Code("bs add") + "\n")
		return ctx.Edit(b.String())
	}
	var enabled, disabled []*target
	for _, t := range targets {
		if t.Disabled {
			disabled = append(disabled, t)
		} else {
			enabled = append(enabled, t)
		}
	}
	render := func(list []*target) {
		for _, t := range list {
			b.WriteString("\n" + renderTarget(t, tl))
		}
	}
	if len(enabled) > 0 {
		b.WriteString("\n🔛 " + tl("已启用", "Enabled") + "\n")
		render(enabled)
	}
	if len(disabled) > 0 {
		b.WriteString("\n⏸ " + tl("已禁用", "Disabled") + "\n")
		render(disabled)
	}
	return ctx.Edit(b.String())
}

func renderTarget(t *target, tl func(zh, en string) string) string {
	disp := t.Display
	if disp == "" {
		disp = plugin.Code(t.Target)
	} else {
		disp = plugin.Escape(disp)
	}
	s := fmt.Sprintf("• %s  %s", plugin.Code("#"+fmt.Sprint(t.ID)), disp)
	if t.TopicID > 0 {
		s += tl(" ｜ 话题 ", " | topic ") + plugin.Code(t.TopicID)
	}
	return s
}

func (p *BsPlugin) cmdDel(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	id, err := parseID(ctx.GetArg(1))
	if err != nil {
		return ctx.Edit("❌ " + tl("请提供要删除的目标 ID", "provide the target id to delete"))
	}
	p.mu.Lock()
	idx := -1
	for i, t := range p.db.Targets {
		if t.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		p.mu.Unlock()
		return ctx.Edit("❌ " + fmt.Sprintf(tl("目标 %d 不存在", "target %d does not exist"), id))
	}
	p.db.Targets = append(p.db.Targets[:idx], p.db.Targets[idx+1:]...)
	saveErr := p.saveLocked()
	p.mu.Unlock()
	if saveErr != nil {
		return ctx.Edit("❌ " + plugin.Code(saveErr.Error()))
	}
	return ctx.Edit(fmt.Sprintf("✅ "+tl("目标 %d 已删除", "target %d deleted"), id))
}

func (p *BsPlugin) cmdToggle(ctx *plugin.CommandContext, off bool) error {
	tl := ctx.Tlocal
	id, err := parseID(ctx.GetArg(1))
	if err != nil {
		return ctx.Edit("❌ " + tl("请提供目标 ID", "provide the target id"))
	}
	p.mu.Lock()
	var found *target
	for _, t := range p.db.Targets {
		if t.ID == id {
			found = t
			break
		}
	}
	if found == nil {
		p.mu.Unlock()
		return ctx.Edit("❌ " + fmt.Sprintf(tl("目标 %d 不存在", "target %d does not exist"), id))
	}
	found.Disabled = off
	saveErr := p.saveLocked()
	p.mu.Unlock()
	if saveErr != nil {
		return ctx.Edit("❌ " + plugin.Code(saveErr.Error()))
	}
	if off {
		return ctx.Edit(fmt.Sprintf("⏸ "+tl("目标 %d 已禁用", "target %d disabled"), id))
	}
	return ctx.Edit(fmt.Sprintf("🔛 "+tl("目标 %d 已启用", "target %d enabled"), id))
}

// ---------------------------------------------------------------- bot page

func (p *BsPlugin) page(c *plugin.BotContext) (*plugin.View, error) {
	tl := c.Tlocal
	switch {
	case c.Data == "add":
		c.Ask("new")
		return &plugin.View{Text: "✏️ " + tl(
			"发送要添加的目标：`对话ID|@名|me[|话题ID]`（全角 ｜ 也可以）",
			"Send the target to add: `chatID|@name|me[|topicID]` (full-width ǀ works too)")}, nil
	case c.Data == "new":
		spec, err := parseTargetSpec(strings.TrimSpace(c.Input))
		if err != nil || spec.Chat == "" {
			c.Alert("❌ " + tl("无效的目标", "invalid target"))
			break
		}
		peer, display, rerr := p.resolveEntityBot(c.Context(), spec.Chat)
		if rerr != nil {
			c.Alert("❌ " + tl("无法解析目标", "cannot resolve target") + "\n" + plugin.Code(rerr.Error()))
			break
		}
		p.mu.Lock()
		id := p.db.NextID
		p.db.Targets = append(p.db.Targets, &target{
			ID: id, Target: spec.Chat, TopicID: spec.TopicID,
			Display: display, Peer: refFromPeer(peer), CreatedAt: time.Now().Unix(),
		})
		p.db.NextID++
		err = p.saveLocked()
		p.mu.Unlock()
		if err != nil {
			c.Alert("❌ " + plugin.Code(err.Error()))
			break
		}
		c.Toast(fmt.Sprintf(tl("已添加 #%d", "added #%d"), id))
	}
	if op, idStr, ok := strings.Cut(c.Data, ":"); ok {
		id, err := strconv.Atoi(idStr)
		if err == nil {
			saved := true
			p.mu.Lock()
			for i, t := range p.db.Targets {
				if t.ID != id {
					continue
				}
				switch op {
				case "rm":
					p.db.Targets = append(p.db.Targets[:i], p.db.Targets[i+1:]...)
				case "off":
					t.Disabled = true
				case "on":
					t.Disabled = false
				}
				if err := p.saveLocked(); err != nil {
					saved = false
					c.Alert("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Code(err.Error()))
				}
				break
			}
			p.mu.Unlock()
			if saved {
				switch op {
				case "rm":
					c.Toast(fmt.Sprintf(tl("已删除 #%d", "deleted #%d"), id))
				case "off":
					c.Toast(fmt.Sprintf(tl("已禁用 #%d", "disabled #%d"), id))
				case "on":
					c.Toast(fmt.Sprintf(tl("已启用 #%d", "enabled #%d"), id))
				}
			}
		}
	}
	return p.renderPage(tl), nil
}

func (p *BsPlugin) renderPage(tl func(zh, en string) string) *plugin.View {
	p.mu.Lock()
	targets := append([]*target(nil), p.db.Targets...)
	p.mu.Unlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	v := &plugin.View{Text: "🚚 **" + tl("保送目标", "Forward targets") + "**  " + plugin.Code(len(targets))}
	if len(targets) == 0 {
		v.Text += "\n\n" + tl("还没有目标，点 ➕ 添加", "No targets yet; tap ➕ to add one")
		v.Buttons = append(v.Buttons, plugin.Row(plugin.Btn("➕ "+tl("添加目标", "Add target"), "add").Success()))
		return v
	}
	for _, t := range targets {
		v.Text += "\n\n" + renderTarget(t, tl)
		var row []plugin.Button
		if t.Disabled {
			row = append(row, plugin.Btn("▶️ #"+fmt.Sprint(t.ID), fmt.Sprintf("on:%d", t.ID)))
		} else {
			row = append(row, plugin.Btn("⏸ #"+fmt.Sprint(t.ID), fmt.Sprintf("off:%d", t.ID)))
		}
		row = append(row, plugin.Btn("🗑 #"+fmt.Sprint(t.ID), fmt.Sprintf("rm:%d", t.ID)).Danger())
		v.Buttons = append(v.Buttons, row)
	}
	v.Buttons = append(v.Buttons, plugin.Row(plugin.Btn("➕ "+tl("添加目标", "Add target"), "add").Success()))
	return v
}
