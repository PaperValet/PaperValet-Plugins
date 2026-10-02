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

	host    plugin.Host
	set     plugin.Settings
	ownerID int64
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
	p.host = mgr.Host()
	if h := mgr.Host(); h != nil {
		p.ownerID = h.SelfID()
	}
	set, err := mgr.Host().Settings(&plugin.SettingsSpec{
		Plugin:  p.Name(),
		Title:   "💾 保存",
		TitleEN: "💾 Save",
		Settings: []plugin.Setting{
			{
				Key: "target", Label: "默认保存目标", LabelEN: "Default target",
				Hint:   "@用户名 / 数字 chatID / me（收藏夹）/ local（本地目录）",
				HintEN: "@username / numeric chat id / me (Saved Messages) / local (disk)",
				Kind:   plugin.SettingText, Default: "me", Validate: validPanelTarget,
			},
			{
				Key: "show_source", Label: "保存后回复来源", LabelEN: "Reply source after saving",
				Hint:   "保存后回复一条带原消息链接的来源消息",
				HintEN: "Reply a source message with the original link after saving",
				Kind:   plugin.SettingToggle,
			},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	p.mu.Lock()
	p.db = loadDB(p.cfgPath, p.legacy)
	p.mu.Unlock()
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "save",
		Description: "保存/转发消息：回复、链接、链接范围，受限会话自动下载重传，可保存到本地",
		DescEN:      "Save/forward messages by reply, links or link ranges; re-uploads from restricted chats; can save locally",
		Usage:       "save（回复）· save <链接…> [临时目标] · save <链接1>|<链接2> · save help",
		UsageEN:     "save (reply) · save <links…> [temp target] · save <link1>|<link2> · save help",
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
	if c, ok := p.db.Users[fmt.Sprint(uid)]; ok {
		p.mu.Unlock()
		if c.Target == "" {
			c.Target = "me"
		}
		return c
	}
	p.mu.Unlock()
	// The panel owns the owner's defaults; the legacy per-user file only
	// supplies values for delegated users.
	if p.set != nil && (p.ownerID == 0 || uid == p.ownerID) {
		return UserConfig{Target: p.set.String("target"), ShowSource: p.set.Bool("show_source")}
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
	b.WriteString("\n**" + tl("设置（机器人面板）", "Settings (bot panel)") + "**\n")
	b.WriteString(tl("默认保存目标、保存后是否回复来源，在机器人的 /menu 里调整\n", "Default target and source replies are set in the bot's /menu\n"))
	b.WriteString("\n**local**\n")
	b.WriteString(tl("媒体保存到 ", "Media is saved to ") + plugin.Code("save/<chatId>/") + tl("，旁路 .json 元数据并生成索引；纯文本跳过", " with a .json sidecar and an index file; text-only messages are skipped") + "\n\n")
	b.WriteString("💡 " + tl("会话禁止转发时会自动下载后重新发送", "When a chat forbids forwarding, content is downloaded and re-sent"))
	return b.String()
}

func (p *SavePlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil || ctx.API == nil {
		return plugin.ErrNoMessage
	}
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
