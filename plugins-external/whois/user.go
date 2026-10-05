package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// idDataPoints calibrates the estimated registration date: (user id,
// unix timestamp) samples taken from ids.ts (2026 calibration).
var idDataPoints = [][2]int64{
	{0, 1376438400}, {50000000, 1400000000}, {150000000, 1451606400},
	{350000000, 1483228800}, {500000000, 1514764800}, {900000000, 1559347200},
	{1100000000, 1585699200}, {1450000000, 1609459200}, {2150000000, 1640995200},
	{5100000000, 1654041600}, {5600000000, 1672531200}, {6800000000, 1704067200},
	{7800000000, 1735689600}, {8500000000, 1767225600},
}

// estimateRegDate interpolates a user id over idDataPoints and renders
// YYYY-MM (the ids.ts estimate, ±2 months in practice).
func estimateRegDate(id int64) string {
	if id < 0 {
		return ""
	}
	lo, hi := idDataPoints[0], idDataPoints[len(idDataPoints)-1]
	for i := 0; i+1 < len(idDataPoints); i++ {
		if id >= idDataPoints[i][0] && id <= idDataPoints[i+1][0] {
			lo, hi = idDataPoints[i], idDataPoints[i+1]
			break
		}
	}
	ts := lo[1] + (id-lo[0])*(hi[1]-lo[1])/(hi[0]-lo[0])
	return time.Unix(ts, 0).UTC().Format("2006-01")
}

// activeUsernames merges u.Username and the collectible Usernames list,
// keeping only active entries (ids.ts getUsernames).
func activeUsernames(u *tg.User) []string {
	seen := map[string]bool{}
	var out []string
	if u.Username != "" {
		seen[u.Username] = true
		out = append(out, u.Username)
	}
	for _, un := range u.Usernames {
		if un.Username != "" && un.Active && !seen[un.Username] {
			seen[un.Username] = true
			out = append(out, un.Username)
		}
	}
	return out
}

// inputUser builds the InputUser for a user id (+ hash when known),
// yielding InputUserSelf for the logged-in account.
func inputUser(u *tg.User) tg.InputUserClass {
	if u.Self {
		return &tg.InputUserSelf{}
	}
	return &tg.InputUser{UserID: u.ID, AccessHash: u.AccessHash}
}

