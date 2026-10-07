// Package sticker ports three TeleBox plugins into one command:
// sticker (favorite stickers into packs), pic_to_sticker and
// sticker_to_pic.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	maxPackCap = 120 // Telegram limit per pack (source uses 120)
	maxPackTry = 50  // auto-named packs to try
)

// tmpDirRoot is derived from the plugin DataDir in Init; the fallback keeps
// the old relative path when no host is available (tests).
var tmpDirRoot = "data/sticker/tmp"

// baseEmojis backs up stickers that carry no alt emoji (source BASE_EMOJIS).
var baseEmojis = []string{"😀", "😁", "😂", "🤣", "😊", "😇", "🙂", "😉", "😋", "😎", "😍", "😘", "😜", "🤗", "🤔", "😴", "😌", "😅", "😆", "😄"}

type StickerPlugin struct {
	host plugin.Host
	set  plugin.Settings
}

func New() *StickerPlugin { return &StickerPlugin{} }

var Metadata = &plugin.PluginMetadata{
	Name:        "sticker",
	Description: "贴纸收藏、图转贴纸、贴纸转图",
	DescEN:      "Favorite stickers into packs, photo→sticker, sticker→photo",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

func (p *StickerPlugin) Name() string        { return "sticker" }
func (p *StickerPlugin) Description() string { return Metadata.Description }
func (p *StickerPlugin) DescEN() string      { return Metadata.DescEN }

func (p *StickerPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	if p.host != nil {
		if dir, err := p.host.DataDir("sticker"); err == nil && dir != "" {
			tmpDirRoot = filepath.Join(dir, "tmp")
			_ = os.MkdirAll(tmpDirRoot, 0o755)
		}
	}

	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin:  "sticker",
		Title:   "🧩 贴纸",
		TitleEN: "🧩 Sticker",
		Settings: []plugin.Setting{
			{Key: "emoji", Label: "默认表情", LabelEN: "Default emoji",
				Hint: "图转贴纸的默认表情", HintEN: "Default emoji for photo→sticker",
				Kind: plugin.SettingText, Default: "🙂",
				Validate: func(s string) (string, error) {
					s = strings.TrimSpace(s)
					if s == "" {
						return "", plugin.Invalid("表情不能为空", "emoji must not be empty")
					}
					return s, nil
				}},
			{Key: "size", Label: "贴纸边长", LabelEN: "Sticker size",
				Hint: "128-512 像素", HintEN: "128-512 pixels",
				Kind: plugin.SettingNumber, Default: 512, Min: 128, Max: 512},
			{Key: "quality", Label: "贴纸质量", LabelEN: "Sticker quality",
				Hint: "1-100，越大画质越好", HintEN: "1-100, higher is better",
				Kind: plugin.SettingNumber, Default: 90, Min: 1, Max: 100},
			{Key: "bg", Label: "背景", LabelEN: "Background",
				Hint: "透明 / 白色 / 黑色", HintEN: "transparent / white / black",
				Kind: plugin.SettingChoice, Default: "transparent",
				Choices: []plugin.Choice{
					{Value: "transparent", Label: "透明", LabelEN: "Transparent"},
					{Value: "white", Label: "白色", LabelEN: "White"},
					{Value: "black", Label: "黑色", LabelEN: "Black"},
				}},
			{Key: "pack", Label: "默认贴纸包", LabelEN: "Default sticker pack",
				Hint:   "收藏贴纸的目标包 shortName，留空自动用 用户名_static/_animated/_video；保存时验证包存在",
				HintEN: "Target pack shortName for favoriting; empty auto-uses username_static/_animated/_video; validated on save",
				Kind:   plugin.SettingText, Default: "",
				Validate: func(s string) (string, error) {
					s = strings.TrimSpace(s)
					if s == "" {
						return "", nil // empty = auto packs
					}
					if !validPackName(s) {
						return "", plugin.Invalid(
							"贴纸包名只能包含字母、数字和下划线，且必须以字母开头",
							"Pack names may only contain letters, digits and underscores, starting with a letter")
					}
					if err := p.validatePackExists(s); err != nil {
						return "", err
					}
					return s, nil
				}},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	p.migrateLegacyConfig()

	return mgr.RegisterCommand(&plugin.Command{
		Name:        "sticker",
		Description: "收藏贴纸到贴纸包；图转贴纸；贴纸转图片",
		DescEN:      "Favorite stickers into packs; photo→sticker; sticker→photo",
		Usage:       "sticker（回复贴纸收藏）· sticker to <包名> · sticker pic [表情|batch]（回复图片）· sticker topng [doc|png]（回复贴纸）",
		UsageEN:     "sticker (reply to favorite) · sticker to <pack> · sticker pic [emoji|batch] (reply photo) · sticker topng [doc|png] (reply sticker)",
		Plugin:      "sticker",
		Category:    "tools",
		Handler:     p.handle,
	})
}

func (p *StickerPlugin) Start(_ context.Context) error {
	_ = os.MkdirAll(tmpDirRoot, 0o755)
	return nil
}

func (p *StickerPlugin) Stop(context.Context) error { return nil }

// ---------------------------------------------------------------- settings

// validatePackExists checks with the Telegram API that a pack short name is
// reachable, for the settings panel's Validate. Offline (no client yet) the
// syntax check above is the best available and favorite re-checks anyway.
func (p *StickerPlugin) validatePackExists(pack string) error {
	if p.host == nil {
		return nil
	}
	api := p.host.API()
	if api == nil {
		return nil
	}
	_, err := api.MessagesGetStickerSet(context.Background(), &tg.MessagesGetStickerSetRequest{
		Stickerset: &tg.InputStickerSetShortName{ShortName: pack},
		Hash:       0,
	})
	if err != nil {
		if tgerr.Is(err, "STICKERSET_INVALID") {
			return plugin.Invalid(
				fmt.Sprintf("无法访问贴纸包 %s，请确保它存在且您有权访问", pack),
				fmt.Sprintf("Cannot access sticker pack %s; make sure it exists and you can access it", pack))
		}
		if _, ok := tgerr.AsFloodWait(err); ok {
			return plugin.Invalid("请求过于频繁，请稍后重试", "Too many requests, try again later")
		}
		return nil // transient network issues must not block saving
	}
	return nil
}

// defaultPack reads the default pack from the settings panel.
func (p *StickerPlugin) defaultPack() string {
	if p.set == nil {
		return ""
	}
	return strings.TrimSpace(p.set.String("pack"))
}

// migrateLegacyConfig moves a legacy config.json DefaultPack into the
// settings panel (pack key) once, when the panel value was never set; the
// legacy file is removed afterwards.
func (p *StickerPlugin) migrateLegacyConfig() {
	if p.host == nil || p.set == nil {
		return
	}
	if dir, err := p.host.DataDir("sticker"); err == nil && dir != "" {
		migrateLegacyPack(filepath.Join(dir, "config.json"), p.set)
	}
}

// migrateLegacyPack carries a legacy DefaultPack into the panel: fill it in
// when the panel value was never set, then remove the legacy file either
// way. Old configs with an empty DefaultPack are just dropped.
func migrateLegacyPack(path string, set plugin.Settings) {
	if set == nil {
		return
	}
	cfg := loadConfig(path)
	if cfg.DefaultPack != "" && strings.TrimSpace(set.String("pack")) == "" {
		_ = set.Set("pack", cfg.DefaultPack)
	}
	_ = os.Remove(path)
}

// ---------------------------------------------------------------- dispatch

// handle routes subcommands; also exported for tests through parseSub.
func (p *StickerPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil || ctx.API == nil {
		return plugin.ErrNoMessage
	}
	sub := parseSub(ctx.Args)
	if sub.kind == subHelp {
		return ctx.Edit(helpText(ctx))
	}
	if sub.kind == subUnknown {
		return ctx.Edit("❌ " + ctx.Tlocal("未知子命令", "Unknown subcommand") + " " + plugin.Code(sub.unknown) + "\n\n" + helpText(ctx))
	}
	switch sub.kind {
	case subFav, subFavTo:
		// Reply to a sticker → favorite (a bare pack arg doubles as the
		// target pack, more useful than the source's silent ignore).
		if reply, err := ctx.ReplyMessage(); err == nil && reply != nil && isStickerMsg(reply) {
			return p.handleFavorite(ctx, sub)
		}
		if sub.kind == subFavTo {
			return ctx.Edit("❌ " + ctx.Tlocal("请回复一个贴纸消息", "Reply to a sticker message"))
		}
		return p.handleFavorite(ctx, sub)
	case subStatus:
		return p.handleStatus(ctx)
	case subPic, subPicBatch:
		return p.handlePicToSticker(ctx, sub)
	case subToPic:
		return p.handleStickerToPic(ctx, sub)
	}
	return ctx.Edit(helpText(ctx))
}

// ---------------------------------------------------------------- helpers

// tgErrText renders a Telegram error as a short bilingual user message.
func tgErrText(ctx *plugin.CommandContext, err error) error {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return ctx.Edit(fmt.Sprintf("❌ %s\n\n%s",
			ctx.Tlocal("请求过于频繁", "Too many requests"),
			ctx.Tlocal(fmt.Sprintf("请等待 %d 秒后重试", int(d.Seconds())), fmt.Sprintf("Please retry in %d seconds", int(d.Seconds())))))
	}
	if tgerr.Is(err, "STICKERSET_INVALID") {
		return ctx.Edit("❌ " + ctx.Tlocal("贴纸包不存在或无权访问", "Sticker set does not exist or is not accessible"))
	}
	msg := err.Error()
	if e, ok := tgerr.As(err); ok {
		msg = e.Type + ": " + e.Message
	}
	return ctx.Edit("❌ " + plugin.Escape(msg))
}

// docOf extracts the document from a message's media.
func docOf(m *tg.Message) (*tg.Document, bool) {
	if m == nil {
		return nil, false
	}
	md, ok := m.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, false
	}
	d, ok := md.Document.(*tg.Document)
	return d, ok
}

