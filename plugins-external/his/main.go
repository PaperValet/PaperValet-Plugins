package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	defaultCount = 30
	maxCount     = 100
	previewRunes = 50
	chunkRunes   = 3800
	// maxFloodRetries bounds FLOOD_WAIT retries so an extreme rate limit
	// cannot hang the command until the host timeout.
	maxFloodRetries = 3
)

var Metadata = &plugin.PluginMetadata{
	Name:        "his",
	Description: "查看某人在群里的发言记录",
	DescEN:      "List someone's recent messages in a group",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type HisPlugin struct{}

func New() *HisPlugin { return &HisPlugin{} }

func (p *HisPlugin) Name() string        { return "his" }
func (p *HisPlugin) Description() string { return Metadata.Description }
func (p *HisPlugin) DescEN() string      { return Metadata.DescEN }

func (p *HisPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "his",
		Description: "列出用户或频道身份在本群最近的消息，带跳转链接",
		DescEN:      "List the recent messages of a user or channel identity in this group, with jump links",
		Usage:       "his [@用户名|ID] [数量]",
		UsageEN:     "his [@username|ID] [count]",
		Plugin:      p.Name(),
		Category:    "group",
		Handler:     p.handle,
	})
}

func (p *HisPlugin) Start(context.Context) error { return nil }
func (p *HisPlugin) Stop(context.Context) error  { return nil }

// query is a parsed command line. Target is empty when the replied sender
// should be used.
type query struct {
	Target string
	Count  int
}

// parseArgs reads `[target] [count]`. With a reply, a lone number is the
// count, matching TeleBox. ok=false means the arguments are malformed.
func parseArgs(args []string, isReply bool) (q query, ok bool) {
	q.Count = defaultCount
	switch len(args) {
	case 0:
		return q, isReply
	case 1:
		if n, err := strconv.Atoi(args[0]); err == nil && isReply && n > 0 {
			q.Count = min(n, maxCount)
			return q, true
		}
		q.Target = args[0]
		return q, validTarget(q.Target)
	case 2:
		n, err := strconv.Atoi(args[1])
		if err != nil || n <= 0 || !validTarget(args[0]) {
			return q, false
		}
		q.Target, q.Count = args[0], min(n, maxCount)
		return q, true
	}
	return q, false
}