// renderUser builds the full user card.
func renderUser(ctx *plugin.CommandContext, u *tg.User) (string, error) {
	tl := ctx.Tlocal
	if u.Self || u.ID == ctx.SelfID {
		u = &tg.User{ID: ctx.SelfID, Self: true}
	}
	// users.getFullUser: bio, common chat count, personal channel.
	full, err := ctx.API.UsersGetFullUser(ctx.Context(), inputUser(u))
	if err != nil {
		if tgerr.Is(err, "FLOOD_WAIT") {
			return "", err
		}
		full = &tg.UsersUserFull{} // degrade to the bare card
	}
	if fu, ok := findUser(full.Users, u.ID); ok {
		u = fu
	}
	in := inputUser(u)

	var lines []string
	lines = append(lines, fmt.Sprintf("> %s %s", tl("用户ID", "User ID"), plugin.Code(u.ID)))
	if un := activeUsernames(u); len(un) > 0 {
		at := make([]string, len(un))
		for i, name := range un {
			at[i] = plugin.Code("@" + name)
		}
		lines = append(lines, fmt.Sprintf("> %s %s", tl("用户名", "Usernames"), strings.Join(at, " · ")))
	} else {
		lines = append(lines, fmt.Sprintf("> %s %s", tl("用户名", "Usernames"), tl("无", "none")))
	}
	if u.Phone != "" {
		lines = append(lines, fmt.Sprintf("> %s %s", tl("手机", "Phone"), plugin.Code(u.Phone)))
	}
	if d := estimateRegDate(u.ID); d != "" {
		lines = append(lines, fmt.Sprintf("> %s %s (±2%s)",
			tl("注册估算", "Est. registered"), plugin.Code(d), tl("月", "mo")))
	}
	lines = append(lines, fmt.Sprintf("> %s %s", tl("数据中心", "DC"), userDC(u)))
	if s, ok := userStatusText(tl, u.Status); ok {
		lines = append(lines, fmt.Sprintf("> %s %s", tl("最后上线", "Last seen"), s))
	}
	if full.FullUser.About != "" {
		lines = append(lines, fmt.Sprintf("> %s %s", tl("简介", "Bio"), plugin.Escape(oneLine(clip(full.FullUser.About, 120)))))
	}
	if n := full.FullUser.CommonChatsCount; n > 0 {
		line := fmt.Sprintf("> %s %s", tl("共同群", "Common groups"), plugin.Code(fmt.Sprintf("%d", n)))
		if names := commonChatNames(ctx, in); len(names) > 0 {
			line += " · " + strings.Join(names, "、")
		}
		lines = append(lines, line)
	}
	if pc, ok := full.FullUser.GetPersonalChannelID(); ok && pc != 0 {
		if link := personalChannelLink(full.Chats, pc); link != "" {
			lines = append(lines, fmt.Sprintf("> %s %s", tl("个人频道", "Personal channel"), link))
		}
	}
	if when, ok := joinedHere(ctx, u); ok {
		lines = append(lines, fmt.Sprintf("> %s %s", tl("入群时间", "Joined here"), plugin.Code(when)))
	}
	if badges := userBadges(tl, u); len(badges) > 0 {
		lines = append(lines, "> "+strings.Join(badges, " "))
	}
	return "👤 " + plugin.Mention(displayName(u), u.ID) + "\n\n" + strings.Join(lines, "\n"), nil
}

// findUser locates the user in a users slice; self matches by flag when
// the synthetic id lookup fails.
func findUser(users []tg.UserClass, id int64) (*tg.User, bool) {
	var self *tg.User
	for _, uc := range users {
		v, ok := uc.(*tg.User)
		if !ok {
			continue
		}
		if v.ID == id {
			return v, true
		}
		if v.Self {
			self = v
		}
	}
	return self, self != nil
}

// displayName is the user's readable name, falling back to the handle or id.
func displayName(u *tg.User) string {
	if u.Deleted {
		return "Deleted Account"
	}
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" && u.Username != "" {
		return "@" + u.Username
	}
	if name == "" {
		return fmt.Sprintf("User %d", u.ID)
	}
	return name
}

// userDC derives the data center from the profile photo's dcId.
func userDC(u *tg.User) string {
	if photo, ok := u.Photo.(*tg.UserProfilePhoto); ok && photo.DCID != 0 {
		return fmt.Sprintf("DC%d", photo.DCID)
	}
	return "-"
}

// userBadges renders the flags row, same order for every language.
func userBadges(tl func(string, string) string, u *tg.User) []string {
	var out []string
	if u.Premium {
		out = append(out, "⭐ Premium")
	}
	if u.Bot {
		out = append(out, "🤖 "+tl("机器人", "Bot"))
	}
	if u.Verified {
		out = append(out, "✅ "+tl("已验证", "Verified"))
	}
	if u.Scam {
		out = append(out, "🚩 "+tl("诈骗", "Scam"))
	}
	if u.Fake {
		out = append(out, "❌ "+tl("虚假", "Fake"))
	}
	if u.Support {
		out = append(out, "🛡 "+tl("官方支持", "Support"))
	}
	if u.Deleted {
		out = append(out, "⚰️ "+tl("已注销", "Deleted"))
	}
	return out
}

