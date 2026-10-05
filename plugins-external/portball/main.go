package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	minSeconds  = 60
	maxSeconds  = 366 * 86400 // Telegram treats longer restrictions as permanent
	deleteDelay = 5 * time.Second
)

var Metadata = &plugin.PluginMetadata{
	Name:        "portball",
	Description: "回复消息临时禁言",
	DescEN:      "Temporarily mute someone by reply",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type PortballPlugin struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New() *PortballPlugin { return &PortballPlugin{} }

func (p *PortballPlugin) Name() string        { return "portball" }
func (p *PortballPlugin) Description() string { return Metadata.Description }
func (p *PortballPlugin) DescEN() string      { return Metadata.DescEN }

func (p *PortballPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "portball",
		Description: "回复某人的消息，在超级群里禁言一段时间，到期自动解除",
		DescEN:      "Reply to someone's message to mute them in a supergroup for a while; lifted automatically",
		Usage:       "portball [理由] <时长>",
		UsageEN:     "portball [reason] <duration>",
		Plugin:      p.Name(),
		Category:    "group",
		Handler:     p.handle,
	})
}

func (p *PortballPlugin) Start(context.Context) error {
	p.lifetime()
	return nil
}

func (p *PortballPlugin) Stop(context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

// lifetime returns the plugin-scoped context used by delayed deletes.
func (p *PortballPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	return p.ctx
}

var unitSeconds = map[byte]int{'s': 1, 'm': 60, 'h': 3600, 'd': 86400}

// parseDuration reads `300`, `5m`, `1h30m` or `2d`. A bare number is
// seconds. ok=false means the text is not a duration at all.
func parseDuration(s string) (secs int, ok bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, n >= 0
	}
	seen := map[byte]bool{}
	num := 0
	digits := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			num = num*10 + int(c-'0')
			digits = true
			if num > maxSeconds*10 {
				return 0, false
			}
		case unitSeconds[c] > 0:
			if !digits || seen[c] {
				return 0, false
			}
			seen[c] = true
			secs += num * unitSeconds[c]
			if secs > maxSeconds*10 {
				return 0, false
			}
			num, digits = 0, false
		default:
			return 0, false
		}
	}
	if digits {
		return 0, false // trailing number without a unit, e.g. 1h30
	}
	return secs, true
}

// splitArgs treats the last argument as the duration and the rest as the
// reason. ok=false means no duration was given.
func splitArgs(args []string) (reason, dur string, ok bool) {
	if len(args) == 0 {
		return "", "", false
	}
	return strings.Join(args[:len(args)-1], " "), args[len(args)-1], true
}

// formatDuration renders seconds as the largest units, e.g. 1天2小时.
func formatDuration(tl func(string, string) string, secs int) string {
	parts := []struct {
		n      int
		zh, en string
	}{
		{secs / 86400, "天", "d"},
		{secs % 86400 / 3600, "小时", "h"},
		{secs % 3600 / 60, "分钟", "m"},
		{secs % 60, "秒", "s"},
	}
	sep := tl("", " ")
	var b strings.Builder
	for _, u := range parts {
		if u.n == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(sep)
		}
		b.WriteString(strconv.Itoa(u.n))
		b.WriteString(tl(u.zh, u.en))
	}
	if b.Len() == 0 {
		return tl("0秒", "0s")
	}
	return b.String()
}

func helpText(tl func(string, string) string) string {
	var b strings.Builder
	b.WriteString("🔇 **" + tl("临时禁言", "Temporary mute") + "**\n\n")
	b.WriteString("**" + tl("用法", "Usage") + "**\n")
	b.WriteString(plugin.Code(tl("portball [理由] <时长>", "portball [reason] <duration>")) + "  " +
		tl("回复目标的消息使用", "send as a reply to the target") + "\n\n")
	b.WriteString("**" + tl("时长", "Duration") + "**\n")
	b.WriteString(tl(
		"单位 s 秒 / m 分 / h 时 / d 天，可组合如 1h30m，纯数字按秒算；最短 1 分钟，最长 366 天",
		"Units s / m / h / d, combinable like 1h30m; a bare number is seconds. From 1 minute to 366 days") + "\n\n")
	b.WriteString("**" + tl("示例", "Examples") + "**\n")
	b.WriteString(plugin.Code("portball 10m") + "\n")
	b.WriteString(plugin.Code(tl("portball 刷屏 1h30m", "portball flooding 1h30m")) + "\n\n")
	b.WriteString("💡 " + tl("只能在超级群里用，需要封禁成员的管理员权限",
		"Supergroups only; needs the admin right to ban members"))
	return b.String()
}

