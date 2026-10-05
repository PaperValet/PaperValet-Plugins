package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
	// The TeleBox source defaulted to gpt-4, which has no image input; gpt-4o
	// matches the plugin's image-aware purpose out of the box.
	defaultModel = "gpt-4o"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "xmsl",
	Description: "羡慕死了：AI 生成一句羡慕回复",
	DescEN:      "AI-generated envious one-liners",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type XmslPlugin struct {
	http *http.Client
	set  plugin.Settings
	host plugin.Host
}

func New() *XmslPlugin {
	return &XmslPlugin{http: &http.Client{Timeout: 60 * time.Second}}
}

func (p *XmslPlugin) Name() string        { return "xmsl" }
func (p *XmslPlugin) Description() string { return Metadata.Description }
func (p *XmslPlugin) DescEN() string      { return Metadata.DescEN }

func (p *XmslPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin:  p.Name(),
		Title:   "🤢 羡慕死了",
		TitleEN: "🤢 Envy me",
		Settings: []plugin.Setting{
			{
				Key: "mode", Label: "API 模式", LabelEN: "API mode",
				// No Default: an unset mode lets XMSL_API_MODE apply, like the
				// source's empty-value env fallback.
				Kind: plugin.SettingChoice,
				Choices: []plugin.Choice{
					{Value: "openai", Label: "OpenAI 兼容", LabelEN: "OpenAI-compatible"},
					{Value: "gemini", Label: "Gemini", LabelEN: "Gemini"},
				},
				Hint:   "请求的接口协议，默认 openai（留空可用 XMSL_API_MODE）",
				HintEN: "Wire protocol; openai by default (blank falls back to XMSL_API_MODE)",
			},
			{
				Key: "key", Label: "API 密钥", LabelEN: "API key",
				Kind: plugin.SettingText, Secret: true,
				Hint:   "也可用环境变量 XMSL_API_KEY 提供",
				HintEN: "Or provide XMSL_API_KEY in the environment",
			},
			{
				Key: "baseurl", Label: "API 地址", LabelEN: "Base URL",
				Kind: plugin.SettingText, Default: defaultBaseURL,
				Hint:   "如 https://api.openai.com/v1 ，留空可用 XMSL_BASE_URL",
				HintEN: "e.g. https://api.openai.com/v1; blank falls back to XMSL_BASE_URL",
				Validate: func(s string) (string, error) {
					return normalizeBaseURL(s)
				},
			},
			{
				Key: "model", Label: "模型", LabelEN: "Model",
				Kind: plugin.SettingText, Default: defaultModel,
				Hint:   "需支持图片输入，默认 gpt-4o；留空可用 XMSL_MODEL",
				HintEN: "Must accept image input, gpt-4o by default; blank falls back to XMSL_MODEL",
				Validate: func(s string) (string, error) {
					if strings.TrimSpace(s) == "" {
						return "", nil
					}
					return strings.TrimSpace(s), nil
				},
			},
		},
	})
	if err != nil {
		return err
	}
	p.set = set

	return mgr.RegisterCommand(&plugin.Command{
		Name:        "xmsl",
		Description: "AI 生成一句羡慕回复，支持图片/贴纸",
		DescEN:      "AI-generated envious one-liner, image aware",
		Usage:       "xmsl [文本] | 回复图片/贴纸: xmsl | xmsl help",
		UsageEN:     "xmsl [text] | reply to image/sticker: xmsl | xmsl help",
		Plugin:      p.Name(),
		Category:    "fun",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *XmslPlugin) Start(context.Context) error { return nil }
func (p *XmslPlugin) Stop(context.Context) error  { return nil }

// apiConfig snapshots settings with the environment-variable fallbacks the
// TeleBox source kept (XMSL_API_KEY / XMSL_BASE_URL / XMSL_MODEL / XMSL_API_MODE).
type apiConfig struct {
	mode    string
	key     string
	baseURL string
	model   string
}

func (p *XmslPlugin) config() apiConfig {
	cfg := apiConfig{
		mode: strings.ToLower(strings.TrimSpace(p.set.String("mode"))),
		key:  strings.TrimSpace(p.set.String("key")),
	}
	if cfg.key == "" {
		cfg.key = strings.TrimSpace(getenv("XMSL_API_KEY"))
	}
	cfg.baseURL = strings.TrimSpace(p.set.String("baseurl"))
	if cfg.baseURL == "" {
		cfg.baseURL = strings.TrimSpace(getenv("XMSL_BASE_URL"))
	}
	cfg.baseURL = strings.TrimRight(cfg.baseURL, "/")
	if cfg.baseURL == "" {
		cfg.baseURL = defaultBaseURL
	}
	cfg.model = strings.TrimSpace(p.set.String("model"))
	if cfg.model == "" {
		cfg.model = strings.TrimSpace(getenv("XMSL_MODEL"))
	}
	if cfg.model == "" {
		cfg.model = defaultModel
	}
	// The env mode only applies while the panel has no explicit choice,
	// mirroring the source's "fall back when unset" behavior.
	if cfg.mode == "" {
		if m := strings.ToLower(strings.TrimSpace(getenv("XMSL_API_MODE"))); m == "gemini" || m == "openai" {
			cfg.mode = m
		}
	}
	if cfg.mode != "gemini" {
		cfg.mode = "openai"
	}
	return cfg
}

// getenv is split out so tests can stub the environment.
var getenv = os.Getenv

func helpText(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🤢 **羡慕死了**\n\n"+
			plugin.Code("xmsl <内容>")+" — AI 生成一句羡慕回复\n"+
			"回复图片/贴纸 "+plugin.Code("xmsl")+" — 识别图片后羡慕\n"+
			plugin.Code("xmsl help")+" — 本帮助\n\n"+
			"**支持的媒体**\n"+
			"• 图片 jpeg/png/gif/webp\n"+
			"• 静态贴纸（webp）\n"+
			"• 视频贴纸（webm，需 ffmpeg）\n"+
			"• 动画贴纸（tgs，需 rlottie-python + ffmpeg）\n\n"+
			"API 模式、密钥、地址、模型在机器人面板里设置（也可用环境变量 XMSL_API_KEY 等提供）",
		"🤢 **So envious**\n\n"+
			plugin.Code("xmsl <text>")+" — AI writes one envious line\n"+
			"Reply to an image/sticker with "+plugin.Code("xmsl")+" — recognize it, then envy\n"+
			plugin.Code("xmsl help")+" — this help\n\n"+
			"**Supported media**\n"+
			"• images: jpeg/png/gif/webp\n"+
			"• static stickers (webp)\n"+
			"• video stickers (webm, needs ffmpeg)\n"+
			"• animated stickers (tgs, needs rlottie-python + ffmpeg)\n\n"+
			"API mode, key, base URL and model are set in the bot panel (or via XMSL_API_KEY etc.)")
}

