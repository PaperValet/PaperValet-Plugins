package main

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "autodel",
	Description: "定时自动删除自己发送的消息",
	DescEN:      "Auto-delete your own messages on a timer",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// AutoDelPlugin merges TeleBox autodel (per-chat TTL) + autodelcmd
// (command-output rules) into one owner-only plugin.
type AutoDelPlugin struct {
	mu  sync.Mutex
	dir string

	ttl     map[string]int // chat id → seconds; "0" = global
	rules   []CmdRule
	pending []PendingDel

	pendingDirty     bool        // pending.json needs a (debounced) write
	pendingSaveTimer *time.Timer // debounces pending.json writes

	host plugin.Host
	mgr  plugin.Manager
	set  plugin.Settings
	log  plugin.Logger

	stopListen func()
	cancel     context.CancelFunc
	ctx        context.Context
	wg         sync.WaitGroup
}

func New() *AutoDelPlugin {
	return &AutoDelPlugin{ttl: map[string]int{}}
}

func (p *AutoDelPlugin) Name() string        { return "autodel" }
func (p *AutoDelPlugin) Description() string { return Metadata.Description }
func (p *AutoDelPlugin) DescEN() string      { return Metadata.DescEN }

func (p *AutoDelPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.mgr = mgr
	p.host = mgr.Host()
	p.log = p.host.Logger(p.Name())
	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return err
	}
	p.dir = dir

	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin: p.Name(), Title: "🗑 自动删除", TitleEN: "🗑 Auto-delete",
		Settings: []plugin.Setting{
			{Key: "cmd_enabled", Label: "命令输出清理", LabelEN: "Clean command outputs", Kind: plugin.SettingToggle, Default: false,
				Hint:   "开启后按规则延迟删除命令消息及输出",
				HintEN: "When on, command messages and outputs are deleted per rules"},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	if err := p.host.Bot(p.Name()).SetPage(&plugin.Page{Title: "删除设置", TitleEN: "Auto-delete", Handle: p.page}); err != nil && err != plugin.ErrBotNotReady {
		return err
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "autodel",
		Description: "定时自动删除自己发送的消息，含命令输出清理",
		DescEN:      "Auto-delete your own messages on a timer, incl. command outputs",
		Usage:       "autodel <时长> [global] · autodel cancel [global] · autodel list · autodel status · autodel cmd add <命令> <秒> [参数…] [-r] [-e] · autodel cmd del <ID|命令> · autodel cmd reset · autodel help",
		UsageEN:     "autodel <duration> [global] · autodel cancel [global] · autodel list · autodel status · autodel cmd add <cmd> <secs> [params…] [-r] [-e] · autodel cmd del <ID|cmd> · autodel cmd reset · autodel help",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *AutoDelPlugin) Start(_ context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return nil
	}
	var err error
	if p.ttl, err = loadTTL(filepath.Join(p.dir, settingsFile)); err != nil {
		p.mu.Unlock()
		return err
	}
	if err := p.loadRulesLocked(); err != nil {
		p.mu.Unlock()
		return err
	}
	if p.pending, err = loadPending(filepath.Join(p.dir, pendingFile)); err != nil {
		p.mu.Unlock()
		return err
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.mu.Unlock()

	p.stopListen = p.host.Listen(p.Name(), p.onMessage)
	p.restorePending()
	return nil
}

func (p *AutoDelPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	stop, cancel := p.stopListen, p.cancel
	p.stopListen, p.cancel, p.ctx = nil, nil, nil
	p.stopPendingTimerLocked()
	if p.pendingDirty {
		if err := p.savePendingLocked(); err != nil && p.log != nil {
			p.log.Warn("autodel: final pending save failed", "error", err)
		}
		p.pendingDirty = false
	}
	p.mu.Unlock()
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel() // deleteNow workers see this and abort network calls
	}
	// Bounded wait: a stuck delete must not block unload forever. The
	// caller's ctx bounds the wait too (whichever ends first).
	waitParent := ctx
	if waitParent == nil {
		waitParent = context.Background()
	}
	waitCtx, waitCancel := context.WithTimeout(waitParent, stopWait)
	defer waitCancel()
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-waitCtx.Done():
		if p.log != nil {
			p.log.Warn("autodel: Stop timed out waiting for workers")
		}
	}
	return nil
}