func validTarget(s string) bool {
	if strings.HasPrefix(s, "@") {
		return len(s) > 1
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

// realReply returns the id of the message the command actually replies to.
// A plain message inside a forum topic carries a reply header pointing at
// the topic root; that is not a reply (same distinction as save's
// replyTarget and the host's re.go).
func realReply(ev *plugin.MessageEvent) int {
	if ev == nil || ev.Message == nil {
		return 0
	}
	hdr, ok := ev.Message.ReplyTo.(*tg.MessageReplyHeader)
	if !ok {
		return 0
	}
	id, has := hdr.GetReplyToMsgID()
	if !has || id <= 0 {
		return 0
	}
	if hdr.ForumTopic {
		if _, hasTop := hdr.GetReplyToTopID(); !hasTop {
			return 0
		}
	}
	return id
}

func (p *HisPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	q, ok := parseArgs(ctx.Args, realReply(ctx.Message) != 0)
	if !ok {
		return ctx.Edit(helpText(ctx.Tlocal))
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit(fail(ctx.Tlocal, err))
	}
	switch peer.(type) {
	case *tg.InputPeerChannel, *tg.InputPeerChat:
	default:
		return ctx.Edit("❌ " + ctx.Tlocal("只能在群组里使用", "Use this in a group"))
	}
	from, err := resolveFrom(ctx, peer, q.Target)
	if err != nil {
		return ctx.Edit(fail(ctx.Tlocal, err))
	}
	_ = edit(ctx, "⏳ "+ctx.Tlocal("正在查询…", "Searching…"))

	res, err := search(ctx.Context(), ctx.API, peer, from, q.Count)
	if err != nil {
		return ctx.Edit(fail(ctx.Tlocal, err))
	}
	mod, ok := res.AsModified()
	if !ok {
		return ctx.Edit("❌ " + ctx.Tlocal("没有找到消息", "No messages found"))
	}
	name := targetName(from, mod.GetUsers(), mod.GetChats(), q.Target)
	base := linkBase(peer, mod.GetChats())
	var lines []string
	for _, m := range mod.GetMessages() {
		text, ok := preview(ctx.Tlocal, m)
		if !ok {
			continue
		}
		line := fmt.Sprintf("%d. ", len(lines)+1)
		if base != "" {
			line += plugin.Link(text, fmt.Sprintf("%s%d", base, m.GetID()))
		} else {
			line += plugin.Escape(text)
		}
		lines = append(lines, line)
		if len(lines) >= q.Count {
			break
		}
	}
	if len(lines) == 0 {
		return ctx.Edit("📭 " + fmt.Sprintf(ctx.Tlocal("没有找到 %s 在本群的消息", "No messages from %s in this group"), plugin.Bold(name)))
	}
	head := "📜 **" + ctx.Tlocal("发言记录", "Message history") + "**\n" +
		plugin.Escape(name) + " · " + fmt.Sprintf(ctx.Tlocal("最近 %d 条", "latest %d"), len(lines))
	chunks := chunk(head, lines, chunkRunes)
	if err := edit(ctx, chunks[0]); err != nil {
		return err
	}
	for _, c := range chunks[1:] {
		if err := send(ctx, peer, c); err != nil {
			return err
		}
	}
	return nil
}

// resolveFrom turns the target (or the replied sender) into the peer that
// messages.search filters by.
func resolveFrom(ctx *plugin.CommandContext, peer tg.InputPeerClass, target string) (tg.InputPeerClass, error) {
	tl := ctx.Tlocal
	if ctx.PeerResolver == nil {
		return nil, errors.New("no peer resolver")
	}
	switch {
	case strings.HasPrefix(target, "@"):
		return ctx.PeerResolver.ResolveUsername(ctx.Context(), target[1:])
	case target != "":
		id, _ := strconv.ParseInt(target, 10, 64)
		if id == ctx.SelfID && id != 0 {
			return &tg.InputPeerSelf{}, nil
		}
		from, err := ctx.PeerResolver.ResolveFromChatID(ctx.Context(), id)
		if err != nil {
			return nil, errors.New(tl("找不到这个 ID，换成 @用户名 或回复对方的消息试试", "Unknown ID; try @username or reply to their message"))
		}
		return from, nil
	}
	reply := realReply(ctx.Message)
	if reply == 0 {
		return nil, errors.New(tl("回复一条消息，或指定 @用户名/ID", "Reply to a message, or give a @username/ID"))
	}
	msgs, _, _, err := plugin.GetMessages(ctx.Context(), ctx.API, peer, reply)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, errors.New(tl("读不到被回复的消息", "Cannot read the replied message"))
	}
	m := msgs[0]
	switch f := m.FromID.(type) {
	case *tg.PeerUser:
		if f.UserID == ctx.SelfID {
			return &tg.InputPeerSelf{}, nil
		}
		return ctx.PeerResolver.ResolveUserFromMessage(ctx.Context(), peer, m.ID, f.UserID)
	case *tg.PeerChannel:
		return ctx.PeerResolver.ResolveFromChatID(ctx.Context(), plugin.ChannelChatID(f.ChannelID))
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: f.ChatID}, nil
	}
	// No from_id: an anonymous admin posting as the group itself.
	return peer, nil
}

// search asks Telegram for the sender's messages directly, so the result
// does not depend on how busy the group is.
func search(ctx context.Context, api *tg.Client, peer, from tg.InputPeerClass, n int) (tg.MessagesMessagesClass, error) {
	req := &tg.MessagesSearchRequest{
		Peer:   peer,
		Filter: &tg.InputMessagesFilterEmpty{},
		Limit:  n,
	}
	req.SetFromID(from)
	for attempt := 0; ; attempt++ {
		res, err := api.MessagesSearch(ctx, req)
		d, ok := tgerr.AsFloodWait(err)
		if !ok || d > 30*time.Second || attempt >= maxFloodRetries {
			return res, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d + time.Second):
		}
	}
}