// statusCard is the bare-command reply, replacing the source's showStatus.
func (p *XmslPlugin) statusCard(ctx *plugin.CommandContext) string {
	cfg := p.config()
	icon := "🟠"
	if cfg.mode == "gemini" {
		icon = "🔵"
	}
	keyState := "❌ " + ctx.Tlocal("未设置", "not set")
	if cfg.key != "" {
		keyState = "✅ " + ctx.Tlocal("已设置", "set")
	}
	return "🤢 **" + ctx.Tlocal("羡慕死了 · 状态", "Envy · Status") + "**\n" +
		icon + " " + ctx.Tlocal("模式", "Mode") + "  " + plugin.Code(cfg.mode) + "\n" +
		"🔑 " + ctx.Tlocal("密钥", "Key") + "  " + keyState + "\n" +
		"📍 " + ctx.Tlocal("地址", "URL") + "  " + plugin.Code(cfg.baseURL) + "\n" +
		"🤖 " + ctx.Tlocal("模型", "Model") + "  " + plugin.Code(cfg.model) + "\n\n" +
		ctx.Tlocal("设置在机器人面板里修改；发送 ", "Edit them in the bot panel; send ") + plugin.Code("xmsl help") + ctx.Tlocal(" 查看帮助", " for help")
}

func (p *XmslPlugin) handle(ctx *plugin.CommandContext) error {
	if len(ctx.Args) == 1 && strings.EqualFold(ctx.Args[0], "help") {
		return ctx.Edit(helpText(ctx))
	}

	isReply := ctx.Message != nil && ctx.Message.IsReply && ctx.Message.ReplyToID > 0
	question := strings.TrimSpace(ctx.RawArgs)

	// Reply without arguments: use the replied message's media (caption rides
	// along as the question) or its text, like the source.
	if isReply && question == "" {
		reply, err := ctx.ReplyMessage()
		if err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("获取回复消息失败：", "Failed to load the replied message: ") + plugin.Escape(err.Error()))
		}
		media, merr := p.mediaFromReply(ctx, reply)
		if merr != nil {
			var ce *convertError
			if errors.As(merr, &ce) {
				return ctx.Edit(convertErrText(ctx, ce))
			}
			return ctx.Edit("❌ " + plugin.Escape(merr.Error()))
		}
		if media != nil {
			return p.ask(ctx, strings.TrimSpace(reply.Message), media)
		}
		question = strings.TrimSpace(reply.Message)
		if question == "" {
			return ctx.Edit("❌ " + ctx.Tlocal("被回复的消息没有文字或可识别的媒体", "The replied message has no text or recognizable media"))
		}
	}

	if question == "" {
		return ctx.Edit(p.statusCard(ctx))
	}
	return p.ask(ctx, question, nil)
}
