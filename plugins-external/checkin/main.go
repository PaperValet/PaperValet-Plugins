// Package main implements the checkin plugin: a daily auto sign-in
// scheduler ported from TeleBox's checkin.ts. The account sends a sign-in
// command to configured bot targets at a random time in a daily window,
// optionally clicks the sign-in inline button, and pushes a summary.
package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // timezone names work without system tzdata

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	pollEvery  = time.Second
	maxTargets = 30
	tickEvery  = 30 * time.Second
	maxText    = 3800
)

var Metadata = &plugin.PluginMetadata{
	Name:        "checkin",
	Description: "每日自动签到",
	DescEN:      "Daily automatic check-in",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type pendingAdd struct {
	promptID  int
	senderID  int64
	expiresAt time.Time
	target    Target
}

type CheckinPlugin struct {
	mu       sync.Mutex
	dir      string
	cfg      config
	loc      *time.Location
	pending  map[int64]*pendingAdd // chatID -> reply-with-command flow
	running  bool
	set      plugin.Settings
	log      plugin.Logger
	api      *tg.Client
	resolver plugin.PeerResolver
	host     plugin.Host

	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}

	stopListen func()
}

func New() *CheckinPlugin {
	return &CheckinPlugin{
		cfg:     defaultConfig(),
		loc:     shanghai,
		pending: map[int64]*pendingAdd{},
		wake:    make(chan struct{}, 1),
	}
}

func (p *CheckinPlugin) Name() string        { return "checkin" }
func (p *CheckinPlugin) Description() string { return Metadata.Description }
func (p *CheckinPlugin) DescEN() string      { return Metadata.DescEN }

func (p *CheckinPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	host := mgr.Host()
	p.host = host
	p.log = host.Logger(p.Name())
	dir, err := host.DataDir(p.Name())
	if err != nil {
		return err
	}
	p.dir = dir
	p.api, p.resolver = host.API(), host.PeerResolver()

	set, err := host.Settings(&plugin.SettingsSpec{
		Plugin:  p.Name(),
		Title:   "✅ 每日签到",
		TitleEN: "✅ Daily Check-In",
		Settings: []plugin.Setting{
			{Key: "timezone", Label: "时区", LabelEN: "Timezone", Kind: plugin.SettingText, Default: "",
				Hint:     "IANA 名称如 Asia/Shanghai，留空用 Asia/Shanghai",
				HintEN:   "An IANA name like Asia/Shanghai; empty keeps Asia/Shanghai",
				Validate: validTimezone},
			{Key: "notify", Label: "自动签到后推送汇总", LabelEN: "Push summary after auto runs", Kind: plugin.SettingToggle, Default: true},
			{Key: "notify_chat", Label: "汇总推送会话", LabelEN: "Summary chat", Kind: plugin.SettingText, Default: "",
				Hint:     "数字 chat id（如 -1001234567890），留空则推到添加目标时的会话",
				HintEN:   "Numeric chat id (e.g. -1001234567890); empty pushes to the chat where the last target was added",
				Validate: validChatID},
		},
		// Re-plan when the timezone changes.
		OnChange: func(key string) {
			if key == "timezone" {
				p.applyZone()
				p.replan()
				p.poke()
			}
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	if err := host.Bot(p.Name()).SetPage(&plugin.Page{
		Title:   "签到目标",
		TitleEN: "Sign-in Targets",
		Handle:  p.page,
	}); err != nil && !errors.Is(err, plugin.ErrBotNotReady) {
		return err
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "checkin",
		Description: "每日自动签到：定时向目标机器人发送签到命令并点击按钮，汇总结果",
		DescEN:      "Daily auto check-in: sends sign-in commands to target bots on schedule, clicks buttons and reports",
		Usage: "checkin · checkin add <ID> <名称> <目标> [data:回调|text:按钮] · checkin del <ID> · checkin list · " +
			"checkin toggle <ID> · checkin test <ID> · checkin status · checkin reset · checkin help",
		UsageEN: "checkin · checkin add <ID> <name> <target> [data:callback|text:button] · checkin del <ID> · checkin list · " +
			"checkin toggle <ID> · checkin test <ID> · checkin status · checkin reset · checkin help",
		Plugin:    p.Name(),
		Category:  "tools",
		OwnerOnly: true,
		Handler:   p.handle,
	})
}

// Start loads the config and starts the scheduler loop.
func (p *CheckinPlugin) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return nil
	}
	if err := p.load(); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("checkin: load: %w", err)
	}
	p.applyZoneLocked()
	if p.cfg.NextRunAt <= 0 || p.cfg.NextRunDate == "" ||
		(p.cfg.LastRunDate != "" && p.cfg.NextRunDate <= p.cfg.LastRunDate) {
		p.rescheduleLocked(time.Now())
	}
	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	done := p.done
	p.stopListen = p.host.Listen(p.Name(), p.onMessage)
	p.mu.Unlock()
	go p.loop(runCtx, done)
	return nil
}

