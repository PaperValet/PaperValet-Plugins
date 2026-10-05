// Package main implements yvlu: turn replied messages into quote stickers or
// images via a quote-api instance, ported from the TeleBox yvlu plugin.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	maxCount     = 5
	quoteTimeout = 90 * time.Second
)

var Metadata = &plugin.PluginMetadata{
	Name:        "yvlu",
	Description: "把回复的消息做成语录贴纸/图片",
	DescEN:      "Turn replied messages into quote stickers or images",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// YvluPlugin generates quote stickers/images from replied messages.
type YvluPlugin struct {
	http     *http.Client
	settings plugin.Settings
	tmpDir   string
	log      plugin.Logger
}

func New() *YvluPlugin {
	return &YvluPlugin{http: &http.Client{Timeout: quoteTimeout}}
}

func (p *YvluPlugin) Name() string        { return Metadata.Name }
func (p *YvluPlugin) Description() string { return Metadata.Description }
func (p *YvluPlugin) DescEN() string      { return Metadata.DescEN }

var stickerSetRe = regexp.MustCompile(`^[a-zA-Z0-9_]{1,64}$`)

// validateStickerSet normalizes the pack short name typed in the settings
// panel; an empty value clears the setting.
func validateStickerSet(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if !stickerSetRe.MatchString(s) {
		return "", plugin.Invalid(
			"贴纸包名称只能包含字母、数字和下划线（1-64 字符）",
			"Pack names may only contain letters, digits and underscores (1-64 chars)")
	}
	return s, nil
}

func (p *YvluPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	host := mgr.Host()
	p.log = host.Logger("yvlu")
	set, err := host.Settings(&plugin.SettingsSpec{
		Plugin:  p.Name(),
		Title:   "💬 语录生成",
		TitleEN: "💬 Quote maker",
		Settings: []plugin.Setting{{
			Key: "stickerSet", Label: "贴纸包名称", LabelEN: "Sticker pack name",
			Hint:   "yvlu s 保存贴纸的目标贴纸包 shortName，不存在时自动创建；仅字母、数字、下划线",
			HintEN: "Target pack shortName for yvlu s, auto-created when missing; letters, digits and underscores only",
			Kind:   plugin.SettingText, Default: "",
			Validate: validateStickerSet,
		}},
	})
	if err != nil {
		return err
	}
	p.settings = set
	if dir, derr := host.DataDir(p.Name()); derr == nil {
		p.tmpDir = filepath.Join(dir, "tmp")
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "yvlu",
		Description: "回复消息生成语录贴纸/图片，可存入贴纸包",
		DescEN:      "Reply to generate a quote sticker/image, saveable to a pack",
		Usage:       "yvlu [数量 1-5] | yvlu r [数量] | yvlu webp|image|stories [数量] | yvlu s",
		UsageEN:     "yvlu [count 1-5] | yvlu r [count] | yvlu webp|image|stories [count] | yvlu s",
		Plugin:      p.Name(),
		Category:    "fun",
		Handler:     p.handle,
	})
}

func (p *YvluPlugin) Start(context.Context) error { return nil }
func (p *YvluPlugin) Stop(context.Context) error  { return nil }

// ---------------------------------------------------------------- args

type yvluArgs struct {
	count     int    // messages to quote, 1..maxCount
	withReply bool   // include what the messages replied to
	format    string // "", "webp", "image", "stories"
	save      bool   // yvlu s: save replied sticker/photo to the pack
	help      bool   // unrecognized arguments
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// normalizeFormat maps the four accepted spellings; "" means not a format.
func normalizeFormat(s string) string {
	switch strings.ToLower(s) {
	case "webp":
		return "webp"
	case "image", "png":
		return "image"
	case "stories":
		return "stories"
	}
	return ""
}

func atoiOr(s string, def int) int {
	if !isDigits(s) {
		return def
	}
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
		if n > 99 {
			return 99 // keep parse sane; the max check reports the error
		}
	}
	return n
}

