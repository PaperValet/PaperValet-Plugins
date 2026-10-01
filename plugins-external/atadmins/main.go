package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	maxChunkLen   = 3500 // visible characters per message (reference value)
	maxChunkCount = 25   // mentions per message (reference value)
	sendInterval  = 800 * time.Millisecond
	deleteDelay   = 3 * time.Second
	pageSize      = 100
)

var Metadata = &plugin.PluginMetadata{
	Name:        "atadmins",
	Description: "一键艾特全部管理员",
	DescEN:      "Mention all group admins at once",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type AtAdminsPlugin struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New() *AtAdminsPlugin { return &AtAdminsPlugin{} }

func (p *AtAdminsPlugin) Name() string        { return "atadmins" }
func (p *AtAdminsPlugin) Description() string { return Metadata.Description }
func (p *AtAdminsPlugin) DescEN() string      { return Metadata.DescEN }

func (p *AtAdminsPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "atadmins",
		Description: "一键艾特群组内全部管理员，可附带消息，回复消息时召唤到该消息",
		DescEN:      "Mention every admin in the group, with an optional message; replies to the replied message",
		Usage:       "atadmins [消息内容] | atadmins help",
		UsageEN:     "atadmins [message] | atadmins help",
		Plugin:      p.Name(),
		Category:    "group",
		Handler:     p.handleAtAdmins,
	})
}

func (p *AtAdminsPlugin) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	return nil
}

func (p *AtAdminsPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

// lifetime returns the plugin-scoped context used by delayed jobs.
func (p *AtAdminsPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	return p.ctx
}

func helpText(ctx *plugin.CommandContext) string {
	var b strings.Builder
	b.WriteString("👮 **" + ctx.Tlocal("一键 AT 管理员", "Mention Admins") + "**\n\n")
	b.WriteString("**" + ctx.Tlocal("用法", "Usage") + "**\n")
	b.WriteString(plugin.Code("atadmins") + "  " + ctx.Tlocal("使用默认消息召唤管理员", "summon admins with the default message") + "\n")
	b.WriteString(plugin.Code(ctx.Tlocal("atadmins [消息内容]", "atadmins [message]")) + "  " + ctx.Tlocal("附带自定义消息召唤", "summon with a custom message") + "\n")
	b.WriteString(plugin.Code("atadmins help") + "  " + ctx.Tlocal("显示本帮助", "show this help") + "\n\n")
	b.WriteString("**" + ctx.Tlocal("说明", "Notes") + "**\n")
	b.WriteString(ctx.Tlocal(
		"仅限群组使用；自动排除机器人和已删除账户；超过 25 人或过长时自动分片；回复某条消息时召唤会回复到该消息；命令消息 3 秒后自动删除",
		"Groups only; bots and deleted accounts are skipped; split into several messages above 25 mentions or when too long; when used as a reply the summons reply to that message; the command message is deleted after 3 seconds") + "\n\n")
	b.WriteString("💡 " + ctx.Tlocal("示例：", "Example: ") + plugin.Code(ctx.Tlocal("atadmins 请查看置顶消息", "atadmins please check the pinned message")))
	return b.String()
}

func (p *AtAdminsPlugin) handleAtAdmins(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil || ctx.API == nil {
		return plugin.ErrNoMessage
	}
	if len(ctx.Args) > 0 {
		if sub := strings.ToLower(ctx.Args[0]); sub == "help" || sub == "h" {
			return ctx.Edit(helpText(ctx))
		}
	}

	peer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("会话解析失败: ", "Failed to resolve chat: ") + plugin.Escape(err.Error()))
	}
	switch peer.(type) {
	case *tg.InputPeerChannel, *tg.InputPeerChat:
	default:
		return ctx.Edit("❌ " + ctx.Tlocal("此命令只能在群组中使用", "This command only works in groups"))
	}

	users, err := fetchAdmins(ctx, peer)
	if err != nil {
		return ctx.Edit(fetchErrorText(ctx, err))
	}

	var (
		admins   []admin
		botCount int
		byID     = map[int64]*tg.User{}
	)
	for _, u := range users {
		if u.Deleted {
			continue
		}
		if u.Bot {
			botCount++
			continue
		}
		if _, dup := byID[u.ID]; dup {
			continue
		}
		byID[u.ID] = u
		admins = append(admins, admin{ID: u.ID, Name: displayName(u)})
	}
	if len(admins) == 0 {
		return ctx.Edit("❌ **" + ctx.Tlocal("未找到可召唤的管理员", "No admins to mention") + "**\n\n" +
			"**" + ctx.Tlocal("统计", "Stats") + "**\n" +
			ctx.Tlocal("总管理员", "Admins") + "  " + plugin.Code(len(users)) + "\n" +
			ctx.Tlocal("机器人管理员", "Bot admins") + "  " + plugin.Code(botCount) + "\n" +
			ctx.Tlocal("可召唤", "Mentionable") + "  " + plugin.Code(0) + "\n\n" +
			"💡 " + ctx.Tlocal("可能所有管理员都是机器人或已删除账户", "All admins may be bots or deleted accounts"))
	}

	say := strings.TrimSpace(ctx.RawArgs)
	if say == "" {
		say = strings.TrimSpace(strings.Join(ctx.Args, " "))
	}
	if say == "" {
		say = ctx.Tlocal("召唤本群所有管理员", "Calling all admins of this group")
	}

	resolver := func(id int64) (tg.InputUserClass, error) {
		if u, ok := byID[id]; ok {
			return u.AsInput(), nil
		}
		return nil, fmt.Errorf("unknown user %d", id)
	}

	replyTo := 0
	if ctx.Message.IsReply && ctx.Message.ReplyToID > 0 {
		replyTo = ctx.Message.ReplyToID
	}

	chunks := chunkMentions(admins, say+ctx.Tlocal("：", ": ")+"\n\n", maxChunkLen, maxChunkCount)
	for i, part := range chunks {
		if i > 0 {
			select {
			case <-ctx.Context().Done():
				return ctx.Context().Err()
			case <-time.After(sendInterval):
			}
		}
		text, entities := plugin.ParseMarkdown(part, resolver)
		req := &tg.MessagesSendMessageRequest{
			Peer:      peer,
			Message:   text,
			RandomID:  randomID(),
			NoWebpage: true,
		}
		if len(entities) > 0 {
			req.SetEntities(entities)
		}
		if replyTo > 0 {
			req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
		}
		if _, err := ctx.API.MessagesSendMessage(ctx.Context(), req); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("发送失败: ", "Send failed: ") + plugin.Escape(err.Error()))
		}
	}

	// Delete the command message after a short delay, like the reference.
	p.scheduleDelete(ctx)
	return nil
}

