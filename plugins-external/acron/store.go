package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const tasksFile = "tasks.json"

type storeFile struct {
	NextID int     `json:"next_id"`
	Tasks  []*Task `json:"tasks"`
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

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

// peerRef is a persisted InputPeer so restored tasks need no resolver.
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

// resolveChatRef resolves a chat argument (numeric id, "me", or @username)
// to a peer reference plus chat id, fetching a display name.
func resolveChatRef(ctx context.Context, resolver plugin.PeerResolver, api *tg.Client, chatArg string) (*peerRef, int64, string, error) {
	if resolver == nil {
		return nil, 0, "", errors.New("no peer resolver")
	}
	arg := strings.TrimSpace(chatArg)
	if strings.EqualFold(arg, "me") {
		return &peerRef{Type: "self"}, 0, "me", nil
	}
	if id, ok := parseChatID(arg); ok {
		peer, err := resolver.ResolveFromChatID(ctx, id)
		if err != nil {
			return nil, 0, "", err
		}
		if peer == nil {
			return nil, 0, "", fmt.Errorf("cannot resolve chat %s", chatArg)
		}
		return refFromPeer(peer), plugin.ChatIDOfInput(peer), peerDisplay(ctx, api, peer), nil
	}
	name := strings.TrimPrefix(arg, "@")
	if name == "" {
		return nil, 0, "", errors.New("empty chat name")
	}
	peer, err := resolver.ResolveUsername(ctx, name)
	if err != nil {
		return nil, 0, "", err
	}
	if peer == nil {
		return nil, 0, "", fmt.Errorf("cannot resolve @%s", name)
	}
	return refFromPeer(peer), plugin.ChatIDOfInput(peer), peerDisplay(ctx, api, peer), nil
}

// peerDisplay fetches the chat/user title for list output; falls back to a
// plain description when the extra request fails.
func peerDisplay(ctx context.Context, api *tg.Client, peer tg.InputPeerClass) string {
	switch v := peer.(type) {
	case *tg.InputPeerChannel:
		if api == nil {
			return ""
		}
		res, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{
			&tg.InputChannel{ChannelID: v.ChannelID, AccessHash: v.AccessHash},
		})
		if err == nil {
			for _, c := range res.GetChats() {
				if ch, ok := c.(*tg.Channel); ok {
					return ch.Title
				}
			}
		}
	case *tg.InputPeerChat:
		if api == nil {
			return ""
		}
		res, err := api.MessagesGetChats(ctx, []int64{v.ChatID})
		if err == nil {
			for _, c := range res.GetChats() {
				if ch, ok := c.(*tg.Chat); ok {
					return ch.Title
				}
			}
		}
	case *tg.InputPeerUser:
		if api == nil {
			return ""
		}
		users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{
			&tg.InputUser{UserID: v.UserID, AccessHash: v.AccessHash},
		})
		if err == nil {
			for _, u := range users {
				if usr, ok := u.(*tg.User); ok {
					return userName(usr)
				}
			}
		}
	}
	return describeInputPeer(peer)
}

func userName(u *tg.User) string {
	name := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
	if name == "" && u.Username != "" {
		name = "@" + u.Username
	}
	return name
}

// describeInputPeer derives a human display string from an input peer.
func describeInputPeer(p tg.InputPeerClass) string {
	switch v := p.(type) {
	case *tg.InputPeerUser:
		if v.UserID > 0 {
			return fmt.Sprintf("用户 %d", v.UserID)
		}
		return fmt.Sprintf("%d", v.UserID)
	case *tg.InputPeerSelf:
		return "me"
	case *tg.InputPeerChat:
		return fmt.Sprintf("群 %d", v.ChatID)
	case *tg.InputPeerChannel:
		return fmt.Sprintf("频道 %d", v.ChannelID)
	}
	return ""
}

// ---------------------------------------------------------------- entities

func encodeEntity(e tg.MessageEntityClass) (string, error) {
	var b bin.Buffer
	if err := e.Encode(&b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b.Buf), nil
}

func decodeEntity(s string) (tg.MessageEntityClass, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return tg.DecodeMessageEntity(&bin.Buffer{Buf: raw})
}

func encodeEntities(ents []tg.MessageEntityClass) []string {
	var out []string
	for _, e := range ents {
		if s, err := encodeEntity(e); err == nil {
			out = append(out, s)
		}
	}
	return out
}

func decodeEntities(in []string) []tg.MessageEntityClass {
	var out []tg.MessageEntityClass
	for _, s := range in {
		if e, err := decodeEntity(s); err == nil {
			out = append(out, e)
		}
	}
	return out
}
