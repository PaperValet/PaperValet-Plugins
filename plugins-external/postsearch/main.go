// postsearch: multi-channel Telegram post search, ported from TeleBox
// "search" (channel_search_config.json + messages.search + linked-group
// comments + ad filtering). See search.go for the search engine.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "postsearch",
	Description: "多频道帖子搜索",
	DescEN:      "Search posts across channels and groups",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

const (
	// queryFetch is the per-channel fetch limit for keyword searches
	// (source: searchMessages limit 200).
	queryFetch = 200
	// kkpFetch is the per-channel fetch limit for random video browse
	// (source: 100 channels, 200 megagroups).
	kkpFetch      = 100
	kkpFetchGroup = 200
	// listHardCap bounds how many result lines are rendered in list mode.
	listHardCap = 30
)

type Plugin struct {
	svc *service
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string        { return "postsearch" }
func (p *Plugin) Description() string { return Metadata.Description }
func (p *Plugin) DescEN() string      { return Metadata.DescEN }

func (p *Plugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.svc = newService(mgr.Host())
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "postsearch",
		Description: "搜索已添加频道/群组的帖子：关键词列链接、随机一条、广告过滤",
		DescEN:      "Search posts in your channels/groups: keyword link list, random pick, ad filter",
		Usage:       "postsearch <关键词> [-r] · postsearch rand · postsearch add|del|default|list|export|import|ad … · postsearch help",
		UsageEN:     "postsearch <query> [-r] · postsearch rand · postsearch add|del|default|list|export|import|ad … · postsearch help",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *Plugin) Start(ctx context.Context) error { return p.svc.load() }
func (p *Plugin) Stop(ctx context.Context) error  { return nil }

// ---------------------------------------------------------------- command

// handle routes a subcommand, mirroring the source's switch.
func (p *Plugin) handle(ctx *plugin.CommandContext) error {
	s := p.svc
	fullArgs := ctx.GetArgs()
	useRandom := containsFlag(fullArgs, "-r")
	fullArgs = removeFlag(fullArgs, "-r")

	if strings.TrimSpace(fullArgs) == "" {
		return ctx.Edit(helpText(ctx))
	}
	fields := strings.Fields(fullArgs)
	sub := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(fullArgs, fields[0]))
	switch sub {
	case "help", "h":
		return ctx.Edit(helpText(ctx))
	case "add":
		return s.cmdAdd(ctx, rest)
	case "del", "rm":
		return s.cmdDelete(ctx, rest)
	case "default":
		return s.cmdDefault(ctx, rest)
	case "list":
		return s.cmdList(ctx)
	case "export":
		return s.cmdExport(ctx)
	case "import":
		return s.cmdImport(ctx)
	case "ad":
		return s.cmdAd(ctx, rest)
	case "kkp", "rand", "random":
		return s.cmdSearch(ctx, "", false, true)
	default:
		return s.cmdSearch(ctx, fullArgs, false, useRandom)
	}
}

// containsFlag reports whether flag appears as its own token.
func containsFlag(s, flag string) bool {
	for _, t := range strings.Fields(strings.ToLower(s)) {
		if t == flag {
			return true
		}
	}
	return false
}

// removeFlag drops every token equal (case-insensitive) to flag.
func removeFlag(s, flag string) string {
	var out []string
	for _, t := range strings.Fields(s) {
		if strings.EqualFold(t, flag) {
			continue
		}
		out = append(out, t)
	}
	return strings.Join(out, " ")
}

func helpText(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🔍 **帖子搜索**\n\n"+
			"**搜索**\n"+
			plugin.Code("postsearch <关键词>")+" 搜索已添加频道/群组，返回帖子链接列表\n"+
			plugin.Code("postsearch <关键词> -r")+" 从结果中随机挑一条转发\n"+
			plugin.Code("postsearch rand")+" 随机速览（不限关键词）\n\n"+
			"**频道管理**\n"+
			plugin.Code("postsearch add <@频道|链接>")+" 添加，多个用 "+plugin.Code("\\")+" 分隔\n"+
			plugin.Code("postsearch del <@频道|序号>")+" 删除，"+plugin.Code("postsearch del all")+" 清空\n"+
			plugin.Code("postsearch default <@频道>")+" 设默认频道，"+plugin.Code("d")+" 取消\n"+
			plugin.Code("postsearch list")+" 列出频道\n"+
			plugin.Code("postsearch export")+" 导出频道列表文件\n"+
			plugin.Code("postsearch import")+" 回复导出文件导入\n\n"+
			"**广告过滤**\n"+
			plugin.Code("postsearch ad add|del <词…>")+" 增删过滤词\n"+
			plugin.Code("postsearch ad list")+" 查看过滤词",
		"🔍 **Post search**\n\n"+
			"**Search**\n"+
			plugin.Code("postsearch <query>")+" search your channels/groups, get post links\n"+
			plugin.Code("postsearch <query> -r")+" pick a random hit and forward it\n"+
			plugin.Code("postsearch rand")+" random browse (no keywords)\n\n"+
			"**Channels**\n"+
			plugin.Code("postsearch add <@channel|link>")+" add, several split by "+plugin.Code("\\")+"\n"+
			plugin.Code("postsearch del <@channel|#>")+" delete, "+plugin.Code("postsearch del all")+" wipe\n"+
			plugin.Code("postsearch default <@channel>")+" set default, "+plugin.Code("d")+" clears\n"+
			plugin.Code("postsearch list")+" list channels\n"+
			plugin.Code("postsearch export")+" export the channel list as a file\n"+
			plugin.Code("postsearch import")+" reply to the export file to import\n\n"+
			"**Ad filter**\n"+
			plugin.Code("postsearch ad add|del <words…>")+" manage filter words\n"+
			plugin.Code("postsearch ad list")+" show them")
}

// fail renders a bilingual error card, with FLOOD_WAIT awareness.
func fail(tl func(zh, en string) string, err error) string {
	out := "❌ " + tl("搜索失败", "Search failed") + "\n"
	if d, ok := tgerr.AsFloodWait(err); ok {
		return out + fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "Flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		switch e.Type {
		case "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID":
			return out + tl("这个用户名不存在", "That username does not exist")
		case "CHANNEL_PRIVATE", "CHAT_FORBIDDEN":
			return out + tl("无法访问该频道/群组", "Cannot access that channel/group")
		case "PEER_ID_INVALID", "CHANNEL_INVALID":
			return out + tl("找不到该频道，请先订阅", "Cannot find that channel; join it first")
		case "SEARCH_QUERY_EMPTY":
			return out + tl("关键词为空", "Empty query")
		}
		return out + plugin.Code(e.Type)
	}
	return out + plugin.Escape(errText(err))
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	r := []rune(err.Error())
	if len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return err.Error()
}
