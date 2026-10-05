package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "autochangename",
	Description: "自动轮换账号名字/简介/用户名",
	DescEN:      "Auto-rotate the account name, bio or username",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type ACNPlugin struct {
	host plugin.Host
	set  plugin.Settings
	log  plugin.Logger
	dir  string
	st   *stateStore

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}
	rng    *rand.Rand

	// rotMu serializes rotations: the scheduler loop, the `now` command
	// and the panel button can all fire rotate concurrently.
	rotMu sync.Mutex
}

func New() *ACNPlugin { return &ACNPlugin{} }

func (p *ACNPlugin) Name() string        { return "autochangename" }
func (p *ACNPlugin) Description() string { return Metadata.Description }
func (p *ACNPlugin) DescEN() string      { return Metadata.DescEN }

func (p *ACNPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	if p.host == nil {
		return errors.New("autochangename: no host")
	}
	p.log = p.host.Logger(p.Name())
	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return fmt.Errorf("autochangename: data dir: %w", err)
	}
	p.dir = dir
	p.st = newStateStore(filepath.Join(p.dir, stateFile))
	p.rng = rand.New(rand.NewSource(time.Now().UnixNano()))

	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin:  p.Name(),
		Title:   "✏️ 自动改名",
		TitleEN: "✏️ Auto name rotation",
		Settings: []plugin.Setting{
			{Key: "enabled", Label: "启用自动轮换", LabelEN: "Enable rotation",
				Kind: plugin.SettingToggle, Default: false,
				Hint:   "开启后按间隔或 cron 自动切换",
				HintEN: "Rotate automatically on an interval or cron"},
			{Key: "target", Label: "轮换对象", LabelEN: "Rotation target",
				Kind: plugin.SettingChoice, Default: targetName,
				Choices: []plugin.Choice{
					{Value: targetName, Label: "名字（姓/名）", LabelEN: "Name (first/last)"},
					{Value: targetBio, Label: "简介 (bio)", LabelEN: "Bio"},
					{Value: targetUsername, Label: "用户名 @", LabelEN: "Username @"},
				}},
			{Key: "interval", Label: "间隔（分钟）", LabelEN: "Interval (minutes)",
				Kind: plugin.SettingNumber, Default: 60, Min: 1, Max: 43200,
				Hint:   "两次轮换的最小间隔；设置了 cron 时忽略",
				HintEN: "Minutes between rotations; ignored when a cron is set"},
			{Key: "cron", Label: "Cron 表达式", LabelEN: "Cron expression",
				Kind: plugin.SettingText, Default: "", Validate: validCronSetting,
				Hint:   "5 段标准 cron（分 时 日 月 周），如 0 9 * * *；留空用间隔",
				HintEN: "5-field standard cron (min hour day month weekday), e.g. 0 9 * * *; empty uses the interval"},
			{Key: "random", Label: "随机顺序", LabelEN: "Random order",
				Kind: plugin.SettingToggle, Default: false,
				Hint:   "关闭则按列表顺序循环",
				HintEN: "Off cycles through the list in order"},
			{Key: "restore_on_stop", Label: "停止时恢复原名", LabelEN: "Restore on disable",
				Kind: plugin.SettingToggle, Default: true,
				Hint:   "关闭轮换时恢复最初的账号资料",
				HintEN: "Restore the original profile when rotation is disabled"},
		},
		// Restoring on disable needs the API client, which is only
		// available once Start has run, so defer via OnChange.
		OnChange: func(key string) {
			if key != "enabled" || p.set.Bool("enabled") || !p.set.Bool("restore_on_stop") {
				return
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
				defer cancel()
				if err := p.doRestore(ctx); err != nil && p.log != nil {
					p.log.Info("autochangename: restore on disable", "error", err)
				}
			}()
		},
	})
	if err != nil {
		return err
	}
	p.set = set

	if err := p.host.Bot(p.Name()).SetPage(&plugin.Page{
		Title: "轮换列表", TitleEN: "Rotation list",
		Handle: p.page,
	}); err != nil && !errors.Is(err, plugin.ErrBotNotReady) {
		return err
	}

	return mgr.RegisterCommand(&plugin.Command{
		Name:        "autochangename",
		Aliases:     []string{"acn"},
		Description: "自动轮换账号名字/简介/用户名：列表管理、立即切换、恢复原名",
		DescEN:      "Auto-rotate the account name, bio or username: manage the list, switch now, restore",
		Usage:       "autochangename list · add <文本|回复多行消息> · del <序号> · clear · now · status · restore · help",
		UsageEN:     "autochangename list · add <text|reply multiline> · del <index> · clear · now · status · restore · help",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

// validCronSetting validates the panel cron setting.
func validCronSetting(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if _, err := cronParse(s); err != nil {
		return "", plugin.Invalid("不是有效的 5 段 cron 表达式，例如 0 9 * * *", "not a valid 5-field cron expression, e.g. 0 9 * * *")
	}
	return s, nil
}

// Start loads state and launches the scheduler loop.
func (p *ACNPlugin) Start(ctx context.Context) error {
	if p.st == nil {
		return errors.New("autochangename: not initialized")
	}
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	if err := p.st.load(); err != nil {
		return fmt.Errorf("autochangename: load state: %w", err)
	}

	p.mu.Lock()
	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	p.wake = make(chan struct{}, 1)
	wake, done := p.wake, p.done
	p.mu.Unlock()

	go p.loop(runCtx, done, wake)
	p.poke()
	return nil
}

// Stop cancels the scheduler and waits for it to finish.
func (p *ACNPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return nil
}

func (p *ACNPlugin) poke() {
	p.mu.Lock()
	wake := p.wake
	p.mu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// loop wakes at most every tickPrecision and applies what is due.
func (p *ACNPlugin) loop(ctx context.Context, done chan struct{}, wake chan struct{}) {
	defer close(done)
	timer := time.NewTimer(pollEvery)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-timer.C:
		}
		p.runDue(ctx)
		if ctx.Err() != nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(pollEvery)
	}
}

// runDue rotates when enabled and the schedule says it is time.
func (p *ACNPlugin) runDue(ctx context.Context) {
	if !p.set.Bool("enabled") {
		return
	}
	target := p.set.String("target")
	if !validTarget(target) {
		target = targetName
	}
	items := p.st.items(target)
	if len(items) == 0 {
		return
	}
	lastFire, zero := p.st.lastFire()
	var last time.Time
	if !zero {
		last = time.Unix(lastFire, 0)
	}
	due, err := nextDue(last, p.set.Int("interval"), p.set.String("cron"), time.Now())
	if err != nil || due.After(time.Now()) {
		return
	}
	if _, err := p.rotate(ctx, target, items); err != nil {
		if p.log != nil {
			p.log.Warn("autochangename: rotate failed", "error", err)
		}
		// Back off so a failing RPC does not hot-loop.
		_ = p.st.setLastFire(time.Now().Add(-backoff).Unix())
	}
}

// rotate applies the next item to the account and records state. It returns
// the item actually applied so callers report the real result.
func (p *ACNPlugin) rotate(ctx context.Context, target string, items []string) (string, error) {
	api := p.host.API()
	if api == nil {
		return "", errors.New("no API client")
	}
	// Serialize the whole rotation: concurrent rotates race both the RPC
	// (last write wins, possibly not what setIndex recorded) and the
	// index bookkeeping.
	p.rotMu.Lock()
	defer p.rotMu.Unlock()

	cur := p.st.curIndex(target)
	nxt := nextIndex(len(items), cur, p.set.Bool("random"), func(n int) int {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.rng.Intn(n)
	})
	item := items[nxt]

	// Capture originals before the first change of each field.
	if err := p.captureOrigin(ctx, api, target); err != nil {
		if p.log != nil {
			p.log.Warn("autochangename: capture origin failed", "error", err)
		}
	}

	if err := applyItem(ctx, api, target, item); err != nil {
		return "", err
	}
	_ = p.st.setIndex(target, nxt)
	_ = p.st.setLastFire(time.Now().Unix())
	return item, nil
}

// captureOrigin remembers the current profile before it is changed.
func (p *ACNPlugin) captureOrigin(ctx context.Context, api *tg.Client, target string) error {
	o := p.st.origin()
	switch target {
	case targetBio:
		if o.CapturedBio {
			return nil
		}
		about, err := fetchAbout(ctx, api)
		if err != nil {
			return err
		}
		return p.st.setOrigin(originInfo{Bio: about, CapturedBio: true})
	case targetUsername:
		if o.CapturedUsername {
			return nil
		}
		un, err := fetchUsername(ctx, api)
		if err != nil {
			return err
		}
		// An empty username is captured as "no original to restore", but
		// still marked so we do not re-fetch every rotation.
		return p.st.setOrigin(originInfo{Username: un, CapturedUsername: true})
	default:
		if o.First != "" || o.SetLast {
			return nil
		}
		first, last, err := fetchName(ctx, api)
		if err != nil {
			return err
		}
		return p.st.setOrigin(originInfo{First: first, Last: last, SetLast: last != ""})
	}
}

// applyItem pushes one list entry to Telegram.
func applyItem(ctx context.Context, api *tg.Client, target, item string) error {
	cctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	switch target {
	case targetBio:
		_, err := api.AccountUpdateProfile(cctx, bioRequest(item))
		return err
	case targetUsername:
		_, err := api.AccountUpdateUsername(cctx, item)
		return err
	default:
		first, last, setLast := splitNameItem(item)
		_, err := api.AccountUpdateProfile(cctx, nameRequest(first, last, setLast))
		return err
	}
}

// fetchName reads the account's current first/last name.
func fetchName(ctx context.Context, api *tg.Client) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	users, err := api.UsersGetUsers(cctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	if err != nil {
		return "", "", err
	}
	if len(users) == 0 {
		return "", "", errors.New("empty user response")
	}
	u, ok := users[0].(*tg.User)
	if !ok {
		return "", "", fmt.Errorf("unexpected user type %T", users[0])
	}
	return u.FirstName, u.LastName, nil
}

// fetchAbout reads the account bio via users.getFullUser.
func fetchAbout(ctx context.Context, api *tg.Client) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	full, err := api.UsersGetFullUser(cctx, &tg.InputUserSelf{})
	if err != nil {
		return "", err
	}
	return full.FullUser.About, nil
}

