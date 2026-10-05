package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const dataFile = "data.json"

var Metadata = &plugin.PluginMetadata{
	Name:        "pmcaptcha",
	Description: "陌生人私信人机验证门禁",
	DescEN:      "Captcha gate for strangers in private messages",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type PMPlugin struct {
	mu   sync.Mutex
	dir  string
	host plugin.Host
	log  plugin.Logger
	set  plugin.Settings

	ctx        context.Context
	cancel     context.CancelFunc
	stopListen func()
	wg         sync.WaitGroup

	records    *records
	challenges map[int64]*challenge
	botFlag    map[int64]bool
}

func New() *PMPlugin {
	return &PMPlugin{records: newRecords(), challenges: map[int64]*challenge{}}
}

func (p *PMPlugin) Name() string        { return "pmcaptcha" }
func (p *PMPlugin) Description() string { return Metadata.Description }
func (p *PMPlugin) DescEN() string      { return Metadata.DescEN }

func (p *PMPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.log = p.host.Logger(p.Name())
	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return err
	}
	p.dir = dir
	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin: p.Name(), Title: "🔒 私信验证", TitleEN: "🔒 PM Captcha",
		Settings: []plugin.Setting{
			{Key: "enabled", Label: "启用插件", LabelEN: "Enable plugin",
				Hint: "陌生人私信触发验证流程", HintEN: "Strangers trigger the challenge flow",
				Kind: plugin.SettingToggle, Default: true},
			{Key: "captcha_on", Label: "发送验证题", LabelEN: "Send challenge",
				Hint: "关闭后只静音+归档，不发题目", HintEN: "When off, only mute+archive, no challenge",
				Kind: plugin.SettingToggle, Default: true},
			{Key: "timeout", Label: "验证超时（秒）", LabelEN: "Timeout (seconds)",
				Hint: "0 = 不限时", HintEN: "0 = no limit",
				Kind: plugin.SettingNumber, Default: 30, Min: 0, Max: 3600},
			{Key: "tries", Label: "最大尝试次数", LabelEN: "Max tries",
				Hint: "0 = 不限次数", HintEN: "0 = unlimited",
				Kind: plugin.SettingNumber, Default: 3, Min: 0, Max: 20},
			{Key: "unlock_hours", Label: "通过后免验证时长（小时）", LabelEN: "Pass validity (hours)",
				Hint: "0 = 永久", HintEN: "0 = forever",
				Kind: plugin.SettingNumber, Default: 0, Min: 0, Max: 8760},
			{Key: "fail_action", Label: "验证失败动作", LabelEN: "On-failure action",
				Hint: "静音+归档之外的附加动作", HintEN: "Extra action beyond mute+archive",
				Kind: plugin.SettingChoice, Default: "none",
				Choices: []plugin.Choice{
					{Value: "none", Label: "无（仅静音+归档）", LabelEN: "None (mute+archive only)"},
					{Value: "block", Label: "屏蔽用户", LabelEN: "Block user"},
					{Value: "delete", Label: "双方删除对话", LabelEN: "Delete history (both sides)"},
					{Value: "report", Label: "举报垃圾信息", LabelEN: "Report spam"},
				}},
			{Key: "pass_action", Label: "验证通过动作", LabelEN: "On-pass action",
				Kind: plugin.SettingChoice, Default: "unmute",
				Choices: []plugin.Choice{
					{Value: "unmute", Label: "取消静音", LabelEN: "Unmute"},
					{Value: "unarchive", Label: "取消归档", LabelEN: "Unarchive"},
					{Value: "wl", Label: "加入白名单", LabelEN: "Add to whitelist"},
					{Value: "none", Label: "无", LabelEN: "None"},
				}},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "pmc",
		Description: "私信人机验证：查状态、管理白名单与验证记录",
		DescEN:      "PM captcha gate: status, whitelist and verification records",
		Usage: "pmc status · pmc wl [add|del|pass|clear] <ID/@用户> · pmc record [verified|failed] · " +
			"pmc record del verified|failed <ID|all> · pmc test · pmc help",
		UsageEN: "pmc status · pmc wl [add|del|pass|clear] <ID/@user> · pmc record [verified|failed] · " +
			"pmc record del verified|failed <ID|all> · pmc test · pmc help",
		Plugin:    p.Name(),
		Category:  "tools",
		OwnerOnly: true,
		Handler:   p.handle,
	})
}

