package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
	defaultModel   = "gpt-4o-mini"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "ai",
	Description: "AI 对话",
	DescEN:      "Chat with an AI",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type AIPlugin struct {
	http  *http.Client
	set   plugin.Settings
	host  plugin.Host
	store *historyStore
}

func New() *AIPlugin {
	return &AIPlugin{http: &http.Client{Timeout: 10 * time.Minute}}
}

func (p *AIPlugin) Name() string        { return "ai" }
func (p *AIPlugin) Description() string { return Metadata.Description }
func (p *AIPlugin) DescEN() string      { return Metadata.DescEN }

func (p *AIPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin:  p.Name(),
		Title:   "🤖 AI 对话",
		TitleEN: "🤖 AI chat",
		Settings: []plugin.Setting{
			{
				Key: "api_key", Label: "API 密钥", LabelEN: "API key",
				Kind: plugin.SettingText, Secret: true,
				Hint:   "OpenAI 兼容接口或 Gemini 的密钥",
				HintEN: "Key for an OpenAI-compatible API or Gemini",
			},
			{
				Key: "base_url", Label: "API 地址", LabelEN: "Base URL",
				Kind: plugin.SettingText, Default: defaultBaseURL,
				Hint:   "如 https://api.openai.com/v1 ，按域名自动识别 Gemini / 豆包",
				HintEN: "e.g. https://api.openai.com/v1; Gemini and Doubao are detected from the host",
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
				Hint:   "如 gpt-4o-mini、gemini-2.0-flash、doubao-seed-1-6",
				HintEN: "e.g. gpt-4o-mini, gemini-2.0-flash, doubao-seed-1-6",
				Validate: func(s string) (string, error) {
					s = strings.TrimSpace(s)
					if s == "" {
						return "", plugin.Invalid("模型名不能为空", "model name is required")
					}
					return s, nil
				},
			},
			{
				Key: "system_prompt", Label: "系统提示词", LabelEN: "System prompt",
				Kind:   plugin.SettingText,
				Hint:   "每次对话都携带的角色设定，可留空",
				HintEN: "Persona sent with every request; optional",
			},
			{
				Key: "context_turns", Label: "上下文轮数", LabelEN: "Context turns",
				Kind: plugin.SettingNumber, Default: 8, Min: 0, Max: 50,
				Hint:   "每次提问携带的历史问答轮数，0 为不带记忆",
				HintEN: "Past Q&A turns resent with each question; 0 disables memory",
			},
			{
				Key: "timeout", Label: "超时（秒）", LabelEN: "Timeout (s)",
				Kind: plugin.SettingNumber, Default: 60, Min: 10, Max: 600,
			},
		},
	})
	if err != nil {
		return err
	}
	p.set = set

	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return err
	}
	p.store = newHistoryStore(dir, p.host.Logger(p.Name()))
	if err := p.store.load(); err != nil {
		p.host.Logger(p.Name()).Warn("history load failed, starting fresh", "error", err)
	}

	return mgr.RegisterCommand(&plugin.Command{
		Name:        "ai",
		Description: "AI 对话，支持上下文和多轮追问（模型与提示词在面板设置）",
		DescEN:      "Chat with an AI with per-chat context (model and prompt set in the panel)",
		Usage:       "ai <问题> · 回复消息: ai · ai reset · ai help",
		UsageEN:     "ai <question> · reply with ai · ai reset · ai help",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		RateLimit:   30,
		Handler:     p.handle,
	})
}

func (p *AIPlugin) Start(context.Context) error { return nil }
func (p *AIPlugin) Stop(context.Context) error  { return nil }

// chatConfig snapshots the settings for one request.
type chatConfig struct {
	apiKey     string
	baseURL    string
	model      string
	system     string
	turns      int
	timeoutSec int
}

func (p *AIPlugin) config() chatConfig {
	cfg := chatConfig{
		apiKey: strings.TrimSpace(p.set.String("api_key")),
	}
	cfg.baseURL = strings.TrimSpace(p.set.String("base_url"))
	if cfg.baseURL == "" {
		cfg.baseURL = defaultBaseURL
	}
	cfg.model = strings.TrimSpace(p.set.String("model"))
	if cfg.model == "" {
		cfg.model = defaultModel
	}
	cfg.system = p.set.String("system_prompt")
	cfg.turns = p.set.Int("context_turns")
	if cfg.turns < 0 {
		cfg.turns = 0
	}
	cfg.timeoutSec = p.set.Int("timeout")
	if cfg.timeoutSec < 10 {
		cfg.timeoutSec = 10
	}
	return cfg
}

