package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "whois",
	Description: "查看用户/群组详细信息",
	DescEN:      "Detailed user or chat info",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type WhoisPlugin struct{}

func New() *WhoisPlugin { return &WhoisPlugin{} }

func (p *WhoisPlugin) Name() string        { return "whois" }
func (p *WhoisPlugin) Description() string { return Metadata.Description }
func (p *WhoisPlugin) DescEN() string      { return Metadata.DescEN }

func (p *WhoisPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "whois",
		Description: "查询用户或群组的详细资料卡片",
		DescEN:      "Show a detailed info card of a user or chat",
		Usage:       "whois [回复 | @用户名 | ID]",
		UsageEN:     "whois [reply | @username | ID]",
		Plugin:      p.Name(),
		Category:    "info",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *WhoisPlugin) Start(context.Context) error { return nil }
func (p *WhoisPlugin) Stop(context.Context) error  { return nil }

func (p *WhoisPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil || ctx.API == nil {
		return plugin.ErrNoMessage
	}
	if len(ctx.Args) == 1 && strings.EqualFold(ctx.Args[0], "help") {
		return ctx.Edit(helpText(ctx.Tlocal))
	}
	_ = ctx.Edit("🔍 " + ctx.Tlocal("正在查询…", "Looking up…"))

	res, err := p.resolveTarget(ctx)
	if err != nil {
		return ctx.Edit(fail(ctx.Tlocal, err))
	}
	var text string
	if res.user != nil {
		text, err = renderUser(ctx, res.user)
	} else {
		text, err = renderChat(ctx, res.peer)
	}
	if err != nil {
		return ctx.Edit(fail(ctx.Tlocal, err))
	}
	return ctx.Edit(text)
}

// target is the resolved command target: exactly one of user (a personal
// user) and peer (a chat/channel input peer) is set.
type target struct {
	user *tg.User
	peer tg.InputPeerClass
}

// resolveTarget decides what to look up:
//
//  1. reply — GetMessages + FromID (PeerChannel = channel identity → whois
//     that channel; no FromID = anonymous admin → whois the current group);
//  2. @username — ResolveUsername;
//  3. numeric id — positive users, -100… channels, -… basic groups;
//  4. nothing — the account itself.
//
// Resolve failures fall through to a hint to try @ or a reply.
func (p *WhoisPlugin) resolveTarget(ctx *plugin.CommandContext) (target, error) {
	tl := ctx.Tlocal
	arg := strings.TrimSpace(ctx.GetArg(0))
	if arg == "" && realReplyID(ctx.Message.Message) != 0 {
		return p.resolveReply(ctx)
	}
	switch {
	case strings.HasPrefix(arg, "@"):
		peer, err := ctx.PeerResolver.ResolveUsername(ctx.Context(), strings.TrimPrefix(arg, "@"))
		if err != nil {
			return target{}, err
		}
		if user, ok := peerUser(peer); ok {
			return target{user: user}, nil
		}
		return target{peer: peer}, nil
	case isNumeric(arg):
		id, _ := parseID(arg)
		return p.resolveID(ctx, id)
	case arg == "":
		return target{user: &tg.User{ID: ctx.SelfID, Self: true}}, nil
	}
	return target{}, errors.New(tl(
		"无效参数：用 @用户名、数字 ID，或回复对方的消息",
		"Invalid argument: use @username, a numeric ID, or reply to their message"))
}

// resolveReply inspects the replied message sender.
func (p *WhoisPlugin) resolveReply(ctx *plugin.CommandContext) (target, error) {
	tl := ctx.Tlocal
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return target{}, err
	}
	msgs, _, _, err := plugin.GetMessages(ctx.Context(), ctx.API, peer, realReplyID(ctx.Message.Message))
	if err != nil || len(msgs) == 0 {
		return target{}, errors.New(tl("读不到被回复的消息", "Cannot read the replied message"))
	}
	m := msgs[0]
	switch f := m.FromID.(type) {
	case *tg.PeerUser:
		if f.UserID == ctx.SelfID {
			return target{user: &tg.User{ID: ctx.SelfID, Self: true}}, nil
		}
		return p.resolveID(ctx, f.UserID)
	case *tg.PeerChannel:
		return p.resolveID(ctx, plugin.ChannelChatID(f.ChannelID))
	case *tg.PeerChat:
		return target{peer: &tg.InputPeerChat{ChatID: f.ChatID}}, nil
	}
	// No FromID: an anonymous admin speaking as the group itself.
	if _, isChannel := peer.(*tg.InputPeerChannel); isChannel {
		return target{peer: peer}, nil
	}
	return target{}, errors.New(tl(
		"被回复的消息是匿名管理员或频道身份发的，无法查到用户",
		"The replied message came from an anonymous admin or channel identity"))
}

// resolveID turns a chat-id-form id into a user or chat target.
func (p *WhoisPlugin) resolveID(ctx *plugin.CommandContext, id int64) (target, error) {
	tl := ctx.Tlocal
	if id > 0 {
		if id == ctx.SelfID {
			return target{user: &tg.User{ID: ctx.SelfID, Self: true}}, nil
		}
		peer, err := ctx.PeerResolver.ResolveFromChatID(ctx.Context(), id)
		if err != nil {
			return target{}, errors.New(tl(
				"找不到这个用户，换成 @用户名 或回复对方的消息试试",
				"Unknown user; try @username or reply to their message"))
		}
		if user, ok := peerUser(peer); ok {
			return target{user: user}, nil
		}
		return target{peer: peer}, nil
	}
	peer, err := ctx.PeerResolver.ResolveFromChatID(ctx.Context(), id)
	if err != nil {
		return target{}, errors.New(tl(
			"找不到这个群组，换成 @用户名 或在群内使用试试",
			"Unknown chat; try @username or run this inside that chat"))
	}
	return target{peer: peer}, nil
}

// peerUser converts a resolver answer to a *tg.User when it is one.
func peerUser(peer tg.InputPeerClass) (*tg.User, bool) {
	switch v := peer.(type) {
	case *tg.InputPeerSelf:
		return &tg.User{ID: 0, Self: true}, true
	case *tg.InputPeerUser:
		return &tg.User{ID: v.UserID, AccessHash: v.AccessHash}, true
	case *tg.InputPeerUserFromMessage:
		return &tg.User{ID: v.UserID}, true
	}
	return nil, false
}

// isNumeric reports whether s is an optionally signed integer.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	body := strings.TrimLeft(s, "+-")
	if body == "" {
		return false
	}
	for _, r := range body {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseID converts a numeric argument to a chat-id-form id.
func parseID(s string) (int64, bool) {
	var id int64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &id); err != nil {
		return 0, false
	}
	return id, true
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