// targetName picks a readable name for the searched peer.
func targetName(from tg.InputPeerClass, users []tg.UserClass, chats []tg.ChatClass, fallback string) string {
	var uid, cid int64
	switch v := from.(type) {
	case *tg.InputPeerUser:
		uid = v.UserID
	case *tg.InputPeerUserFromMessage:
		uid = v.UserID
	case *tg.InputPeerChannel:
		cid = v.ChannelID
	case *tg.InputPeerChannelFromMessage:
		cid = v.ChannelID
	case *tg.InputPeerChat:
		cid = v.ChatID
	case *tg.InputPeerSelf:
		for _, u := range users {
			if v, ok := u.(*tg.User); ok && v.Self {
				return userName(v)
			}
		}
	}
	for _, u := range users {
		if v, ok := u.(*tg.User); ok && uid != 0 && v.ID == uid {
			return userName(v)
		}
	}
	for _, c := range chats {
		switch v := c.(type) {
		case *tg.Channel:
			if v.ID == cid {
				return withHandle(v.Title, v.Username)
			}
		case *tg.Chat:
			if v.ID == cid {
				return v.Title
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	if uid != 0 {
		return strconv.FormatInt(uid, 10)
	}
	return strconv.FormatInt(cid, 10)
}

func userName(u *tg.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if u.Deleted && name == "" {
		name = "Deleted Account"
	}
	if name == "" {
		name = strconv.FormatInt(u.ID, 10)
	}
	return withHandle(name, u.Username)
}

func withHandle(name, username string) string {
	if username == "" {
		return name
	}
	if name == "" {
		return "@" + username
	}
	return name + " @" + username
}

// linkBase returns the message link prefix for supergroups, or "" for basic
// groups, which have no message links.
func linkBase(peer tg.InputPeerClass, chats []tg.ChatClass) string {
	ch, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		return ""
	}
	for _, c := range chats {
		if v, ok := c.(*tg.Channel); ok && v.ID == ch.ChannelID {
			if u := v.Username; u != "" {
				return "https://t.me/" + u + "/"
			}
			for _, un := range v.Usernames {
				if un.Active {
					return "https://t.me/" + un.Username + "/"
				}
			}
		}
	}
	return fmt.Sprintf("https://t.me/c/%d/", ch.ChannelID)
}

// preview renders one message as a single short line.
func preview(tl func(string, string) string, m tg.MessageClass) (string, bool) {
	var s string
	switch v := m.(type) {
	case *tg.Message:
		s = oneLine(v.Message)
		if tag := mediaTag(tl, v.Media); tag != "" {
			s = strings.TrimSpace(tag + " " + s)
		}
		if s == "" {
			s = tl("[空消息]", "[empty]")
		}
	case *tg.MessageService:
		s = actionText(tl, v.Action)
	default:
		return "", false
	}
	return clip(s, previewRunes), true
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func mediaTag(tl func(string, string) string, media tg.MessageMediaClass) string {
	switch v := media.(type) {
	case nil, *tg.MessageMediaEmpty:
		return ""
	case *tg.MessageMediaPhoto:
		return tl("[图片]", "[photo]")
	case *tg.MessageMediaDocument:
		return documentTag(tl, v)
	case *tg.MessageMediaContact:
		return tl("[联系人]", "[contact]")
	case *tg.MessageMediaGeo, *tg.MessageMediaGeoLive:
		return tl("[位置]", "[location]")
	case *tg.MessageMediaVenue:
		return tl("[地点]", "[venue]")
	case *tg.MessageMediaPoll:
		return tl("[投票]", "[poll]")
	case *tg.MessageMediaWebPage:
		return "" // the link is already in the text
	case *tg.MessageMediaDice:
		return tl("[骰子]", "[dice]") + " " + v.Emoticon
	case *tg.MessageMediaGame:
		return tl("[游戏]", "[game]")
	case *tg.MessageMediaStory:
		return tl("[动态]", "[story]")
	case *tg.MessageMediaInvoice:
		return tl("[账单]", "[invoice]")
	case *tg.MessageMediaGiveaway, *tg.MessageMediaGiveawayResults:
		return tl("[抽奖]", "[giveaway]")
	case *tg.MessageMediaPaidMedia:
		return tl("[付费媒体]", "[paid media]")
	case *tg.MessageMediaToDo:
		return tl("[待办]", "[to-do]")
	}
	return tl("[媒体]", "[media]")
}

func documentTag(tl func(string, string) string, m *tg.MessageMediaDocument) string {
	switch {
	case m.Round:
		return tl("[视频消息]", "[video message]")
	case m.Voice:
		return tl("[语音]", "[voice]")
	}
	doc, _ := m.Document.(*tg.Document)
	var video, audio, sticker, animated bool
	if doc != nil {
		for _, a := range doc.Attributes {
			switch at := a.(type) {
			case *tg.DocumentAttributeSticker, *tg.DocumentAttributeCustomEmoji:
				sticker = true
			case *tg.DocumentAttributeAnimated:
				animated = true
			case *tg.DocumentAttributeVideo:
				if at.RoundMessage {
					return tl("[视频消息]", "[video message]")
				}
				video = true
			case *tg.DocumentAttributeAudio:
				if at.Voice {
					return tl("[语音]", "[voice]")
				}
				audio = true
			}
		}
	}
	switch {
	case sticker:
		return tl("[贴纸]", "[sticker]")
	case animated:
		return tl("[动图]", "[GIF]")
	case video || m.Video:
		return tl("[视频]", "[video]")
	case audio:
		return tl("[音频]", "[audio]")
	}
	return tl("[文件]", "[file]")
}

func actionText(tl func(string, string) string, a tg.MessageActionClass) string {
	switch v := a.(type) {
	case *tg.MessageActionPinMessage:
		return tl("[置顶消息]", "[pinned a message]")
	case *tg.MessageActionChatEditTitle:
		return tl("[修改群名] ", "[renamed the group] ") + oneLine(v.Title)
	case *tg.MessageActionChatEditPhoto:
		return tl("[修改群头像]", "[changed the group photo]")
	case *tg.MessageActionChatAddUser, *tg.MessageActionChatJoinedByLink, *tg.MessageActionChatJoinedByRequest:
		return tl("[加入群组]", "[joined the group]")
	case *tg.MessageActionChatDeleteUser:
		return tl("[离开群组]", "[left the group]")
	}
	return tl("[服务消息]", "[service message]")
}

// chunk packs head and lines into messages of at most max runes; head only
// starts the first one.
func chunk(head string, lines []string, max int) []string {
	var out []string
	cur := head + "\n\n"
	n := len([]rune(cur))
	empty := true
	for _, l := range lines {
		ln := len([]rune(l)) + 1
		if !empty && n+ln > max {
			out = append(out, strings.TrimRight(cur, "\n"))
			cur, n = "", 0
		}
		cur += l + "\n"
		n += ln
		empty = false
	}
	return append(out, strings.TrimRight(cur, "\n"))
}

func edit(ctx *plugin.CommandContext, text string) error {
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

func send(ctx *plugin.CommandContext, peer tg.InputPeerClass, text string) error {
	plain, ents := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesSendMessageRequest{Peer: peer, Message: plain, RandomID: time.Now().UnixNano(), NoWebpage: true}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	_, err := ctx.API.MessagesSendMessage(ctx.Context(), req)
	return err
}

func helpText(tl func(string, string) string) string {
	return "📜 **" + tl("发言记录", "Message history") + "**\n\n> " +
		plugin.Code("his") + " " + tl("回复某人时查看他的最近 30 条", "reply to someone to list their latest 30") + "\n> " +
		plugin.Code(tl("his <数量>", "his <count>")) + " " + tl("回复时指定条数", "same, with a count") + "\n> " +
		plugin.Code(tl("his <@用户名|ID> [数量]", "his <@username|ID> [count]")) + " " + tl("按用户名或 ID 查询", "look up by username or ID") + "\n\n" +
		fmt.Sprintf(tl("仅限群组，最多 %d 条。ID 也可以是频道身份", "Groups only, up to %d messages. The ID may be a channel identity too"), maxCount)
}

func fail(tl func(string, string) string, err error) string {
	out := "❌ **" + tl("查询失败", "Lookup failed") + "**\n\n"
	if d, ok := tgerr.AsFloodWait(err); ok {
		return out + fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "Flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		switch e.Type {
		case "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID":
			return out + tl("这个用户名不存在", "That username does not exist")
		case "CHAT_ADMIN_REQUIRED":
			return out + tl("需要管理员权限", "Admin rights are needed")
		case "CHANNEL_PRIVATE", "CHAT_FORBIDDEN":
			return out + tl("无法访问该群组", "Cannot access this group")
		case "PEER_ID_INVALID", "INPUT_USER_DEACTIVATED":
			return out + tl("找不到这个对象", "Cannot find that target")
		}
		return out + plugin.Code(e.Type)
	}
	return out + plugin.Escape(err.Error())
}
