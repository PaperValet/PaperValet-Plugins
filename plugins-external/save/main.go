package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	configPath   = "data/save/config.json"
	legacyPath   = "data/save_db.json"
	tempDirPath  = "data/save/tmp"
	localRootDir = "save"
	maxRange     = 500 // messages per range command
	maxLinks     = 50  // links per command
	editInterval = 1500 * time.Millisecond
)

var Metadata = &plugin.PluginMetadata{
	Name:        "save",
	Description: "突破限制保存 / 转发消息",
	DescEN:      "Save / forward messages, bypassing forward restrictions",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

var errStopped = errors.New("plugin is stopping")

// SavePlugin forwards messages to a configurable target and re-uploads
// content when the source chat restricts forwarding.
type SavePlugin struct {
	mu       sync.Mutex
	db       *SaveDB
	cfgPath  string
	legacy   string
	tmpDir   string
	localDir string

	jobsMu  sync.Mutex
	jobs    map[int]context.CancelFunc
	jobSeq  int
	stopped bool
	wg      sync.WaitGroup
}

func New() *SavePlugin {
	return &SavePlugin{
		cfgPath:  configPath,
		legacy:   legacyPath,
		tmpDir:   tempDirPath,
		localDir: localRootDir,
		db:       &SaveDB{Users: map[string]UserConfig{}},
		jobs:     map[int]context.CancelFunc{},
	}
}

func (p *SavePlugin) Name() string        { return "save" }
func (p *SavePlugin) Description() string { return Metadata.Description }
func (p *SavePlugin) DescEN() string      { return Metadata.DescEN }

func (p *SavePlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.mu.Lock()
	p.db = loadDB(p.cfgPath, p.legacy)
	p.mu.Unlock()
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "save",
		Description: "保存/转发消息：回复、链接、链接范围，受限会话自动下载重传，可保存到本地",
		DescEN:      "Save/forward messages by reply, links or link ranges; re-uploads from restricted chats; can save locally",
		Usage:       "save（回复）· save <链接…> [临时目标] · save <链接1>|<链接2> · save to <目标> · save target · save source [on|off] · save help",
		UsageEN:     "save (reply) · save <links…> [temp target] · save <link1>|<link2> · save to <target> · save target · save source [on|off] · save help",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *SavePlugin) Start(_ context.Context) error {
	p.jobsMu.Lock()
	p.stopped = false
	p.jobsMu.Unlock()
	p.cleanTemp()
	return nil
}

// Stop cancels running saves, waits for them and removes temp files.
func (p *SavePlugin) Stop(ctx context.Context) error {
	p.jobsMu.Lock()
	p.stopped = true
	for _, cancel := range p.jobs {
		cancel()
	}
	p.jobsMu.Unlock()
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(15 * time.Second):
	}
	p.cleanTemp()
	return nil
}

func (p *SavePlugin) cleanTemp() {
	entries, err := os.ReadDir(p.tmpDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(p.tmpDir, e.Name()))
	}
}

func (p *SavePlugin) beginJob(parent context.Context) (context.Context, func(), error) {
	p.jobsMu.Lock()
	defer p.jobsMu.Unlock()
	if p.stopped {
		return nil, nil, errStopped
	}
	c, cancel := context.WithCancel(parent)
	p.jobSeq++
	id := p.jobSeq
	p.jobs[id] = cancel
	p.wg.Add(1)
	return c, func() {
		cancel()
		p.jobsMu.Lock()
		delete(p.jobs, id)
		p.jobsMu.Unlock()
		p.wg.Done()
	}, nil
}

func (p *SavePlugin) userConfig(uid int64) UserConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.db.Users[fmt.Sprint(uid)]; ok {
		if c.Target == "" {
			c.Target = "me"
		}
		return c
	}
	return defaultConfig()
}

func (p *SavePlugin) updateConfig(uid int64, f func(*UserConfig)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := fmt.Sprint(uid)
	c, ok := p.db.Users[key]
	if !ok {
		c = defaultConfig()
	}
	old, had := p.db.Users[key]
	f(&c)
	p.db.Users[key] = c
	if err := writeDB(p.cfgPath, p.db); err != nil {
		if had {
			p.db.Users[key] = old
		} else {
			delete(p.db.Users, key)
		}
		return err
	}
	return nil
}

