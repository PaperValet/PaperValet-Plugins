// Package main implements the lottery plugin: group lotteries where members
// join by sending a keyword, prizes come from stock-managed warehouses, and
// the draw picks winners with a fair crypto/rand shuffle.
package main

import (
	"context"
	"sync"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	claimTimeout   = 24 * time.Hour // pending prizes expire after this
	notifyInterval = time.Second    // spacing between winner DMs
	apiTimeout     = 30 * time.Second
	deleteDelay    = 5 * time.Second
	maxListLen     = 3800 // participants list sent as text below this
)

var Metadata = &plugin.PluginMetadata{
	Name:        "lottery",
	Description: "群抽奖",
	DescEN:      "Group lottery",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// lottery is one draw activity.
type lottery struct {
	ID           int64         `json:"id"`
	ChatID       int64         `json:"chat_id"`
	Title        string        `json:"title"`
	Keyword      string        `json:"keyword"`
	MaxUsers     int           `json:"max_participants"`
	WinnerCnt    int           `json:"winner_count"`
	Warehouse    string        `json:"prize_warehouse"`
	CreatorID    int64         `json:"creator_id"`
	CreatedAt    int64         `json:"created_at"`
	MessageID    int           `json:"message_id,omitempty"` // pinned announcement
	Status       string        `json:"status"`               // active|completed
	AutoDrawAt   int64         `json:"auto_draw_at,omitempty"`
	Participants []participant `json:"participants"`
	Winners      []winner      `json:"winners,omitempty"`
}

// participant is one joined user.
type participant struct {
	UserID    int64  `json:"user_id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	JoinedAt  int64  `json:"joined_at"`
}

// winner is a drawn winner.
type winner struct {
	UserID     int64  `json:"user_id"`
	Username   string `json:"username,omitempty"`
	FirstName  string `json:"first_name,omitempty"`
	LastName   string `json:"last_name,omitempty"`
	Prize      string `json:"prize"`
	Status     string `json:"status"` // pending|sent|expired
	AssignedAt int64  `json:"assigned_at"`
	ExpiresAt  int64  `json:"expires_at"`
}

type LotteryPlugin struct {
	mu         sync.Mutex
	host       plugin.Host
	log        plugin.Logger
	store      *store
	ctx        context.Context
	cancel     context.CancelFunc
	stopListen func()
	wg         sync.WaitGroup
}

func New() *LotteryPlugin { return &LotteryPlugin{} }

func (p *LotteryPlugin) Name() string        { return "lottery" }
func (p *LotteryPlugin) Description() string { return Metadata.Description }
func (p *LotteryPlugin) DescEN() string      { return Metadata.DescEN }

func (p *LotteryPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.log = p.host.Logger(p.Name())
	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return err
	}
	p.store = newStore(dir)
	if err := p.host.Bot(p.Name()).SetPage(&plugin.Page{
		Title: "抽奖", TitleEN: "Lotteries",
		Handle: p.page,
	}); err != nil && err != plugin.ErrBotNotReady {
		return err
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "lottery",
		Description: "群抽奖：创建后发送关键词报名，人满或定时自动开奖，也可手动开奖",
		DescEN:      "Group lottery: join by keyword, auto-draw when full or on schedule, manual draw too",
		Usage:       "lottery create <标题> <关键词> <人数> <中奖数> [仓库] [at 时间|notify] · lottery draw · lottery delete · lottery status · lottery list · lottery winners · lottery claim <ID/@用户名> · lottery prize create|add|list|clear <…> · lottery help",
		UsageEN:     "lottery create <title> <keyword> <max> <winners> [warehouse] [at time|notify] · lottery draw · lottery delete · lottery status · lottery list · lottery winners · lottery claim <ID/@user> · lottery prize create|add|list|clear <…> · lottery help",
		Plugin:      p.Name(),
		Category:    "group",
		Handler:     p.handle,
	})
}

// Start loads state, starts the join listener and the auto-draw scheduler.
func (p *LotteryPlugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopListen != nil {
		return nil
	}
	if err := p.store.load(); err != nil {
		return err
	}
	p.expireClaimsLocked(time.Now())
	p.saveLocked()
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.stopListen = p.host.Listen(p.Name(), p.onMessage)
	p.wg.Add(1)
	go p.autoLoop(p.ctx)
	return nil
}

// Stop removes the listener, stops background work and waits for it.
func (p *LotteryPlugin) Stop(context.Context) error {
	p.mu.Lock()
	stop, cancel := p.stopListen, p.cancel
	p.stopListen, p.cancel = nil, nil
	p.mu.Unlock()
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel()
	}
	p.wg.Wait()
	return nil
}

func (p *LotteryPlugin) saveLocked() error {
	if err := p.store.save(); err != nil && p.log != nil {
		p.log.Warn("lottery: save failed", "error", err)
		return err
	}
	return nil
}

// lifetime returns the plugin's lifetime context.
func (p *LotteryPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}