func (p *AIPlugin) handle(ctx *plugin.CommandContext) error {
	if a := strings.ToLower(ctx.GetArg(0)); a == "help" || a == "h" {
		return editNoPreview(ctx, help(ctx.Tlocal))
	}
	if strings.EqualFold(ctx.GetArg(0), "reset") {
		return p.handleReset(ctx)
	}

	cfg := p.config()
	if cfg.apiKey == "" {
		return editNoPreview(ctx, "❌ "+ctx.Tlocal("还没有配置 API 密钥", "No API key configured yet")+
			"\n> "+ctx.Tlocal("请在机器人面板 /menu → AI 里设置密钥、地址和模型", "Set the key, base URL and model in the bot panel: /menu → AI"))
	}

	// The replied message's text becomes 上下文 for the question, like the
	// source plugin; a bare `ai` while replying re-asks that message.
	question := strings.TrimSpace(ctx.RawArgs)
	contextText := ""
	if ctx.Message != nil && ctx.Message.IsReply {
		if r, err := ctx.ReplyMessage(); err == nil && r != nil {
			contextText = strings.TrimSpace(r.Message)
		}
	}
	if question == "" && contextText != "" {
		question, contextText = contextText, ""
	}
	if question == "" {
		return editNoPreview(ctx, help(ctx.Tlocal))
	}
	if contextText != "" && contextText != question {
		question = "上下文:\n" + contextText + "\n\n问题:\n" + question
	}

	chatID := int64(0)
	if ctx.Message != nil {
		chatID = ctx.Message.ChatID
	}
	hist := p.store.appendUser(chatID, question)

	_ = ctx.Edit("⏳ " + ctx.Tlocal("正在思考…", "Thinking…"))

	answer, err := p.chat(ctx.Context(), cfg, trimTurns(hist, cfg.turns))
	if err != nil {
		p.store.rollbackUser(chatID, question)
		return editNoPreview(ctx, errCard(ctx.Tlocal, err))
	}
	p.store.appendAssistant(chatID, answer)

	parts := chunkText(answerCard(ctx.Tlocal, cfg.model, question, answer), maxMessageRunes)
	if len(parts) == 0 {
		return nil
	}
	if err := editNoPreview(ctx, parts[0]); err != nil {
		return err
	}
	for i, c := range parts[1:] {
		if err := sendNoPreview(ctx, contCard(ctx.Tlocal, i+2, len(parts), c)); err != nil {
			return err
		}
	}
	return nil
}

func (p *AIPlugin) handleReset(ctx *plugin.CommandContext) error {
	chatID := int64(0)
	if ctx.Message != nil {
		chatID = ctx.Message.ChatID
	}
	n := p.store.reset(chatID)
	return editNoPreview(ctx, "🧹 **AI**\n> "+ctx.Tlocal(
		"已清空本会话的对话记录（"+itoa(n)+" 条消息）",
		"Cleared this chat's history ("+itoa(n)+" messages)"))
}

func (p *AIPlugin) chat(ctx context.Context, cfg chatConfig, hist []turn) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.timeoutSec)*time.Second)
	defer cancel()
	switch detectProvider(cfg.baseURL) {
	case providerGemini:
		return geminiChat(ctx, p.http, cfg, hist)
	case providerDoubao:
		return openaiChat(ctx, p.http, cfg, hist, chatEndpoint(cfg.baseURL, providerDoubao))
	default:
		return openaiChat(ctx, p.http, cfg, hist, chatEndpoint(cfg.baseURL, providerOpenAI))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func help(tl func(string, string) string) string {
	return tl(
		"🤖 **AI 对话**\n\n"+
			plugin.Code("ai 你好")+" 向 AI 提问，自动携带本会话历史\n"+
			plugin.Code("ai")+" 回复一条消息：以那条消息为提问\n"+
			"回复任意消息发 "+plugin.Code("ai 追问")+" 可把它当作上下文\n"+
			plugin.Code("ai reset")+" 清空本会话的对话记录\n\n"+
			"API 密钥、地址、模型、系统提示词和上下文轮数都在机器人面板里设置",
		"🤖 **AI chat**\n\n"+
			plugin.Code("ai hello")+" ask the AI; this chat's history is included\n"+
			plugin.Code("ai")+" reply to a message to ask about it\n"+
			"Reply with "+plugin.Code("ai follow-up")+" to use a message as context\n"+
			plugin.Code("ai reset")+" clear this chat's history\n\n"+
			"API key, base URL, model, system prompt and context turns are set in the bot panel")
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

// sendNoPreview posts an extra message to the same chat.
func sendNoPreview(ctx *plugin.CommandContext, text string) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	plain, ents := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesSendMessageRequest{
		Peer:      peer,
		Message:   plain,
		RandomID:  time.Now().UnixNano(),
		NoWebpage: true,
	}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	_, err = ctx.API.MessagesSendMessage(ctx.Context(), req)
	return err
}