func (p *PortballPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil || ctx.API == nil {
		return plugin.ErrNoMessage
	}
	tl := ctx.Tlocal
	if len(ctx.Args) == 1 && strings.EqualFold(ctx.Args[0], "help") {
		return ctx.Edit(helpText(tl))
	}
	reason, durText, ok := splitArgs(ctx.Args)
	if !ok {
		return ctx.Edit(helpText(tl))
	}
	secs, ok := parseDuration(durText)
	if !ok {
		return p.oops(ctx, tl("时长格式不对：", "Invalid duration: ")+plugin.Code(durText)+"\n"+
			tl("例如 300、10m、1h30m、2d", "e.g. 300, 10m, 1h30m, 2d"))
	}
	if secs < minSeconds {
		return p.oops(ctx, tl("禁言时长至少 1 分钟", "The mute must last at least 1 minute"))
	}
	if secs > maxSeconds {
		return p.oops(ctx, tl("禁言时长最多 366 天", "The mute can last 366 days at most"))
	}

	peer, err := ctx.ResolvePeer()
	if err != nil {
		return p.oops(ctx, failText(tl, err))
	}
	var channel *tg.InputPeerChannel
	switch v := peer.(type) {
	case *tg.InputPeerChannel:
		channel = v
	case *tg.InputPeerChat:
		return p.oops(ctx, tl("普通群不支持限时禁言，请先升级为超级群",
			"Basic groups have no timed mutes; upgrade to a supergroup first"))
	default:
		return p.oops(ctx, tl("只能在群组里使用", "Use this in a group"))
	}
	replyID := realReplyID(ctx.Message.Message)
	if replyID == 0 {
		return p.oops(ctx, tl("请回复要禁言的人的消息", "Reply to a message from the person to mute"))
	}
	msgs, users, chats, err := plugin.GetMessages(ctx.Context(), ctx.API, peer, replyID)
	if err != nil {
		return p.oops(ctx, failText(tl, err))
	}
	if len(msgs) == 0 {
		return p.oops(ctx, tl("被回复的消息已不存在", "The replied message is gone"))
	}
	target, err := resolveTarget(tl, msgs[0], channel, ctx.SelfID, users, chats)
	if err != nil {
		return p.oops(ctx, "❌ "+plugin.Escape(err.Error()))
	}

	until := time.Now().Add(time.Duration(secs) * time.Second)
	_, err = ctx.API.ChannelsEditBanned(ctx.Context(), &tg.ChannelsEditBannedRequest{
		Channel:      &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
		Participant:  target.peer,
		BannedRights: muteRights(int(until.Unix())),
	})
	if err != nil {
		return p.oops(ctx, failText(tl, err))
	}

	if err := send(ctx, peer, resultText(tl, target.name, secs, reason, until), replyID, target.resolver); err != nil {
		// The mute already landed; fall back to editing the command.
		return ctx.Edit(resultText(tl, target.name, secs, reason, until))
	}
	if err := ctx.Delete(); err != nil && ctx.Logger != nil {
		ctx.Logger.Warn("portball: delete command message failed", "error", err)
	}
	return nil
}

// realReplyID returns the replied message id, ignoring the implicit
// reply-to-topic-root header every forum topic message carries.
func realReplyID(msg *tg.Message) int {
	h, ok := msg.ReplyTo.(*tg.MessageReplyHeader)
	if !ok || h.ReplyToMsgID == 0 {
		return 0
	}
	if h.ForumTopic {
		if _, has := h.GetReplyToTopID(); !has {
			return 0
		}
	}
	return h.ReplyToMsgID
}

type target struct {
	peer tg.InputPeerClass
	name string // Markdown
	user tg.InputUserClass
}

// resolver lets the Markdown mention use the hash Telegram just returned.
func (t target) resolver(id int64) (tg.InputUserClass, error) {
	if u, ok := t.user.(*tg.InputUser); ok && u.UserID == id {
		return u, nil
	}
	if u, ok := t.user.(*tg.InputUserFromMessage); ok && u.UserID == id {
		return u, nil
	}
	return nil, fmt.Errorf("unknown user %d", id)
}

// resolveTarget finds who sent msg and builds the participant peer from
// the access hashes Telegram returned with it. Min entities carry no usable
// hash, so they are addressed through the message instead.
func resolveTarget(tl func(string, string) string, msg *tg.Message, group *tg.InputPeerChannel, self int64,
	users []tg.UserClass, chats []tg.ChatClass) (target, error) {
	switch from := msg.FromID.(type) {
	case *tg.PeerUser:
		if from.UserID == self {
			return target{}, errors.New(tl("不能禁言自己", "You cannot mute yourself"))
		}
		for _, u := range users {
			if user, ok := u.(*tg.User); ok && user.ID == from.UserID {
				var peer tg.InputPeerClass = user.AsInputPeer()
				var in tg.InputUserClass = user.AsInput()
				if user.Min {
					peer = &tg.InputPeerUserFromMessage{Peer: group, MsgID: msg.ID, UserID: user.ID}
					in = &tg.InputUserFromMessage{Peer: group, MsgID: msg.ID, UserID: user.ID}
				}
				return target{peer: peer, user: in, name: plugin.Mention(userName(tl, user), user.ID)}, nil
			}
		}
		return target{}, errors.New(tl("无法获取该用户信息", "Cannot load that user"))
	case *tg.PeerChannel:
		if from.ChannelID == group.ChannelID {
			return target{}, errors.New(tl("这是匿名管理员发的消息，无法禁言", "That was sent by an anonymous admin and cannot be muted"))
		}
		for _, c := range chats {
			if ch, ok := c.(*tg.Channel); ok && ch.ID == from.ChannelID {
				name := plugin.Escape(ch.Title)
				if ch.Username != "" {
					name = plugin.Link(ch.Title, "https://t.me/"+ch.Username)
				}
				var peer tg.InputPeerClass = ch.AsInputPeer()
				if ch.Min {
					peer = &tg.InputPeerChannelFromMessage{Peer: group, MsgID: msg.ID, ChannelID: ch.ID}
				}
				return target{peer: peer, name: name}, nil
			}
		}
		return target{}, errors.New(tl("无法获取该频道信息", "Cannot load that channel"))
	}
	return target{}, errors.New(tl("无法确定消息的发送者", "Cannot tell who sent that message"))
}