// parseArgs mirrors the source: "" or N; r [N] | r fmt [N]; s; fmt [N].
func parseArgs(args []string) yvluArgs {
	a := yvluArgs{count: 1}
	if len(args) == 0 || args[0] == "" {
		return a
	}
	switch first := args[0]; {
	case isDigits(first):
		a.count = atoiOr(first, 1)
	case strings.EqualFold(first, "r"):
		a.withReply = true
		rest := args[1:]
		if len(rest) > 0 {
			if f := normalizeFormat(rest[0]); f != "" {
				a.format = f
				if len(rest) > 1 {
					a.count = atoiOr(rest[1], 1)
				}
			} else {
				a.count = atoiOr(rest[0], 1)
			}
		}
	case strings.EqualFold(first, "s"):
		a.save = true
	default:
		if f := normalizeFormat(first); f != "" {
			a.format = f
			if len(args) > 1 {
				a.count = atoiOr(args[1], 1)
			}
		} else {
			a.help = true
		}
	}
	if a.count < 1 {
		a.count = 1
	}
	return a
}

// ---------------------------------------------------------------- handler

func (p *YvluPlugin) help(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"💬 **语录生成**\n\n"+
			"**基本用法**\n"+
			"• "+plugin.Code("yvlu")+" 回复一条消息，生成语录贴纸\n"+
			"• "+plugin.Code("yvlu 3")+" 生成该消息及其后 2 条（共 3 条，最多 "+fmt.Sprint(maxCount)+" 条）\n"+
			"• "+plugin.Code("yvlu r")+" 附带被引用消息的原文\n\n"+
			"**输出格式**（默认 webp 贴纸）\n"+
			"• "+plugin.Code("yvlu webp")+" 静态 WebP 贴纸\n"+
			"• "+plugin.Code("yvlu image")+" / "+plugin.Code("yvlu png")+" 背景大图 PNG\n"+
			"• "+plugin.Code("yvlu stories")+" 故事竖版 PNG\n"+
			"• 组合："+plugin.Code("yvlu r image 3")+"\n\n"+
			"**存入贴纸包**\n"+
			"• "+plugin.Code("yvlu s")+" 回复贴纸/图片，存入面板配置的贴纸包（不存在则自动创建）\n\n"+
			"💡 回复时选中部分文字可只引用该片段",
		"💬 **Quote maker**\n\n"+
			"**Basics**\n"+
			"• "+plugin.Code("yvlu")+" reply to a message to make a quote sticker\n"+
			"• "+plugin.Code("yvlu 3")+" quote it plus the 2 following messages (max "+fmt.Sprint(maxCount)+")\n"+
			"• "+plugin.Code("yvlu r")+" include the text each message replied to\n\n"+
			"**Output format** (webp sticker by default)\n"+
			"• "+plugin.Code("yvlu webp")+" static WebP sticker\n"+
			"• "+plugin.Code("yvlu image")+" / "+plugin.Code("yvlu png")+" big PNG image\n"+
			"• "+plugin.Code("yvlu stories")+" story-size PNG\n"+
			"• combine: "+plugin.Code("yvlu r image 3")+"\n\n"+
			"**Save to a pack**\n"+
			"• "+plugin.Code("yvlu s")+" reply to a sticker/photo to store it in the pack set in the panel (created when missing)\n\n"+
			"💡 Reply with a text selection to quote just that fragment")
}

func (p *YvluPlugin) handle(ctx *plugin.CommandContext) error {
	if len(ctx.Args) == 1 && strings.EqualFold(ctx.Args[0], "help") {
		return ctx.Edit(p.help(ctx))
	}
	if ctx.API == nil || ctx.Message == nil || ctx.Message.Message == nil {
		return ctx.Edit("❌ " + ctx.Tlocal("客户端未就绪", "Client not ready"))
	}
	args := parseArgs(ctx.Args)
	switch {
	case args.help:
		return ctx.Edit(p.help(ctx))
	case args.save:
		return p.handleSave(ctx)
	default:
		return p.handleGenerate(ctx, args)
	}
}

