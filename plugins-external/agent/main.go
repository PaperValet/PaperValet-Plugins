// Package main implements the PaperValet "agent" external plugin.
//
// An AI agent: the owner gives it a task in plain language, it works through
// it step by step with tools (search, read, write, run commands, send files)
// and reports back. Model, endpoint and limits live in the bot panel.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
	"github.com/gotd/td/tg"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "agent",
	Description: "AI 智能体：下达任务后自动调用工具多步完成，可执行命令与发送文件",
	DescEN:      "AI agent: give it a task and it works through it with tools, running commands and sending files",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// Defaults, overridable from the bot panel.
const (
	defaultBaseURL    = "https://api.deepseek.com/v1"
	defaultModel      = "deepseek-chat"
	defaultMaxSteps   = 15
	defaultCmdTimeout = 60
	// workspaceDirname is the workspace location under data/agent, used
	// whenever the owner leaves the setting empty. A hardcoded absolute
	// path would be wrong for a non-root install and on Windows.
	workspaceDirname = "workspace"
)

func helpText(ctx *plugin.CommandContext) string {
	return strings.Join([]string{
		"🤖 " + plugin.Bold(ctx.Tlocal("AI 智能体", "AI agent")),
		"",
		ctx.Tlocal("用自然语言下达任务，它会自己调用工具多步完成。",
			"Give it a task in plain language; it works through it with tools."),
		"",
		"> " + plugin.Code(".agent 帮我找出 diss 源码发给我"),
		"> " + plugin.Code(".agent 看看服务器磁盘占用"),
		"",
		plugin.Bold(ctx.Tlocal("子命令", "Subcommands")),
		"> " + plugin.Code("agent reset") + " — " + ctx.Tlocal("清空对话上下文", "clear the conversation"),
		"> " + plugin.Code("agent status") + " — " + ctx.Tlocal("查看当前配置", "show current config"),
		"> " + plugin.Code("agent help") + " — " + ctx.Tlocal("这份帮助", "this help"),
		"",
		plugin.Bold(ctx.Tlocal("可用的工具", "Tools")),
		"> " + ctx.Tlocal("列目录 · 读文件 · 搜索 · 写文件 · 执行命令 · 发送文件",
			"list · read · search · write · run command · send file"),
		"",
		"> " + ctx.Tlocal("模型、密钥与限制在机器人面板设置：/menu → 外置插件 → agent",
			"Model, key and limits live in the bot panel: /menu → External plugins → agent"),
	}, "\n")
}

// ============================================================
// Plugin
// ============================================================

type AgentPlugin struct {
	host    plugin.Host
	set     plugin.Settings
	logger  plugin.Logger
	dataDir string

	mu       sync.Mutex
	sessions map[int64]*session // chatID → conversation
	running  map[int64]context.CancelFunc

	// life bounds every background run so Stop can cancel in-flight tasks
	// instead of leaving them editing messages through a torn-down host.
	lifeMu sync.Mutex
	life   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	selfID int64
	tools  []toolSpec
	http   *httpDoer
}

func New() *AgentPlugin {
	return &AgentPlugin{
		sessions: map[int64]*session{},
		running:  map[int64]context.CancelFunc{},
		http:     newHTTPDoer(),
	}
}

// lifetime returns the plugin-scoped context, creating it on first use so a
// plugin built outside Init (tests) still works.
func (p *AgentPlugin) lifetime() context.Context {
	p.lifeMu.Lock()
	defer p.lifeMu.Unlock()
	if p.cancel == nil {
		p.life, p.cancel = context.WithCancel(context.Background())
	}
	return p.life
}

func (p *AgentPlugin) Name() string        { return Metadata.Name }
func (p *AgentPlugin) Description() string { return Metadata.Description }
func (p *AgentPlugin) DescEN() string      { return Metadata.DescEN }