// isStickerMsg reports whether the message carries a sticker document.
func isStickerMsg(m *tg.Message) bool {
	d, ok := docOf(m)
	if !ok {
		return false
	}
	_, is := stickerAttr(d)
	return is
}

// stickerAttr returns the sticker attribute of a document.
func stickerAttr(d *tg.Document) (*tg.DocumentAttributeSticker, bool) {
	for _, a := range d.Attributes {
		if s, ok := a.(*tg.DocumentAttributeSticker); ok {
			return s, true
		}
	}
	return nil, false
}

// stickerKind classifies a sticker document by mime type.
type stickerKind int

const (
	kindStatic stickerKind = iota
	kindAnimated
	kindVideo
)

func classifySticker(d *tg.Document) stickerKind {
	switch d.MimeType {
	case "application/x-tgsticker":
		return kindAnimated
	case "video/webm":
		return kindVideo
	}
	return kindStatic
}

func kindSuffix(k stickerKind) string {
	switch k {
	case kindAnimated:
		return "_animated"
	case kindVideo:
		return "_video"
	}
	return "_static"
}

// kindLabel returns a bilingual label for a sticker kind.
func kindLabel(k stickerKind, tl func(string, string) string) string {
	switch k {
	case kindAnimated:
		return tl("动态", "animated")
	case kindVideo:
		return tl("视频", "video")
	}
	return tl("静态", "static")
}