// ---------------------------------------------------------------- commands

func helpText(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("💾 **" + tl("save — 突破限制保存 / 转发消息", "save — save / forward messages past restrictions") + "**\n\n")
	b.WriteString("**" + tl("命令", "Commands") + "**\n")
	b.WriteString(line("save", "回复消息：保存到默认目标（相册整组保存）", "reply to a message: save to the default target (whole album)"))
	b.WriteString(line(tl("save <目标>", "save <target>"), "回复消息：临时改目标", "reply: use a temporary target"))
	b.WriteString(line(tl("save <链接…>", "save <links…>"), "批量保存链接", "save several links"))
	b.WriteString(line(tl("save <链接1>|<链接2>", "save <link1>|<link2>"), fmt.Sprintf("保存两链接之间的消息范围（最多 %d 条）", maxRange), fmt.Sprintf("save the range between two links (max %d)", maxRange)))
	b.WriteString(line(tl("save <链接> <临时目标>", "save <link> <temp target>"), "临时改目标（可用 local）", "temporary target (local works too)"))
	b.WriteString("\n**" + tl("设置", "Settings") + "**\n")
	b.WriteString(line(tl("save to <目标>", "save to <target>"), "默认目标：@user / chatid / me / local", "default target: @user / chat id / me / local"))
	b.WriteString(line("save target", "查看默认目标", "show the default target"))
	b.WriteString(line("save source on|off", "保存后回复来源信息", "reply source info after saving"))
	b.WriteString(line("save source", "查看来源开关", "show the source switch"))
	b.WriteString("\n**local**\n")
	b.WriteString(tl("媒体保存到 ", "Media is saved to ") + plugin.Code("save/<chatId>/") + tl("，旁路 .json 元数据并生成索引；纯文本跳过", " with a .json sidecar and an index file; text-only messages are skipped") + "\n\n")
	b.WriteString("💡 " + tl("会话禁止转发时会自动下载后重新发送", "When a chat forbids forwarding, content is downloaded and re-sent"))
	return b.String()
}

func (p *SavePlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil || ctx.API == nil {
		return plugin.ErrNoMessage
	}
	tl := ctx.Tlocal
	args := ctx.Args
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}
	switch sub {
	case "help":
		if len(args) == 1 {
			return ctx.Edit(helpText(ctx))
		}
	case "source":
		return p.cmdSource(ctx, args[1:])
	case "to":
		if len(args) < 2 {
			return ctx.Edit("❌ " + tl("请指定保存目标", "Please give a target") + "\n\n💡 " +
				plugin.Code("save to @username") + " " + plugin.Code("save to -1001234567890") + " " +
				plugin.Code("save to me") + " " + plugin.Code("save to local"))
		}
		return p.cmdSetTarget(ctx, strings.Join(args[1:], " "))
	case "target":
		if len(args) >= 2 {
			return p.cmdSetTarget(ctx, strings.Join(args[1:], " "))
		}
		cfg := p.userConfig(ctx.Message.UserID)
		return ctx.Edit("📌 " + tl("当前默认目标  ", "Default target  ") + plugin.Code(targetLabel(cfg.Target, tl)) +
			"\n\n💡 " + plugin.Code(tl("save to <目标>", "save to <target>")))
	}
	return p.cmdSave(ctx)
}

func targetLabel(t string, tl func(string, string) string) string {
	switch t {
	case "", "me":
		return tl("收藏夹 (me)", "Saved Messages (me)")
	case "local":
		return tl("本地 (local)", "local disk (local)")
	}
	return t
}