// fetchUsername reads the account username via users.getUsers.
func fetchUsername(ctx context.Context, api *tg.Client) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	users, err := api.UsersGetUsers(cctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	if err != nil {
		return "", err
	}
	if len(users) == 0 {
		return "", errors.New("empty user response")
	}
	u, ok := users[0].(*tg.User)
	if !ok {
		return "", fmt.Errorf("unexpected user type %T", users[0])
	}
	return u.Username, nil
}

// doRestore puts the saved originals back.
func (p *ACNPlugin) doRestore(ctx context.Context) error {
	api := p.host.API()
	if api == nil {
		return errors.New("no API client")
	}
	o := p.st.origin()
	if o.First == "" && !o.SetLast && !o.CapturedBio && !o.CapturedUsername {
		return errors.New("nothing captured to restore")
	}
	cctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	var firstErr error
	if o.First != "" || o.SetLast {
		if _, err := api.AccountUpdateProfile(cctx, restoreNameRequest(o.First, o.Last)); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("name: %w", err)
		}
	}
	if o.CapturedBio {
		if _, err := api.AccountUpdateProfile(cctx, bioRequest(o.Bio)); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("bio: %w", err)
		}
	}
	if o.Username != "" {
		if _, err := api.AccountUpdateUsername(cctx, o.Username); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("username: %w", err)
		}
	}
	return firstErr
}

// applyNow rotates immediately from a command (ignores enabled/schedule).
// It reports the item that was actually applied.
func (p *ACNPlugin) applyNow(ctx context.Context, target string) (string, error) {
	items := p.st.items(target)
	if len(items) == 0 {
		return "", errors.New("empty list")
	}
	return p.rotate(ctx, target, items)
}