func (p *AgentPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.logger = p.host.Logger(Metadata.Name)
	p.tools = buildTools()

	dir, err := p.host.DataDir(Metadata.Name)
	if err != nil {
		return err
	}
	p.dataDir = dir

	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin:  Metadata.Name,
		Title:   "🤖 AI 智能体",
		TitleEN: "🤖 AI agent",
		Settings: []plugin.Setting{
			{
				Key: "api_key", Label: "API 密钥", LabelEN: "API key",
				Kind: plugin.SettingText, Secret: true,
				Hint:   "OpenAI 兼容接口的密钥",
				HintEN: "Key for an OpenAI-compatible API",
			},
			{
				Key: "base_url", Label: "API 地址", LabelEN: "Base URL",
				Kind: plugin.SettingText, Default: defaultBaseURL,
				Hint:   "如 https://api.deepseek.com/v1 ，插件自动拼 /chat/completions",
				HintEN: "e.g. https://api.deepseek.com/v1; /chat/completions is appended",
				Validate: func(s string) (string, error) {
					s = strings.TrimSpace(s)
					if s == "" {
						return s, nil
					}
					if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
						return "", plugin.Invalid("必须是 http(s) 开头的完整地址", "must be a full http(s) URL")
					}
					return strings.TrimRight(s, "/"), nil
				},
			},
			{
				Key: "model", Label: "模型", LabelEN: "Model",
				Kind: plugin.SettingText, Default: defaultModel,
				Hint:   "如 deepseek-chat、gpt-4o-mini、gemini-2.0-flash",
				HintEN: "e.g. deepseek-chat, gpt-4o-mini",
				Validate: func(s string) (string, error) {
					s = strings.TrimSpace(s)
					if s == "" {
						return "", plugin.Invalid("模型名不能为空", "model name is required")
					}
					return s, nil
				},
			},
			{
				Key: "workspace", Label: "工作区", LabelEN: "Workspace",
				Kind:   plugin.SettingText,
				Hint:   "相对路径的基准目录，留空则用 data/agent/workspace",
				HintEN: "Base directory for relative paths; empty uses data/agent/workspace",
			},
			{
				Key: "max_steps", Label: "最大步数", LabelEN: "Max steps",
				Kind: plugin.SettingNumber, Default: defaultMaxSteps, Min: 1, Max: 50,
				Hint:   "防止死循环，默认 15",
				HintEN: "Loop guard, default 15",
			},
			{
				Key: "cmd_timeout", Label: "命令超时（秒）", LabelEN: "Command timeout (s)",
				Kind: plugin.SettingNumber, Default: defaultCmdTimeout, Min: 5, Max: 300,
				Hint:   "执行命令的超时，默认 60",
				HintEN: "Timeout for run_command, default 60",
			},
		},
	})
	if err != nil {
		return err
	}
	p.set = set

	return mgr.RegisterCommand(&plugin.Command{
		Name:        "agent",
		Description: "AI 智能体：下达任务后自动多步完成（模型与限制在面板设置）",
		DescEN:      "Give the agent a task and it works through it with tools (configure in the panel)",
		Usage:       "agent <任务> · agent reset · agent status · agent help",
		UsageEN:     "agent <task> · agent reset · agent status · agent help",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		RateLimit:   30,
		Handler:     p.handle,
	})
}

func (p *AgentPlugin) Start(context.Context) error {
	p.selfID = p.host.SelfID()
	p.lifetime()
	cfg := p.config()
	if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
		p.logger.Warn("cannot create workspace", "path", cfg.Workspace, "err", err.Error())
	}
	p.logger.Info("agent started", "self", p.selfID, "model", cfg.Model, "workspace", cfg.Workspace)
	return nil
}

func (p *AgentPlugin) Stop(context.Context) error {
	p.lifeMu.Lock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.lifeMu.Unlock()

	// Wait for in-flight runs so nothing keeps editing messages through a
	// host that is being torn down.
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		p.logger.Warn("stop timed out waiting for running tasks")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions = map[int64]*session{}
	p.running = map[int64]context.CancelFunc{}
	return nil
}

// ============================================================
// Command entry
// ============================================================

