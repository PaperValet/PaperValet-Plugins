package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const maxFloodWait = 120 * time.Second

// pluginResolver matches plugin.PeerResolver so tests can stub it.
type pluginResolver interface {
	ResolveFromChatID(ctx context.Context, chatID int64) (tg.InputPeerClass, error)
	ResolveUsername(ctx context.Context, username string) (tg.InputPeerClass, error)
}

// chatInfo carries everything the forwarder and command layer need about a peer.
type chatInfo struct {
	Peer   tg.InputPeerClass
	ChatID int64 // PaperValet chat id form
	Title  string
	IsUser bool
}

// label returns a readable display name, falling back to the chat id.
func (c *chatInfo) label() string {
	if c == nil {
		return "?"
	}
	if c.Title != "" {
		return c.Title
	}
	return strconv.FormatInt(c.ChatID, 10)
}

// resolveTarget turns a user-supplied target spec into a chatInfo.
// "me"/"here" resolves to the current chat. Numeric ids are chat ids in the
// PaperValet form (users, -group, -100channel); @names go through the
// username resolver.
func resolveTarget(ctx context.Context, r pluginResolver, input string, currentChatID, selfID int64) (*chatInfo, error) {
	t := strings.TrimSpace(input)
	if t == "" {
		return nil, errors.New("empty target")
	}
	low := strings.ToLower(t)
	switch low {
	case "me", "saved", "here", "self":
		if low == "me" || low == "saved" || low == "self" {
			if selfID != 0 {
				return &chatInfo{Peer: &tg.InputPeerSelf{}, ChatID: selfID, IsUser: true, Title: "Saved Messages"}, nil
			}
		}
		if currentChatID == 0 {
			return nil, errors.New("no current chat")
		}
		return resolveChatID(ctx, r, currentChatID)
	}
	if r == nil {
		return nil, errors.New("no peer resolver")
	}
	if strings.HasPrefix(t, "@") {
		if len(t) < 2 {
			return nil, fmt.Errorf("bad username %q", t)
		}
		peer, err := r.ResolveUsername(ctx, t[1:])
		if err != nil {
			return nil, err
		}
		return infoOf(peer), nil
	}
	if id, err := strconv.ParseInt(t, 10, 64); err == nil {
		if id == selfID && id != 0 {
			return &chatInfo{Peer: &tg.InputPeerSelf{}, ChatID: selfID, IsUser: true, Title: "Saved Messages"}, nil
		}
		return resolveChatID(ctx, r, id)
	}
	return nil, fmt.Errorf("invalid target %q", t)
}

// resolveChatID resolves a chat id into an input peer with metadata.
func resolveChatID(ctx context.Context, r pluginResolver, chatID int64) (*chatInfo, error) {
	if r == nil {
		return nil, errors.New("no peer resolver")
	}
	peer, err := r.ResolveFromChatID(ctx, chatID)
	if err != nil {
		return nil, err
	}
	return infoOf(peer), nil
}

// infoOf derives a chatInfo from an already-resolved input peer.
func infoOf(peer tg.InputPeerClass) *chatInfo {
	ci := &chatInfo{Peer: peer}
	switch v := peer.(type) {
	case *tg.InputPeerUser:
		ci.ChatID, ci.IsUser = v.UserID, true
	case *tg.InputPeerSelf:
		ci.IsUser = true
	case *tg.InputPeerChat:
		ci.ChatID = -v.ChatID
	case *tg.InputPeerChannel:
		ci.ChatID = pluginChannelChatID(v.ChannelID)
	case *tg.InputPeerUserFromMessage:
		ci.ChatID, ci.IsUser = v.UserID, true
	case *tg.InputPeerChannelFromMessage:
		ci.ChatID = pluginChannelChatID(v.ChannelID)
	}
	return ci
}

func pluginChannelChatID(id int64) int64 { return -1000000000000 - id }

// fetchTitle fills the display name of a resolved peer from the API. It is
// best-effort; failures leave the chat id as the label.
func fetchTitle(ctx context.Context, api *tg.Client, ci *chatInfo) {
	if api == nil || ci == nil {
		return
	}
	switch v := ci.Peer.(type) {
	case *tg.InputPeerChannel:
		res, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{
			&tg.InputChannel{ChannelID: v.ChannelID, AccessHash: v.AccessHash},
		})
		if err != nil {
			return
		}
		for _, c := range res.GetChats() {
			if ch, ok := c.(*tg.Channel); ok && pluginChannelChatID(ch.ID) == ci.ChatID {
				ci.Title = ch.Title
			}
		}
	case *tg.InputPeerChat:
		res, err := api.MessagesGetChats(ctx, []int64{v.ChatID})
		if err != nil {
			return
		}
		for _, c := range res.GetChats() {
			if ch, ok := c.(*tg.Chat); ok && -ch.ID == ci.ChatID {
				ci.Title = ch.Title
			}
		}
	case *tg.InputPeerUser:
		users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{
			&tg.InputUser{UserID: v.UserID, AccessHash: v.AccessHash},
		})
		if err != nil {
			return
		}
		for _, u := range users {
			if usr, ok := u.(*tg.User); ok && usr.ID == ci.ChatID {
				ci.Title = strings.TrimSpace(usr.FirstName + " " + usr.LastName)
			}
		}
	case *tg.InputPeerSelf:
		ci.Title = "Saved Messages"
	}
}

