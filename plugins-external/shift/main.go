// Package main implements the shift plugin: rule-based automatic message
// forwarding between chats, ported from TeleBox's shift plugin.
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "shift",
	Description: "智能转发规则：把源对话的新消息自动转发到目标对话",
	DescEN:      "Forwarding rules: auto-forward new messages from source chats to target chats",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

const (
	rulesFile  = "rules.json"
	statsFile  = "stats.json"
	maxRules   = 100 // rules per installation
	maxHistory = 30  // days of per-day stats kept
	albumDelay = 1200 * time.Millisecond
)

// ShiftPlugin owns forwarding rules and the message listener that applies them.
type ShiftPlugin struct {
	mu       sync.Mutex
	statsMu  sync.Mutex
	host     plugin.Host
	dir      string
	rules    map[int64]*Rule
	order    []int64 // insertion order of source ids
	stats    *Stats
	selfID   int64
	resolver plugin.PeerResolver
	api      *tg.Client
	logger   plugin.Logger

	listenStop func()
	albums     *albumForwarder
	wgs        sync.WaitGroup // forward workers and backups
	stopped    bool
}

func New() *ShiftPlugin {
	return &ShiftPlugin{rules: map[int64]*Rule{}, stats: &Stats{}}
}

func (p *ShiftPlugin) Name() string        { return "shift" }
func (p *ShiftPlugin) Description() string { return Metadata.Description }
func (p *ShiftPlugin) DescEN() string      { return Metadata.DescEN }

func (p *ShiftPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	host := mgr.Host()
	p.mu.Lock()
	p.host = host
	p.selfID = host.SelfID()
	p.resolver = host.PeerResolver()
	p.api = host.API()
	p.logger = host.Logger(p.Name())
	p.mu.Unlock()
	dir, err := host.DataDir(p.Name())
	if err != nil {
		return fmt.Errorf("shift: data dir: %w", err)
	}
	p.mu.Lock()
	p.dir = dir
	p.mu.Unlock()
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "shift",
		Description: "设置/管理自动转发规则，可按类型过滤、关键词黑名单、正则白名单，附统计与历史备份",
		DescEN:      "Manage auto-forward rules with type filters, keyword blacklist, regex whitelist, plus stats and history backup",
		Usage:       "shift set <源> <目标> [类型…] · shift list · shift del|pause|resume <序号> · shift filter|whitelist <序号> … · shift stats · shift backup <源> <目标> [数量] · shift help",
		UsageEN:     "shift set <source> <target> [types…] · shift list · shift del|pause|resume <n> · shift filter|whitelist <n> … · shift stats · shift backup <source> <target> [count] · shift help",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

// Start loads persisted state and registers the message listener.
func (p *ShiftPlugin) Start(_ context.Context) error {
	p.load()
	p.mu.Lock()
	host, stopped := p.host, p.stopped
	p.stopped = false
	if stopped {
		p.wgs = sync.WaitGroup{}
	}
	p.albums = newAlbumForwarder(p.fireAlbum)
	p.mu.Unlock()
	if host != nil && p.listenStop == nil {
		p.listenStop = host.Listen(p.Name(), p.onMessage)
	}
	return nil
}

// Stop removes the listener, waits for in-flight forwards and persists stats.
func (p *ShiftPlugin) Stop(ctx context.Context) error {
	if p.listenStop != nil {
		p.listenStop()
		p.listenStop = nil
	}
	p.mu.Lock()
	p.stopped = true
	albums := p.albums
	p.albums = nil
	p.mu.Unlock()
	if albums != nil {
		albums.stop()
	}
	done := make(chan struct{})
	go func() { p.wgs.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(15 * time.Second):
	}
	p.saveStats()
	return nil
}

// albumsRef returns the current album forwarder, if any.
func (p *ShiftPlugin) albumsRef() *albumForwarder {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.albums
}

// fireAlbum forwards one completed media group.
func (p *ShiftPlugin) fireAlbum(key albumKey, ids []int) {
	p.mu.Lock()
	rule := p.rules[key.chatID]
	stopped := p.stopped
	p.mu.Unlock()
	if rule == nil || rule.Paused || stopped || rule.Target == key.chatID {
		return
	}
	p.wgs.Add(1)
	go func() {
		defer p.wgs.Done()
		ctx := context.Background()
		src, err := p.peerFor(key.chatID)
		if err != nil {
			p.logWarn("shift: resolve source failed", key.chatID, err)
			return
		}
		dst, err := p.peerFor(rule.Target)
		if err != nil {
			p.logWarn("shift: resolve target failed", rule.Target, err)
			return
		}
		if err := forwardMessages(ctx, p.api, src.Peer, dst.Peer, ids, rule.has("silent"), rule.TopicID); err != nil {
			if !isChatRestricted(err) {
				p.logWarn("shift: album forward failed", key.chatID, err)
			}
			return
		}
		p.statsMu.Lock()
		p.stats.add(key.chatID)
		p.statsMu.Unlock()
	}()
}

// peerFor resolves a chat id to an input peer with best-effort title caching.
func (p *ShiftPlugin) peerFor(chatID int64) (*chatInfo, error) {
	ctx := context.Background()
	if p.resolver == nil {
		return nil, fmt.Errorf("no peer resolver")
	}
	if chatID == p.selfID && chatID != 0 {
		return &chatInfo{Peer: &tg.InputPeerSelf{}, ChatID: chatID, IsUser: true, Title: "Saved Messages"}, nil
	}
	peer, err := p.resolver.ResolveFromChatID(ctx, chatID)
	if err != nil {
		return nil, err
	}
	return infoOf(peer), nil
}

func (p *ShiftPlugin) logWarn(msg string, id int64, err error) {
	if p.logger != nil {
		p.logger.Warn(msg, "chat", id, "error", err)
	}
}
