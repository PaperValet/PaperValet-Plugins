package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// walkHistory pages through messages.getHistory from newest to oldest,
// collecting messages with Date >= from (zero = everything, up to max).
// progress is called with the count so far, throttled by the caller.
// It returns the messages plus user names seen along the way.
func walkHistory(ctx context.Context, api *tg.Client, peer tg.InputPeerClass,
	max int, from time.Time, progress func(int)) ([]*tg.Message, map[int64]string, error) {

	names := map[int64]string{}
	var out []*tg.Message
	offsetID := 0
	stopAt := int64(0)
	if !from.IsZero() {
		stopAt = from.Unix()
	}
	for len(out) < max {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		var page tg.MessagesMessagesClass
		err := retry(ctx, func() error {
			var err error
			page, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer:     peer,
				OffsetID: offsetID,
				Limit:    historyPage,
			})
			return err
		})
		if err != nil {
			return nil, nil, err
		}
		mod, ok := page.AsModified()
		if !ok {
			break
		}
		collectNames(mod.GetUsers(), names)
		msgs := mod.GetMessages()
		if len(msgs) == 0 {
			break
		}
		done := false
		for _, m := range msgs {
			msg, ok := m.(*tg.Message)
			if !ok {
				continue // MessageEmpty / MessageService
			}
			if stopAt > 0 && int64(msg.Date) < stopAt {
				done = true
				break
			}
			out = append(out, msg)
			if len(out) >= max {
				done = true
				break
			}
		}
		if progress != nil {
			progress(len(out))
		}
		if done {
			break
		}
		// Next page: strictly older than the oldest id we saw.
		next := 0
		for _, m := range msgs {
			if id := m.GetID(); id > 0 && (next == 0 || id < next) {
				next = id
			}
		}
		if next == 0 || next == offsetID {
			break
		}
		offsetID = next
	}
	return out, names, nil
}

// collectNames records display names of users returned with a page.
func collectNames(users []tg.UserClass, names map[int64]string) {
	for _, u := range users {
		v, ok := u.(*tg.User)
		if !ok {
			continue
		}
		n := strings.TrimSpace(v.FirstName + " " + v.LastName)
		if n == "" && v.Username != "" {
			n = "@" + v.Username
		}
		if n != "" {
			names[v.ID] = n
		}
	}
}

// chatTitle resolves the chat's display name from the API or the resolver.
func chatTitle(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, names map[int64]string, selfID int64) string {
	switch v := peer.(type) {
	case *tg.InputPeerUser:
		if selfID != 0 && v.UserID == selfID {
			if n, ok := names[selfID]; ok {
				return n
			}
		}
		if n, ok := names[v.UserID]; ok {
			return n
		}
	}
	if api == nil {
		return ""
	}
	switch pr := peer.(type) {
	case *tg.InputPeerChannel:
		ch, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{
			&tg.InputChannel{ChannelID: pr.ChannelID, AccessHash: pr.AccessHash},
		})
		if err == nil {
			for _, c := range ch.GetChats() {
				if title := chatTitleOf(c); title != "" {
					return title
				}
			}
		}
	case *tg.InputPeerChat:
		cs, err := api.MessagesGetChats(ctx, []int64{pr.ChatID})
		if err == nil {
			for _, c := range cs.GetChats() {
				if title := chatTitleOf(c); title != "" {
					return title
				}
			}
		}
	}
	if u, ok := peer.(*tg.InputPeerUser); ok {
		users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{
			&tg.InputUser{UserID: u.UserID, AccessHash: u.AccessHash},
		})
		if err == nil {
			for _, usr := range users {
				if v, ok := usr.(*tg.User); ok && v.ID == u.UserID {
					n := strings.TrimSpace(v.FirstName + " " + v.LastName)
					if n == "" {
						n = v.Username
					}
					return n
				}
			}
		}
	}
	return ""
}

func chatTitleOf(c tg.ChatClass) string {
	switch v := c.(type) {
	case *tg.Channel:
		return v.Title
	case *tg.Chat:
		return v.Title
	case *tg.ChannelForbidden:
		return v.Title
	}
	return ""
}

// retry runs fn, sleeping through short flood waits.
func retry(ctx context.Context, fn func() error) error {
	for {
		err := fn()
		d, ok := tgerr.AsFloodWait(err)
		if !ok || d > maxFloodWait {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d + time.Second):
		}
	}
}

// errText renders an API error bilingually, flood waits first.
func errText(tl func(string, string) string, err error) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		n := int(d.Seconds())
		if n < 1 {
			n = 1
		}
		return tl(fmt.Sprintf("请求过于频繁，请 %d 秒后重试", n), fmt.Sprintf("flood wait, retry in %d seconds", n))
	}
	s := err.Error()
	if strings.Contains(s, "CHANNEL_PRIVATE") || strings.Contains(s, "CHAT_FORBIDDEN") {
		return tl("无法访问该会话", "cannot access this chat")
	}
	if strings.Contains(s, "CHAT_ADMIN_REQUIRED") {
		return tl("需要管理员权限", "missing admin rights")
	}
	return plugin.Escape(trimErr(s))
}

func trimErr(s string) string {
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