func (p *AgentPlugin) handle(ctx *plugin.CommandContext) error {
	arg := strings.TrimSpace(ctx.RawArgs)

	// One spelling per subcommand, no hidden aliases.
	switch strings.ToLower(firstWord(arg)) {
	case "", "help":
		return editNoPreview(ctx, helpText(ctx))
	case "reset":
		// Reset also stops an in-flight run; otherwise the task keeps
		// writing history into the session the owner just cleared.
		p.cancelRun(ctx.Message.ChatID)
		p.clearSession(ctx.Message.ChatID)
		return editNoPreview(ctx, "🧹 "+ctx.Tlocal("已停止当前任务并清空对话上下文。",
			"Stopped the running task and cleared the conversation."))
	case "status":
		return p.handleStatus(ctx)
	}

	cfg := p.config()
	if cfg.APIKey == "" {
		return editNoPreview(ctx, "❌ "+ctx.Tlocal("还没有配置 API 密钥", "No API key configured yet")+
			"\n> "+ctx.Tlocal("请在机器人面板 /menu → 外置插件 → agent 里设置密钥、地址和模型",
			"Set the key, base URL and model in the bot panel: /menu → External plugins → agent"))
	}

	chatID := ctx.Message.ChatID
	msgID := 0
	if ctx.Message.Message != nil {
		msgID = ctx.Message.Message.ID
	}

	runCtx, cancel := context.WithTimeout(p.lifetime(), runTimeout)
	if !p.tryStart(chatID, cancel) {
		cancel()
		return editNoPreview(ctx, "⏳ "+ctx.Tlocal("上一个任务还在执行，请稍候（发 agent reset 可以停掉它）",
			"A task is still running, please wait (agent reset stops it)"))
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runTask(runCtx, chatID, msgID, arg)
	}()
	return nil
}

// firstWord is the leading whitespace-delimited token of s.
func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func (p *AgentPlugin) handleStatus(ctx *plugin.CommandContext) error {
	cfg := p.config()
	key := ctx.Tlocal("未设置", "not set")
	if cfg.APIKey != "" {
		key = ctx.Tlocal("已设置", "configured")
	}
	busy := ctx.Tlocal("空闲", "idle")
	if p.isRunning(ctx.Message.ChatID) {
		busy = ctx.Tlocal("任务执行中", "task running")
	}
	lines := []string{
		"📊 " + plugin.Bold(ctx.Tlocal("智能体状态", "Agent status")),
		"",
		fieldLine(ctx.Tlocal("密钥", "API key"), key),
		fieldLine(ctx.Tlocal("模型", "Model"), cfg.Model),
		fieldLine(ctx.Tlocal("地址", "Base URL"), cfg.BaseURL),
		fieldLine(ctx.Tlocal("工作区", "Workspace"), cfg.Workspace),
		fieldLine(ctx.Tlocal("最大步数", "Max steps"), fmt.Sprintf("%d", cfg.MaxSteps)),
		fieldLine(ctx.Tlocal("命令超时", "Command timeout"), fmt.Sprintf("%ds", cfg.CmdTimeout)),
		fieldLine(ctx.Tlocal("历史消息", "History"), fmt.Sprintf("%d", p.historyLen(ctx.Message.ChatID))),
		fieldLine(ctx.Tlocal("状态", "State"), busy),
	}
	return editNoPreview(ctx, strings.Join(lines, "\n"))
}

// maxHistory caps the replayed conversation window. Package scope so the
// tests can assert against the same constant the trim uses.
const maxHistory = 40

// historyToolClip bounds a single tool result kept in the replayed history.
// The live turn gets the full observation; older turns only need the gist, or
// a handful of 20 KB command outputs push the next request past the model's
// context window.
const historyToolClip = 600

// ============================================================
// Session state
// ============================================================

type session struct {
	Messages []chatMessage
}

func (p *AgentPlugin) historySnapshot(chatID int64) []chatMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[chatID]
	if s == nil || len(s.Messages) == 0 {
		return nil
	}
	// Copy under the lock: a running task appends to this slice while
	// handleStatus and the next run read it.
	out := make([]chatMessage, len(s.Messages))
	copy(out, s.Messages)
	return out
}