func (p *SavePlugin) cmdSetTarget(ctx *plugin.CommandContext, raw string) error {
	tl := ctx.Tlocal
	t, err := normalizeTarget(raw)
	if err != nil {
		return ctx.Edit("❌ " + tl("目标无效：支持 @用户名、数字 chatID、me 或 local", "Invalid target: use @username, numeric chat id, me or local"))
	}
	if t != "me" && t != "local" {
		if _, _, err := resolveTarget(ctx, t); err != nil {
			return ctx.Edit("❌ " + tl("无法访问目标对话 ", "Cannot access target chat ") + plugin.Code(t) + "\n" + plugin.Escape(err.Error()))
		}
	}
	if err := p.updateConfig(ctx.Message.UserID, func(c *UserConfig) { c.Target = t }); err != nil {
		return ctx.Edit("❌ " + tl("保存配置失败: ", "Failed to save config: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + tl("已设置默认保存目标为 ", "Default target set to ") + plugin.Code(targetLabel(t, tl)))
}

func (p *SavePlugin) cmdSource(ctx *plugin.CommandContext, args []string) error {
	tl := ctx.Tlocal
	if len(args) == 0 {
		cfg := p.userConfig(ctx.Message.UserID)
		state := tl("关闭 ❌", "off ❌")
		if cfg.ShowSource {
			state = tl("开启 ✅", "on ✅")
		}
		return ctx.Edit("📊 " + tl("来源显示  ", "Source info  ") + "**" + state + "**\n\n💡 " + plugin.Code("save source on|off"))
	}
	var on bool
	switch strings.ToLower(args[0]) {
	case "on":
		on = true
	case "off":
		on = false
	default:
		return ctx.Edit("❌ " + tl("无效的参数，使用 ", "Invalid argument, use ") + plugin.Code("save source on|off"))
	}
	if err := p.updateConfig(ctx.Message.UserID, func(c *UserConfig) { c.ShowSource = on }); err != nil {
		return ctx.Edit("❌ " + tl("保存配置失败: ", "Failed to save config: ") + plugin.Escape(err.Error()))
	}
	if on {
		return ctx.Edit("✅ " + tl("已开启来源显示：保存后会回复一条包含原消息链接的来源消息", "Source info on: a source message with the original link is replied after saving"))
	}
	return ctx.Edit("✅ " + tl("已关闭来源显示", "Source info off"))
}

// resolveTarget turns a normalized target into an InputPeer and label.
func resolveTarget(ctx *plugin.CommandContext, t string) (tg.InputPeerClass, string, error) {
	tl := ctx.Tlocal
	switch {
	case t == "" || t == "me":
		return &tg.InputPeerSelf{}, tl("收藏夹", "Saved Messages"), nil
	case ctx.PeerResolver == nil:
		return nil, "", errors.New("no peer resolver")
	case strings.HasPrefix(t, "@"):
		peer, err := ctx.PeerResolver.ResolveUsername(ctx.Context(), t[1:])
		return peer, t, err
	}
	var id int64
	if _, err := fmt.Sscan(t, &id); err != nil {
		return nil, "", fmt.Errorf("invalid target %s", t)
	}
	if id == ctx.SelfID && id != 0 {
		return &tg.InputPeerSelf{}, tl("收藏夹", "Saved Messages"), nil
	}
	peer, err := ctx.PeerResolver.ResolveFromChatID(ctx.Context(), id)
	return peer, t, err
}

// replyTarget returns the replied message id and its chat, or ok=false.
// A plain message inside a forum topic carries a reply header pointing at
// the topic root; that is not a reply.
func replyTarget(ev *plugin.MessageEvent) (chatID int64, msgID int, ok bool) {
	if ev == nil || ev.Message == nil {
		return 0, 0, false
	}
	hdr, isHdr := ev.Message.ReplyTo.(*tg.MessageReplyHeader)
	if !isHdr {
		return 0, 0, false
	}
	id, has := hdr.GetReplyToMsgID()
	if !has || id <= 0 {
		return 0, 0, false
	}
	if hdr.ForumTopic {
		if _, hasTop := hdr.GetReplyToTopID(); !hasTop {
			return 0, 0, false
		}
	}
	chatID = ev.ChatID
	if pid, ok := hdr.GetReplyToPeerID(); ok && pid != nil {
		if c := chatIDOfPeer(pid); c != 0 {
			chatID = c
		}
	}
	return chatID, id, true
}

func sortedKeys(m map[int]*tg.Message) []int {
	ids := make([]int, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}
