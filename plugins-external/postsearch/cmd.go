package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// ---------------------------------------------------------------- search

// cmdSearch implements keyword search and random browse.
func (s *service) cmdSearch(ctx *plugin.CommandContext, query string, _spoiler, random bool) error {
	tl := ctx.Tlocal
	cfg := s.store.snapshot()
	if len(cfg.Channels) == 0 {
		return ctx.Edit("❌ " + tl("请先用 ", "Add a channel first with ") + plugin.Code("postsearch add"))
	}
	if strings.TrimSpace(query) == "" {
		_ = ctx.Edit("🎲 " + tl("正在随机寻找视频…", "Looking for a random video…"))
	} else {
		_ = ctx.Edit("🔍 " + tl("正在搜索 ", "Searching ") + plugin.Code(query) + "…")
	}

	var (
		hits  []hit
		order = cfg.searchOrder()
		fil   = cfg.AdFilters
		notes []string
		drop  []string
	)
	for i, handle := range order {
		if i > 0 {
			select {
			case <-ctx.Context().Done():
				return ctx.Edit("❌ " + tl("搜索已取消", "Search cancelled"))
			case <-time.After(50 * time.Millisecond):
			}
		}
		ch, ok := cfg.byHandle(handle)
		if !ok {
			continue
		}
		_ = ctx.Edit(fmt.Sprintf("🔄 %s (%d/%d)", tl("正在搜索", "Searching"), i+1, len(order)) + " · " + plugin.Escape(ch.label()))
		var found []hit
		var err error
		if query == "" {
			found, err = s.randomChannel(ctx.Context(), ch, fil)
		} else {
			found, err = s.searchChannelFull(ctx.Context(), ch, query, fil)
		}
		if err != nil {
			if isGone(err) {
				drop = append(drop, ch.Handle)
				notes = append(notes, ch.label()+" → "+tl("已失效，自动移除", "gone, removed"))
			} else {
				notes = append(notes, ch.label()+": "+errText(err))
			}
			continue
		}
		hits = append(hits, found...)
		if len(hits) > 0 && query != "" && !random {
			break // exact mode stops at the first channel with hits
		}
	}
	if len(drop) > 0 {
		gone := map[string]bool{}
		for _, h := range drop {
			gone[h] = true
			s.dropCached(h)
		}
		_ = s.store.withLock(func(c *config) error {
			c.removeChannels(gone)
			return nil
		})
	}

	hits = dedupeHits(hits)
	if len(hits) == 0 {
		out := "📭 " + tl("没有找到匹配结果", "No matching results")
		for _, n := range notes {
			out += "\n› " + plugin.Escape(n)
		}
		return ctx.Edit(out)
	}

	if random || query == "" {
		pick := hits[rand.IntN(len(hits))]
		return s.sendPick(ctx, pick, query, notes)
	}
	sortHits(hits)
	return s.sendList(ctx, query, hits, notes)
}

// isGone reports whether a channel access error means the source should
// be dropped from the list, like the source's auto-remove.
func isGone(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "PEER_ID_INVALID") ||
		strings.Contains(err.Error(), "CHANNEL_PRIVATE") ||
		strings.Contains(err.Error(), "CHAT_FORBIDDEN") ||
		strings.Contains(err.Error(), "USERNAME_NOT_OCCUPIED"))
}

// searchChannelFull adds the linked-group comment search before the
// channel's own search, mirroring findAndSendVideo.
func (s *service) searchChannelFull(ctx context.Context, ch channel, query string, filters []string) ([]hit, error) {
	var out []hit
	if ch.LinkedGroup != "" {
		linked, err := s.searchLinked(ctx, ch, query, filters)
		if err == nil && len(linked) > 0 {
			out = append(out, linked...)
		}
	}
	found, err := s.searchChannel(ctx, ch, query, filters)
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return append(out, found...), nil
}

