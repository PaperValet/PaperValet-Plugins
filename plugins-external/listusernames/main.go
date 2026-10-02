package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const maxLen = 3800

var Metadata = &plugin.PluginMetadata{
	Name:        "listusernames",
	Description: "列出自己的公开群组和频道",
	DescEN:      "List your public groups and channels",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type ListPlugin struct{}

func New() *ListPlugin { return &ListPlugin{} }

func (p *ListPlugin) Name() string        { return "listusernames" }
func (p *ListPlugin) Description() string { return Metadata.Description }
func (p *ListPlugin) DescEN() string      { return Metadata.DescEN }

func (p *ListPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "listusernames",
		Description: "列出你拥有公开用户名的群组和频道",
		DescEN:      "List groups and channels you own that have a public username",
		Usage:       "listusernames",
		UsageEN:     "listusernames",
		Plugin:      p.Name(),
		Category:    "tools",
		Handler:     p.handle,
	})
}

func (p *ListPlugin) Start(context.Context) error { return nil }
func (p *ListPlugin) Stop(context.Context) error  { return nil }

type entry struct {
	Title, Username string
	ID              int64
	Broadcast       bool
}

func (p *ListPlugin) handle(ctx *plugin.CommandContext) error {
	if a := strings.ToLower(ctx.GetArg(0)); a == "help" || a == "h" {
		return ctx.Edit(ctx.Tlocal(
			"📋 **listusernames**\n\n"+plugin.Code("listusernames")+" 列出你拥有的公开群组和频道",
			"📋 **listusernames**\n\n"+plugin.Code("listusernames")+" list the public groups and channels you own"))
	}
	_ = ctx.Edit("🔄 " + ctx.Tlocal("正在获取公开群组/频道…", "Fetching public groups/channels…"))
	res, err := ctx.API.ChannelsGetAdminedPublicChannels(ctx.Context(), &tg.ChannelsGetAdminedPublicChannelsRequest{})
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("获取列表失败：", "Failed to fetch the list: ") + errText(ctx.Tlocal, err))
	}
	var list []entry
	for _, c := range res.GetChats() {
		if ch, ok := c.(*tg.Channel); ok {
			list = append(list, entry{Title: ch.Title, Username: channelUsername(ch), ID: plugin.ChannelChatID(ch.ID), Broadcast: ch.Broadcast})
		}
	}
	if len(list) == 0 {
		return ctx.Edit("📭 " + ctx.Tlocal("你没有任何公开群组或频道", "You own no public groups or channels"))
	}
	parts := chunk(render(ctx.Tlocal, list), maxLen)
	if err := ctx.Edit(parts[0]); err != nil {
		return err
	}
	for _, s := range parts[1:] {
		if err := ctx.Reply(s); err != nil {
			return err
		}
	}
	return nil
}

func channelUsername(ch *tg.Channel) string {
	if ch.Username != "" {
		return ch.Username
	}
	for _, u := range ch.Usernames {
		if u.Active {
			return u.Username
		}
	}
	return ""
}

func render(tl func(string, string) string, list []entry) []string {
	channels := 0
	lines := []string{fmt.Sprintf(tl("📋 **我的公开群组/频道** · %d", "📋 **My public groups/channels** · %d"), len(list)), ""}
	for i, e := range list {
		kind := tl("👥 群组", "👥 Group")
		if e.Broadcast {
			kind = tl("📢 频道", "📢 Channel")
			channels++
		}
		title := e.Title
		if title == "" {
			title = tl("未知标题", "Untitled")
		}
		user := "-"
		if e.Username != "" {
			user = "@" + e.Username
		}
		lines = append(lines, fmt.Sprintf("**%d.** %s · %s\n> %s · %s", i+1, plugin.Escape(title), kind, plugin.Code(user), plugin.Code(e.ID)))
	}
	lines = append(lines, "", fmt.Sprintf(tl("📊 频道 %d · 群组 %d", "📊 %d channels · %d groups"), channels, len(list)-channels))
	return lines
}

// chunk joins lines into messages no longer than max runes, never splitting a line.
func chunk(lines []string, max int) []string {
	var out []string
	var b strings.Builder
	n := 0
	for _, l := range lines {
		ln := len([]rune(l)) + 1
		if n > 0 && n+ln > max {
			out = append(out, strings.TrimRight(b.String(), "\n"))
			b.Reset()
			n = 0
		}
		b.WriteString(l + "\n")
		n += ln
	}
	if n > 0 {
		out = append(out, strings.TrimRight(b.String(), "\n"))
	}
	return out
}

func errText(tl func(string, string) string, err error) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	if e, ok := tgerr.As(err); ok {
		return plugin.Code(e.Type)
	}
	return plugin.Escape(err.Error())
}
