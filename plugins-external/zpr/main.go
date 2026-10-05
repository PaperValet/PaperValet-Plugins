// Package main implements the zpr plugin: random anime illustrations
// ("纸片人") from the Lolicon API, ported from the TeleBox zpr plugin.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	apiURL      = "https://api.lolicon.app/setu/v2"
	maxImages   = 10
	userAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/127.0.0.0 Safari/537.36"
	apiTimeout  = 15 * time.Second
	maxImage    = 20 << 20
	commandName = "zpr"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "zpr",
	Description: "随机纸片人（Lolicon）",
	DescEN:      "Random anime illustrations (Lolicon)",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// ZprPlugin fetches random illustrations and sends them as
// (spoilered for R18) photos.
type ZprPlugin struct {
	http *http.Client
	set  plugin.Settings
	log  plugin.Logger
	dir  string
}

func New() *ZprPlugin {
	return &ZprPlugin{http: &http.Client{Timeout: 45 * time.Second}}
}

func (p *ZprPlugin) Name() string        { return "zpr" }
func (p *ZprPlugin) Description() string { return Metadata.Description }
func (p *ZprPlugin) DescEN() string      { return Metadata.DescEN }

// localeError is an error whose text is already bilingual.
type localeError struct{ zh, en string }

func (e *localeError) Error() string { return e.zh }

func (p *ZprPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	if host := mgr.Host(); host != nil {
		p.log = host.Logger(p.Name())
		dir, err := host.DataDir(p.Name())
		if err != nil {
			return err
		}
		p.dir = dir
	}
	set, err := mgr.Host().Settings(&plugin.SettingsSpec{
		Plugin:  p.Name(),
		Title:   "🎨 随机纸片人",
		TitleEN: "🎨 Random illustrations",
		Settings: []plugin.Setting{
			{
				Key: "proxy", Label: "图片反代", LabelEN: "Image proxy",
				Hint:   "下载图片所用的镜像，失败时会自动依次尝试其他镜像",
				HintEN: "Mirror used for downloads; others are tried in order on failure",
				Kind:   plugin.SettingChoice, Default: defaultProxy,
				Choices: proxyChoices(),
			},
			{
				Key: "caption", Label: "附带作品信息", LabelEN: "Include artwork info",
				Hint:   "在图片说明中附带 pid / 画师 / 标签",
				HintEN: "Add pid / author / tags to the caption",
				Kind:   plugin.SettingToggle, Default: true,
			},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	return mgr.RegisterCommand(&plugin.Command{
		Name:        commandName,
		Description: "随机获取纸片人图片（Lolicon API），可按标签、数量筛选",
		DescEN:      "Random anime illustrations (Lolicon API), by tag and count",
		Usage:       "zpr [标签] [数量] | zpr r18 [数量] | zpr help",
		UsageEN:     "zpr [tag] [count] | zpr r18 [count] | zpr help",
		Plugin:      p.Name(),
		Category:    "fun",
		RateLimit:   10,
		Handler:     p.handle,
	})
}

func (p *ZprPlugin) Start(_ context.Context) error { return nil }
func (p *ZprPlugin) Stop(_ context.Context) error  { return nil }

// trimErr keeps error text short enough for a chat message.
func trimErr(s string) string {
	r := []rune(s)
	if len(r) <= 300 {
		return s
	}
	return string(r[:300]) + "…"
}

// ---------------------------------------------------------------- handler

func (p *ZprPlugin) help(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🎨 **随机纸片人**\n\n"+
			"**用法**\n"+
			plugin.Code("zpr")+" 随机 1 张\n"+
			plugin.Code("zpr 3")+" 随机 3 张（1-"+fmt.Sprint(maxImages)+"）\n"+
			plugin.Code("zpr 萝莉")+" 按标签筛选\n"+
			plugin.Code("zpr 萝莉 2")+" 标签 + 数量\n"+
			plugin.Code("zpr r18")+" R18 内容（自动加剧透遮罩）\n"+
			plugin.Code("zpr r18 2")+" R18 + 数量\n\n"+
			"**说明**\n"+
			"> 图片来自 Lolicon API（Pixiv 原图）\n"+
			"> R18 图片以剧透效果发送\n"+
			"> 默认反代在机器人面板设置\n\n"+
			"💡 数据来源：api.lolicon.app",
		"🎨 **Random illustrations**\n\n"+
			"**Usage**\n"+
			plugin.Code("zpr")+" one random image\n"+
			plugin.Code("zpr 3")+" three random images (1-"+fmt.Sprint(maxImages)+")\n"+
			plugin.Code("zpr loli")+" filter by tag\n"+
			plugin.Code("zpr loli 2")+" tag + count\n"+
			plugin.Code("zpr r18")+" R18 content (spoiler blur)\n"+
			plugin.Code("zpr r18 2")+" R18 + count\n\n"+
			"**Details**\n"+
			"> Images come from the Lolicon API (Pixiv originals)\n"+
			"> R18 images are sent with the spoiler effect\n"+
			"> The default mirror is set in the bot panel\n\n"+
			"💡 Source: api.lolicon.app")
}

func (p *ZprPlugin) handle(ctx *plugin.CommandContext) error {
	if len(ctx.Args) > 0 && strings.EqualFold(ctx.Args[0], "help") {
		return ctx.Edit(p.help(ctx))
	}
	if ctx.API == nil || ctx.Message == nil || ctx.Message.Message == nil {
		return ctx.Edit("❌ " + ctx.Tlocal("客户端未就绪", "Client not ready"))
	}

	req := parseArgs(ctx.Args)
	if req.bad != "" {
		return ctx.Edit("❌ " + ctx.Tlocal("数量需要是 1-"+fmt.Sprint(maxImages)+" 的数字：", "The count must be a number from 1 to "+fmt.Sprint(maxImages)+": ") +
			plugin.Code(req.bad) + "\n\n💡 " + ctx.Tlocal("示例：", "Example: ") + plugin.Code("zpr "+req.bad+" → zpr 3"))
	}

	proxy := p.settingString("proxy", defaultProxy)
	withCaption := p.set == nil || p.set.Bool("caption")

	_ = ctx.Edit("🔄 " + ctx.Tlocal("正在前往二次元…", "Heading to the 2D world…"))

	items, err := p.fetch(ctx.Context(), req)
	if err != nil {
		return ctx.Edit("❌ " + p.errText(ctx, err))
	}

	_ = ctx.Edit("⏳ " + ctx.Tlocal("下载图片中…", "Downloading images…"))

	files, err := p.downloadAll(ctx.Context(), items, proxy)
	defer p.cleanup(files)
	if err != nil {
		return ctx.Edit("❌ " + p.errText(ctx, err))
	}

	_ = ctx.Edit("📤 " + ctx.Tlocal("传送中…", "Delivering…"))

	if err := p.sendAll(ctx, files, req.r18, withCaption); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("发送失败：", "Failed to send: ") + plugin.Escape(describeSendErr(err, ctx)))
	}
	return ctx.Delete()
}