// mediaType classifies a message like the TeleBox source does.
func mediaType(m *tg.Message) string {
	switch m.Media.(type) {
	case *tg.MessageMediaPhoto:
		return "photo"
	case *tg.MessageMediaDocument:
		d, ok := m.Media.(*tg.MessageMediaDocument).Document.(*tg.Document)
		if !ok {
			return "document"
		}
		kind := "document"
		for _, a := range d.Attributes {
			switch at := a.(type) {
			case *tg.DocumentAttributeSticker:
				return "sticker"
			case *tg.DocumentAttributeVideo:
				kind = "video"
			case *tg.DocumentAttributeAudio:
				if at.Voice {
					return "voice"
				}
				kind = "audio"
			case *tg.DocumentAttributeAnimated:
				return "animation"
			}
		}
		return kind
	case *tg.MessageMediaGeo, *tg.MessageMediaGeoLive, *tg.MessageMediaVenue:
		return "text"
	case *tg.MessageMediaContact:
		return "text"
	default:
		return "text"
	}
}

// forwardMessages forwards ids from src to dst via raw gotd. topicID > 0
// targets a forum topic. It waits out short FLOOD_WAITs.
func forwardMessages(ctx context.Context, api *tg.Client, src, dst tg.InputPeerClass, ids []int, silent bool, topicID int) error {
	if len(ids) == 0 {
		return nil
	}
	for start := 0; start < len(ids); start += 100 {
		end := min(start+100, len(ids))
		batch := ids[start:end]
		req := &tg.MessagesForwardMessagesRequest{
			FromPeer: src,
			ID:       batch,
			RandomID: randomIDs(len(batch)),
			ToPeer:   dst,
			Silent:   silent,
		}
		if topicID > 0 {
			req.SetTopMsgID(topicID)
		}
		if err := callWithFloodWait(ctx, func() error {
			_, err := api.MessagesForwardMessages(ctx, req)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// callWithFloodWait retries f while Telegram asks for short waits.
func callWithFloodWait(ctx context.Context, f func() error) error {
	for attempt := 0; ; attempt++ {
		err := f()
		d, ok := tgerr.AsFloodWait(err)
		if !ok || attempt >= 3 || d > maxFloodWait {
			return err
		}
		if err := sleepCtx(ctx, d+time.Second); err != nil {
			return err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func randomIDs(n int) []int64 {
	ids := make([]int64, n)
	now := time.Now().UnixNano()
	for i := range ids {
		now += int64(i + 1)
		ids[i] = now
	}
	return ids
}

// ---------------------------------------------------------------- album grouping

// albumKey identifies a media group: chat id + grouped id.
type albumKey struct {
	chatID int64
	group  int64
}

// albumBuffer collects messages of one media group until the group is
// complete or the timer fires, then forwards them together.
type albumBuffer struct {
	ids      []int
	timer    *time.Timer
	deadline time.Time
}

// albumForwarder batches per-group messages.
type albumForwarder struct {
	mu      chanMutex
	buffers map[albumKey]*albumBuffer
	// fire forwards the collected ids of a group.
	fire func(key albumKey, ids []int)
}

type chanMutex struct{ c chan struct{} }

func (m *chanMutex) Lock()   { <-m.c }
func (m *chanMutex) Unlock() { m.c <- struct{}{} }

func newAlbumForwarder(fire func(albumKey, []int)) *albumForwarder {
	return &albumForwarder{
		mu:      chanMutex{c: make(chan struct{}, 1)},
		buffers: map[albumKey]*albumBuffer{},
		fire:    fire,
	}
}

// add schedules one message of a media group. The group is forwarded when no
// further message arrives within albumDelay.
func (a *albumForwarder) add(key albumKey, id int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.buffers[key]
	if !ok {
		b = &albumBuffer{}
		a.buffers[key] = b
	}
	if !containsInt(b.ids, id) {
		b.ids = append(b.ids, id)
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(albumDelay, func() { a.flush(key) })
}

// flush forwards and clears one group's buffer.
func (a *albumForwarder) flush(key albumKey) {
	a.mu.Lock()
	b, ok := a.buffers[key]
	if ok {
		delete(a.buffers, key)
	}
	if b != nil && b.timer != nil {
		b.timer.Stop()
	}
	var ids []int
	if b != nil {
		ids = b.ids
	}
	a.mu.Unlock()
	if len(ids) > 0 && a.fire != nil {
		sort.Ints(ids)
		a.fire(key, ids)
	}
}

// stop flushes all pending groups.
func (a *albumForwarder) stop() {
	a.mu.Lock()
	keys := make([]albumKey, 0, len(a.buffers))
	for k := range a.buffers {
		keys = append(keys, k)
	}
	a.mu.Unlock()
	for _, k := range keys {
		a.flush(k)
	}
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
