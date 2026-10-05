package main

import (
	"strings"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// help renders the bilingual usage card.
func (p *ParseHubPlugin) help(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	var b strings.Builder
	b.WriteString("🔗 **" + tl("链接解析", "Link Parsing") + "**\n\n")
	b.WriteString("**" + tl("用法", "Usage") + "**\n")
	b.WriteString(plugin.Code("parsehub <链接>") + " " + tl("直接解析链接", "parse a link directly") + "\n")
	b.WriteString(tl("回复含链接的消息 ", "Reply to a message with a link ") + plugin.Code("parsehub") + "\n\n")
	b.WriteString("**" + tl("支持的平台", "Supported platforms") + "**\n")
	b.WriteString(tl("抖音 视频/图文 · 哔哩哔哩 视频/动态 · YouTube · YouTube Music · TikTok 视频/图文\n小红书 视频/图文 · X(Twitter) 视频/图文 · 百度贴吧 · Facebook · 微博 · Instagram 视频/图文\n……等 40+ 平台",
		"Douyin video/photo · Bilibili video/moment · YouTube · YouTube Music · TikTok video/photo\nXHS video/photo · X (Twitter) video/photo · Tieba · Facebook · Weibo · Instagram video/photo\n…and 40+ more") + "\n\n")
	b.WriteString("**" + tl("示例", "Examples") + "**\n")
	b.WriteString(plugin.Code("parsehub https://twitter.com/user/status/123") + "\n")
	b.WriteString(plugin.Code("parsehub https://www.instagram.com/p/xxxx/") + "\n\n")
	b.WriteString("💡 " + tl("结果通过 @ParseHubot 解析并转发回本会话；删除命令消息等选项在机器人面板设置",
		"Results are parsed via @ParseHubot and forwarded here; options like deleting the command live in the bot panel"))
	return b.String()
}