// errText renders an error bilingually when it carries locale text.
func (p *ZprPlugin) errText(ctx *plugin.CommandContext, err error) string {
	var le *localeError
	if errors.As(err, &le) {
		return ctx.Tlocal(le.zh, le.en)
	}
	return ctx.Tlocal("获取失败：", "Failed: ") + plugin.Escape(trimErr(err.Error()))
}

func (p *ZprPlugin) settingString(key, def string) string {
	if p.set == nil {
		return def
	}
	if v := strings.TrimSpace(p.set.String(key)); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------- args

// setuRequest is one parsed `zpr` invocation.
type setuRequest struct {
	num int    // 1..maxImages
	r18 bool   // r18 flag seen
	tag string // tag filter ("" = none)
	bad string // non-numeric count argument ("" = none)
}

// isNumArg reports whether s is a plain decimal count.
func isNumArg(s string) bool {
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

// clampNum keeps a requested count within 1..maxImages, like the source.
func clampNum(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxImages {
		return maxImages
	}
	return n
}

// parseArgs mirrors the reference's positional grammar:
// zpr [num] | zpr r18 [num] | zpr [tag] [num|r18 [num]]
func parseArgs(args []string) setuRequest {
	req := setuRequest{num: 1}
	if len(args) == 0 {
		return req
	}
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	setNum := func(s string) {
		if isNumArg(s) {
			req.num = clampNum(atoi(s))
		} else if s != "" {
			req.bad = s
		}
	}
	switch first := arg(0); {
	case isNumArg(first):
		req.num = clampNum(atoi(first))
	case strings.EqualFold(first, "r18"):
		req.r18 = true
		setNum(arg(1))
	default: // tag
		req.tag = first
		second := arg(1)
		if isNumArg(second) {
			req.num = clampNum(atoi(second))
		} else if strings.EqualFold(second, "r18") {
			req.r18 = true
			setNum(arg(2))
		}
	}
	return req
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		if n > maxImages {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}