func userName(tl func(string, string) string, u *tg.User) string {
	if u.Deleted {
		return tl("已删除账户", "Deleted account")
	}
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" && u.Username != "" {
		name = "@" + u.Username
	}
	if name == "" {
		name = strconv.FormatInt(u.ID, 10)
	}
	return name
}

// muteRights revokes every send right until the given unix time.
func muteRights(until int) tg.ChatBannedRights {
	return tg.ChatBannedRights{
		SendMessages:    true,
		SendMedia:       true,
		SendStickers:    true,
		SendGifs:        true,
		SendGames:       true,
		SendInline:      true,
		EmbedLinks:      true,
		SendPolls:       true,
		SendPhotos:      true,
		SendVideos:      true,
		SendRoundvideos: true,
		SendAudios:      true,
		SendVoices:      true,
		SendDocs:        true,
		SendPlain:       true,
		UntilDate:       until,
	}
}

func resultText(tl func(string, string) string, name string, secs int, reason string, until time.Time) string {
	var b strings.Builder
	b.WriteString("🔇 **" + tl("已禁言", "Muted") + "**\n\n")
	b.WriteString("> " + tl("对象：", "Who: ") + name + "\n")
	b.WriteString("> " + tl("时长：", "For: ") + formatDuration(tl, secs) + "\n")
	if reason = strings.TrimSpace(reason); reason != "" {
		b.WriteString("> " + tl("理由：", "Reason: ") + plugin.Escape(reason) + "\n")
	}
	// Show the UTC offset: members in other timezones would otherwise
	// read the server-local time as their own.
	b.WriteString("> " + tl("解除：", "Until: ") + formatUntil(until))
	return b.String()
}

// formatUntil renders the lift time in server-local time with its UTC
// offset (or plain UTC), so cross-timezone readers are not misled.
func formatUntil(t time.Time) string {
	if _, off := t.Zone(); off == 0 {
		return t.Format("2006-01-02 15:04 UTC")
	}
	return t.Format("2006-01-02 15:04 (UTC-0700)")
}

func failText(tl func(string, string) string, err error) string {
	out := "❌ **" + tl("禁言失败", "Mute failed") + "**\n\n"
	if d, ok := tgerr.AsFloodWait(err); ok {
		return out + fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "Flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		switch e.Type {
		case "CHAT_ADMIN_REQUIRED", "RIGHT_FORBIDDEN", "CHAT_WRITE_FORBIDDEN":
			return out + tl("需要封禁成员的管理员权限", "The admin right to ban members is needed")
		case "USER_ADMIN_INVALID":
			return out + tl("不能禁言管理员", "Admins cannot be muted")
		case "USER_NOT_PARTICIPANT", "PARTICIPANT_ID_INVALID":
			return out + tl("对方已不在群里", "They are no longer in the group")
		case "CHANNEL_PRIVATE", "CHAT_FORBIDDEN":
			return out + tl("无法访问该群组", "Cannot access this group")
		case "USER_ID_INVALID", "PEER_ID_INVALID", "INPUT_USER_DEACTIVATED":
			return out + tl("找不到这个用户", "Cannot find that user")
		}
		return out + plugin.Code(e.Type)
	}
	return out + plugin.Escape(err.Error())
}

// oops shows an error in the command message and deletes it after 5s.
func (p *PortballPlugin) oops(ctx *plugin.CommandContext, text string) error {
	if !strings.HasPrefix(text, "❌") {
		text = "❌ " + text
	}
	if err := ctx.Edit(text); err != nil {
		return err
	}
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
			ctx.Logger.Warn("portball: delete error message failed", "error", err)
		}
	}()
	return nil
}

func send(ctx *plugin.CommandContext, peer tg.InputPeerClass, text string, replyTo int, resolve func(int64) (tg.InputUserClass, error)) error {
	plain, ents := plugin.ParseMarkdown(text, resolve)
	req := &tg.MessagesSendMessageRequest{
		Peer:      peer,
		Message:   plain,
		RandomID:  time.Now().UnixNano(),
		NoWebpage: true,
		ReplyTo:   &tg.InputReplyToMessage{ReplyToMsgID: replyTo},
	}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	_, err := ctx.API.MessagesSendMessage(ctx.Context(), req)
	return err
}