func (p *PMPlugin) Start(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return nil
	}
	var db records
	if err := readJSON(filepath.Join(p.dir, dataFile), &db); err != nil {
		return fmt.Errorf("pmcaptcha: load data: %w", err)
	}
	p.records = &db
	p.records.ensure()
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.stopListen = p.host.Listen(p.Name(), p.onMessage)
	return nil
}

func (p *PMPlugin) Stop(_ context.Context) error {
	p.mu.Lock()
	stop, cancel := p.stopListen, p.cancel
	p.stopListen, p.cancel = nil, nil
	p.challenges = map[int64]*challenge{}
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

// lifetime returns the context background work runs on.
func (p *PMPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

// options snapshots the panel settings.
func (p *PMPlugin) options() options {
	o := options{enabled: true, captchaOn: true, timeout: 30, tries: 3, passAction: "unmute"}
	if p.set == nil {
		return o
	}
	o.enabled = p.set.Bool("enabled")
	o.captchaOn = p.set.Bool("captcha_on")
	o.timeout = p.set.Int("timeout")
	o.tries = p.set.Int("tries")
	o.unlockHours = p.set.Int("unlock_hours")
	o.failAction = p.set.String("fail_action")
	o.passAction = p.set.String("pass_action")
	return o
}

type options struct {
	enabled     bool
	captchaOn   bool
	timeout     int // seconds, 0 = no limit
	tries       int // 0 = unlimited
	unlockHours int // 0 = forever
	failAction  string
	passAction  string
}

// challenge bookkeeping ----------------------------------------------------

func (p *PMPlugin) hasChallenge(uid int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.challenges[uid]
	return ok
}

func (p *PMPlugin) setChallenge(uid int64, ch *challenge) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.challenges[uid] = ch
}

func (p *PMPlugin) removeChallenge(uid int64) *challenge {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := p.challenges[uid]
	delete(p.challenges, uid)
	return ch
}

func (p *PMPlugin) saveRecords() error {
	return p.records.save(filepath.Join(p.dir, dataFile))
}

// displayName resolves the best known name for a user id.
func (p *PMPlugin) displayName(ctx context.Context, id int64) string {
	if name := p.records.nameOf(id); name != "" {
		return name
	}
	if u := p.userInfo(ctx, id); u != nil {
		return nameOfUser(u)
	}
	return strconv.FormatInt(id, 10)
}

// isVerified reports whether uid passed and the pass is still valid under
// unlock_hours (0 = forever).
func (p *PMPlugin) isVerified(uid int64) bool {
	v, ok := p.records.getVerified(uid)
	if !ok {
		return false
	}
	hours := p.options().unlockHours
	if hours <= 0 {
		return true
	}
	return time.Since(v.Time) < time.Duration(hours)*time.Hour
}

// onMessage is the DM gate. Listeners run on the update path, so the
// network work happens in a goroutine.
func (p *PMPlugin) onMessage(_ context.Context, ev *plugin.MessageEvent, edited bool) {
	if ev == nil || ev.Message == nil || edited {
		return
	}
	peerUser, ok := ev.PeerID.(*tg.PeerUser)
	if !ok {
		return // not a private chat
	}
	if p.set == nil || !p.set.Bool("enabled") {
		return
	}
	self := p.host.SelfID()
	if ev.IsOut {
		// The owner wrote first: auto-verify the peer (the source does this).
		target := peerUser.UserID
		if target > 0 && target != self && !telegramOfficialIDs[target] &&
			!p.hasChallenge(target) && !p.isVerified(target) && !p.records.inWhitelist(target) {
			p.records.addVerified(target, "", "")
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				_ = p.saveRecords()
			}()
		}
		return
	}
	uid := ev.UserID
	if uid <= 0 {
		uid = peerUser.UserID
	}
	if uid <= 0 || uid == self || telegramOfficialIDs[uid] {
		return
	}
	if p.records.inWhitelist(uid) {
		return
	}
	if p.hasChallenge(uid) {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handleReply(p.lifetime(), uid, ev.Text, ev.Message.ID)
		}()
		return
	}
	if p.isVerified(uid) {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ctx := p.lifetime()
		if p.isBot(ctx, uid) {
			return // bots are never challenged, like the source
		}
		p.beginChallenge(ctx, uid)
	}()
}