func (p *AtAdminsPlugin) scheduleDelete(ctx *plugin.CommandContext) {
	life := p.lifetime()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		select {
		case <-life.Done():
			return
		case <-time.After(deleteDelay):
		}
		dctx, cancel := context.WithTimeout(life, 15*time.Second)
		defer cancel()
		del := *ctx
		del.Ctx = dctx
		if err := del.Delete(); err != nil && ctx.Logger != nil {
			ctx.Logger.Warn("atadmins: delete command message failed", "error", err)
		}
	}()
}

type admin struct {
	ID   int64
	Name string
}

func displayName(u *tg.User) string {
	name := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
	if name == "" {
		name = u.Username
	}
	if name == "" {
		name = "用户"
	}
	return name
}

// chunkMentions splits mentions into Markdown messages, each starting with
// header and holding at most maxCount mentions / roughly maxLen visible
// characters. The header is plain text and gets escaped here.
func chunkMentions(admins []admin, header string, maxLen, maxCount int) []string {
	var chunks []string
	var cur strings.Builder
	curLen, count := 0, 0
	hdr := plugin.Escape(header)
	hdrLen := utf8.RuneCountInString(header)
	const sep = " , "
	for _, a := range admins {
		m := plugin.Mention(a.Name, a.ID)
		mLen := utf8.RuneCountInString(a.Name)
		addLen := mLen
		if count > 0 {
			addLen += len(sep)
		}
		if count == 0 {
			cur.WriteString(hdr)
			cur.WriteString(m)
			curLen, count = hdrLen+mLen, 1
			continue
		}
		if count >= maxCount || curLen+addLen > maxLen {
			chunks = append(chunks, cur.String())
			cur.Reset()
			cur.WriteString(hdr)
			cur.WriteString(m)
			curLen, count = hdrLen+mLen, 1
			continue
		}
		cur.WriteString(sep)
		cur.WriteString(m)
		curLen += addLen
		count++
	}
	if count > 0 {
		chunks = append(chunks, cur.String())
	}
	return chunks
}

