// Package subinfo queries proxy subscription links for traffic usage and
// node information, ported from TeleBox's subinfo plugin.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "subinfo",
	Description: "查询机场订阅的流量、到期与节点信息",
	DescEN:      "Query proxy subscription traffic, expiry and node info",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// remoteMappingsURL is a community-maintained key=name map used to give
// subscriptions friendlier provider names.
const remoteMappingsURL = "https://raw.githubusercontent.com/Hyy800/Quantumult-X/refs/heads/Nana/ymys.txt"

// subUserAgent goes to the subscription endpoint (the same one TeleBox
// sends; some panels only return node lists to known clients).
const subUserAgent = "FlClash/v0.8.76 clash-verge Platform/android"

const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/108.0.0.0 Safari/537.36"

var urlRe = regexp.MustCompile(`https?://\S+`)

// queryTime is the clock used for reports; swapped in tests.
var queryTime = time.Now

type SubinfoPlugin struct {
	http *httpClient
	mu   mappingCache // remote provider-name mappings, cached 10 minutes
}

func New() *SubinfoPlugin {
	return &SubinfoPlugin{http: newHTTPClient()}
}

func (p *SubinfoPlugin) Name() string        { return "subinfo" }
func (p *SubinfoPlugin) Description() string { return Metadata.Description }
func (p *SubinfoPlugin) DescEN() string      { return Metadata.DescEN }

func (p *SubinfoPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	detail := &plugin.Command{
		Name:        "subinfo",
		Description: "详细查询机场订阅：流量、到期、节点分布与节点列表",
		DescEN:      "Detailed subscription report: traffic, expiry, node regions and names",
		Usage:       "subinfo [txt] <链接...> · 回复含链接的消息: subinfo",
		UsageEN:     "subinfo [txt] <url...> · reply to a message with links: subinfo",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     func(c *plugin.CommandContext) error { return p.handle(c, false) },
	}
	brief := &plugin.Command{
		Name:        "cha",
		Description: "简洁查询机场订阅：流量与到期时间",
		DescEN:      "Compact subscription check: traffic and expiry",
		Usage:       "cha [txt] <链接...> · 回复含链接的消息: cha",
		UsageEN:     "cha [txt] <url...> · reply to a message with links: cha",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     func(c *plugin.CommandContext) error { return p.handle(c, true) },
	}
	if err := mgr.RegisterCommand(detail); err != nil {
		return err
	}
	return mgr.RegisterCommand(brief)
}

func (p *SubinfoPlugin) Start(context.Context) error { return nil }
func (p *SubinfoPlugin) Stop(context.Context) error  { return nil }

// help renders the usage card.
func help(tl func(zh, en string) string) string {
	return tl(
		"📈 **订阅信息查询**\n\n"+
			"**用法**\n"+
			plugin.Code("subinfo <链接...>")+" 详细查询（可多个链接）\n"+
			plugin.Code("subinfo txt <链接...>")+" 详细查询，以 TXT 文件发送\n"+
			plugin.Code("cha <链接...>")+" 简洁查询\n"+
			plugin.Code("cha txt <链接...>")+" 简洁查询，以 TXT 文件发送\n"+
			"回复包含订阅链接的消息发 "+plugin.Code("subinfo")+" 或 "+plugin.Code("cha")+" 也可\n\n"+
			"**内容**\n"+
			"流量用量与进度、订阅周期与到期、日均用量、节点类型与地区分布、节点列表",
		"📈 **Subscription info**\n\n"+
			"**Usage**\n"+
			plugin.Code("subinfo <url...>")+" detailed report (multiple links)\n"+
			plugin.Code("subinfo txt <url...>")+" detailed report as a TXT file\n"+
			plugin.Code("cha <url...>")+" compact check\n"+
			plugin.Code("cha txt <url...>")+" compact check as a TXT file\n"+
			"Reply to a message containing links with "+plugin.Code("subinfo")+" or "+plugin.Code("cha")+"\n\n"+
			"**Covers**\n"+
			"Traffic usage and progress, cycle and expiry, daily usage, node types and regions, node list")
}