func (p *AgentPlugin) historyLen(chatID int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.sessions[chatID]; s != nil {
		return len(s.Messages)
	}
	return 0
}

func (p *AgentPlugin) appendHistory(chatID int64, msgs ...chatMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[chatID]
	if s == nil {
		s = &session{}
		p.sessions[chatID] = s
	}
	for _, m := range msgs {
		if m.Role == "tool" {
			m.Content = clipRunes(m.Content, historyToolClip)
		}
		s.Messages = append(s.Messages, m)
	}
	if len(s.Messages) > maxHistory {
		s.Messages = s.Messages[len(s.Messages)-maxHistory:]
		for len(s.Messages) > 0 && s.Messages[0].Role == "tool" {
			s.Messages = s.Messages[1:]
		}
	}
}

func (p *AgentPlugin) clearSession(chatID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, chatID)
}

// tryStart claims the chat for one run and stores the cancel func so
// agent reset can stop it.
func (p *AgentPlugin) tryStart(chatID int64, cancel context.CancelFunc) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.running[chatID]; ok {
		return false
	}
	p.running[chatID] = cancel
	return true
}

func (p *AgentPlugin) finishRun(chatID int64) {
	p.mu.Lock()
	cancel := p.running[chatID]
	delete(p.running, chatID)
	p.mu.Unlock()
	if cancel != nil {
		// The run is over; cancel releases the deadline timer instead of
		// letting it tick for the rest of runTimeout.
		cancel()
	}
}

func (p *AgentPlugin) isRunning(chatID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.running[chatID]
	return ok
}

// cancelRun stops the in-flight task of a chat, if any.
func (p *AgentPlugin) cancelRun(chatID int64) {
	p.mu.Lock()
	cancel := p.running[chatID]
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ============================================================
// Config snapshot
// ============================================================

type config struct {
	APIKey     string
	BaseURL    string
	Model      string
	Workspace  string
	MaxSteps   int
	CmdTimeout int
}

func (p *AgentPlugin) config() config {
	c := config{
		BaseURL:    defaultBaseURL,
		Model:      defaultModel,
		Workspace:  p.defaultWorkspace(),
		MaxSteps:   defaultMaxSteps,
		CmdTimeout: defaultCmdTimeout,
	}
	if p.set == nil {
		return c
	}
	c.APIKey = p.set.String("api_key")
	if v := strings.TrimSpace(p.set.String("base_url")); v != "" {
		c.BaseURL = strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(p.set.String("model")); v != "" {
		c.Model = v
	}
	if v := strings.TrimSpace(p.set.String("workspace")); v != "" {
		c.Workspace = expandHome(v)
	}
	if v := p.set.Int("max_steps"); v > 0 {
		c.MaxSteps = v
	}
	if v := p.set.Int("cmd_timeout"); v > 0 {
		c.CmdTimeout = v
	}
	return c
}

// defaultWorkspace keeps plugin state under data/agent, which works for a
// non-root install and on every platform.
func (p *AgentPlugin) defaultWorkspace() string {
	if p.dataDir == "" {
		return workspaceDirname
	}
	return filepath.Join(p.dataDir, workspaceDirname)
}

// expandHome resolves a leading ~ so an owner typing "~/agent" does not end
// up with a directory literally named "~".
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}

// ============================================================
// Helpers
// ============================================================

func fieldLine(label, value string) string {
	return "> " + plugin.Bold(label+"：") + " " + plugin.Code(value)
}

// editNoPreview edits the command message without a link preview, which
// ctx.Edit cannot turn off.
func editNoPreview(ctx *plugin.CommandContext, text string) error {
	if ctx.Message == nil || ctx.Message.Message == nil || ctx.API == nil {
		return plugin.ErrNoMessage
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	plain, ents := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesEditMessageRequest{Peer: peer, ID: ctx.Message.Message.ID, Message: plain, NoWebpage: true}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	_, err = ctx.API.MessagesEditMessage(ctx.Context(), req)
	return err
}