var floodRe = regexp.MustCompile(`FLOOD_WAIT[_ (]*(\d+)`)

func fetchErrorText(ctx *plugin.CommandContext, err error) string {
	msg := err.Error()
	out := "❌ **" + ctx.Tlocal("获取管理员列表失败", "Failed to fetch admin list") + "**\n\n"
	switch {
	case strings.Contains(msg, "CHAT_ADMIN_REQUIRED"):
		out += "💡 " + ctx.Tlocal("需要管理员权限才能获取管理员列表", "Admin rights are required to list admins")
	case strings.Contains(msg, "CHANNEL_PRIVATE"):
		out += "💡 " + ctx.Tlocal("无法访问此群组的管理员信息", "Cannot access this group's admin info")
	case strings.Contains(msg, "FLOOD_WAIT"):
		wait := "60"
		if m := floodRe.FindStringSubmatch(msg); m != nil {
			wait = m[1]
		}
		out += "💡 " + ctx.Tlocal("请求过于频繁，请等待 "+wait+" 秒后重试", "Too many requests, retry in "+wait+" seconds")
	default:
		out += "💡 " + ctx.Tlocal("错误详情: ", "Details: ") + plugin.Escape(msg)
	}
	return out
}

func fetchAdmins(ctx *plugin.CommandContext, peer tg.InputPeerClass) ([]*tg.User, error) {
	switch p := peer.(type) {
	case *tg.InputPeerChannel:
		channel := &tg.InputChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash}
		var out []*tg.User
		for offset := 0; offset < 10000; {
			res, err := ctx.API.ChannelsGetParticipants(ctx.Context(), &tg.ChannelsGetParticipantsRequest{
				Channel: channel,
				Filter:  &tg.ChannelParticipantsAdmins{},
				Offset:  offset,
				Limit:   pageSize,
			})
			if err != nil {
				return nil, err
			}
			participants, ok := res.(*tg.ChannelsChannelParticipants)
			if !ok {
				break // not modified
			}
			users := map[int64]*tg.User{}
			for _, u := range participants.Users {
				if user, ok := u.(*tg.User); ok {
					users[user.ID] = user
				}
			}
			for _, part := range participants.Participants {
				var uid int64
				switch v := part.(type) {
				case *tg.ChannelParticipantAdmin:
					uid = v.UserID
				case *tg.ChannelParticipantCreator:
					uid = v.UserID
				default:
					continue
				}
				if u, ok := users[uid]; ok {
					out = append(out, u)
				}
			}
			n := len(participants.Participants)
			offset += n
			if n < pageSize || offset >= participants.Count {
				break
			}
		}
		return out, nil

	case *tg.InputPeerChat:
		full, err := ctx.API.MessagesGetFullChat(ctx.Context(), p.ChatID)
		if err != nil {
			return nil, err
		}
		chatFull, ok := full.FullChat.(*tg.ChatFull)
		if !ok {
			return nil, fmt.Errorf("unexpected full chat type %T", full.FullChat)
		}
		cps, ok := chatFull.Participants.(*tg.ChatParticipants)
		if !ok {
			return nil, errors.New("CHAT_ADMIN_REQUIRED: participant list unavailable")
		}
		adminIDs := map[int64]bool{}
		var order []int64
		for _, cp := range cps.Participants {
			switch v := cp.(type) {
			case *tg.ChatParticipantAdmin:
				adminIDs[v.UserID] = true
				order = append(order, v.UserID)
			case *tg.ChatParticipantCreator:
				adminIDs[v.UserID] = true
				order = append([]int64{v.UserID}, order...)
			}
		}
		users := map[int64]*tg.User{}
		for _, u := range full.Users {
			if user, ok := u.(*tg.User); ok && adminIDs[user.ID] {
				users[user.ID] = user
			}
		}
		var out []*tg.User
		for _, id := range order {
			if u, ok := users[id]; ok {
				out = append(out, u)
			}
		}
		return out, nil
	}
	return nil, errors.New("not a group")
}

func randomID() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.LittleEndian.Uint64(b[:]))
}
