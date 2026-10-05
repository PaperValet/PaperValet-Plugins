package main

import (
	"fmt"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	kickedPage     = 200
	kickedScanMax  = 10000 // safety cap on scanned banned entries
	unbanThrottle  = 500 * time.Millisecond
	emptyResultTTL = 3 * time.Second
	unblockWaitDel = 5 * time.Second
)

// unblockMember unbans the entities banned in the current group. Without
// `all` it only touches entries the account itself kicked.
func (p *CleanPlugin) unblockMember(j *job, all bool) {
	tl := j.Tlocal
	_ = j.Edit("🔓 **" + tl("群组解封", "Unban in group") + "**\n\n⏳ " +
		tl("正在获取封禁列表…", "Fetching the ban list…"))

	type bannedEntry struct {
		peer   tg.InputPeerClass // input peer for editBanned
		kicked int64             // who banned it
		kind   string            // user / channel / chat
		name   string            // display name for failures
	}
	var entries []bannedEntry
	scanned := 0
	offset := 0
	for offset < kickedScanMax {
		if err := j.Context().Err(); err != nil {
			return
		}
		var res tg.ChannelsChannelParticipantsClass
		err := retry(j.Context(), func() error {
			r, e := j.API.ChannelsGetParticipants(j.Context(), &tg.ChannelsGetParticipantsRequest{
				Channel: channelOf(j.peer),
				Filter:  &tg.ChannelParticipantsKicked{Q: ""},
				Offset:  offset,
				Limit:   kickedPage,
				Hash:    0,
			})
			res = r
			return e
		})
		if err != nil {
			p.finishErr(j.CommandContext, err)
			return
		}
		cp, ok := res.(*tg.ChannelsChannelParticipants)
		if !ok {
			break
		}
		if len(cp.Participants) == 0 {
			break
		}
		users := map[int64]*tg.User{}
		for _, u := range cp.Users {
			if v, ok := u.(*tg.User); ok {
				users[v.ID] = v
			}
		}
		chats := map[int64]*tg.Channel{}
		for _, c := range cp.Chats {
			if v, ok := c.(*tg.Channel); ok {
				chats[v.ID] = v
			}
		}
		for _, pc := range cp.Participants {
			b, ok := pc.(*tg.ChannelParticipantBanned)
			if !ok {
				continue
			}
			scanned++
			var e bannedEntry
			switch peer := b.Peer.(type) {
			case *tg.PeerUser:
				e.kind = "user"
				u := users[peer.UserID]
				if u == nil {
					continue // no access hash: skip like the source's entity lookup
				}
				e.peer = &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}
				e.kicked = b.KickedBy
				e.name = displayUser(u)
			case *tg.PeerChannel:
				e.kind = "channel"
				c := chats[peer.ChannelID]
				if c == nil {
					continue
				}
				e.peer = &tg.InputPeerChannel{ChannelID: c.ID, AccessHash: c.AccessHash}
				e.kicked = b.KickedBy
				e.name = c.Title
			case *tg.PeerChat:
				e.kind = "chat"
				c := chats[peer.ChatID]
				if c == nil {
					continue
				}
				e.peer = &tg.InputPeerChat{ChatID: peer.ChatID}
				e.kicked = b.KickedBy
				e.name = c.Title
			}
			if !all && e.kicked != j.SelfID {
				continue
			}
			entries = append(entries, e)
		}
		if len(cp.Participants) < kickedPage {
			break
		}
		offset += kickedPage
		sleepCtx(j.Context(), 100*time.Millisecond)
	}

	if len(entries) == 0 {
		p.finish(j.CommandContext, "ℹ️ "+tl("没有找到需要解封的实体", "No banned entities to unban"), emptyResultTTL)
		return
	}

	_ = j.Edit(fmt.Sprintf("🔓 **%s**\n\n⚡ %s %d %s…",
		tl("群组解封", "Unban in group"), tl("正在解封", "Unbanning"), len(entries), tl("个实体", "entities")))

	var ok, failed int
	stats := map[string]int{"user": 0, "channel": 0, "chat": 0}
	var failures []string
	for _, e := range entries {
		if err := j.Context().Err(); err != nil {
			return
		}
		stats[e.kind]++
		err := retry(j.Context(), func() error {
			_, er := j.API.ChannelsEditBanned(j.Context(), &tg.ChannelsEditBannedRequest{
				Channel:      channelOf(j.peer),
				Participant:  e.peer,
				BannedRights: tg.ChatBannedRights{},
			})
			return er
		})
		if err == nil {
			ok++
		} else if fatalErr(err) {
			p.finishErr(j.CommandContext, err)
			return
		} else {
			failed++
			if len(failures) < 10 {
				failures = append(failures, fmt.Sprintf("%s[%s](%d)", e.name, e.kind, peerIDOf(e.peer)))
			}
		}
		sleepCtx(j.Context(), unbanThrottle)
	}

	s := fmt.Sprintf("✅ **%s**\n\n", tl("群组解封完成", "Unban finished"))
	s += statsLine(tl, stats)
	s += fmt.Sprintf("\n> %s %d", tl("成功解封", "Unbanned"), ok)
	if failed > 0 {
		s += fmt.Sprintf(" · %s %d", tl("失败", "failed"), failed)
	}
	if len(failures) > 0 {
		s += "\n\n" + tl("失败列表：", "Failures:")
		for _, f := range failures {
			s += "\n• " + plugin.Escape(f)
		}
	}
	p.finish(j.CommandContext, s, unblockWaitDel)
}

func statsLine(tl func(string, string) string, stats map[string]int) string {
	var parts []string
	if n := stats["user"]; n > 0 {
		parts = append(parts, fmt.Sprintf("👤 %s %d", tl("用户", "Users"), n))
	}
	if n := stats["channel"]; n > 0 {
		parts = append(parts, fmt.Sprintf("📢 %s %d", tl("频道", "Channels"), n))
	}
	if n := stats["chat"]; n > 0 {
		parts = append(parts, fmt.Sprintf("💬 %s %d", tl("群组", "Groups"), n))
	}
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += " · "
		}
		out += part
	}
	return "> " + out
}

func displayUser(u *tg.User) string {
	name := u.FirstName
	if u.LastName != "" {
		name += " " + u.LastName
	}
	if name == "" {
		if u.Username != "" {
			name = "@" + u.Username
		} else {
			name = fmt.Sprintf("%d", u.ID)
		}
	}
	return name
}

// peerIDOf extracts the raw id an input peer addresses.
func peerIDOf(peer tg.InputPeerClass) int64 {
	switch v := peer.(type) {
	case *tg.InputPeerUser:
		return v.UserID
	case *tg.InputPeerChannel:
		return v.ChannelID
	case *tg.InputPeerChat:
		return v.ChatID
	}
	return 0
}

// channelOf converts the job's peer into an InputChannel; InputPeerChat has
// no channel form and returns nil (the caller must not run it there).
func channelOf(peer tg.InputPeerClass) *tg.InputChannel {
	if c, ok := peer.(*tg.InputPeerChannel); ok {
		return &tg.InputChannel{ChannelID: c.ChannelID, AccessHash: c.AccessHash}
	}
	return &tg.InputChannel{}
}