// sendList renders hits as a link list, chunked.
func (s *service) sendList(ctx *plugin.CommandContext, query string, hits []hit, notes []string) error {
	tl := ctx.Tlocal
	if len(hits) > listHardCap {
		hits = hits[:listHardCap]
	}
	var lines []string
	for i, h := range hits {
		text := preview(h.Msg, tl)
		link := h.Chat.link(h.Msg.ID)
		var line string
		if link != "" {
			line = fmt.Sprintf("**%d.** ", i+1) + plugin.Link(text, link)
		} else {
			line = fmt.Sprintf("**%d.** ", i+1) + plugin.Escape(text)
		}
		if tag := metaLine(h, tl); tag != "" {
			line += "\n" + tag
		}
		lines = append(lines, line)
	}
	head := "🔍 **" + tl("帖子搜索", "Post search") + "**\n" +
		tl("关键词", "Query") + " " + plugin.Code(query) + " · " +
		fmt.Sprintf(tl("共 %d 条", "%d hits"), len(hits))
	chunks := chunkLines(head, lines, 3800)
	if err := editNoPreview(ctx, chunks[0]); err != nil {
		return err
	}
	for _, c := range chunks[1:] {
		if err := replyNoPreview(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// sendPick forwards one chosen hit; on forward failure falls back to a
// link reply (re-upload without spoiler support would need download+upload
// which the SDK's media path does not expose for arbitrary peers).
func (s *service) sendPick(ctx *plugin.CommandContext, h hit, query string, notes []string) error {
	tl := ctx.Tlocal
	_ = ctx.Edit("✅ " + tl("已找到结果，准备发送…", "Found something, sending…"))
	dest, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit(fail(tl, err))
	}
	srcChat := h.Chat.ChatID
	src, err := s.resolveByChatID(ctx.Context(), srcChat)
	if err != nil {
		return ctx.Edit(fail(tl, err))
	}
	_, err = ctx.API.MessagesForwardMessages(ctx.Context(), &tg.MessagesForwardMessagesRequest{
		FromPeer: src,
		ID:       []int{h.Msg.ID},
		ToPeer:   dest,
		RandomID: []int64{rand.Int64()},
	})
	if err == nil {
		_ = ctx.Delete()
		return nil
	}
	link := h.Chat.link(h.Msg.ID)
	if link == "" {
		return ctx.Edit("❌ " + tl("转发失败：", "Forward failed: ") + plugin.Escape(errText(err)))
	}
	return ctx.Edit("⚠️ " + tl("转发失败，请通过链接查看", "Forward failed, use the link") + "\n" +
		plugin.Link(preview(h.Msg, tl), link))
}

func (s *service) resolveByChatID(ctx context.Context, chatID int64) (tg.InputPeerClass, error) {
	return s.host.PeerResolver().ResolveFromChatID(ctx, chatID)
}

// preview renders one message as a short single line, like his.
func preview(m *tg.Message, tl func(zh, en string) string) string {
	txt := oneLine(m.Message)
	if txt == "" {
		if f := fileOf(m); f != "" {
			txt = f
		} else {
			txt = tl("[帖子]", "[post]")
		}
	}
	return clip(oneLine(txt), 80)
}

func metaLine(h hit, tl func(zh, en string) string) string {
	var parts []string
	if _, v, ok := videoOf(h.Msg); ok {
		parts = append(parts, fmt.Sprintf("🎬 %d:%02d", int(v.Duration)/60, int(v.Duration)%60))
		if h.FileName != "" {
			parts = append(parts, "📎 "+plugin.Code(clip(h.FileName, 60)))
		}
	} else if h.FileName != "" {
		parts = append(parts, "📎 "+plugin.Code(clip(h.FileName, 60)))
	}
	if d := time.Unix(int64(h.Msg.Date), 0); h.Msg.Date > 0 {
		parts = append(parts, d.Format("2006-01-02"))
	}
	return strings.Join(parts, " · ")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// chunkLines packs lines into messages of at most max runes; the head only
// starts the first chunk and continuations get a leading ellipsis line.
func chunkLines(head string, lines []string, max int) []string {
	var out []string
	cur := head + "\n\n"
	headOnly := len([]rune(cur))
	for _, l := range lines {
		if len([]rune(cur))+len([]rune(l))+1 > max && len([]rune(cur)) > headOnly {
			out = append(out, strings.TrimRight(cur, "\n"))
			cur = "…\n"
		}
		cur += l + "\n"
	}
	return append(out, strings.TrimRight(cur, "\n"))
}

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

func replyNoPreview(ctx *plugin.CommandContext, text string) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	plain, ents := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesSendMessageRequest{
		Peer: peer, Message: plain, RandomID: rand.Int64(), NoWebpage: true,
		ReplyTo: &tg.InputReplyToMessage{ReplyToMsgID: ctx.Message.Message.ID},
	}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	_, err = ctx.API.MessagesSendMessage(ctx.Context(), req)
	return err
}

// ---------------------------------------------------------------- manage

func (s *service) cmdAdd(ctx *plugin.CommandContext, args string) error {
	tl := ctx.Tlocal
	if strings.TrimSpace(args) == "" {
		return ctx.Edit("❌ " + tl("请提供频道链接或 @username，用 \\ 分隔多个", "Give channel links or @usernames, split by \\"))
	}
	parts := strings.Split(args, "\\")
	var added, failed int
	var errs []string
	for _, raw := range parts {
		handle := strings.TrimSpace(raw)
		if handle == "" {
			continue
		}
		info, err := s.addOne(ctx.Context(), handle)
		switch {
		case err == errDuplicate:
			errs = append(errs, handle+": "+tl("已存在", "already added"))
			failed++
		case err != nil:
			errs = append(errs, handle+": "+errText(err))
			failed++
		default:
			added++
			errs = append(errs, handle+": ✅ "+tl("已添加 ", "added ")+plugin.Escape(info.Title))
		}
	}
	out := fmt.Sprintf("✅ "+tl("成功添加 %d 个频道", "Added %d channels"), added)
	if failed > 0 {
		out = fmt.Sprintf("✅ %d · ❌ %d", added, failed)
	}
	for _, e := range errs {
		out += "\n› " + plugin.Escape(clip(e, 120))
	}
	return ctx.Edit(out)
}

var errDuplicate = fmt.Errorf("duplicate")

// addOne resolves, validates and stores one channel, discovering its
// linked discussion group for broadcasts.
func (s *service) addOne(ctx context.Context, handle string) (chatInfo, error) {
	peer, err := s.resolve(ctx, handle)
	if err != nil {
		return chatInfo{}, err
	}
	info, err := s.lookup(ctx, peer)
	if err != nil {
		return chatInfo{}, err
	}
	if !info.Megagroup && !info.Broadcast && !info.Basic {
		return info, fmt.Errorf("not a public channel or group")
	}
	linked := ""
	if info.Broadcast && !info.Megagroup {
		linked, _ = s.discoverLinkedGroup(ctx, peer) // best effort, like source
	}
	err = s.store.withLock(func(c *config) error {
		if _, ok := c.byHandle(handle); ok {
			return errDuplicate
		}
		c.Channels = append(c.Channels, channel{
			Title: info.Title, Handle: handle, LinkedGroup: linked,
			Username: info.Username, ChatID: info.ChatID,
		})
		if c.DefaultChannel == "" {
			c.DefaultChannel = handle
		}
		return nil
	})
	return info, err
}

func (s *service) cmdDelete(ctx *plugin.CommandContext, args string) error {
	tl := ctx.Tlocal
	if strings.TrimSpace(args) == "" {
		return ctx.Edit("❌ " + tl("用法: postsearch del <频道|序号> 或 postsearch del all", "Usage: postsearch del <channel|#> or postsearch del all"))
	}
	if strings.EqualFold(strings.TrimSpace(args), "all") {
		var n int
		err := s.store.withLock(func(c *config) error {
			n = len(c.Channels)
			c.Channels = nil
			c.DefaultChannel = ""
			return nil
		})
		if err != nil {
			return ctx.Edit(fail(tl, err))
		}
		return ctx.Edit(fmt.Sprintf("✅ "+tl("已清空 %d 个频道", "Removed all %d channels"), n))
	}
	var removed []channel
	err := s.store.withLock(func(c *config) error {
		tokens := strings.FieldsFunc(args, func(r rune) bool { return r == ' ' || r == '\\' })
		removed = c.removeChannels(expandIndexes(c, tokens))
		return nil
	})
	if err != nil {
		return ctx.Edit(fail(tl, err))
	}
	if len(removed) == 0 {
		return ctx.Edit("❓ " + tl("未找到指定的频道或序号", "No such channel or index"))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✅ %s:\n", tl("已移除", "Removed"))
	for _, ch := range removed {
		b.WriteString("› " + plugin.Escape(ch.label()) + "\n")
	}
	return ctx.Edit(strings.TrimRight(b.String(), "\n"))
}

func (s *service) cmdDefault(ctx *plugin.CommandContext, args string) error {
	tl := ctx.Tlocal
	handle := strings.TrimSpace(args)
	if handle == "" {
		return ctx.Edit("❌ " + tl("用法: postsearch default <频道> 或 postsearch default d", "Usage: postsearch default <channel> or postsearch default d"))
	}
	if strings.EqualFold(handle, "d") {
		err := s.store.withLock(func(c *config) error { c.DefaultChannel = ""; return nil })
		if err != nil {
			return ctx.Edit(fail(tl, err))
		}
		return ctx.Edit("✅ " + tl("默认频道已移除", "Default channel cleared"))
	}
	err := s.store.withLock(func(c *config) error {
		if _, ok := c.byHandle(handle); !ok {
			return errNotAdded
		}
		c.DefaultChannel = handle
		return nil
	})
	if err != nil {
		if err == errNotAdded {
			return ctx.Edit("❌ " + tl("请先用 postsearch add 添加此频道", "Add the channel with postsearch add first"))
		}
		return ctx.Edit(fail(tl, err))
	}
	return ctx.Edit("✅ " + fmt.Sprintf(tl("已将 %s 设为默认频道", "%s is now the default channel"), plugin.Code(handle)))
}

var errNotAdded = fmt.Errorf("not added")

func (s *service) cmdList(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	cfg := s.store.snapshot()
	if len(cfg.Channels) == 0 {
		return ctx.Edit("📭 " + tl("没有添加任何搜索频道", "No channels added yet"))
	}
	var b strings.Builder
	b.WriteString("📋 **" + tl("搜索频道列表", "Channel list") + "**\n\n")
	for i, ch := range cfg.Channels {
		mark := ""
		if ch.Handle == cfg.DefaultChannel {
			mark = " · " + tl("默认", "default")
		}
		fmt.Fprintf(&b, "%d\\. %s%s\n", i+1, plugin.Escape(ch.label()), mark)
	}
	return ctx.Edit(strings.TrimRight(b.String(), "\n"))
}

func (s *service) cmdExport(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	cfg := s.store.snapshot()
	if len(cfg.Channels) == 0 {
		return ctx.Edit("📭 " + tl("没有可导出的频道", "Nothing to export"))
	}
	dir, err := s.host.DataDir("postsearch")
	if err != nil {
		return ctx.Edit(fail(tl, err))
	}
	path := filepath.Join(dir, "channels_backup.txt")
	var b strings.Builder
	for _, ch := range cfg.Channels {
		b.WriteString(ch.Handle + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return ctx.Edit(fail(tl, err))
	}
	defer os.Remove(path)
	if err := ctx.ReplyMedia(path, "✅ "+tl("频道源已导出", "Channel list exported")); err != nil {
		return ctx.Edit(fail(tl, err))
	}
	return nil
}

func (s *service) cmdImport(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	msg, err := ctx.ReplyMessage()
	if err != nil {
		return ctx.Edit("❌ " + tl("请回复一个导出的备份文件", "Reply to an exported backup file"))
	}
	if _, ok := msg.Media.(*tg.MessageMediaDocument); !ok {
		return ctx.Edit("❌ " + tl("请回复一个导出的备份文件", "Reply to an exported backup file"))
	}
	path, err := s.host.Downloader().DownloadMedia(ctx.Context(), plugin.EventFromMessage(msg))
	if err != nil {
		return ctx.Edit(fail(tl, err))
	}
	defer os.Remove(path)
	b, err := os.ReadFile(path)
	if err != nil {
		return ctx.Edit(fail(tl, err))
	}
	var handles []string
	for _, line := range strings.Split(string(b), "\n") {
		if h := strings.TrimSpace(line); h != "" {
			handles = append(handles, h)
		}
	}
	if len(handles) == 0 {
		return ctx.Edit("❌ " + tl("备份文件无效", "Invalid backup file"))
	}
	_ = ctx.Edit(fmt.Sprintf("⚙️ "+tl("正在导入 %d 个频道…", "Importing %d channels…"), len(handles)))
	// Add first, replace last: clearing the list before adding would lose
	// the old channels when the import dies mid-way (network error on
	// handle 2 of N).
	old := s.store.snapshot()
	oldChannels, oldDefault, oldFilters := old.Channels, old.DefaultChannel, old.AdFilters
	_ = s.store.withLock(func(c *config) error {
		c.Channels = nil
		c.DefaultChannel = ""
		return nil
	})
	err = s.cmdAdd(ctx, strings.Join(handles, "\\"))
	if err != nil && len(s.store.snapshot().Channels) == 0 {
		// Nothing made it in: roll back so the old list survives.
		_ = s.store.withLock(func(c *config) error {
			c.Channels = oldChannels
			c.DefaultChannel = oldDefault
			c.AdFilters = oldFilters
			return nil
		})
	}
	return err
}

func (s *service) cmdAd(ctx *plugin.CommandContext, args string) error {
	tl := ctx.Tlocal
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return ctx.Edit("❌ " + tl("用法: postsearch ad add|del|list [关键词]", "Usage: postsearch ad add|del|list [words]"))
	}
	sub := strings.ToLower(fields[0])
	words := fields[1:]
	switch sub {
	case "list":
		cfg := s.store.snapshot()
		if len(cfg.AdFilters) == 0 {
			return ctx.Edit("📭 " + tl("当前没有广告过滤词", "No ad filter words"))
		}
		return ctx.Edit("🧹 **" + tl("广告过滤词", "Ad filter words") + "**\n\n" + plugin.Code(strings.Join(cfg.AdFilters, ", ")))
	case "add":
		if len(words) == 0 {
			return ctx.Edit("❌ " + tl("请提供关键词", "Give some words"))
		}
		err := s.store.withLock(func(c *config) error {
			c.AdFilters = append(c.AdFilters, words...)
			return nil
		})
		if err != nil {
			return ctx.Edit(fail(tl, err))
		}
		return ctx.Edit(fmt.Sprintf("✅ "+tl("已添加 %d 个过滤词", "Added %d filter words"), len(words)))
	case "del":
		if len(words) == 0 {
			return ctx.Edit("❌ " + tl("请提供关键词", "Give some words"))
		}
		var n int
		err := s.store.withLock(func(c *config) error {
			rm := map[string]bool{}
			for _, w := range words {
				rm[strings.ToLower(w)] = true
			}
			var kept []string
			for _, f := range c.AdFilters {
				if rm[strings.ToLower(f)] {
					n++
					continue
				}
				kept = append(kept, f)
			}
			c.AdFilters = kept
			return nil
		})
		if err != nil {
			return ctx.Edit(fail(tl, err))
		}
		return ctx.Edit(fmt.Sprintf("✅ "+tl("已删除 %d 个过滤词", "Removed %d filter words"), n))
	}
	return ctx.Edit("❌ " + tl("用法: postsearch ad add|del|list [关键词]", "Usage: postsearch ad add|del|list [words]"))
}