func (p *YvluPlugin) handleGenerate(ctx *plugin.CommandContext, opts yvluArgs) error {
	if !ctx.Message.IsReply || ctx.Message.ReplyToID == 0 {
		return ctx.Edit("❌ " + ctx.Tlocal("请回复一条消息", "Reply to a message first"))
	}
	if opts.count > maxCount {
		return ctx.Edit("❌ " + ctx.Tlocal(
			fmt.Sprintf("太多了 哒咩（最多 %d 条）", maxCount),
			fmt.Sprintf("Too many, at most %d messages", maxCount)))
	}
	_ = ctx.Edit("🔄 " + ctx.Tlocal("正在生成语录...", "Generating the quote..."))

	peer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit("❌ " + plugin.Escape(describeErr(err)))
	}
	env := newEnv(ctx.Context(), ctx.API, peer, p.tmp())

	replied, err := env.fetchOne(ctx.Message.ReplyToID)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("获取回复消息失败: ", "Failed to load the replied message: ") +
			plugin.Escape(describeErr(err)))
	}

	// count=1 quotes only the replied message; more also takes the messages
	// right after it (same semantics as the source's reverse history fetch).
	msgs := []*tg.Message{replied}
	if opts.count > 1 {
		if hist, herr := env.fetchFollowing(replied.ID, opts.count); herr == nil && len(hist) > 0 {
			msgs = append(msgs, hist...)
			if len(msgs) > opts.count {
				msgs = msgs[:opts.count]
			}
		}
	}

	// Reply with a text selection: quote just that fragment of the message.
	var frag *quotedFragment
	if hdr, ok := ctx.Message.Message.ReplyTo.(*tg.MessageReplyHeader); ok && hdr.Quote && strings.TrimSpace(hdr.QuoteText) != "" {
		frag = &quotedFragment{text: hdr.QuoteText, entities: hdr.QuoteEntities}
	}

	items, err := buildItemsChecked(env, msgs, opts, frag)
	if err != nil {
		return ctx.Edit("❌ " + plugin.Escape(describeErr(err)))
	}

	data, kind, err := p.postQuote(ctx.Context(), buildRequest(opts.format, items))
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("语录生成失败: ", "Quote generation failed: ") +
			plugin.Escape(describeErr(err)))
	}

	_ = ctx.Edit("📤 " + ctx.Tlocal("生成成功，正在发送...", "Generated, sending..."))
	path, err := p.writeTemp(kind, data)
	if err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()))
	}
	defer os.Remove(path)
	if err := p.sendResult(ctx, path, kind, replied.ID); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("发送失败: ", "Send failed: ") + plugin.Escape(describeErr(err)))
	}
	if err := ctx.Delete(); err != nil {
		_ = ctx.Edit("✅ " + ctx.Tlocal("语录已生成", "Quote generated"))
	}
	return nil
}

// tmp returns the temp dir, falling back to the system temp dir.
func (p *YvluPlugin) tmp() string {
	if p.tmpDir == "" {
		p.tmpDir = filepath.Join(os.TempDir(), "yvlu")
	}
	return p.tmpDir
}

func (p *YvluPlugin) writeTemp(kind string, data []byte) (string, error) {
	if err := os.MkdirAll(p.tmp(), 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(p.tmp(), fmt.Sprintf("quote_%d.%s", time.Now().UnixNano(), kind))
	return path, os.WriteFile(path, data, 0o600)
}

// describeErr turns errors (incl. Telegram API errors) into short text.
func describeErr(err error) string {
	if err == nil {
		return "unknown error"
	}
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf("FLOOD_WAIT %d s", int(d.Seconds()))
	}
	var gerr *tgerr.Error
	if errors.As(err, &gerr) {
		return gerr.Type + ": " + gerr.Message
	}
	s := err.Error()
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return s
}
