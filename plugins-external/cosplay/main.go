// Package main implements the cosplay plugin: random cosplay photo sets
// from cosplaytele.com, ported from the TeleBox cosplay plugin.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "cosplay",
	Description: "随机 Cosplay 图片",
	DescEN:      "Random cosplay photos",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// CosplayPlugin fetches a random cosplay photo set and sends photos from
// it as a (spoilered) album.
type CosplayPlugin struct {
	http *http.Client
	set  plugin.Settings
}

func New() *CosplayPlugin {
	return &CosplayPlugin{http: &http.Client{
		Timeout: downloadTimeout, // per-request deadlines come from contexts
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			return nil
		},
	}}
}

func (p *CosplayPlugin) Name() string        { return "cosplay" }
func (p *CosplayPlugin) Description() string { return Metadata.Description }
func (p *CosplayPlugin) DescEN() string      { return Metadata.DescEN }

func (p *CosplayPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	set, err := mgr.Host().Settings(&plugin.SettingsSpec{
		Plugin:  p.Name(),
		Title:   "🎭 Cosplay",
		TitleEN: "🎭 Cosplay",
		Settings: []plugin.Setting{
			{
				Key: "count", Label: "默认张数", LabelEN: "Default count",
				Hint:   "不带数字发 cosplay 时发送的张数",
				HintEN: "Photos sent when cosplay is used without a number",
				Kind:   plugin.SettingNumber, Default: defaultCount, Min: 1, Max: maxImages,
			},
			{
				Key: "spoiler", Label: "剧透遮罩", LabelEN: "Spoiler blur",
				Hint:   "图片以剧透效果发送，点开才显示",
				HintEN: "Photos are blurred until tapped",
				Kind:   plugin.SettingToggle, Default: true,
			},
			{
				Key: "link", Label: "附带套图链接", LabelEN: "Photo set link",
				Hint:   "在图片说明里附上原套图链接",
				HintEN: "Put the photo set URL in the caption",
				Kind:   plugin.SettingToggle, Default: true,
			},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "cosplay",
		Description: "随机获取 Cosplay 图片，多张来自同一套图",
		DescEN:      "Random cosplay photos, several from the same photo set",
		Usage:       "cosplay [张数 1-10] | cosplay help",
		UsageEN:     "cosplay [count 1-10] | cosplay help",
		Plugin:      p.Name(),
		Category:    "fun",
		Handler:     p.handle,
	})
}

func (p *CosplayPlugin) Start(_ context.Context) error { return nil }
func (p *CosplayPlugin) Stop(_ context.Context) error  { return nil }

// ---------------------------------------------------------------- handler

func (p *CosplayPlugin) help(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🎭 **Cosplay**\n\n"+
			"**用法**\n"+
			plugin.Code("cosplay")+" 随机一张（默认张数在机器人面板设置）\n"+
			plugin.Code("cosplay 5")+" 同一套图里随机 5 张，最多 "+fmt.Sprint(maxImages)+" 张\n\n"+
			"**说明**\n"+
			"> 每次随机挑一套图，多张图片都来自它\n"+
			"> 只取 gallery 中的原图，不足张数自动换套\n"+
			"> 图片默认带剧透遮罩，说明里附套图链接（可在面板关闭）\n\n"+
			"💡 来源：cosplaytele.com",
		"🎭 **Cosplay**\n\n"+
			"**Usage**\n"+
			plugin.Code("cosplay")+" one random photo (default count set in the bot panel)\n"+
			plugin.Code("cosplay 5")+" 5 random photos from one set, up to "+fmt.Sprint(maxImages)+"\n\n"+
			"**Details**\n"+
			"> A random photo set is picked each time; all photos come from it\n"+
			"> Only full-size gallery images; sets with too few photos are skipped\n"+
			"> Photos are spoilered and captioned with the set link (both switchable in the panel)\n\n"+
			"💡 Source: cosplaytele.com")
}

func (p *CosplayPlugin) handle(ctx *plugin.CommandContext) error {
	if len(ctx.Args) > 0 && strings.EqualFold(ctx.Args[0], "help") {
		return ctx.Edit(p.help(ctx))
	}
	if ctx.API == nil || ctx.Message == nil || ctx.Message.Message == nil {
		return ctx.Edit("❌ " + ctx.Tlocal("客户端未就绪", "Client not ready"))
	}

	count := 0
	if p.set != nil {
		count = p.set.Int("count")
	}
	if len(ctx.Args) > 0 {
		n, ok := parseCount(ctx.Args[0])
		if !ok {
			return ctx.Edit("❌ " + ctx.Tlocal("张数要是 1-10 的数字：", "The count must be a number from 1 to 10: ") + plugin.Code(ctx.Args[0]) +
				"\n\n💡 " + ctx.Tlocal("例如 ", "e.g. ") + plugin.Code("cosplay 5"))
		}
		count = n
	}
	count = clampCount(count)
	spoiler := p.set == nil || p.set.Bool("spoiler")
	withLink := p.set == nil || p.set.Bool("link")

	_ = ctx.Edit("🔄 " + ctx.Tlocal(fmt.Sprintf("正在从随机套图中获取 %d 张图片…", count), fmt.Sprintf("Fetching %d photo(s) from a random set…", count)))

	ps, imgs, err := p.fromSite(ctx.Context(), count)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("获取图片失败：", "Failed to get photos: ") + plugin.Escape(trimErr(err.Error())))
	}

	_ = ctx.Edit("⏳ " + ctx.Tlocal(
		fmt.Sprintf("套图 %s 中找到 %d 张，正在下载…", plugin.Bold(plugin.Escape(ps.Title)), len(imgs)),
		fmt.Sprintf("Found %d photo(s) in %s, downloading…", len(imgs), plugin.Bold(plugin.Escape(ps.Title)))))

	files, err := p.download(ctx.Context(), imgs)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("下载图片失败：", "Failed to download photos: ") + plugin.Escape(trimErr(err.Error())))
	}
	defer p.cleanup(files)

	_ = ctx.Edit("📤 " + ctx.Tlocal("下载完成，正在发送…", "Downloaded, sending…"))
	caption := ""
	if withLink {
		caption = ctx.Tlocal("套图链接: ", "Photo set: ") + ps.URL
	}
	if err := p.sendAlbum(ctx, files, caption, spoiler); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("发送图片失败：", "Failed to send photos: ") + plugin.Escape(trimErr(describeErr(err))))
	}
	return ctx.Delete()
}

// trimErr keeps error text short enough for a chat message.
func trimErr(s string) string {
	r := []rune(s)
	if len(r) <= 300 {
		return s
	}
	return string(r[:300]) + "…"
}

// describeErr turns Telegram API errors into a short readable message,
// calling out FLOOD_WAIT explicitly.
func describeErr(err error) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf("FLOOD_WAIT %d s", int(d.Seconds()))
	}
	var gerr *tgerr.Error
	if errors.As(err, &gerr) {
		return gerr.Type + ": " + gerr.Message
	}
	return err.Error()
}
