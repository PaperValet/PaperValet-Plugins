// Package main implements the setu plugin: anime images through the
// @FinelyGirlsBot Telegram bot, ported from the TeleBox botmzt plugin.
package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const botUsername = "FinelyGirlsBot"

// replyTimeout bounds the whole bot round-trip; the source used 15s and
// image bots occasionally stretch past that, so keep a margin.
const replyTimeout = 20 * time.Second

var Metadata = &plugin.PluginMetadata{
	Name:        "setu",
	Description: "通过 FinelyGirls 机器人获取二次元图片",
	DescEN:      "Anime images via the FinelyGirls bot",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// category is one image kind the source bot serves.
type category struct {
	Key     string // what the owner types: setu <key>
	Command string // the bot command sent: /<command>
	Text    bool   // text reply (check-in) instead of media
}

var categories = []category{
	{Key: "rand", Command: "rand"},
	{Key: "pic", Command: "pic"},
	{Key: "leg", Command: "leg"},
	{Key: "ass", Command: "ass"},
	{Key: "chest", Command: "chest"},
	{Key: "coser", Command: "cos"},
	{Key: "nsfw", Command: "nsfw"},
	{Key: "naizi", Command: "naizi"},
	{Key: "qd", Command: "checkin", Text: true},
}

var categoryNames = map[string][2]string{
	"rand": {"随机", "Random"}, "pic": {"妹子", "Girls"}, "leg": {"腿部", "Legs"},
	"ass": {"臀部", "Ass"}, "chest": {"胸部", "Chest"}, "coser": {"Cosplay", "Cosplay"},
	"nsfw": {"NSFW", "NSFW"}, "naizi": {"奶子", "Boobs"}, "qd": {"签到", "Check-in"},
}

func categoryByKey(key string) *category {
	for i := range categories {
		if categories[i].Key == key {
			return &categories[i]
		}
	}
	return nil
}

// SetuPlugin drives @FinelyGirlsBot for the owner.
type SetuPlugin struct {
	host plugin.Host
	set  plugin.Settings
	log  plugin.Logger

	mu   sync.Mutex
	busy bool              // one bot conversation at a time
	peer *tg.InputPeerUser // cached bot peer
	ctx  context.Context   // lifetime for background sends
	cnl  context.CancelFunc
}

func New() *SetuPlugin { return &SetuPlugin{} }

func (p *SetuPlugin) Name() string        { return "setu" }
func (p *SetuPlugin) Description() string { return Metadata.Description }
func (p *SetuPlugin) DescEN() string      { return Metadata.DescEN }

// categoryChoices lists the settings panel options in display order.
func categoryChoices() []plugin.Choice {
	out := make([]plugin.Choice, 0, len(categories))
	for _, c := range categories {
		n := categoryNames[c.Key]
		out = append(out, plugin.Choice{Value: c.Key, Label: n[0], LabelEN: n[1]})
	}
	return out
}

func (p *SetuPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.log = p.host.Logger(p.Name())
	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin: p.Name(), Title: "🎨 二次元图片", TitleEN: "🎨 Anime images",
		Settings: []plugin.Setting{
			{
				Key: "category", Label: "默认分类", LabelEN: "Default category",
				Hint:   "不带分类发 setu 时用它",
				HintEN: "Used when setu is sent without a category",
				Kind:   plugin.SettingChoice, Default: "rand", Choices: categoryChoices(),
			},
			{
				Key: "spoiler", Label: "剧透遮罩", LabelEN: "Spoiler overlay",
				Hint:   "图片带剧透效果发送，点击后可见",
				HintEN: "Images arrive behind a spoiler, tap to reveal",
				Kind:   plugin.SettingToggle, Default: true,
			},
			{
				Key: "delete_cmd", Label: "删除命令消息", LabelEN: "Delete command message",
				Hint:   "成功后删除触发命令的那条消息",
				HintEN: "Remove the command message once it succeeds",
				Kind:   plugin.SettingToggle, Default: true,
			},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "setu",
		Description: "通过 FinelyGirls 机器人获取二次元图片，带剧透与签到",
		DescEN:      "Anime images via the FinelyGirls bot, spoiler-wrapped, with daily check-in",
		Usage:       "setu [rand|pic|leg|ass|chest|coser|nsfw|naizi|qd]",
		UsageEN:     "setu [rand|pic|leg|ass|chest|coser|nsfw|naizi|qd]",
		Plugin:      p.Name(),
		Category:    "media",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *SetuPlugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cnl == nil {
		p.ctx, p.cnl = context.WithCancel(context.Background())
	}
	return nil
}

func (p *SetuPlugin) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cnl != nil {
		p.cnl()
		p.cnl = nil
	}
	return nil
}

func (p *SetuPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

// parseCategory reads the command args: "setu", "setu help" or "setu <cat>";
// with no category the settings default applies.
func parseCategory(args []string, def string) (*category, string, bool) {
	if len(args) == 0 {
		if c := categoryByKey(def); c != nil {
			return c, "", true
		}
		return &categories[0], "", true
	}
	a := strings.ToLower(args[0])
	if a == "help" || a == "h" {
		return nil, "help", false
	}
	if c := categoryByKey(a); c != nil {
		return c, "", true
	}
	return nil, a, false
}

func (p *SetuPlugin) help(tl func(string, string) string) string {
	var rows []string
	for _, c := range categories {
		n := categoryNames[c.Key]
		rows = append(rows, plugin.Code("setu "+c.Key)+" "+tl(n[0], n[1]))
	}
	return tl(
		"🎨 **二次元图片** · @"+botUsername+"\n\n"+
			strings.Join(rows, "\n")+"\n\n"+
			"不带分类时用设置里的默认分类\n"+
			"回复某条消息发命令，图片也回复那条消息",
		"🎨 **Anime images** · @"+botUsername+"\n\n"+
			strings.Join(rows, "\n")+"\n\n"+
			"The default category from settings applies when none is given\n"+
			"Send it as a reply and the image replies to the same message")
}

func (p *SetuPlugin) handle(ctx *plugin.CommandContext) error {
	cat, arg, ok := parseCategory(ctx.Args, p.set.String("category"))
	if !ok {
		if arg == "help" {
			return ctx.Edit(p.help(ctx.Tlocal))
		}
		return ctx.Edit("❌ " + ctx.Tlocal("未知分类 ", "Unknown category ") + plugin.Code(arg) + ctx.Tlocal("，发 ", ", send ") + plugin.Code("setu help"))
	}

	replyTo := 0
	if ctx.Message.IsReply {
		replyTo = ctx.Message.ReplyToID
	}

	p.mu.Lock()
	inUse := p.busy
	if !inUse {
		p.busy = true
	}
	p.mu.Unlock()
	if inUse {
		return ctx.Edit("⏳ " + ctx.Tlocal("上一个请求还没完成，请稍后再试", "A request is still running, try again shortly"))
	}
	defer func() {
		p.mu.Lock()
		p.busy = false
		p.mu.Unlock()
	}()

	if cat.Text {
		_ = ctx.Edit("📅 " + ctx.Tlocal("正在签到…", "Checking in…"))
	} else {
		_ = ctx.Edit("🔄 " + ctx.Tlocal("获取图片中…", "Fetching the image…"))
	}

	res, err := p.askBot(cat)
	switch {
	case err != nil:
		return ctx.Edit("❌ " + p.errText(ctx.Tlocal, err))
	case cat.Text:
		text := res.Text
		if strings.TrimSpace(text) == "" {
			text = ctx.Tlocal("签到成功", "Checked in")
		}
		return ctx.Edit("✅ **" + ctx.Tlocal("签到完成", "Check-in done") + "**\n\n" + plugin.Escape(clipText(text, 500)))
	case res.Media == nil:
		if strings.TrimSpace(res.Text) != "" {
			return ctx.Edit("❌ " + ctx.Tlocal("**机器人返回：**\n> ", "**The bot says:**\n> ") + plugin.Escape(clipText(res.Text, 500)))
		}
		return ctx.Edit("❌ " + ctx.Tlocal("机器人没有返回图片，请稍后重试", "The bot sent no image, try again later"))
	}
	if err := p.sendMedia(ctx, res.Media, replyTo, res.lastID); err != nil {
		return ctx.Edit("❌ " + p.errText(ctx.Tlocal, err))
	}
	if p.set.Bool("delete_cmd") {
		_ = ctx.Delete()
	}
	return nil
}
