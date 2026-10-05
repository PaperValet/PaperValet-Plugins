package main

import (
	"strings"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// subKind enumerates the sticker command's subcommands.
type subKind int

const (
	subUnknown  subKind = iota
	subHelp             // help / h
	subFav              // (no args, reply to sticker)
	subFavTo            // to <pack> (reply to sticker)
	subPack             // <pack>
	subCancel           // cancel
	subStatus           // (no args, no sticker reply) — show current settings
	subPic              // pic [emoji]
	subPicBatch         // pic batch
	subToPic            // topng [png|doc|transparent|emoji]
)

// sub is the parsed command line.
type sub struct {
	kind    subKind
	pack    string // subFavTo / subPack
	emoji   string // subPic custom emoji
	png     bool   // subToPic: PNG output (default JPG)
	doc     bool   // subToPic: send as document
	transp  bool   // subToPic: keep transparency (PNG)
	unknown string // subUnknown: the offending token
}

// toPicOpts reuses parseToPicArgs.
type toPicOpts struct {
	format string // "jpg" | "png"
	doc    bool
	transp bool
}

var reservedWords = map[string]bool{
	"help": true, "h": true, "to": true, "cancel": true,
	"pic": true, "batch": true, "topng": true, "png": true,
	"doc": true, "transparent": true, "status": true, "config": true,
}

// validPackName reports whether a Telegram sticker pack short name is valid:
// letters, digits and underscores, must start with a letter, 1-64 chars.
func validPackName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	c := s[0]
	if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// parseSub parses the sticker command's args (first line only).
func parseSub(args []string) sub {
	if len(args) == 0 {
		return sub{kind: subFav} // reply-to-sticker, or status when no sticker
	}
	first := strings.ToLower(args[0])
	switch first {
	case "help", "h":
		return sub{kind: subHelp}
	case "to":
		if len(args) >= 2 {
			return sub{kind: subFavTo, pack: args[1]}
		}
		return sub{kind: subUnknown, unknown: "to"}
	case "cancel":
		return sub{kind: subCancel}
	case "pic":
		if len(args) >= 2 && strings.EqualFold(args[1], "batch") {
			return sub{kind: subPicBatch}
		}
		if len(args) >= 2 && reservedWords[strings.ToLower(args[1])] {
			return sub{kind: subUnknown, unknown: args[1]}
		}
		s := sub{kind: subPic}
		if len(args) >= 2 {
			s.emoji = args[1]
		}
		return s
	case "topng", "tojpg":
		return parseToPicSub(args)
	case "status", "config":
		return sub{kind: subStatus}
	}
	if len(args) == 1 && !reservedWords[first] {
		return sub{kind: subPack, pack: args[0]}
	}
	return sub{kind: subUnknown, unknown: args[0]}
}

// parseToPicSub parses sticker topng variants:
//
//	sticker topng                 → JPG
//	sticker topng png             → PNG, no transparency
//	sticker topng transparent     → PNG, keep transparency
//	sticker topng doc [png] [transparent] → document (jpg default)
func parseToPicSub(args []string) sub {
	s := sub{kind: subToPic, png: false, doc: false, transp: false}
	rest := args[1:]
	for _, a := range rest {
		switch strings.ToLower(a) {
		case "png":
			s.png = true
		case "doc":
			s.doc = true
		case "transparent", "trans":
			s.transp = true
		case "jpg", "jpeg":
			s.png = false
		default:
			return sub{kind: subUnknown, unknown: a}
		}
	}
	if s.transp {
		s.png = true
	}
	return s
}

// parseToPicArgs is the pure form used by tests and handleStickerToPic.
func parseToPicArgs(args []string) (toPicOpts, bool) {
	s := parseToPicSub(args)
	if s.kind != subToPic {
		return toPicOpts{}, false
	}
	f := "jpg"
	if s.png {
		f = "png"
	}
	return toPicOpts{format: f, doc: s.doc, transp: s.transp}, true
}

// helpText renders the bilingual help card.
func helpText(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	var b strings.Builder
	b.WriteString("🧩 **" + tl("sticker — 贴纸收藏 · 图转贴纸 · 贴纸转图", "sticker — favorite stickers, photo→sticker, sticker→photo") + "**\n\n")
	b.WriteString("**" + tl("收藏贴纸", "Favorite a sticker") + "**\n")
	b.WriteString(line(tl("sticker（回复贴纸）", "sticker (reply to sticker)"), tl("收藏到默认或自动创建的贴纸包", "save to the default or an auto-created pack")))
	b.WriteString(line(tl("sticker to <包名>", "sticker to <pack>"), tl("本次保存到指定贴纸包", "save to the given pack this time")))
	b.WriteString(line("sticker <"+tl("包名", "pack")+">", tl("设置默认贴纸包（先验证）", "set the default pack (validated first)")))
	b.WriteString(line("sticker cancel", tl("取消默认贴纸包", "clear the default pack")))
	b.WriteString(line("sticker status", tl("查看当前设置", "show current settings")))
	b.WriteString("\n**" + tl("图转贴纸", "Photo→sticker") + "**\n")
	b.WriteString(line(tl("sticker pic（回复图片）", "sticker pic (reply to photo)"), tl("转为 WebP 贴纸", "convert to a WebP sticker")))
	b.WriteString(line(tl("sticker pic <表情>", "sticker pic <emoji>"), tl("指定贴纸表情", "use this emoji on the sticker")))
	b.WriteString(line(tl("sticker pic batch", "sticker pic batch"), tl("批量转换相册里的图片", "convert a whole album")))
	b.WriteString("\n**" + tl("贴纸转图", "Sticker→photo") + "**\n")
	b.WriteString(line(tl("sticker topng（回复贴纸）", "sticker topng (reply to sticker)"), tl("转为 JPG 图片", "convert to a JPG photo")))
	b.WriteString(line(tl("sticker topng png", "sticker topng png"), tl("转为 PNG（默认白底）", "convert to PNG (white background)")))
	b.WriteString(line(tl("sticker topng transparent", "sticker topng transparent"), tl("PNG 保留透明背景", "PNG keeping transparency")))
	b.WriteString(line(tl("sticker topng doc", "sticker topng doc"), tl("以文档形式发送", "send as document")))
	b.WriteString("\n**" + tl("设置（机器人面板）", "Settings (bot panel)") + "**\n")
	b.WriteString(tl("默认表情、贴纸边长、质量、背景在机器人的 /menu 里调整\n", "Default emoji, sticker size, quality and background are set in the bot's /menu\n"))
	b.WriteString("\n💡 " + tl(
		"首次使用收藏功能前，请先私聊过 @Stickers 机器人；贴纸包名只能含字母数字下划线，且以字母开头",
		"Message the @Stickers bot once before using favorite; pack names: letters, digits and underscores, starting with a letter"))
	return b.String()
}

func line(cmd, desc string) string { return plugin.Code(cmd) + "  " + desc + "\n" }