// randomEmoji picks a fallback base emoji.
func randomEmoji() string {
	return baseEmojis[int(timeNowUnixNano())%len(baseEmojis)]
}

// meUser fetches the logged-in account.
func meUser(ctx *plugin.CommandContext) (*tg.User, error) {
	users, err := ctx.API.UsersGetUsers(ctx.Context(), []tg.InputUserClass{&tg.InputUserSelf{}})
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if me, ok := u.(*tg.User); ok {
			return me, nil
		}
	}
	return nil, errors.New("cannot get self user")
}

// usernameOf returns the primary username, falling back to collectible ones.
func usernameOf(u *tg.User) string {
	if s := strings.TrimSpace(u.Username); s != "" {
		return s
	}
	for _, un := range u.Usernames {
		if un.Active && strings.TrimSpace(un.Username) != "" {
			return strings.TrimSpace(un.Username)
		}
	}
	return ""
}

// sendMessage sends a plain text message to peer (no markdown).
func sendMessage(ctx *plugin.CommandContext, peer tg.InputPeerClass, text string) error {
	_, err := ctx.API.MessagesSendMessage(ctx.Context(), &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  text,
		RandomID: timeNowUnixNano(),
	})
	return err
}

// resolveStickersBot resolves the @stickers bot peer.
func resolveStickersBot(ctx *plugin.CommandContext) (tg.InputPeerClass, error) {
	if ctx.PeerResolver == nil {
		return nil, errors.New("no peer resolver")
	}
	return ctx.PeerResolver.ResolveUsername(ctx.Context(), "stickers")
}

// inputDocOf builds the InputDocument for a sticker document.
func inputDocOf(d *tg.Document) *tg.InputDocument {
	return &tg.InputDocument{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}
}
