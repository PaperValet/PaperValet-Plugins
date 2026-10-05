package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// ---------------------------------------------------------------- spec parsing

// targetSpec is the parsed form of "chat[|topic]".
type targetSpec struct {
	Chat    string
	TopicID int
}

// parseTargetSpec splits "chat|topic" (full-width ｜ allowed, spaces around
// the separator tolerated) and validates the topic id.
func parseTargetSpec(raw string) (targetSpec, error) {
	var spec targetSpec
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return spec, errors.New("empty target")
	}
	parts := strings.Split(strings.ReplaceAll(raw, "｜", "|"), "|")
	spec.Chat = strings.TrimSpace(parts[0])
	if spec.Chat == "" {
		return spec, errors.New("empty chat")
	}
	for _, extra := range parts[1:] {
		extra = strings.TrimSpace(extra)
		if extra == "" {
			continue
		}
		n, err := strconv.Atoi(extra)
		if err != nil || n <= 0 {
			return targetSpec{}, errors.New("invalid topic id")
		}
		if spec.TopicID == 0 {
			spec.TopicID = n
		}
	}
	return spec, nil
}

// parseID parses a numeric target id.
func parseID(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, errors.New("invalid id")
	}
	return n, nil
}

// parseCount parses the optional N argument of the forward command.
func parseCount(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, errors.New("count must be a positive integer")
	}
	return n, nil
}

// ---------------------------------------------------------------- peer persistence

// peerRef is a persisted InputPeer so targets need no resolver after add.
type peerRef struct {
	Type       string `json:"type"` // user|chat|channel|self
	ID         int64  `json:"id"`
	AccessHash int64  `json:"access_hash,omitempty"`
}

func refFromPeer(p tg.InputPeerClass) *peerRef {
	switch v := p.(type) {
	case *tg.InputPeerUser:
		return &peerRef{Type: "user", ID: v.UserID, AccessHash: v.AccessHash}
	case *tg.InputPeerChat:
		return &peerRef{Type: "chat", ID: v.ChatID}
	case *tg.InputPeerChannel:
		return &peerRef{Type: "channel", ID: v.ChannelID, AccessHash: v.AccessHash}
	case *tg.InputPeerSelf:
		return &peerRef{Type: "self"}
	}
	return nil
}

func (r *peerRef) input() tg.InputPeerClass {
	if r == nil {
		return nil
	}
	switch r.Type {
	case "user":
		return &tg.InputPeerUser{UserID: r.ID, AccessHash: r.AccessHash}
	case "chat":
		return &tg.InputPeerChat{ChatID: r.ID}
	case "channel":
		return &tg.InputPeerChannel{ChannelID: r.ID, AccessHash: r.AccessHash}
	case "self":
		return &tg.InputPeerSelf{}
	}
	return nil
}

// ---------------------------------------------------------------- entity resolve

// resolveTargetInput turns user text into an InputPeer: me/self, @username
// or a numeric chat id (channel -100…, basic group -…, bare user id).
func resolveTargetInput(ctx context.Context, r plugin.PeerResolver, selfID int64, s string) (tg.InputPeerClass, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "", "me", "self":
		return &tg.InputPeerSelf{}, nil
	}
	if strings.HasPrefix(s, "@") {
		if name := strings.TrimPrefix(s, "@"); name != "" {
			if r == nil {
				return nil, errors.New("no peer resolver")
			}
			return r.ResolveUsername(ctx, name)
		}
		return nil, errors.New("empty username")
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid chat id %q", s)
	}
	if id == selfID && selfID != 0 {
		return &tg.InputPeerSelf{}, nil
	}
	if r == nil {
		return nil, errors.New("no peer resolver")
	}
	return r.ResolveFromChatID(ctx, id)
}

// entityName builds a display name from chat/user entity lists.
func entityName(chats []tg.ChatClass, users []tg.UserClass, chatID int64) string {
	for _, c := range chats {
		switch v := c.(type) {
		case *tg.Channel:
			if plugin.ChannelChatID(v.ID) == chatID {
				return v.Title
			}
		case *tg.Chat:
			if -v.ID == chatID {
				return v.Title
			}
		case *tg.ChannelForbidden:
			if plugin.ChannelChatID(v.ID) == chatID {
				return v.Title
			}
		}
	}
	for _, u := range users {
		if v, ok := u.(*tg.User); ok && v.ID == chatID {
			name := strings.TrimSpace(v.FirstName + " " + v.LastName)
			if name == "" {
				name = v.Username
			}
			if name == "" {
				name = fmt.Sprint(v.ID)
			}
			return name
		}
	}
	return ""
}

// describePeer fetches a display name for peer (chat id form in chatID).
func describePeer(ctx context.Context, api *tg.Client, peer tg.InputPeerClass) (string, int64) {
	if api == nil || peer == nil {
		return "", 0
	}
	chatID := plugin.ChatIDOfInput(peer)
	switch v := peer.(type) {
	case *tg.InputPeerSelf:
		_ = v
	case *tg.InputPeerUser:
		users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUser{UserID: v.UserID, AccessHash: v.AccessHash}})
		if err == nil {
			if name := entityName(nil, users, v.UserID); name != "" {
				return name, v.UserID
			}
		}
	case *tg.InputPeerChannel:
		res, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: v.ChannelID, AccessHash: v.AccessHash}})
		if err == nil {
			if chats := res.GetChats(); len(chats) > 0 {
				if name := entityName(chats, nil, chatID); name != "" {
					return name, chatID
				}
			}
		}
	case *tg.InputPeerChat:
		res, err := api.MessagesGetChats(ctx, []int64{v.ChatID})
		if err == nil {
			if chats := res.GetChats(); len(chats) > 0 {
				if name := entityName(chats, nil, chatID); name != "" {
					return name, chatID
				}
			}
		}
	}
	return "", chatID
}

// resolveEntity resolves the add-target input and returns the input peer
// and a display name.
func (p *BsPlugin) resolveEntity(ctx *plugin.CommandContext, s string) (tg.InputPeerClass, string, error) {
	peer, err := resolveTargetInput(ctx.Context(), ctx.PeerResolver, ctx.SelfID, s)
	if err != nil {
		return nil, "", err
	}
	name, _ := p.describe(ctx.Context(), ctx.API, peer, ctx.Tlocal)
	return peer, name, nil
}

// resolveEntityBot is resolveEntity for the bot page context.
func (p *BsPlugin) resolveEntityBot(ctx context.Context, s string) (tg.InputPeerClass, string, error) {
	var (
		api      *tg.Client
		resolver plugin.PeerResolver
		selfID   int64
	)
	if p.host != nil {
		api, resolver, selfID = p.host.API(), p.host.PeerResolver(), p.host.SelfID()
	}
	peer, err := resolveTargetInput(ctx, resolver, selfID, s)
	if err != nil {
		return nil, "", err
	}
	name, _ := p.describe(ctx, api, peer, func(zh, en string) string { return zh })
	return peer, name, nil
}

func (p *BsPlugin) describe(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, tl func(zh, en string) string) (string, int64) {
	if _, self := peer.(*tg.InputPeerSelf); self {
		return tl("收藏夹 (me)", "Saved Messages (me)"), 0
	}
	name, chatID := describePeer(ctx, api, peer)
	if name == "" {
		return plugin.Code(fmt.Sprint(chatID)), chatID
	}
	return name, chatID
}

// ---------------------------------------------------------------- json io

// writeJSON writes atomically (temp file + rename).
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
