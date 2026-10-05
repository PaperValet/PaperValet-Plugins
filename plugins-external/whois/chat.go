package main

import (
	"fmt"
	"strings"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// renderChat builds the channel/supergroup/basic-group card.
func renderChat(ctx *plugin.CommandContext, peer tg.InputPeerClass) (string, error) {
	tl := ctx.Tlocal
	switch v := peer.(type) {
	case *tg.InputPeerChannel:
		return renderChannel(ctx, &tg.InputChannel{ChannelID: v.ChannelID, AccessHash: v.AccessHash})
	case *tg.InputPeerChat:
		return renderBasicChat(ctx, v.ChatID)
	}
	return "", fmt.Errorf("%s", tl("只能在群组或频道里使用", "Use this in a group or channel"))
}

// renderChannel is the supergroup/channel card via channels.getFullChannel.
func renderChannel(ctx *plugin.CommandContext, in *tg.InputChannel) (string, error) {
	tl := ctx.Tlocal
	full, err := ctx.API.ChannelsGetFullChannel(ctx.Context(), in)
	if err != nil {
		return "", err
	}
	var ch *tg.Channel
	for _, c := range full.Chats {
		if v, ok := c.(*tg.Channel); ok && v.ID == in.ChannelID {
			ch = v
			break
		}
	}
	if ch == nil {
		return "", fmt.Errorf("%s", tl("读不到频道信息", "Cannot read the channel info"))
	}
	cf, ok := full.FullChat.(*tg.ChannelFull)
	if !ok {
		return "", fmt.Errorf("%s", tl("读不到频道详情", "Cannot read the channel details"))
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("> %s %s", tl("群ID", "Chat ID"), plugin.Code(plugin.ChannelChatID(ch.ID))))
	if handles := channelHandles(ch); len(handles) > 0 {
		lines = append(lines, fmt.Sprintf("> %s %s", tl("用户名", "Username"), strings.Join(handles, " · ")))
	}
	if n, ok := cf.GetParticipantsCount(); ok {
		label := tl("成员", "Members")
		if ch.Broadcast {
			label = tl("订阅者", "Subscribers")
		}
		lines = append(lines, fmt.Sprintf("> %s %s", label, plugin.Code(fmt.Sprintf("%d", n))))
	}
	if n, ok := cf.GetAdminsCount(); ok && n > 0 {
		lines = append(lines, fmt.Sprintf("> %s %s", tl("管理员", "Admins"), plugin.Code(fmt.Sprintf("%d", n))))
	}
	if n, ok := cf.GetOnlineCount(); ok && n > 0 && !ch.Broadcast {
		lines = append(lines, fmt.Sprintf("> %s %s", tl("在线", "Online"), plugin.Code(fmt.Sprintf("%d", n))))
	}
	if cf.About != "" {
		lines = append(lines, fmt.Sprintf("> %s %s", tl("简介", "About"), plugin.Escape(oneLine(clip(cf.About, 120)))))
	}
	if id, ok := cf.GetLinkedChatID(); ok && id != 0 {
		if link := linkedChatLine(ctx, full.Chats, id); link != "" {
			lines = append(lines, fmt.Sprintf("> %s %s", tl("关联讨论组", "Linked discussion"), link))
		}
	}
	if line, ok := myRoleLine(tl, ch); ok {
		lines = append(lines, line)
	}
	badges := chatBadges(tl, ch.Verified, ch.Scam, ch.Fake, ch.Megagroup)
	if len(badges) > 0 {
		lines = append(lines, "> "+strings.Join(badges, " "))
	}
	icon := "👥"
	if ch.Broadcast {
		icon = "📣"
	}
	return icon + " " + plugin.Bold(ch.Title) + "\n\n" + strings.Join(lines, "\n"), nil
}

// renderBasicChat is the legacy basic-group card via messages.getFullChat.
func renderBasicChat(ctx *plugin.CommandContext, chatID int64) (string, error) {
	tl := ctx.Tlocal
	full, err := ctx.API.MessagesGetFullChat(ctx.Context(), chatID)
	if err != nil {
		return "", err
	}
	var chat *tg.Chat
	for _, c := range full.Chats {
		switch v := c.(type) {
		case *tg.Chat:
			if v.ID == chatID {
				chat = v
			}
		case *tg.ChatForbidden:
			if v.ID == chatID {
				return "👥 " + plugin.Bold(v.Title) + "\n\n> " + tl("群组不可用", "Chat unavailable"), nil
			}
		}
	}
	if chat == nil {
		return "", fmt.Errorf("%s", tl("读不到群组信息", "Cannot read the group info"))
	}
	cf, ok := full.FullChat.(*tg.ChatFull)
	if !ok {
		return "", fmt.Errorf("%s", tl("读不到群组详情", "Cannot read the group details"))
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("> %s %s", tl("群ID", "Chat ID"), plugin.Code(-chat.ID)))
	if n := chat.ParticipantsCount; n > 0 {
		lines = append(lines, fmt.Sprintf("> %s %s", tl("成员", "Members"), plugin.Code(fmt.Sprintf("%d", n))))
	}
	if cf.About != "" {
		lines = append(lines, fmt.Sprintf("> %s %s", tl("简介", "About"), plugin.Escape(oneLine(clip(cf.About, 120)))))
	}
	if line, ok := myBasicRoleLine(tl, cf, ctx.SelfID); ok {
		lines = append(lines, line)
	}
	return "👥 " + plugin.Bold(chat.Title) + "\n\n" + strings.Join(lines, "\n"), nil
}

// channelHandles lists the channel's active @handles as code spans.
func channelHandles(ch *tg.Channel) []string {
	var out []string
	if ch.Username != "" {
		out = append(out, plugin.Code("@"+ch.Username))
	}
	for _, un := range ch.Usernames {
		if un.Active && un.Username != ch.Username {
			out = append(out, plugin.Code("@"+un.Username))
		}
	}
	return out
}

// linkedChatLine links the linked discussion group by its title.
func linkedChatLine(ctx *plugin.CommandContext, chats []tg.ChatClass, linkedID int64) string {
	for _, c := range chats {
		if ch, ok := c.(*tg.Channel); ok && ch.ID == linkedID {
			if name := activeChannelHandle(ch); name != "" {
				return plugin.Link(plugin.Escape(ch.Title), "https://t.me/"+name)
			}
			return plugin.Escape(ch.Title)
		}
	}
	return ""
}

// myRoleLine describes the account's own standing in the channel, when
// the dialog snapshot carries it.
func myRoleLine(tl func(string, string) string, ch *tg.Channel) (string, bool) {
	switch {
	case ch.Creator:
		return fmt.Sprintf("> %s %s", tl("我的身份", "My role"), tl("创建者", "creator")), true
	case ch.AdminRights != (tg.ChatAdminRights{}):
		return fmt.Sprintf("> %s %s", tl("我的身份", "My role"), tl("管理员", "admin")), true
	case ch.Left:
		return fmt.Sprintf("> %s %s", tl("我的身份", "My role"), tl("不在群里", "not a member")), true
	}
	return "", false
}

// myBasicRoleLine scans the basic-group participant list for the account.
func myBasicRoleLine(tl func(string, string) string, cf *tg.ChatFull, self int64) (string, bool) {
	parts, ok := cf.Participants.(*tg.ChatParticipants)
	if !ok {
		return "", false
	}
	for _, pc := range parts.Participants {
		if pc.GetUserID() != self {
			continue
		}
		switch pc.(type) {
		case *tg.ChatParticipantCreator:
			return fmt.Sprintf("> %s %s", tl("我的身份", "My role"), tl("创建者", "creator")), true
		case *tg.ChatParticipantAdmin:
			return fmt.Sprintf("> %s %s", tl("我的身份", "My role"), tl("管理员", "admin")), true
		}
		return fmt.Sprintf("> %s %s", tl("我的身份", "My role"), tl("成员", "member")), true
	}
	return "", false
}

// chatBadges renders the channel flags row.
func chatBadges(tl func(string, string) string, verified, scam, fake, megagroup bool) []string {
	var out []string
	if verified {
		out = append(out, "✅ "+tl("已验证", "Verified"))
	}
	if scam {
		out = append(out, "🚩 "+tl("诈骗", "Scam"))
	}
	if fake {
		out = append(out, "❌ "+tl("虚假", "Fake"))
	}
	if !megagroup && !scam && !fake {
		out = append(out, tl("频道", "Channel"))
	}
	return out
}