// handle is the shared entry for both commands; brief selects the compact
// format.
func (p *SubinfoPlugin) handle(ctx *plugin.CommandContext, brief bool) error {
	a := strings.ToLower(strings.TrimSpace(ctx.RawArgs))
	if a == "help" || a == "h" {
		return ctx.Edit(help(ctx.Tlocal))
	}

	args := ctx.Args
	txt := len(args) > 0 && strings.EqualFold(args[0], "txt")
	if txt {
		args = args[1:]
	}

	// Candidate text: the replied-to message (text or caption) plus args.
	var sb strings.Builder
	if ctx.Message.IsReply {
		if r, err := ctx.ReplyMessage(); err == nil && r != nil {
			sb.WriteString(r.Message)
			sb.WriteString(" ")
		}
	}
	sb.WriteString(strings.Join(args, " "))

	urls := extractURLs(sb.String())
	if len(urls) == 0 {
		if strings.TrimSpace(ctx.RawArgs) != "" || ctx.Message.IsReply {
			return ctx.Edit("❌ " + ctx.Tlocal("未找到有效的订阅链接", "No subscription link found"))
		}
		return ctx.Edit(help(ctx.Tlocal))
	}

	_ = ctx.Edit(fmt.Sprintf(ctx.Tlocal("⏳ 正在查询 %d 个订阅链接…", "⏳ Querying %d subscription links…"), len(urls)))

	results := p.queryAll(ctx.Context(), urls)

	text := renderReport(ctx.Tlocal, urls, results, brief, txt)
	caption := fmt.Sprintf(ctx.Tlocal("✅ 订阅查询报告（%d 个链接）", "✅ Subscription report (%d links)"), len(urls))
	namePrefix := "subinfo"
	if brief {
		namePrefix = "cha"
	}

	if txt {
		if err := p.sendTxt(ctx, namePrefix, text, caption); err != nil {
			if ctx.Logger != nil {
				ctx.Logger.Warn("subinfo: send txt failed", "error", err)
			}
			preview := firstChunk(splitLongMessage(text, 900))
			return ctx.Edit("❌ " + ctx.Tlocal("发送 TXT 文件失败", "Failed to send the TXT file") + "\n\n" + preview)
		}
		// The report went out as a document; drop the command message.
		if err := ctx.Delete(); err != nil && ctx.Logger != nil {
			ctx.Logger.Warn("subinfo: delete command message failed", "error", err)
		}
		return nil
	}

	parts := splitLongMessage(text, 4090)
	if err := editNoPreview(ctx, parts[0]); err != nil {
		return err
	}
	for _, part := range parts[1:] { // keep order: sequential sends
		if err := sendTextNoPreview(ctx, part); err != nil {
			if ctx.Logger != nil {
				ctx.Logger.Warn("subinfo: send chunk failed", "error", err)
			}
			break
		}
	}
	return nil
}

// sendTxt writes text to a temp file and uploads it as a document.
func (p *SubinfoPlugin) sendTxt(ctx *plugin.CommandContext, prefix, text, caption string) error {
	name := fmt.Sprintf("%s_report_%s.txt", prefix, queryTime().Format("20060102_150405"))
	path := filepath.Join(os.TempDir(), name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return err
	}
	defer os.Remove(path)
	return ctx.Media.SendFile(ctx.Context(), ctx.Message.ChatID, path, caption, 0)
}

// firstChunk returns the first chunk, or "" when empty.
func firstChunk(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// queryAll runs the per-link queries in parallel and returns results in the
// input order.
func (p *SubinfoPlugin) queryAll(ctx context.Context, urls []string) []*subResult {
	results := make([]*subResult, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			results[i] = p.processSubscription(ctx, u)
		}(i, u)
	}
	wg.Wait()
	return results
}

// editNoPreview edits the command message without a link preview, which
// ctx.Edit cannot turn off.
func editNoPreview(ctx *plugin.CommandContext, text string) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	plain, ents := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesEditMessageRequest{Peer: peer, ID: ctx.Message.Message.ID, Message: plain, NoWebpage: true}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	_, err = ctx.API.MessagesEditMessage(ctx.Context(), req)
	return err
}

// sendTextNoPreview posts a plain new message without a link preview,
// replying to the command message.
func sendTextNoPreview(ctx *plugin.CommandContext, text string) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	plain, ents := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesSendMessageRequest{
		Peer:      peer,
		Message:   plain,
		Entities:  ents,
		RandomID:  time.Now().UnixNano(),
		NoWebpage: true,
	}
	if ctx.Message != nil && ctx.Message.Message != nil {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: ctx.Message.Message.ID})
	}
	_, err = ctx.API.MessagesSendMessage(ctx.Context(), req)
	return err
}

// extractURLs pulls unique http(s) links from text, preserving order.
func extractURLs(text string) []string {
	var urls []string
	seen := map[string]bool{}
	for _, m := range urlRe.FindAllString(text, -1) {
		if seen[m] {
			continue
		}
		seen[m] = true
		urls = append(urls, m)
	}
	return urls
}