// ---------------------------------------------------------------- bot page

// page shows TTL entries and cmd rules with delete buttons.
func (p *AutoDelPlugin) page(c *plugin.BotContext) (*plugin.View, error) {
	tl := c.Tlocal
	if op, arg, ok := strings.Cut(c.Data, ":"); ok {
		p.mu.Lock()
		var found bool
		switch op {
		case "c": // rule by id
			for i, r := range p.rules {
				if r.ID == arg {
					p.rules = append(p.rules[:i], p.rules[i+1:]...)
					found = true
					break
				}
			}
		case "t": // TTL entry by chat id ("0" = global)
			if _, exists := p.ttl[arg]; exists {
				delete(p.ttl, arg)
				found = true
			}
		}
		var err error
		if found {
			if op == "c" {
				err = p.saveRulesLocked()
			} else {
				err = p.saveTTLLocked()
			}
		}
		p.mu.Unlock()
		switch {
		case err != nil:
			c.Alert(tl("保存失败: ", "Save failed: ") + err.Error())
		case found:
			c.Toast(tl("已删除", "Deleted"))
		default:
			c.Toast(tl("已经不在列表里了", "Already gone"))
		}
	}

	p.mu.Lock()
	ttl := make(map[string]int, len(p.ttl))
	for k, v := range p.ttl {
		ttl[k] = v
	}
	rules := append([]CmdRule(nil), p.rules...)
	cmdEnabled := p.set != nil && p.set.Bool("cmd_enabled")
	p.mu.Unlock()

	v := &plugin.View{Text: "🗑 **" + tl("自动删除", "Auto-delete") + "**"}
	if cmdEnabled {
		v.Text += "\n✅ " + tl("命令输出清理已开启", "Command-output cleaning is on")
	} else {
		v.Text += "\n⭕ " + tl("命令输出清理已关闭", "Command-output cleaning is off")
	}
	v.Text += "\n\n⏱ **" + tl("定时删除 (TTL)", "Timed delete (TTL)") + "**"
	if len(ttl) == 0 {
		v.Text += "\n" + tl("暂无设置", "none")
	} else {
		for _, k := range sortedNumKeys(ttl) {
			label := tl("全局", "global")
			if k != "0" {
				label = tl("聊天 ", "chat ") + k
			}
			v.Text += "\n• " + label + "  " + plugin.Code(formatSeconds(ttl[k]))
			v.Buttons = append(v.Buttons, plugin.Row(plugin.Btn("🗑 "+label, "t:"+k).Danger()))
		}
	}
	v.Text += "\n\n⚙️ **" + tl("命令规则", "Command rules") + "**  " + plugin.Code(strconv.Itoa(len(rules)))
	if len(rules) == 0 {
		v.Text += "\n" + tl("暂无规则", "none")
	}
	for _, r := range rules {
		v.Text += "\n• " + ruleText(r)
		v.Buttons = append(v.Buttons, plugin.Row(plugin.Btn("🗑 #"+r.ID+" "+r.Command, "c:"+r.ID).Danger()))
	}
	return v, nil
}

// ruleText renders one rule line (Markdown-safe).
func ruleText(r CmdRule) string {
	s := plugin.Code(r.Command)
	if len(r.Parameters) > 0 {
		s += " " + plugin.Code("["+strings.Join(r.Parameters, ",")+"]")
	}
	s += " → " + plugin.Code(formatSeconds(r.Delay))
	if r.DeleteResponse {
		s += " 🔄"
	}
	if r.ExactMatch {
		s += " 🎯"
	}
	return s
}

func sortedNumKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i])
		b, _ := strconv.Atoi(out[j])
		return a < b
	})
	return out
}
