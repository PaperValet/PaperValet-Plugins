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
	defaultWorkspace  = "/root/agent-workspace"
	defaultMaxSteps   = 15
	defaultCmdTimeout = 60
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
		"> " + tlocal("列目录 · 读文件 · 搜索 · 写文件 · 执行命令 · 发送文件",
			"list · read · search · write · run command · send file"),
		"",
		"> " + ctx.Tlocal("模型、密钥与限制在机器人面板设置：/menu → AI 智能体",
			"Model, key and limits live in the bot panel: /menu → AI agent"),
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
	running  map[int64]bool     // chatID → a task is in flight

	selfID int64
	tools  []toolSpec
	http   *httpDoer
}

func New() *AgentPlugin {
	return &AgentPlugin{
		sessions: map[int64]*session{},
		running:  map[int64]bool{},
		http:     newHTTPDoer(),
	}
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
				Kind: plugin.SettingText, Default: defaultWorkspace,
				Hint:   "相对路径的基准目录",
				HintEN: "Base directory for relative paths",
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
	_ = os.MkdirAll(p.config().Workspace, 0o755)
	p.logger.Info("agent started", "self", p.selfID, "model", p.config().Model)
	return nil
}

func (p *AgentPlugin) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions = map[int64]*session{}
	p.running = map[int64]bool{}
	return nil
}

// ============================================================
// Command entry
// ============================================================

func (p *AgentPlugin) handle(ctx *plugin.CommandContext) error {
	arg := strings.TrimSpace(ctx.RawArgs)

	if a := strings.ToLower(ctx.GetArg(0)); a == "help" || a == "h" {
		return editNoPreview(ctx, helpText(ctx))
	}
	if arg == "" {
		return editNoPreview(ctx, helpText(ctx))
	}
	switch strings.ToLower(arg) {
	case "reset", "new":
		p.clearSession(ctx.Message.ChatID)
		return editNoPreview(ctx, "🧹 "+ctx.Tlocal("已清空对话上下文。", "Conversation context cleared."))
	case "status":
		return p.handleStatus(ctx)
	}

	cfg := p.config()
	if cfg.APIKey == "" {
		return editNoPreview(ctx, "❌ "+ctx.Tlocal("还没有配置 API 密钥", "No API key configured yet")+
			"\n> "+ctx.Tlocal("请在机器人面板 /menu → AI 智能体 里设置密钥、地址和模型",
			"Set the key, base URL and model in the bot panel: /menu → AI agent"))
	}

	chatID := ctx.Message.ChatID
	if !p.tryStart(chatID) {
		return editNoPreview(ctx, "⏳ "+ctx.Tlocal("上一个任务还在执行，请稍候（或发 agent reset 清空上下文）",
			"A task is still running, please wait (or send agent reset)"))
	}

	msgID := 0
	if ctx.Message != nil && ctx.Message.Message != nil {
		msgID = ctx.Message.Message.ID
	}
	go p.runTask(chatID, msgID, arg)
	return nil
}

func (p *AgentPlugin) handleStatus(ctx *plugin.CommandContext) error {
	cfg := p.config()
	s := p.getSession(ctx.Message.ChatID)
	turns := 0
	if s != nil {
		turns = len(s.Messages)
	}
	lines := []string{
		"📊 " + ctx.Tlocal("智能体状态", "Agent status"),
		"",
		fieldLine(ctx.Tlocal("模型", "Model"), cfg.Model),
		fieldLine(ctx.Tlocal("地址", "Base URL"), cfg.BaseURL),
		fieldLine(ctx.Tlocal("工作区", "Workspace"), cfg.Workspace),
		fieldLine(ctx.Tlocal("最大步数", "Max steps"), fmt.Sprintf("%d", cfg.MaxSteps)),
		fieldLine(ctx.Tlocal("历史消息", "History"), fmt.Sprintf("%d", turns)),
	}
	return editNoPreview(ctx, strings.Join(lines, "\n"))
}

// ============================================================
// Session state
// ============================================================

type session struct {
	Messages []chatMessage
}

func (p *AgentPlugin) getSession(chatID int64) *session {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[chatID]
}

func (p *AgentPlugin) appendHistory(chatID int64, msgs ...chatMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[chatID]
	if s == nil {
		s = &session{}
		p.sessions[chatID] = s
	}
	s.Messages = append(s.Messages, msgs...)
	const maxHistory = 40
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

func (p *AgentPlugin) tryStart(chatID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running[chatID] {
		return false
	}
	p.running[chatID] = true
	return true
}

func (p *AgentPlugin) finishRun(chatID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.running, chatID)
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
		Workspace:  defaultWorkspace,
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
		c.Workspace = v
	}
	if v := p.set.Int("max_steps"); v > 0 {
		c.MaxSteps = v
	}
	if v := p.set.Int("cmd_timeout"); v > 0 {
		c.CmdTimeout = v
	}
	return c
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