// userStatusText renders the privacy-limited last-seen status Telegram
// reports; ok=false hides the line entirely (hidden or unknown).
func userStatusText(tl func(string, string) string, s tg.UserStatusClass) (string, bool) {
	switch v := s.(type) {
	case *tg.UserStatusOnline:
		return "🟢 " + tl("在线", "online"), true
	case *tg.UserStatusOffline:
		if v.WasOnline > 0 {
			return time.Unix(int64(v.WasOnline), 0).Format("2006-01-02 15:04"), true
		}
		return tl("离线", "offline"), true
	case *tg.UserStatusRecently:
		return tl("最近", "recently"), true
	case *tg.UserStatusLastWeek:
		return tl("一周内", "last week"), true
	case *tg.UserStatusLastMonth:
		return tl("一月内", "last month"), true
	}
	return "", false
}

// commonChatNames lists up to five shared group titles.
func commonChatNames(ctx *plugin.CommandContext, in tg.InputUserClass) []string {
	res, err := ctx.API.MessagesGetCommonChats(ctx.Context(), &tg.MessagesGetCommonChatsRequest{
		UserID: in, MaxID: 0, Limit: 5,
	})
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range res.GetChats() {
		if len(out) >= 5 {
			break
		}
		switch v := c.(type) {
		case *tg.Channel:
			out = append(out, plugin.Escape(v.Title))
		case *tg.Chat:
			out = append(out, plugin.Escape(v.Title))
		case *tg.ChatForbidden:
			out = append(out, plugin.Escape(v.Title))
		}
	}
	return out
}

// personalChannelLink links the user's public personal channel when the
// full-info chats list carries it.
func personalChannelLink(chats []tg.ChatClass, channelID int64) string {
	for _, c := range chats {
		if ch, ok := c.(*tg.Channel); ok && ch.ID == channelID {
			if name := activeChannelHandle(ch); name != "" {
				return plugin.Link(plugin.Escape(ch.Title), "https://t.me/"+name)
			}
			return plugin.Escape(ch.Title)
		}
	}
	return ""
}

// joinedHere reports when the user joined the current chat, from
// channels.getParticipant (supergroups) or the messages.getFullChat
// participant list (basic groups).
func joinedHere(ctx *plugin.CommandContext, u *tg.User) (string, bool) {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return "", false
	}
	switch v := peer.(type) {
	case *tg.InputPeerChannel:
		res, err := ctx.API.ChannelsGetParticipant(ctx.Context(), &tg.ChannelsGetParticipantRequest{
			Channel:     &tg.InputChannel{ChannelID: v.ChannelID, AccessHash: v.AccessHash},
			Participant: &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash},
		})
		if err != nil {
			return "", false
		}
		if part, ok := res.Participant.(interface{ GetDate() int }); ok {
			if d := part.GetDate(); d > 0 {
				return time.Unix(int64(d), 0).Format("2006-01-02 15:04"), true
			}
		}
	case *tg.InputPeerChat:
		full, err := ctx.API.MessagesGetFullChat(ctx.Context(), v.ChatID)
		if err != nil {
			return "", false
		}
		cf, ok := full.FullChat.(*tg.ChatFull)
		if !ok {
			return "", false
		}
		parts, ok := cf.Participants.(*tg.ChatParticipants)
		if !ok {
			return "", false
		}
		for _, pc := range parts.Participants {
			if pc.GetUserID() != u.ID {
				continue
			}
			if d := chatParticipantDate(pc); d > 0 {
				return time.Unix(int64(d), 0).Format("2006-01-02 15:04"), true
			}
		}
	}
	return "", false
}

// activeChannelHandle returns the active @handle of a channel, if any.
func activeChannelHandle(ch *tg.Channel) string {
	if ch.Username != "" {
		return ch.Username
	}
	for _, un := range ch.Usernames {
		if un.Active {
			return un.Username
		}
	}
	return ""
}

// chatParticipantDate returns when a basic-group participant joined
// (0 for the creator, who has no join date).
func chatParticipantDate(pc tg.ChatParticipantClass) int {
	switch v := pc.(type) {
	case *tg.ChatParticipant:
		return v.Date
	case *tg.ChatParticipantAdmin:
		return v.Date
	}
	return 0
}

// oneLine collapses all whitespace runs into single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip truncates s to at most n runes, appending an ellipsis.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