// Stop halts the scheduler, the listener and waits.
func (p *CheckinPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	cancel, done, stopListen := p.cancel, p.done, p.stopListen
	p.cancel, p.done, p.stopListen = nil, nil, nil
	p.pending = map[int64]*pendingAdd{}
	p.mu.Unlock()
	if stopListen != nil {
		stopListen()
	}
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

func (p *CheckinPlugin) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// loop ticks every 30s like the TeleBox source; pokes wake it early.
func (p *CheckinPlugin) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.wake:
		}
		if ctx.Err() != nil {
			return
		}
		p.tick(ctx)
	}
}

// tick decides whether the planned run is due, missed or still waiting.
func (p *CheckinPlugin) tick(ctx context.Context) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	conf := p.cfg
	if conf.NextRunAt <= 0 || conf.NextRunDate == "" ||
		(conf.LastRunDate != "" && conf.NextRunDate <= conf.LastRunDate) {
		p.rescheduleLocked(time.Now())
		p.mu.Unlock()
		return
	}
	state := dueState(time.Now(), time.Unix(conf.NextRunAt, 0), p.loc)
	if state == stateWait {
		p.mu.Unlock()
		return
	}
	// Write the run to disk before running, so a crash mid-run cannot
	// re-check-in (same as the source).
	p.cfg.LastRunDate = conf.NextRunDate
	p.rescheduleLocked(time.Now())
	p.running = true
	enabled := conf.enabled()
	p.mu.Unlock()

	if state == stateRun && enabled {
		p.runAll(ctx, sourceAuto)
	}
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
}

// runAll signs in to every enabled target and pushes the summary.
func (p *CheckinPlugin) runAll(ctx context.Context, source string) {
	var results []runResult
	for _, t := range p.enabledTargets() {
		if len(results) > 0 {
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
			}
		}
		results = append(results, p.runSingle(ctx, t))
	}
	if source == sourceAuto && p.set != nil && !p.set.Bool("notify") {
		return
	}
	p.report(ctx, source, results)
}

// applyZone reloads the timezone from the panel.
func (p *CheckinPlugin) applyZone() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applyZoneLocked()
}

func (p *CheckinPlugin) applyZoneLocked() {
	if p.set == nil {
		return
	}
	if name := strings.TrimSpace(p.set.String("timezone")); name != "" {
		if l, err := time.LoadLocation(name); err == nil {
			p.loc = l
		}
	} else {
		p.loc = shanghai
	}
}

// replan recomputes the next run from now (timezone change).
func (p *CheckinPlugin) replan() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rescheduleLocked(time.Now())
}

func (p *CheckinPlugin) enabledTargets() []Target {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Target
	for _, t := range p.cfg.Targets {
		if t != nil && t.Enabled {
			out = append(out, *t)
		}
	}
	return out
}

// ---------------------------------------------------------------- helpers

func validTimezone(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if strings.EqualFold(s, "local") {
		return "", nil
	}
	if _, err := time.LoadLocation(s); err != nil {
		return "", plugin.Invalid("不是有效的时区名，例如 Asia/Shanghai", "not a valid zone, e.g. Asia/Shanghai")
	}
	return s, nil
}

func validChatID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if !chatIDRe.MatchString(s) {
		return "", plugin.Invalid("请输入数字 chat id（如 -1001234567890）", "expected a numeric chat id (e.g. -1001234567890)")
	}
	return s, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// rescheduleLocked plans the next run and persists it.
func (p *CheckinPlugin) rescheduleLocked(now time.Time) {
	plan, err := planNextRun(now, p.cfg, p.loc, rand.Intn)
	if err != nil {
		if p.log != nil {
			p.log.Warn("checkin: plan next run failed", "error", err)
		}
		return
	}
	p.cfg.NextRunAt = plan.at.Unix()
	p.cfg.NextRunDate = plan.date
	_ = p.saveLocked()
}

func (p *CheckinPlugin) saveLocked() error {
	return writeJSON(configPath(p.dir), p.cfg)
}

func (p *CheckinPlugin) load() error {
	if err := readJSON(configPath(p.dir), &p.cfg); err != nil {
		return err
	}
	p.cfg.normalize()
	return nil
}

var shanghai = mustLocation("Asia/Shanghai")

func mustLocation(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		return time.FixedZone(name, 8*3600)
	}
	return l
}
