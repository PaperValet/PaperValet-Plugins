package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// stickerSetShortName is the Lu Xiaoxun hour-clock sticker set. Users must
// have it installed for sends to work: https://t.me/addstickers/luxiaoxunbs
const stickerSetShortName = "luxiaoxunbs"

// hourIndex mirrors the TeleBox source: the sticker shows hour-1 (12h
// clock), stepping up one hour after the half hour; a 00:xx first half
// (hour-1 = -1) shows 11. The index is clamped by the set size.
func hourIndex(now time.Time, setLen int) int {
	if setLen <= 0 {
		return -1
	}
	h := now.Hour() - 1
	if now.Minute() > 30 {
		h++
	}
	if h == -1 {
		return 11 % setLen
	}
	h %= 12
	return h % setLen
}

// nextHourly returns the next full hour strictly after now (the source
// cron "0 * * * *" fires at :00).
func nextHourly(now time.Time) time.Time {
	n := now.Truncate(time.Hour).Add(time.Hour)
	for !n.After(now) {
		n = n.Add(time.Hour)
	}
	return n
}

// pickSticker selects the sticker for hourIdx (0 = "1 o'clock", 11 = "12"),
// clamped to the set size like the source.
func pickSticker(docs []*tg.Document, hourIdx int) *tg.Document {
	if hourIdx < 0 || len(docs) == 0 {
		return nil
	}
	return docs[hourIdx%len(docs)]
}

// loadStickerSet fetches the current sticker set as input media documents.
// It works without logging in by asking Telegram for the set by short name;
// account access to public sticker sets is not required for the *listing*,
// but a bot API client needs auth, so callers pass a real *tg.Client.
func loadStickerSet(ctx context.Context, api *tg.Client) ([]*tg.Document, string, error) {
	if api == nil {
		return nil, "", fmt.Errorf("no api client")
	}
	res, err := api.MessagesGetStickerSet(ctx, &tg.MessagesGetStickerSetRequest{
		Stickerset: &tg.InputStickerSetShortName{ShortName: stickerSetShortName},
		Hash:       0,
	})
	if err != nil {
		return nil, "", err
	}
	set, ok := res.(*tg.MessagesStickerSet)
	if !ok {
		return nil, "", fmt.Errorf("unexpected sticker set type %T", res)
	}
	var docs []*tg.Document
	for _, d := range set.Documents {
		if doc, ok := d.(*tg.Document); ok {
			docs = append(docs, doc)
		}
	}
	title := set.Set.Title
	return docs, title, nil
}

// sendStickerToChat deletes lastID (best effort) and sends the sticker
// document doc, returning the new message id. Users/chats answer
// updateShortSentMessage (no id via updates); channels answer a full
// Updates box, so both shapes are handled.
func sendStickerToChat(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, doc *tg.Document, lastID int) (int, error) {
	if lastID > 0 {
		if ch, ok := peer.(*tg.InputPeerChannel); ok {
			_, _ = api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
				Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
				ID:      []int{lastID},
			})
		} else {
			_, _ = api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{ID: []int{lastID}, Revoke: true})
		}
	}
	media := &tg.InputMediaDocument{ID: &tg.InputDocument{
		ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference,
	}}
	res, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer: peer, Media: media, RandomID: time.Now().UnixNano(),
	})
	if err != nil {
		return 0, err
	}
	return sentMessageID(res), nil
}

// sentMessageID digs the new message id out of the send result.
func sentMessageID(res tg.UpdatesClass) int {
	switch v := res.(type) {
	case *tg.UpdateShortSentMessage:
		return v.ID
	case *tg.UpdateShort:
		if m, ok := v.Update.(*tg.UpdateNewMessage); ok {
			if msg, ok := m.Message.(*tg.Message); ok {
				return msg.ID
			}
		}
	}
	var ups []tg.UpdateClass
	switch v := res.(type) {
	case *tg.Updates:
		ups = v.Updates
	case *tg.UpdatesCombined:
		ups = v.Updates
	}
	for _, u := range ups {
		var msg tg.MessageClass
		switch m := u.(type) {
		case *tg.UpdateNewMessage:
			msg = m.Message
		case *tg.UpdateNewChannelMessage:
			msg = m.Message
		default:
			continue
		}
		if m, ok := msg.(*tg.Message); ok {
			return m.ID
		}
	}
	return 0
}

// fatalSendError reports whether a send failure means the subscription is
// dead (chat gone or writes forbidden) and should be dropped, as the source
// does for CHAT_WRITE_FORBIDDEN / CHAT_NOT_FOUND.
func fatalSendError(errText string) bool {
	for _, code := range []string{
		"CHAT_WRITE_FORBIDDEN", "CHAT_NOT_FOUND", "USER_IS_BLOCKED",
		"PEER_ID_INVALID", "CHANNEL_PRIVATE", "CHAT_ID_INVALID",
	} {
		if strings.Contains(errText, code) {
			return true
		}
	}
	return false
}

// floodWait extracts a FLOOD_WAIT duration from an error, if any.
func floodWait(err error) (time.Duration, bool) {
	return tgerr.AsFloodWait(err)
}

// isAdmin reports whether a channel participant is creator or admin.
func isAdmin(p tg.ChannelParticipantClass) bool {
	switch p.(type) {
	case *tg.ChannelParticipantCreator, *tg.ChannelParticipantAdmin:
		return true
	}
	return false
}

// isChatAdmin reports whether a basic-group participant entry is the
// creator or an admin with the given user id.
func isChatAdmin(p tg.ChatParticipantClass, userID int64) bool {
	switch v := p.(type) {
	case *tg.ChatParticipantCreator:
		return v.UserID == userID
	case *tg.ChatParticipantAdmin:
		return v.UserID == userID
	}
	return false
}

// canAdminChannel reports whether userID may manage the hourly report in a
// supergroup (creator/admin), via channels.getParticipant.
func canAdminChannel(ctx context.Context, api *tg.Client, ch *tg.InputPeerChannel, userID int64) (bool, error) {
	res, err := api.ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{
		Channel:     &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
		Participant: &tg.InputPeerUser{UserID: userID},
	})
	if err != nil {
		return false, err
	}
	return isAdmin(res.Participant), nil
}

// canAdminBasicGroup reports whether userID may manage the report in a
// basic group, via messages.getFullChat (ban-plugin pattern).
func canAdminBasicGroup(ctx context.Context, api *tg.Client, chatID int64, userID int64) (bool, error) {
	full, err := api.MessagesGetFullChat(ctx, chatID)
	if err != nil {
		return false, err
	}
	fc, ok := full.FullChat.(*tg.ChatFull)
	if !ok {
		return false, fmt.Errorf("unexpected full chat type %T", full.FullChat)
	}
	ps, ok := fc.Participants.(*tg.ChatParticipants)
	if !ok {
		// ChatParticipantsForbidden: cannot verify — deny
		return false, nil
	}
	for _, p := range ps.Participants {
		if isChatAdmin(p, userID) {
			return true, nil
		}
	}
	return false, nil
}
